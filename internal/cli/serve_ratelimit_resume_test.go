package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// detBackoff is a deterministic backoff closure for the planner tests: the
// banner hint when present, else attempt*unit. No jitter, so nextPasteAt is
// exactly predictable.
func detBackoff(unit time.Duration) func(int, time.Duration) time.Duration {
	return func(attempt int, hint time.Duration) time.Duration {
		if hint > 0 {
			return hint
		}
		return time.Duration(attempt) * unit
	}
}

// TestPlanRateLimitResume_WaitsBackoffBeforePaste is the #618 AC5 mutation
// anchor: a fresh rate-limit detection does NOT paste immediately — it returns
// resumeWait and only pastes once the backoff has elapsed. Mutation: making
// first-detection return resumePaste (skip the backoff), or dropping the
// `now.Before(nextPasteAt)` guard, makes the early-observation assertions below
// flip to resumePaste and the test fails.
func TestPlanRateLimitResume_WaitsBackoffBeforePaste(t *testing.T) {
	backoff := detBackoff(time.Second)
	var rs rateLimitResumeState
	t0 := time.Unix(1_700_000_000, 0).UTC()

	// First detection: wait, no paste. nextPasteAt = t0 + backoff(1,0) = t0+1s.
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, t0, 3, backoff); got != resumeWait {
		t.Fatalf("first detection = %v, want resumeWait (must NOT paste into an un-elapsed throttle)", got)
	}
	if !rs.active || rs.attempts != 0 {
		t.Fatalf("after first detection: active=%v attempts=%d, want active=true attempts=0", rs.active, rs.attempts)
	}
	if want := t0.Add(time.Second); !rs.nextPasteAt.Equal(want) {
		t.Fatalf("nextPasteAt = %v, want %v (backoff(1) with no banner hint)", rs.nextPasteAt, want)
	}

	// Still rate-limited but BEFORE the backoff elapses: still wait, no paste.
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, t0.Add(500*time.Millisecond), 3, backoff); got != resumeWait {
		t.Fatalf("before backoff elapsed = %v, want resumeWait", got)
	}
	if rs.attempts != 0 {
		t.Fatalf("attempts bumped to %d before backoff elapsed, want 0", rs.attempts)
	}

	// At the backoff deadline: paste now.
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, t0.Add(time.Second), 3, backoff); got != resumePaste {
		t.Fatalf("at backoff deadline = %v, want resumePaste", got)
	}
	if rs.attempts != 1 {
		t.Fatalf("attempts = %d after first paste, want 1", rs.attempts)
	}
}

// TestPlanRateLimitResume_FullEpisode walks a complete episode: wait → pastes →
// bounded give-up → quiescent → recover-resets.
func TestPlanRateLimitResume_FullEpisode(t *testing.T) {
	backoff := detBackoff(time.Second)
	var rs rateLimitResumeState
	now := time.Unix(1_700_000_000, 0).UTC()
	const maxAttempts = 2

	// Detect → wait.
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, now, maxAttempts, backoff); got != resumeWait {
		t.Fatalf("detect = %v, want resumeWait", got)
	}

	// Advance to each nextPasteAt and paste, up to maxAttempts.
	for i := 1; i <= maxAttempts; i++ {
		now = rs.nextPasteAt
		if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, now, maxAttempts, backoff); got != resumePaste {
			t.Fatalf("paste %d = %v, want resumePaste", i, got)
		}
		if rs.attempts != i {
			t.Fatalf("attempts = %d after paste %d, want %d", rs.attempts, i, i)
		}
	}

	// Next eligible observation past the ceiling → give up (once), then noop.
	now = rs.nextPasteAt
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, now, maxAttempts, backoff); got != resumeGiveUp {
		t.Fatalf("past ceiling = %v, want resumeGiveUp", got)
	}
	if !rs.gaveUp {
		t.Fatalf("gaveUp = false after ceiling, want true")
	}
	if got := planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &rs, now.Add(time.Hour), maxAttempts, backoff); got != resumeNoop {
		t.Fatalf("after give-up = %v, want resumeNoop (stop pasting)", got)
	}

	// Chamber leaves rate-limited → recovered, state fully reset.
	if got := planRateLimitResume(tmuxio.StateIdle, tmuxio.Evidence{}, &rs, now.Add(2*time.Hour), maxAttempts, backoff); got != resumeRecovered {
		t.Fatalf("recover = %v, want resumeRecovered", got)
	}
	if rs != (rateLimitResumeState{}) {
		t.Fatalf("rs after recover = %+v, want zero value (episode reset)", rs)
	}
}

// TestPlanRateLimitResume_BannerHintTakesPrecedence: when the regex exposes a
// retry_seconds hint, the first wait uses it rather than the exponential floor.
func TestPlanRateLimitResume_BannerHintTakesPrecedence(t *testing.T) {
	backoff := detBackoff(time.Second)
	var rs rateLimitResumeState
	t0 := time.Unix(1_700_000_000, 0).UTC()

	planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{RetryAfter: 30 * time.Second}, &rs, t0, 3, backoff)
	if want := t0.Add(30 * time.Second); !rs.nextPasteAt.Equal(want) {
		t.Fatalf("nextPasteAt = %v, want %v (banner hint, not the 1s exponential floor)", rs.nextPasteAt, want)
	}
}

// TestPlanRateLimitResume_UsageLimitedNotResumed: a hard usage-limit (#540) is
// NOT auto-resumed — pasting "continue" can't clear a quota park. A fresh
// usage-limited observation is a noop; a usage-limit arriving mid-episode ends
// the (rate-limit) episode rather than continuing to paste.
func TestPlanRateLimitResume_UsageLimitedNotResumed(t *testing.T) {
	backoff := detBackoff(time.Second)
	now := time.Unix(1_700_000_000, 0).UTC()

	var fresh rateLimitResumeState
	if got := planRateLimitResume(tmuxio.StateUsageLimited, tmuxio.Evidence{}, &fresh, now, 3, backoff); got != resumeNoop {
		t.Fatalf("fresh usage-limited = %v, want resumeNoop (hard park, not auto-resumed)", got)
	}
	if fresh.active {
		t.Fatalf("usage-limited started an episode, want none")
	}

	// Rate-limited episode that transitions to usage-limited: episode ends.
	var mid rateLimitResumeState
	planRateLimitResume(tmuxio.StateRateLimited, tmuxio.Evidence{}, &mid, now, 3, backoff) // start episode
	if got := planRateLimitResume(tmuxio.StateUsageLimited, tmuxio.Evidence{}, &mid, now.Add(time.Minute), 3, backoff); got != resumeRecovered {
		t.Fatalf("rate→usage transition = %v, want resumeRecovered (left rate-limited)", got)
	}
	if mid != (rateLimitResumeState{}) {
		t.Fatalf("rs after rate→usage = %+v, want zero", mid)
	}
}

// TestPlanRateLimitResume_NoEpisodeWhenNeverRateLimited: an idle chamber with no
// episode is a plain noop and never starts spuriously tracking.
func TestPlanRateLimitResume_NoEpisodeWhenNeverRateLimited(t *testing.T) {
	backoff := detBackoff(time.Second)
	var rs rateLimitResumeState
	now := time.Unix(1_700_000_000, 0).UTC()
	for _, st := range []tmuxio.State{tmuxio.StateIdle, tmuxio.StateWorking, tmuxio.StateAwaitingOperator} {
		if got := planRateLimitResume(st, tmuxio.Evidence{}, &rs, now, 3, backoff); got != resumeNoop {
			t.Fatalf("observed=%v = %v, want resumeNoop", st, got)
		}
		if rs != (rateLimitResumeState{}) {
			t.Fatalf("rs mutated by non-rate-limited observed=%v: %+v", st, rs)
		}
	}
}

// TestServe_RateLimitResume_PastesContinueAndRecovers is the end-to-end wiring
// test: with the chamber visibly rate-limited and NO queued message, the
// self-observe path waits out the (tiny) backoff, pastes `continue`, increments
// the #618 attempt metric, and — once the pane leaves rate-limited — records a
// recovered outcome. The no-message setup proves auto-resume is queue-
// independent (the #613 delivery-path backoff only fires when there's a message
// to deliver). The synthetic RateLimitPattern mirrors how production keeps the
// adapter profiles empty pending real pane captures (#504/#540).
func TestServe_RateLimitResume_PastesContinueAndRecovers(t *testing.T) {
	// Shrink AgentState's temporal delta so each self-probe is fast.
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited.*?retry after (?P<retry_seconds>\d+ms)`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	var mu sync.Mutex
	rateLimited := true
	var continuePastes int
	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "#{pane_in_mode}") {
				return []byte("0"), nil
			}
			return []byte("0/0"), nil
		case "capture-pane":
			mu.Lock()
			defer mu.Unlock()
			if rateLimited {
				return []byte("Rate limited retry after 1ms"), nil
			}
			return []byte("done — back to work"), nil // non-banner → not rate-limited
		case "send-keys":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "-l") && strings.Contains(joined, rateLimitResumeText) {
				mu.Lock()
				continuePastes++
				mu.Unlock()
			}
			return nil, nil
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRunner) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	// Deliberately NO queued message — auto-resume must fire from the
	// self-observe path alone.

	m := metrics.New()
	opts := fastOpts("bob")
	opts.Metrics = m
	opts.ProviderCapDisabled = false    // capOn → the self-observe probe runs
	opts.MaxConcurrentPerProvider = 100 // cap never binds
	opts.ObservedStateInterval = time.Millisecond
	opts.RateLimitResumeMaxAttempts = 3
	stop, wait, logbuf := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	resumeLabels := func(outcome string) map[string]string {
		return map[string]string{"outcome": outcome, "agent": "bob", "provider": "anthropic"}
	}

	// 1) The continue-paste fires after the backoff, logged + metered.
	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		n := continuePastes
		mu.Unlock()
		return n >= 1 && strings.Contains(logbuf.String(), "rate_limit_resume agent=bob")
	}, "expected a continue-paste + rate_limit_resume log within 2s")
	if got := gatherCounter(t, m, "tmux_tell_rate_limit_resume_total", resumeLabels("attempt")); got < 1 {
		t.Fatalf("rate_limit_resume_total{outcome=attempt} = %v, want >= 1", got)
	}

	// 2) The pane leaves rate-limited → the episode ends with a recovered outcome.
	mu.Lock()
	rateLimited = false
	mu.Unlock()
	waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(logbuf.String(), "rate_limit_resume_recovered agent=bob")
	}, "expected rate_limit_resume_recovered after the pane left rate-limited")
	if got := gatherCounter(t, m, "tmux_tell_rate_limit_resume_total", resumeLabels("recovered")); got < 1 {
		t.Fatalf("rate_limit_resume_total{outcome=recovered} = %v, want >= 1", got)
	}
}

// TestServe_RateLimitResume_DisabledNoPaste pins the AC4 escape hatch: with
// RateLimitResumeDisabled, a visibly rate-limited chamber is never pasted into.
func TestServe_RateLimitResume_DisabledNoPaste(t *testing.T) {
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited.*?retry after (?P<retry_seconds>\d+ms)`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	var mu sync.Mutex
	var continuePastes int
	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "#{pane_in_mode}") {
				return []byte("0"), nil
			}
			return []byte("0/0"), nil
		case "capture-pane":
			return []byte("Rate limited retry after 1ms"), nil
		case "send-keys":
			joined := strings.Join(args, " ")
			if strings.Contains(joined, "-l") && strings.Contains(joined, rateLimitResumeText) {
				mu.Lock()
				continuePastes++
				mu.Unlock()
			}
			return nil, nil
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRunner) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}

	opts := fastOpts("bob")
	opts.ProviderCapDisabled = false
	opts.MaxConcurrentPerProvider = 100
	opts.ObservedStateInterval = time.Millisecond
	opts.RateLimitResumeDisabled = true // escape hatch
	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Give the self-observe probe ample time to run; assert it never pasted.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	n := continuePastes
	mu.Unlock()
	if n != 0 {
		t.Fatalf("continue-pastes = %d with resume disabled, want 0", n)
	}
}

package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/metrics"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

func TestServe_RateLimitedDefersAndReportsMetrics(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited.*?retry after (?P<retry_seconds>\d+ms)`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "#{pane_in_mode}") {
				return []byte("0"), nil
			}
			return []byte("0/0"), nil
		case "capture-pane":
			return []byte("Rate limited retry after 400ms"), nil
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRunner) })

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false
	opts.Metrics = m
	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	waitFor(t, 2*time.Second, func() bool {
		return gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds",
			map[string]string{"agent": "bob", "provider": "anthropic"}) > 0
	}, "expected rate-limited age gauge to become positive")

	labels := map[string]string{"agent": "bob", "provider": "anthropic"}
	firstAge := gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds", labels)
	firstRetry := gatherGauge(t, m, "tmux_tell_chamber_rate_limit_retry_after_seconds", labels)
	if firstAge <= 0 {
		t.Fatalf("initial rate-limited age = %v, want > 0", firstAge)
	}
	if firstRetry <= 0 {
		t.Fatalf("initial retry-after = %v, want > 0", firstRetry)
	}

	// #613: the cumulative counter records this episode once at the
	// first-detection transition, under cause=overloaded (StateRateLimited).
	counterLabels := map[string]string{"cause": "overloaded", "agent": "bob", "provider": "anthropic"}
	if got := gatherCounter(t, m, "tmux_tell_rate_limit_total", counterLabels); got != 1 {
		t.Fatalf("rate_limit_total{cause=overloaded} = %v after first detection, want 1", got)
	}

	time.Sleep(200 * time.Millisecond)

	secondAge := gatherGauge(t, m, "tmux_tell_chamber_rate_limited_seconds", labels)
	secondRetry := gatherGauge(t, m, "tmux_tell_chamber_rate_limit_retry_after_seconds", labels)
	if secondAge <= firstAge {
		t.Fatalf("rate-limited age did not advance: first=%v second=%v", firstAge, secondAge)
	}
	if secondRetry >= firstRetry {
		t.Fatalf("retry-after did not count down: first=%v second=%v", firstRetry, secondRetry)
	}
	// (The once-per-episode property — that re-polls of the SAME parked message
	// do NOT re-increment the counter — is pinned deterministically by
	// TestServe_RateLimited_CounterIsOncePerEpisode below, which forces many
	// gate cycles on one episode rather than relying on this test's timing.)

	delivered, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if err != nil {
		t.Fatalf("list delivered: %v", err)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %d while rate-limited, want 0", len(delivered))
	}
}

// TestServe_RateLimited_CounterIsOncePerEpisode deterministically pins #613's
// once-per-episode grain: a single rate-limit episode (one parked message,
// re-detected across MANY gate cycles) increments tmux_tell_rate_limit_total
// exactly once. It forces the re-cycling that TestServe_RateLimited...Metrics
// can't (that test's window is shorter than one backoff), by shrinking the
// gate's temporal delta and using a 1ms banner retry hint so the serve loop
// spins through the same parked message dozens of times.
//
// Mutation anchor: moving the IncRateLimit call out of the
// `rateLimitFirstDetection` guard makes the counter track the gate-cycle count
// (>= the asserted loop-iteration floor) instead of staying at 1 — this test
// then fails.
func TestServe_RateLimited_CounterIsOncePerEpisode(t *testing.T) {
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%9"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if _, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hi"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevProfile := tmuxio.ActivePaneProfile()
	tmuxio.SetActivePaneProfile(tmuxio.PaneProfile{
		PromptSentinel:   tmuxio.CodexPromptSentinel,
		RateLimitPattern: `Rate limited.*?retry after (?P<retry_seconds>\d+ms)`,
	})
	t.Cleanup(func() { tmuxio.SetActivePaneProfile(prevProfile) })

	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "display-message":
			if strings.Contains(strings.Join(args, " "), "#{pane_in_mode}") {
				return []byte("0"), nil
			}
			return []byte("0/0"), nil
		case "capture-pane":
			// Tiny retry hint → ~1ms backoff → rapid re-cycling of the same
			// parked message, so many gate cycles land in one episode.
			return []byte("Rate limited retry after 1ms"), nil
		default:
			return nil, nil
		}
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prevRunner) })

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false
	opts.Metrics = m
	stop, wait, _ := runServeInBackground(t, s, opts)
	t.Cleanup(func() { stop(); wait() })

	// Wait until the serve loop has demonstrably cycled several times (each
	// iteration re-claims and re-detects the same still-parked message). The
	// loop-iteration counter is the proof that multiple gate cycles occurred
	// within this one rate-limit episode.
	waitFor(t, 3*time.Second, func() bool {
		return gatherCounter(t, m, "tmux_tell_mailman_loop_iterations_total",
			map[string]string{"agent": "bob"}) >= 3
	}, "expected the serve loop to cycle >= 3 times on the parked message")

	// Still one episode: the message never delivered (always rate-limited).
	delivered, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "bob", State: store.StateDelivered, Limit: 10})
	if err != nil {
		t.Fatalf("list delivered: %v", err)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered = %d, want 0 (message stays parked across the episode)", len(delivered))
	}

	counterLabels := map[string]string{"cause": "overloaded", "agent": "bob", "provider": "anthropic"}
	if got := gatherCounter(t, m, "tmux_tell_rate_limit_total", counterLabels); got != 1 {
		t.Fatalf("rate_limit_total{cause=overloaded} = %v across a multi-cycle episode, want 1 (once per episode-start, not per gate cycle)", got)
	}
}

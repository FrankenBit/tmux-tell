package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/discover"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// TestServe_DriftDetectionWithCanonicalAlias is the post-#38 form of
// the silent-drift regression: registered short name `surveyor`,
// drifted pane runs `--resume Surveyor` (capital S, treated here as
// an alias on the canonical row). The mailman should resolve the
// running name back to canonical via the alias and reroute.
func TestServe_DriftDetectionWithCanonicalAlias(t *testing.T) {
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		// %3 runs `surveyor` (canonical match); %4 runs `Pilot`.
		return []byte("%3\t300\t✳ Surveyor\tclaude\n" +
			"%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 300:
				return "claude\x00--resume\x00surveyor\x00", nil
			case 400:
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	var (
		bodyMu   sync.Mutex
		body     string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				body = string(b)
				bodyMu.Unlock()
			}
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // drifted; %4 is Pilot now
	_ = s.UpsertAgent(ctx, "pilot", "%4")    // pilot registered correctly
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hello",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if seen := paneSeen.Load(); seen != "%3" {
		t.Errorf("paste-buffer pane = %v, want %%3 (drift should have rerouted via canonical); log=%s",
			seen, logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "drift_detected") {
		t.Errorf("expected drift_detected log; got %s", logbuf.String())
	}
}

// TestServe_SilentDriftDetectionAtDelivery is the regression for #37.
// Scenario: the registry says surveyor → %4, but %4 is actually
// Pilot's pane now (post-tmux-restore drift). With drift detection
// disabled, the mailman would deliver to %4 (Pilot's pane) and
// happily mark it delivered. With detection on, the mailman:
//
//  1. Reads the running agent from %4 via PaneAgentName → "Pilot".
//  2. Mismatch against opts.Agent="surveyor".
//  3. LookupByName("surveyor") returns %3 (where surveyor actually is).
//  4. UpsertAgent updates the registry: surveyor → %3.
//  5. Delivery proceeds to %3, not %4.
//
// We assert all five steps via the log lines and the final registry
// state.
func TestServe_SilentDriftDetectionAtDelivery(t *testing.T) {
	// Fake tmux: %3 is surveyor's pane (pid 300), %4 is Pilot's
	// pane (pid 400).
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%3\t300\t✳ Surveyor\tclaude\n" +
			"%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			switch pid {
			case 300:
				return "claude\x00--resume\x00surveyor\x00", nil
			case 400:
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	// Fake delivery: paste-buffer + Enter succeed; verify-token check
	// finds whatever was written via load-buffer (mailman's standard
	// fake-runner pattern).
	var (
		bodyMu  sync.Mutex
		body    string
		paneSeen atomic.Value // tracks the pane id paste-buffer was called against
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		switch args[0] {
		case "load-buffer":
			if stdin != nil {
				b, _ := io.ReadAll(stdin)
				bodyMu.Lock()
				body = string(b)
				bodyMu.Unlock()
			}
			return nil, nil
		case "paste-buffer":
			for i, a := range args {
				if a == "-t" && i+1 < len(args) {
					paneSeen.Store(args[i+1])
				}
			}
			return nil, nil
		case "send-keys", "delete-buffer":
			return nil, nil
		case "capture-pane":
			bodyMu.Lock()
			defer bodyMu.Unlock()
			return []byte(body), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4") // ← drifted: %4 belongs to Pilot now
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hello",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false // ← opt into the check
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	// 1. Delivery succeeded.
	d, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "surveyor", State: store.StateDelivered, Limit: 10,
	})
	if len(d) != 1 {
		t.Fatalf("delivered = %d, want 1; log=%s", len(d), logbuf.String())
	}

	// 2. Delivery targeted %3 (surveyor's real pane), NOT %4 (Pilot's).
	if seen := paneSeen.Load(); seen != "%3" {
		t.Errorf("paste-buffer pane = %v, want %%3 (drift should have rerouted)", seen)
	}

	// 3. Registry was updated to point at the correct pane.
	agent, _ := s.GetAgent(ctx, "surveyor")
	if agent == nil || agent.PaneID != "%3" {
		t.Errorf("registry pane = %v, want %%3", agent)
	}

	// 4. drift_detected log line was emitted with the right fields.
	logs := logbuf.String()
	if !strings.Contains(logs, "drift_detected") {
		t.Errorf("expected drift_detected log line; got %s", logs)
	}
	if !strings.Contains(logs, "registered_pane=%4") {
		t.Errorf("log should name the drifted pane %%4; got %s", logs)
	}
	if !strings.Contains(logs, "rediscovered=%3") {
		t.Errorf("log should name the rediscovered pane %%3; got %s", logs)
	}
}

// TestServe_DriftDetectedButUnrecoverable: registered pane is drifted
// AND LookupByName can't find the agent anywhere (e.g. the operator
// quit that session). The mailman logs a WARN and falls through to
// the existing delivery + auto-heal paths.
func TestServe_DriftDetectedButUnrecoverable(t *testing.T) {
	// Live pane %4 runs Pilot; surveyor isn't running anywhere.
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return []byte("%4\t400\t✳ Pilot\tclaude\n"), nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })

	walker := &discover.Walker{
		CmdlineReader: func(pid int) (string, error) {
			if pid == 400 {
				return "claude\x00--resume\x00Pilot\x00", nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}

	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		// Make paste-buffer at %4 succeed (existing test shape).
		switch args[0] {
		case "load-buffer", "paste-buffer", "send-keys", "delete-buffer":
			return nil, nil
		case "capture-pane":
			// Empty content; verify-token won't match → ErrUnverifiedDelivery.
			return []byte("\n"), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })
	prevRetry := tmuxio.SetRetryDelaysForTest([]time.Duration{time.Microsecond})
	t.Cleanup(func() { tmuxio.SetRetryDelaysForTest(prevRetry) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "surveyor", "%4")
	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "surveyor", Body: "hi",
	})

	opts := fastOpts("surveyor")
	opts.DriftCheckDisabled = false
	opts.Walker = walker

	stop, wait, logbuf := runServeInBackgroundOpts(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// Either delivered (verified or unverified) is fine — the
		// point is we don't crash.
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "surveyor", Limit: 10,
		})
		if len(all) >= 1 {
			done := false
			for _, m := range all {
				if m.State == store.StateDelivered || m.State == store.StateFailed {
					done = true
					break
				}
			}
			if done {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if !strings.Contains(logbuf.String(), "drift_detected_unrecoverable") {
		t.Errorf("expected drift_detected_unrecoverable WARN log; got %s", logbuf.String())
	}
}

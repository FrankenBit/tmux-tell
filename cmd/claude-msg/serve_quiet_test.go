package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// TestServe_QuietGate_BlocksUntilQuiet drives a fake tmux that reports
// activity on the first probe (operator typing) and quiet on the
// second. The mailman must perform two probe iterations before
// delivery and never call load-buffer until the quiet path runs.
func TestServe_QuietGate_BlocksUntilQuiet(t *testing.T) {
	var (
		mu             sync.Mutex
		captureIdx     int
		captureScript  = []string{
			"> typing in progress\n",       // round 1: before-probe
			"> typing in progressmore\n",   // round 1: after-probe (activity!)
			"> typing in progressmore\n",   // round 2: before-probe
			"> typing in progressmore─\n",  // round 2: after-probe (quiet)
			"id 1234 verify-token\n",       // delivery: capture-pane verify
		}
		loadBufferUsed bool
		bspaceCount    int
		probeCount     int
	)

	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "capture-pane":
			if captureIdx < len(captureScript) {
				out := captureScript[captureIdx]
				captureIdx++
				return []byte(out), nil
			}
			return []byte(captureScript[len(captureScript)-1]), nil
		case "send-keys":
			for i, a := range args {
				if a == "-l" && i+1 < len(args) && args[i+1] == tmuxio.QuietProbe {
					probeCount++
					return nil, nil
				}
				if a == "BSpace" {
					bspaceCount++
					return nil, nil
				}
			}
		case "load-buffer":
			loadBufferUsed = true
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// Note: rendered chat header includes "id 1234" so the verify
	// token in our scripted capture-pane delivery response matches.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "test body",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, _ = res, err
	// Force the public_id for predictable verify-token in the capture script.
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET public_id='1234' WHERE id=?`, 1); err != nil {
		t.Fatalf("rewrite public_id: %v", err)
	}

	opts := fastOpts("bob")
	opts.QuietDisabled = false
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:   5 * time.Millisecond,
		BackoffInterval: 5 * time.Millisecond,
		MaxWait:         100 * time.Millisecond,
		CaptureLines:    5,
	}

	stop, wait, _ := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	mu.Lock()
	defer mu.Unlock()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	if len(delivered) != 1 {
		t.Fatalf("delivered = %d, want 1", len(delivered))
	}
	if probeCount != 2 {
		t.Errorf("probe injections = %d, want 2 (one per round)", probeCount)
	}
	if bspaceCount != 1 {
		t.Errorf("backspaces = %d, want 1 (only after the quiet exit)", bspaceCount)
	}
	if !loadBufferUsed {
		t.Errorf("load-buffer should have run after the quiet path")
	}
}

// TestServe_QuietGate_CapExceededLogsAndDelivers asserts that when the
// gate hits its total-time cap, the mailman logs a WARN and proceeds
// with delivery rather than failing the message.
func TestServe_QuietGate_CapExceededLogsAndDelivers(t *testing.T) {
	var (
		mu             sync.Mutex
		captureIdx     int
		loadBufferUsed bool
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "capture-pane":
			// Always return "activity" — before and after never match.
			captureIdx++
			if captureIdx%2 == 1 {
				return []byte("> A\n"), nil
			}
			return []byte("> B\n"), nil
		case "load-buffer":
			loadBufferUsed = true
		case "send-keys":
			// After cap exceeded, the load-buffer path drives this.
			for i, a := range args {
				if a == "-l" || a == "BSpace" {
					_ = i
				}
			}
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "always-activity",
	})

	opts := fastOpts("bob")
	opts.QuietDisabled = false
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:   2 * time.Millisecond,
		BackoffInterval: 2 * time.Millisecond,
		MaxWait:         20 * time.Millisecond, // very short cap
		CaptureLines:    5,
	}

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) >= 1 {
			break
		}
		// Also accept marked-failed (verify token won't match here
		// since the capture script doesn't include it).
		failed, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateFailed, Limit: 10,
		})
		if len(failed) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	mu.Lock()
	defer mu.Unlock()

	if !loadBufferUsed {
		t.Errorf("cap-exceeded path should still call load-buffer (deliver-anyway); log=%s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "quiet_cap_exceeded") {
		t.Errorf("expected quiet_cap_exceeded WARN log; got %s", logbuf.String())
	}
}

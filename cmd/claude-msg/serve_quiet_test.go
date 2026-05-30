package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// TestServe_QuietGate_DeliversAfterInputActivity drives a fake tmux
// that reports operator activity on the first probe (typed text),
// then a clean quiet path on the second. The mailman must perform
// both probe iterations before delivery, and the load-buffer + Enter
// path only runs after the quiet exit.
func TestServe_QuietGate_DeliversAfterInputActivity(t *testing.T) {
	var (
		mu             sync.Mutex
		captureIdx     int
		cursorIdx      int
		captureScript  = []string{
			// Each row pair = (before-probe, after-probe). Input row
			// is index 1. Round 1: operator typed 'x' → activity.
			"ctx\n> \n",
			"ctx\n> ─x\n",
			// Round 2: input row clean except for our probe → quiet.
			"ctx\n> ─x\n",
			"ctx\n> ─x─\n",
			// Delivery: capture-pane for verify-token of "id <id>".
			"ctx\nid TEST verify\n",
		}
		cursorScript    = []int{1, 1}
		loadBufferUsed  bool
		probeCount      int
		bspaceCount     int
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
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
		case "display-message":
			var cy int
			if cursorIdx < len(cursorScript) {
				cy = cursorScript[cursorIdx]
				cursorIdx++
			}
			return []byte(fmt.Sprintf("%d\n", cy)), nil
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

	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "test body",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Force the public_id so the verify token in our scripted
	// capture-pane delivery response matches.
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET public_id='TEST' WHERE id=?`, 1); err != nil {
		t.Fatalf("rewrite public_id: %v", err)
	}

	opts := fastOpts("bob")
	opts.QuietDisabled = false
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:        5 * time.Millisecond,
		InputActivityBackoff: 5 * time.Millisecond,
		TUINoiseBackoff:      2 * time.Millisecond,
		MaxWait:              200 * time.Millisecond,
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
		t.Errorf("probe injections = %d, want 2", probeCount)
	}
	// Quiet exit backspaces the 2 accumulated probes.
	if bspaceCount != 2 {
		t.Errorf("backspaces = %d, want 2", bspaceCount)
	}
	if !loadBufferUsed {
		t.Errorf("load-buffer should run after the quiet exit")
	}
}

// TestServe_QuietGate_CapExceededLogsAndDelivers asserts that when the
// gate hits its total-time cap, the mailman logs a WARN and proceeds
// with delivery rather than failing the message. Also asserts the
// accumulated probes are backspaced before delivery so the input row
// is clean even on cap-exceeded — the visual-mess fix from 2026-05-30.
func TestServe_QuietGate_CapExceededLogsAndDelivers(t *testing.T) {
	var (
		mu             sync.Mutex
		captureIdx     int
		cursorIdx      int
		loadBufferUsed bool
		bspaceCount    int
		probeCount     int
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "capture-pane":
			// Always alternate "before" and "after" with a status-line
			// tick — DeltaTUINoise on every iteration so the loop
			// never finds quiet.
			captureIdx++
			if captureIdx%2 == 1 {
				return []byte(fmt.Sprintf("tick %d\n> \n", captureIdx)), nil
			}
			return []byte(fmt.Sprintf("tick %d\n> ─\n", captureIdx)), nil
		case "display-message":
			cursorIdx++
			return []byte("1\n"), nil
		case "load-buffer":
			loadBufferUsed = true
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
		FromAgent: "alice", ToAgent: "bob", Body: "always-noise",
	})

	opts := fastOpts("bob")
	opts.QuietDisabled = false
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:        2 * time.Millisecond,
		InputActivityBackoff: 2 * time.Millisecond,
		TUINoiseBackoff:      2 * time.Millisecond,
		MaxWait:              30 * time.Millisecond,
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
		t.Errorf("cap-exceeded path should still call load-buffer; log=%s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "quiet_cap_exceeded") {
		t.Errorf("expected quiet_cap_exceeded WARN log; got %s", logbuf.String())
	}
	// Cap-exceeded cleanup: accumulated probes backspaced before delivery.
	if probeCount == 0 || bspaceCount == 0 {
		t.Errorf("expected probes + backspaces on cap-exceeded; got probes=%d bspaces=%d",
			probeCount, bspaceCount)
	}
	if bspaceCount != probeCount {
		t.Errorf("cap-exceeded should backspace exactly probeCount=%d; got %d",
			probeCount, bspaceCount)
	}
}

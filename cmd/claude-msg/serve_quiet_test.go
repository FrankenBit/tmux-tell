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
			// is the last "> " row. Round 1: operator typed 'x' between
			// our two probes → activity (strip-2-trailing-probes fails
			// because row ends with 'x').
			"ctx\n> \n",
			"ctx\n> ──x\n",
			// Round 2: operator's x still there, our 2 prior probes
			// accumulated, plus 2 new probes appended cleanly. Strip-2
			// from "> ──x──" → "> ──x" matches before → quiet.
			"ctx\n> ──x\n",
			"ctx\n> ──x──\n",
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
	if probeCount != 4 {
		t.Errorf("probe injections = %d, want 4 (two iters of two probes each)", probeCount)
	}
	// Quiet exit backspaces the 4 accumulated probes.
	if bspaceCount != 4 {
		t.Errorf("backspaces = %d, want 4 (all accumulated probes cleaned on quiet exit)", bspaceCount)
	}
	if !loadBufferUsed {
		t.Errorf("load-buffer should run after the quiet exit")
	}
}

// TestServe_UnverifiedDelivery_MarksDeliveredWithWarn pins the
// 2026-05-30 behavior change: when Deliver returns
// tmuxio.ErrUnverifiedDelivery (paste + Enter ran, but the verify
// token never surfaced — typically because Claude was mid-turn at
// paste time), the mailman marks the message DELIVERED with a WARN
// log rather than FAILED. Before this change, the user had to
// manually re-send messages dropped by an over-tight verify budget.
func TestServe_UnverifiedDelivery_MarksDeliveredWithWarn(t *testing.T) {
	// Shorten the verify retry budget so the test doesn't bump up
	// against fastOpts's DeliverTimeout (5s). Production budget is ~5s.
	prevDelays := tmuxio.SetRetryDelaysForTest([]time.Duration{
		time.Microsecond, time.Microsecond, time.Microsecond,
	})
	t.Cleanup(func() { tmuxio.SetRetryDelaysForTest(prevDelays) })

	// Quiet gate passes immediately (probe added, nothing else
	// changed). After paste/Enter, the verify token never appears
	// in capture-pane, so Deliver returns ErrUnverifiedDelivery.
	var (
		mu          sync.Mutex
		captureIdx  int
		cursorIdx   int
		captureScript = []string{
			"ctx\n> \n",  // quiet-gate before-probe
			"ctx\n> ─\n", // quiet-gate after-probe → DeltaQuiet
		}
		cursorScript = []int{1}
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
			// All post-paste verify attempts return content that does
			// NOT contain the token.
			return []byte("no token here\n"), nil
		case "display-message":
			var cy int
			if cursorIdx < len(cursorScript) {
				cy = cursorScript[cursorIdx]
				cursorIdx++
			}
			return []byte(fmt.Sprintf("%d\n", cy)), nil
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
		FromAgent: "alice", ToAgent: "bob", Body: "body",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob")
	opts.QuietDisabled = false
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:        2 * time.Millisecond,
		InputActivityBackoff: 2 * time.Millisecond,
		MaxWait:              200 * time.Millisecond,
	}

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	wait()

	delivered, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateDelivered, Limit: 10,
	})
	failed, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "bob", State: store.StateFailed, Limit: 10,
	})
	if len(delivered) != 1 {
		t.Errorf("delivered = %d, want 1; log=%s", len(delivered), logbuf.String())
	}
	if len(failed) != 0 {
		t.Errorf("failed = %d, want 0 (unverified should NOT fail); log=%s",
			len(failed), logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "delivered_unverified") {
		t.Errorf("expected delivered_unverified WARN log; got %s", logbuf.String())
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
			// Alternate "before" and "after" where the operator typed
			// 'x' between captures so the input row gains `──x`
			// rather than `──`. analyzeDelta sees the trailing `x`,
			// returns DeltaInputActivity every iteration → cap exceeded.
			captureIdx++
			if captureIdx%2 == 1 {
				return []byte(fmt.Sprintf("tick %d\n> \n", captureIdx)), nil
			}
			return []byte(fmt.Sprintf("tick %d\n> ──x\n", captureIdx)), nil
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

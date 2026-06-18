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

// TestServe_CopyMode_DefersRevertsNeverPastes is #526's end-to-end pin and the
// mutation-verification anchor. With the recipient pane scrolled up
// (pane_in_mode=1 always), the observe-gate classifies StateInCopyMode, holds
// to MaxWait, returns ErrCopyModeUnsafe, and serve REVERTS the message to
// queued — it must NEVER paste into a scrolled pane (the 83b3 bug it fixes).
// The copy-mode deferral counter increments.
//
// The load-bearing assertion is the negative one: zero paste primitives
// (load-buffer / paste-buffer / send-keys) over the whole run. If the amended
// D4 (ErrCopyModeUnsafe → revert, NOT deliver-anyway) regresses to the generic
// MaxWait deliver-anyway, a paste fires here and this test fails.
func TestServe_CopyMode_DefersRevertsNeverPastes(t *testing.T) {
	var mu sync.Mutex
	calls := []string{}
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		// pane_in_mode → "1": the operator is scrolled up the whole test.
		if args[0] == "display-message" && strings.Contains(args[len(args)-1], "pane_in_mode") {
			return []byte("1\n"), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")

	r, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "must-not-paste-into-scroll",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	m := metrics.New()
	opts := fastOpts("bob")
	opts.GateDisabled = false // enable the observe-gate (the unit under test)
	opts.Metrics = m
	opts.ObserveGateOpts = tmuxio.ObserveGateOpts{
		PollIntervalMin: time.Millisecond,
		PollIntervalMax: time.Millisecond,
		MaxWait:         15 * time.Millisecond,
	}

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	waitFor(t, 2*time.Second, func() bool {
		return gatherCounter(t, m, "tmux_tell_copymode_defer_total",
			map[string]string{"agent": "bob"}) >= 1
	}, "expected copy-mode deferral counter to increment")
	stop()
	wait()

	// Message must NOT be delivered — it reverts to queued each cycle.
	final, gerr := s.GetMessage(ctx, r.PublicID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if final.State == store.StateDelivered {
		t.Errorf("message delivered despite copy-mode (should revert to queued); calls=%v", calls)
	}
	if !strings.Contains(logbuf.String(), "gate_copymode_persist") {
		t.Errorf("expected gate_copymode_persist log line; got:\n%s", logbuf.String())
	}

	// Load-bearing negative assertion: never paste into a scrolled pane.
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "send-keys") ||
			strings.HasPrefix(c, "load-buffer") ||
			strings.HasPrefix(c, "paste-buffer") {
			t.Errorf("paste primitive fired into a scrolled pane: %q — reproduces 83b3", c)
		}
	}
}

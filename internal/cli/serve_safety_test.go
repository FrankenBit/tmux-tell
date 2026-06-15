package cli

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestServe_PrePasteSafety_AbortsOnAwaitingOperator pins #105 Half 2's
// load-bearing invariant: when the pre-paste safety check observes a
// paste-unsafe state (here: StateAwaitingOperator via the popup-marker
// in the pane content), the delivery is aborted and the message is
// reverted from `delivering` back to `queued` for a later retry.
//
// The setup deliberately leaves PrePasteSafetyDisabled false (overriding
// fastOpts's default-true) and provides a fake AgentState runner that
// always returns AwaitingOperator-classified content. The observe-gate
// is also disabled so the gate doesn't intervene — this test isolates
// the pre-paste safety check itself.
func TestServe_PrePasteSafety_AbortsOnAwaitingOperator(t *testing.T) {
	// Fake AgentState runner: every capture-pane returns content that
	// classifies as StateAwaitingOperator (contains the marker
	// substring, plus cursor-past-sentinel via display-message). This
	// makes the pre-paste safety check's AgentState call see a
	// paste-unsafe state on every probe.
	popupPane := "history\n" + tmuxio.PromptSentinel + "operator typing\n" +
		"footer with " + tmuxio.AwaitingOperatorMarker + "\n"
	var mu sync.Mutex
	calls := []string{}
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "capture-pane":
			return []byte(popupPane), nil
		case "display-message":
			// Cursor PAST the sentinel position → cursor-aware path
			// confirms StateAwaitingOperator.
			return []byte("20/1\n"), nil
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
		FromAgent: "alice", ToAgent: "bob", Body: "should-not-paste",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	opts := fastOpts("bob")
	opts.PrePasteSafetyDisabled = false // enable the check (override fastOpts default)

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	// Let the mailman loop observe + abort a few times.
	time.Sleep(50 * time.Millisecond)
	stop()
	wait()

	// Message should remain in 'queued' state (NOT delivered) because
	// the safety check kept aborting. RecoverDelivering reverts it.
	final, gerr := s.GetMessage(ctx, r.PublicID)
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if final.State == store.StateDelivered {
		t.Errorf("message delivered despite pre-paste safety check; calls=%v", calls)
	}
	if !strings.Contains(logbuf.String(), "pre_paste_safety_abort") {
		t.Errorf("expected pre_paste_safety_abort log line; got %s", logbuf.String())
	}
	// No send-keys (paste) should have fired.
	mu.Lock()
	defer mu.Unlock()
	for _, c := range calls {
		if strings.HasPrefix(c, "send-keys") {
			t.Errorf("send-keys fired despite safety abort: %q", c)
		}
		if strings.HasPrefix(c, "load-buffer") {
			t.Errorf("load-buffer fired despite safety abort: %q", c)
		}
	}
}

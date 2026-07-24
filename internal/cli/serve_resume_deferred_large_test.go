package cli

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// TestServe_ResumeDeferred_LargePayloadSurvivesCompact is the #842 AC4 regression
// guard: a staged-resume self-handoff with a body ≥ 1 KiB must survive the
// /compact + wake cycle DELIVERED INTACT — byte-identical, not truncated.
//
// #842 is the post-compact "paste-not-submitted / content-not-surviving" class.
// The existing #843 guards (TestServe_ResumeDeferred_AutoFiresOnSessionReset and
// siblings) prove the small-body promotion fires on a /compact, but none
// exercises a ≥1 KiB payload — which is exactly the substantive-orientation size
// a real /compact self-handoff carries (this chamber's own resume notes run
// multi-KiB). This pins that the large body is neither dropped nor truncated
// across the stage → defer → promote → deliver path.
//
// The assertion is byte-integrity on the DELIVERED body (captured via the shared
// deliverRunner's load-buffer sink), not merely that the row reached
// StateDelivered: a truncating paste would flip the state to delivered while
// dropping the tail. The START/END sentinels make head- and tail-truncation each
// independently detectable.
func TestServe_ResumeDeferred_LargePayloadSurvivesCompact(t *testing.T) {
	prevSettle := tmuxio.SetSettleDelayForTest(time.Microsecond)
	t.Cleanup(func() { tmuxio.SetSettleDelayForTest(prevSettle) })

	// Retain the delivered-body capture (resumeDeferredRunner discards it) so we
	// can assert byte-integrity of what actually got pasted.
	var (
		bodyMu   sync.Mutex
		gotBody  string
		paneSeen atomic.Value
	)
	prev := tmuxio.SetTmuxRunner(deliverRunner(&bodyMu, &gotBody, &paneSeen))
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "shipwright", "%1")

	// A ≥1 KiB body with START/END sentinels bracketing a repeated middle, so
	// either-end truncation is detectable and the size is unambiguously over 1 KiB.
	const marker = "AC4-RESUME-1KB"
	largeBody := marker + "-START|" +
		strings.Repeat("substantive post-/compact orientation payload. ", 40) +
		"|END-" + marker
	if len(largeBody) < 1024 {
		t.Fatalf("test payload is %d bytes, need ≥1024 to exercise the #842 AC4 case", len(largeBody))
	}

	// Staged BEFORE the /compact so created_at ordering also proves the handoff
	// lands ahead of later traffic (same shape as the #843 auto-fire guard).
	staged, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "shipwright", ToAgent: "shipwright",
		Body: largeBody, Kind: store.KindMessage,
		DeliverAfter: deferTriggerResume,
	})
	if err != nil {
		t.Fatalf("insert staged: %v", err)
	}
	if got := stateOf(t, s, staged.PublicID); got != store.StateDeferred {
		t.Fatalf("staged row state = %q, want %q (precondition: it must start deferred)",
			got, store.StateDeferred)
	}

	compact, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "shipwright", ToAgent: "shipwright",
		Body: "/compact", Kind: store.KindControl,
	})
	if err != nil {
		t.Fatalf("insert compact: %v", err)
	}

	stop, wait, _ := runServeInBackground(t, s, fastOpts("shipwright"))
	waitForState(t, s, staged.PublicID, store.StateDelivered)
	stop()
	wait()

	if got := stateOf(t, s, compact.PublicID); got != store.StateDelivered {
		t.Errorf("/compact row state = %q, want delivered", got)
	}

	// Load-bearing assertion: the full ≥1 KiB staged body appears INTACT inside the
	// delivered paste. Delivery wraps the body in render chrome — a
	// "[Shipwright · <ts> · id <x> · <size>]\n\n" header (the timestamp/id are
	// non-deterministic, so byte-equality against the raw body is wrong) and a
	// trailing newline — so the integrity check is containment of the contiguous
	// ≥1 KiB payload, not prefix/suffix. A truncating paste would drop the END
	// sentinel and break containment; a lost payload would leave gotBody holding
	// only the /compact echo (a control row delivered via send-keys never writes
	// the load-buffer sink, so gotBody is the message row alone).
	bodyMu.Lock()
	delivered := gotBody
	bodyMu.Unlock()
	if !strings.Contains(delivered, largeBody) {
		t.Errorf("≥1KB staged-resume body did NOT survive /compact intact:\n"+
			"  staged len=%d delivered len=%d\n"+
			"  head-sentinel present=%v tail-sentinel present=%v",
			len(largeBody), len(delivered),
			strings.Contains(delivered, marker+"-START|"),
			strings.Contains(delivered, "|END-"+marker))
	}
}

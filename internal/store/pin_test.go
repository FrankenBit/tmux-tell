// Discipline pins for the internal/store package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the three orthogonal grep handles
// for the discipline.
package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/testpin"
)

// PIN: linkP2ToP1 callers don't pass explicit reply_to — the
// precondition is asserted at the runtime guard. Surveyor #29
// review (a): InsertMessagePair's linkP2ToP1=true MUST reject a
// caller who also set p2.ReplyTo to something explicit, rather than
// silently overwriting it. The doc says "MUST be empty"; this pin
// verifies the runtime guard enforces the doc.
func TestPin_ThreadStructurePrecondition_RejectsExplicitReplyTo(t *testing.T) {
	testpin.Triage(t, "ThreadStructurePrecondition",
		"linkP2ToP1 callers don't pass explicit reply_to — the precondition is asserted at the runtime guard")
	s := openFileStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")

	p1 := store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "first", Kind: store.KindControl,
	}
	p2 := store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "second", Kind: store.KindControl,
		ReplyTo: "ffff", // caller mistake: set ReplyTo AND ask to link
	}
	_, _, err := s.InsertMessagePair(ctx, p1, p2, true)
	if err == nil {
		t.Fatal("expected error when linkP2ToP1=true and p2.ReplyTo is non-empty")
	}
	if !strings.Contains(err.Error(), "linkP2ToP1") {
		t.Errorf("error should explain the linkP2ToP1 precondition; got %v", err)
	}

	// Sanity: no rows should have been inserted.
	depth, _ := s.RecipientQueueDepth(ctx, "bob")
	if depth != 0 {
		t.Errorf("guard fired but %d rows landed; want 0", depth)
	}
}

// PIN: caps are ceilings, never floors — atomic under concurrency via
// BEGIN IMMEDIATE + in-transaction COUNT. Without atomic cap
// enforcement, N concurrent senders against the same recipient could
// all read depth=X, all decide X+1 ≤ cap, and all insert — overshooting
// by up to N-1. With the BEGIN IMMEDIATE wrapping (via
// _txlock=immediate in Open) and the in-transaction COUNT(*), at most
// `cap` inserts can succeed regardless of concurrency.
//
// We point a file-backed DB at a temp dir (not :memory:) because the
// shared-cache memory DB doesn't exercise real cross-connection
// locking the way a file does. Surveyor #29 round-3 review.
func TestPin_AtomicCapEnforcement_CeilingUnderConcurrency(t *testing.T) {
	testpin.Triage(t, "AtomicCapEnforcement",
		"caps are ceilings, never floors — atomic under concurrency via BEGIN IMMEDIATE + in-transaction COUNT")
	const cap = 5
	const concurrentSenders = 20

	s := openFileStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("seed sender: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("seed recipient: %v", err)
	}

	var (
		wg            sync.WaitGroup
		acceptedCount atomic.Int64
		rejectedCount atomic.Int64
		otherErrors   atomic.Int64
	)
	wg.Add(concurrentSenders)
	for i := 0; i < concurrentSenders; i++ {
		go func() {
			defer wg.Done()
			_, err := s.InsertMessage(ctx, store.InsertParams{
				FromAgent:         "alice",
				ToAgent:           "bob",
				Body:              "concurrent",
				MaxRecipientQueue: cap,
				// MaxSenderBacklog left 0 — we're testing the
				// recipient cap; sender backlog of 20 would
				// otherwise trip first.
			})
			switch {
			case err == nil:
				acceptedCount.Add(1)
			case errors.Is(err, store.ErrRecipientQueueFull):
				rejectedCount.Add(1)
			default:
				otherErrors.Add(1)
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if otherErrors.Load() != 0 {
		t.Fatalf("got %d unexpected errors", otherErrors.Load())
	}
	if acceptedCount.Load() != cap {
		t.Errorf("accepted = %d, want exactly cap=%d (no overshoot)",
			acceptedCount.Load(), cap)
	}
	if rejectedCount.Load() != concurrentSenders-cap {
		t.Errorf("rejected = %d, want %d", rejectedCount.Load(), concurrentSenders-cap)
	}

	// Verify the table state matches what the accept/reject counts
	// claimed.
	depth, err := s.RecipientQueueDepth(ctx, "bob")
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != cap {
		t.Errorf("post-test queue depth = %d, want %d", depth, cap)
	}
}

// PIN: operationally-critical signals (currently delivery-failure
// notices) bypass MaxRecipientQueue and MaxSenderBacklog. Losing such
// a signal because a queue is congested would defeat the signal's
// whole point. Store.InsertNotice MUST land regardless of cap state.
//
// Surfaced during #53. The retraction trigger is the "high failure
// rate" edge case — if notice-flood becomes a real problem at homelab
// scale, the commitment retracts in favor of bounded-batching or
// per-kind caps via superseding ADR.
//
// This pin saturates the recipient's queue + inserts a notice and
// verifies the notice lands. A regression that re-introduced
// cap-checks on InsertNotice would silently drop notices in exactly
// the cases the operator needs to know about most.
func TestPin_CapExemption_NoticeBypassesRecipientCap(t *testing.T) {
	testpin.Triage(t, "CapExemption",
		"operationally-critical signals (failure notices) bypass MaxRecipientQueue/MaxSenderBacklog")

	s := openFileStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")

	// Saturate alice's queue with 5 regular messages at cap=5.
	const cap = 5
	for i := 0; i < cap; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "bob", ToAgent: "alice",
			Body: "filler", MaxRecipientQueue: cap,
		}); err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}

	// Confirm a regular InsertMessage with the same cap NOW fails —
	// proves the queue is genuinely saturated. Without this guard,
	// the test could pass on a cap-not-enforced regression.
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "bob", ToAgent: "alice",
		Body: "should be rejected", MaxRecipientQueue: cap,
	}); !errors.Is(err, store.ErrRecipientQueueFull) {
		t.Fatalf("regular insert at cap should fail with ErrRecipientQueueFull; got %v", err)
	}

	// Now insert a notice via InsertNotice. Even with the caller
	// passing a cap of `cap`, the notice MUST land — InsertNotice
	// zeroes the cap fields internally so callers can't accidentally
	// re-cap notices.
	res, err := s.InsertNotice(ctx, store.InsertParams{
		FromAgent: "bob", ToAgent: "alice",
		Body: ":warning: Delivery failure for X",
		Kind: store.KindDeliveryFailureNotice,
		// Caller-supplied caps that would normally block at depth==cap
		MaxRecipientQueue: cap,
		MaxSenderBacklog:  1,
	})
	if err != nil {
		t.Fatalf("notice insert must succeed even at recipient cap: %v", err)
	}
	if res.PublicID == "" {
		t.Errorf("notice insert returned empty public id")
	}

	// Verify the notice landed in alice's inbox.
	inbox, _ := s.ListMessages(ctx, store.ListFilter{
		ToAgent: "alice", State: store.StateQueued, Limit: 10,
	})
	if len(inbox) != cap+1 {
		t.Errorf("inbox = %d, want %d (cap fillers + 1 notice)", len(inbox), cap+1)
	}
	var foundNotice bool
	for _, m := range inbox {
		if m.Kind == store.KindDeliveryFailureNotice {
			foundNotice = true
			break
		}
	}
	if !foundNotice {
		t.Errorf("expected KindDeliveryFailureNotice in inbox; got %d messages, none of that kind", len(inbox))
	}
}

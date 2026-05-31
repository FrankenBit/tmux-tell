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

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/testpin"
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

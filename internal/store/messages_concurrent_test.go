package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// TestInsertMessage_CapEnforcedUnderConcurrency is the load-bearing
// regression for #29. Without atomic cap enforcement, N concurrent
// senders against the same recipient could all read depth=X, all
// decide X+1 ≤ cap, and all insert — overshooting by up to N-1. With
// the BEGIN IMMEDIATE wrapping (via _txlock=immediate in Open) and
// the in-transaction COUNT(*), at most `cap` inserts can succeed
// regardless of concurrency.
//
// We point a file-backed DB at a temp dir (not :memory:) because the
// shared-cache memory DB doesn't exercise real cross-connection
// locking the way a file does.
func TestInsertMessage_CapEnforcedUnderConcurrency(t *testing.T) {
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

// TestInsertMessagePair_AtomicityUnderConcurrency confirms the
// two-row macro path (used by mcp-restart-semaphore + resume_with) is
// also race-safe. With cap=5 and concurrent pair inserts of size 2,
// exactly floor(5/2)=2 pairs (= 4 rows) should land; the rest see
// ErrRecipientQueueFull on the +2 budget check.
func TestInsertMessagePair_AtomicityUnderConcurrency(t *testing.T) {
	const cap = 5
	const concurrentPairs = 10

	s := openFileStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")

	var (
		wg            sync.WaitGroup
		acceptedPairs atomic.Int64
		rejectedPairs atomic.Int64
	)
	wg.Add(concurrentPairs)
	for i := 0; i < concurrentPairs; i++ {
		go func() {
			defer wg.Done()
			p1 := store.InsertParams{
				FromAgent: "alice", ToAgent: "bob",
				Body:              "first",
				Kind:              store.KindControl,
				MaxRecipientQueue: cap,
			}
			p2 := store.InsertParams{
				FromAgent: "alice", ToAgent: "bob",
				Body: "second", Kind: store.KindControl,
			}
			_, _, err := s.InsertMessagePair(ctx, p1, p2, true)
			switch {
			case err == nil:
				acceptedPairs.Add(1)
			case errors.Is(err, store.ErrRecipientQueueFull):
				rejectedPairs.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if acceptedPairs.Load() != cap/2 {
		t.Errorf("accepted pairs = %d, want %d (floor of cap/2)",
			acceptedPairs.Load(), cap/2)
	}
	depth, _ := s.RecipientQueueDepth(ctx, "bob")
	if int(acceptedPairs.Load()*2) != depth {
		t.Errorf("depth = %d, but accepted_pairs*2 = %d",
			depth, acceptedPairs.Load()*2)
	}
}

// openFileStore opens a file-backed DB in t.TempDir(). File-backed is
// load-bearing for these tests because :memory:?cache=shared doesn't
// exercise real cross-connection write locking — the SetMaxOpenConns(1)
// pin means all access goes through one connection in-memory, so the
// race window collapses for the wrong reason.
func openFileStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "race.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

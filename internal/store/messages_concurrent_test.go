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

// The thread-structure-precondition pin
// (TestPin_ThreadStructurePrecondition_RejectsExplicitReplyTo) lives in
// pin_test.go per ADR-0001.

// Symmetric path: linkP2ToP1=false with an explicit p2.ReplyTo should
// still work and validate against the store as before.
func TestInsertMessagePair_NoLink_HonoursExplicitReplyTo(t *testing.T) {
	s := openFileStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%2")

	// Seed a target message we can reply to.
	seed, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "target",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	p1 := store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "first", Kind: store.KindControl,
	}
	p2 := store.InsertParams{
		FromAgent: "alice", ToAgent: "bob",
		Body: "second", Kind: store.KindControl,
		ReplyTo: seed.PublicID,
	}
	_, _, err = s.InsertMessagePair(ctx, p1, p2, false)
	if err != nil {
		t.Fatalf("explicit p2.ReplyTo on no-link path should work: %v", err)
	}
}

// The atomic-cap-enforcement pin
// (TestPin_AtomicCapEnforcement_CeilingUnderConcurrency) lives in
// pin_test.go per ADR-0001.

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

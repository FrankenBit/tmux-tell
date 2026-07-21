package store

import (
	"context"
	"testing"
	"time"
)

// TestRecipientOldestPendingAt pins the #719(A) freshness-signal query: it
// returns the oldest QUEUED-or-DELIVERING real deliverable, excludes the
// synthetic notice kinds (loop-prevention), excludes delivered rows, and
// reports ok=false when the queue holds no such deliverable.
func TestRecipientOldestPendingAt(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Empty queue → ok=false.
	if _, ok, err := s.RecipientOldestPendingAt(ctx, "bob"); err != nil || ok {
		t.Fatalf("empty queue: ok=%v err=%v, want ok=false", ok, err)
	}

	// A notice-kind row alone (isolated agent) must NOT count — a pile of
	// auto-generated notices cannot read as a stale queue and re-trigger.
	if _, err := s.InsertNotice(ctx, InsertParams{
		FromAgent: "sys", ToAgent: "carol", Body: "prior alert", Kind: KindStuckChamberNotice,
	}); err != nil {
		t.Fatalf("insert notice: %v", err)
	}
	if _, ok, err := s.RecipientOldestPendingAt(ctx, "carol"); err != nil || ok {
		t.Fatalf("notice-only queue: ok=%v err=%v, want ok=false (notice kinds excluded)", ok, err)
	}

	// The oldest real queued message is returned (bob has no notice rows).
	r1, err := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "bob", Body: "first"})
	if err != nil {
		t.Fatalf("insert r1: %v", err)
	}
	m1, _ := s.GetMessage(ctx, r1.PublicID)
	got, ok, err := s.RecipientOldestPendingAt(ctx, "bob")
	if err != nil || !ok {
		t.Fatalf("queued: ok=%v err=%v, want ok=true", ok, err)
	}
	if got != m1.CreatedAt {
		t.Errorf("oldest = %q, want r1.created_at %q", got, m1.CreatedAt)
	}

	// A strictly-newer queued message does not move the oldest.
	time.Sleep(3 * time.Millisecond)
	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "bob", Body: "second"}); err != nil {
		t.Fatalf("insert r2: %v", err)
	}
	if got2, _, _ := s.RecipientOldestPendingAt(ctx, "bob"); got2 != m1.CreatedAt {
		t.Errorf("after newer insert, oldest = %q, want still r1 %q", got2, m1.CreatedAt)
	}

	// Claiming r1 moves it queued → delivering; a delivering row STILL counts,
	// so the oldest is unchanged.
	claimed, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.PublicID != r1.PublicID {
		t.Fatalf("claimed %q, want oldest r1 %q", claimed.PublicID, r1.PublicID)
	}
	if got3, _, _ := s.RecipientOldestPendingAt(ctx, "bob"); got3 != m1.CreatedAt {
		t.Errorf("while r1 is delivering, oldest = %q, want still r1 %q (delivering counts)", got3, m1.CreatedAt)
	}

	// Delivering it moves it to delivered, which is EXCLUDED — so the oldest is
	// now r2 (a different, later created_at).
	if err := s.MarkDelivered(ctx, r1.PublicID); err != nil {
		t.Fatalf("mark delivered r1: %v", err)
	}
	got4, ok4, err := s.RecipientOldestPendingAt(ctx, "bob")
	if err != nil || !ok4 {
		t.Fatalf("after delivering r1: ok=%v err=%v, want ok=true (r2 still pending)", ok4, err)
	}
	if got4 == m1.CreatedAt {
		t.Errorf("oldest still = r1 %q after it was delivered; delivered rows must be excluded", got4)
	}
}

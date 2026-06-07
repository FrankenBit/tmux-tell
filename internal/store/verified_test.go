package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// readVerified returns the raw `verified` column for a message: the value and
// whether it's non-NULL. Reads via the raw DB since the marker is intentionally
// not surfaced on the Message struct (#169 keeps the universal scans untouched;
// the bit is consumed via DeliveredVerificationCounts, not per-row rendering).
func readVerified(t *testing.T, s *Store, publicID string) (val int64, nonNull bool) {
	t.Helper()
	var v sql.NullInt64
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT verified FROM messages WHERE public_id = ?`, publicID).Scan(&v); err != nil {
		t.Fatalf("read verified %s: %v", publicID, err)
	}
	return v.Int64, v.Valid
}

// deliverMarked inserts a message to `to`, claims it, and marks it delivered via
// the verified (true) or unverified (false) path, returning the public_id.
func deliverMarked(t *testing.T, s *Store, to string, verified bool) string {
	t.Helper()
	ctx := context.Background()
	r, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: to, Body: "x"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.ClaimNext(ctx, to); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if verified {
		err = s.MarkDelivered(ctx, r.PublicID)
	} else {
		err = s.MarkDeliveredUnverified(ctx, r.PublicID)
	}
	if err != nil {
		t.Fatalf("mark: %v", err)
	}
	return r.PublicID
}

func TestMarkDelivered_WritesVerifiedBit(t *testing.T) {
	s := newTestStore(t)
	v := deliverMarked(t, s, "bob", true)
	u := deliverMarked(t, s, "bob", false)

	if val, ok := readVerified(t, s, v); !ok || val != 1 {
		t.Errorf("verified delivery: verified = (%d, nonNull=%v), want 1", val, ok)
	}
	if val, ok := readVerified(t, s, u); !ok || val != 0 {
		t.Errorf("unverified delivery: verified = (%d, nonNull=%v), want 0", val, ok)
	}
}

func TestVerified_BackCompatNull(t *testing.T) {
	// A delivered row that never went through the marker (pre-migration) reads
	// NULL — never retroactively guessed.
	s := newTestStore(t)
	seedStat(t, s, "old1", "alice", "bob", "delivered", ts(0), ts(time.Second))
	if _, ok := readVerified(t, s, "old1"); ok {
		t.Errorf("pre-migration delivered row should read verified=NULL, got non-NULL")
	}
}

func TestDeliveredVerificationCounts_Split(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	deliverMarked(t, s, "bob", true)  // verified=1
	deliverMarked(t, s, "bob", true)  // verified=1
	deliverMarked(t, s, "bob", false) // verified=0
	// Pre-migration delivered → NULL → Unknown.
	seedStat(t, s, "old1", "alice", "bob", "delivered", ts(0), ts(time.Second))
	// Non-delivered states must be excluded entirely.
	seedStat(t, s, "q1", "alice", "bob", "queued", ts(0), "")
	seedStat(t, s, "f1", "alice", "bob", "failed", ts(0), ts(time.Second))

	got, err := s.DeliveredVerificationCounts(ctx, allWindow())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	want := VerificationCounts{Verified: 2, Unverified: 1, Unknown: 1}
	if got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

func TestDeliveredVerificationCounts_WindowBounds(t *testing.T) {
	// The split honors the StatsWindow (created_at), matching the other
	// aggregates' denominator.
	s := newTestStore(t)
	ctx := context.Background()
	// One in-window unverified delivery, one well before the Since floor.
	seedStat(t, s, "recent", "alice", "bob", "delivered", ts(0), ts(time.Second))
	seedStat(t, s, "old", "alice", "bob", "delivered", ts(-72*time.Hour), ts(-72*time.Hour+time.Second))
	// Stamp verified=0 on both via raw update (seedStat leaves NULL).
	for _, id := range []string{"recent", "old"} {
		if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET verified = 0 WHERE public_id = ?`, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
	}

	got, err := s.DeliveredVerificationCounts(ctx, StatsWindow{Since: statBase.Add(-1 * time.Hour)})
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if got.Unverified != 1 || got.Verified != 0 || got.Unknown != 0 {
		t.Errorf("windowed counts = %+v, want only the in-window unverified row", got)
	}
}

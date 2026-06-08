package store

import (
	"context"
	"testing"
	"time"
)

// seedDelivered inserts a delivered message with explicit timestamps so
// dedupe tests can place rows on either side of the window boundary.
func seedDeliveredRow(t *testing.T, s *Store, publicID, fromAgent, toAgent, body string, verified int, createdAt string) {
	t.Helper()
	ctx := context.Background()
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, verified, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		publicID, fromAgent, toAgent, body, string(KindMessage), string(StateDelivered), verified, createdAt)
	if err != nil {
		t.Fatalf("seedDeliveredRow: %v", err)
	}
}

func TestFindDedupeMatch_MatchFound(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	seedDeliveredRow(t, s, "orig1", "alice", "bob", "hello world", 0, recent)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "hello world", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got == nil {
		t.Fatal("want match, got nil")
	}
	if got.PublicID != "orig1" {
		t.Errorf("want orig1, got %s", got.PublicID)
	}
}

func TestFindDedupeMatch_NoMatchWhenVerified(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	// verified=1: already confirmed — should NOT match
	seedDeliveredRow(t, s, "orig2", "alice", "bob", "hello world", 1, recent)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "hello world", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got != nil {
		t.Errorf("want no match for verified=1 row, got %s", got.PublicID)
	}
}

func TestFindDedupeMatch_NoMatchOutsideWindow(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.Add(-2 * time.Minute).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	seedDeliveredRow(t, s, "orig3", "alice", "bob", "hello world", 0, old)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "hello world", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got != nil {
		t.Errorf("want no match for row outside window, got %s", got.PublicID)
	}
}

func TestFindDedupeMatch_NoMatchDifferentSender(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	// from_agent is "carol", not "alice"
	seedDeliveredRow(t, s, "orig4", "carol", "bob", "hello world", 0, recent)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "hello world", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got != nil {
		t.Errorf("want no match for different sender, got %s", got.PublicID)
	}
}

func TestFindDedupeMatch_NoMatchDifferentBody(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	recent := now.Add(-10 * time.Second).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	seedDeliveredRow(t, s, "orig5", "alice", "bob", "different body", 0, recent)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "hello world", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got != nil {
		t.Errorf("want no match for different body, got %s", got.PublicID)
	}
}

func TestFindDedupeMatch_ReturnsNewestOnMultiple(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC()
	older := now.Add(-50 * time.Second).Format("2006-01-02T15:04:05.000Z")
	newer := now.Add(-5 * time.Second).Format("2006-01-02T15:04:05.000Z")
	cutoff := now.Add(-60 * time.Second).Format("2006-01-02T15:04:05.000Z")

	seedDeliveredRow(t, s, "older-orig", "alice", "bob", "same body", 0, older)
	seedDeliveredRow(t, s, "newer-orig", "alice", "bob", "same body", 0, newer)

	got, err := s.FindDedupeMatch(ctx, "alice", "bob", "same body", cutoff)
	if err != nil {
		t.Fatalf("FindDedupeMatch: %v", err)
	}
	if got == nil {
		t.Fatal("want match, got nil")
	}
	if got.PublicID != "newer-orig" {
		t.Errorf("want newest match (newer-orig), got %s", got.PublicID)
	}
}

func TestMarkVerifiedByDedupe_UpdatesVerified(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	seedDeliveredRow(t, s, "target", "alice", "bob", "msg", 0, now)

	if err := s.MarkVerifiedByDedupe(ctx, "target"); err != nil {
		t.Fatalf("MarkVerifiedByDedupe: %v", err)
	}

	var v int
	_ = s.DB().QueryRowContext(ctx,
		`SELECT verified FROM messages WHERE public_id = ?`, "target").Scan(&v)
	if v != 1 {
		t.Errorf("want verified=1, got %d", v)
	}
}

func TestMarkVerifiedByDedupe_ErrNotFoundWhenAlreadyVerified(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	seedDeliveredRow(t, s, "already", "alice", "bob", "msg", 1, now) // already verified

	err := s.MarkVerifiedByDedupe(ctx, "already")
	if err == nil {
		t.Error("want error for already-verified row, got nil")
	}
}

func TestMarkVerifiedByDedupe_ErrNotFoundWhenMissing(t *testing.T) {
	s, _ := Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	err := s.MarkVerifiedByDedupe(ctx, "nonexistent")
	if err == nil {
		t.Error("want error for missing id, got nil")
	}
}

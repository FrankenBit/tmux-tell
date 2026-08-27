package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// refuseOne fills to's queue to cap and then trips it, returning nothing but
// leaving exactly one more StateRefused row addressed to `to`.
func refuseOne(t *testing.T, s *Store, from, to string, cap int) {
	t.Helper()
	ctx := context.Background()
	var depth int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE to_agent = ? AND state = ?`,
		CanonicalName(to), StateQueued).Scan(&depth); err != nil {
		t.Fatalf("depth: %v", err)
	}
	for i := depth; i < cap; i++ {
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: from, ToAgent: to, Body: "filler", MaxRecipientQueue: cap,
		}); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
	_, err := s.InsertMessage(ctx, InsertParams{
		FromAgent: from, ToAgent: to, Body: "refused-body", MaxRecipientQueue: cap,
	})
	if !errors.Is(err, ErrRecipientQueueFull) {
		t.Fatalf("expected refusal, got %v", err)
	}
}

// TestRefusedInbound pins #933's recipient-side surface: the count of inbound
// a recipient never received.
//
// Each arm asserts a DISCRIMINATING field rather than "the struct is
// non-zero" — an arm that only checks Total passes when the window is broken,
// and one that only checks Recent passes when the agent scoping is.
func TestRefusedInbound(t *testing.T) {
	ctx := context.Background()

	t.Run("counts_refused_inbound_for_this_agent", func(t *testing.T) {
		s := newTestStore(t)
		refuseOne(t, s, "alice", "bob", 2)
		refuseOne(t, s, "alice", "bob", 2)

		got, err := s.RefusedInbound(ctx, "bob")
		if err != nil {
			t.Fatalf("RefusedInbound: %v", err)
		}
		if got.Total != 2 {
			t.Errorf("Total = %d, want 2", got.Total)
		}
		if got.Recent != 2 {
			t.Errorf("Recent = %d, want 2 (both just written, inside the window)", got.Recent)
		}
		if got.NewestAt == "" {
			t.Error("NewestAt is empty, want the newest refused row's created_at")
		}
	})

	t.Run("scoped_to_the_recipient_not_the_sender", func(t *testing.T) {
		s := newTestStore(t)
		refuseOne(t, s, "alice", "bob", 2)

		// bob is the RECIPIENT who lost the message; alice already knows,
		// she got ok:false. A query keyed on from_agent would invert this.
		if got, _ := s.RefusedInbound(ctx, "bob"); got.Total != 1 {
			t.Errorf("bob (recipient) Total = %d, want 1", got.Total)
		}
		if got, _ := s.RefusedInbound(ctx, "alice"); got.Total != 0 {
			t.Errorf("alice (sender) Total = %d, want 0 — this is the RECIPIENT surface", got.Total)
		}
	})

	t.Run("window_splits_recent_from_total", func(t *testing.T) {
		s := newTestStore(t)
		refuseOne(t, s, "alice", "bob", 2)

		// Age the existing refusal well past the window, then add a fresh one.
		old := time.Now().UTC().Add(-72 * time.Hour).Format(sqliteTimeFormat)
		if _, err := s.db.ExecContext(ctx,
			`UPDATE messages SET created_at = ? WHERE to_agent = ? AND state = ?`,
			old, "bob", StateRefused); err != nil {
			t.Fatalf("age the row: %v", err)
		}
		refuseOne(t, s, "alice", "bob", 2)

		got, err := s.RefusedInbound(ctx, "bob")
		if err != nil {
			t.Fatalf("RefusedInbound: %v", err)
		}
		// The split IS the feature: an all-time count never decreases, so a
		// banner built on Total becomes wallpaper. Recent is what decays.
		if got.Recent != 1 {
			t.Errorf("Recent = %d, want 1 — the 72h-old row must fall out of the window", got.Recent)
		}
		if got.Total != 2 {
			t.Errorf("Total = %d, want 2 — the old row must still be counted overall", got.Total)
		}
		// NewestAt must be the FRESH row, not the aged one. Asserting only
		// that it is non-empty leaves MAX-vs-MIN unguarded: measured, a
		// MAX(created_at) -> MIN(created_at) mutation reddened NOTHING until
		// this arm existed. The banner prints "newest N ago", so MIN would
		// report the oldest refusal as if it had just happened.
		if got.NewestAt == "" {
			t.Fatal("NewestAt is empty")
		}
		if got.NewestAt <= old {
			t.Errorf("NewestAt = %q, want the FRESH row (aged row is %q) — MIN would pass a non-empty check",
				got.NewestAt, old)
		}
	})

	t.Run("CONTROL_no_refusals_is_a_zero_struct_and_no_error", func(t *testing.T) {
		s := newTestStore(t)
		if _, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "delivered fine",
			MaxRecipientQueue: 10,
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		got, err := s.RefusedInbound(ctx, "bob")
		if err != nil {
			t.Fatalf("RefusedInbound: %v", err)
		}
		// Without this arm the suite cannot fail in the world where the
		// query counts every row regardless of state.
		if got.Total != 0 || got.Recent != 0 || got.NewestAt != "" {
			t.Fatalf("got %+v, want a zero struct — a successful send is not a refusal", got)
		}
	})

	t.Run("only_refused_state_counts", func(t *testing.T) {
		s := newTestStore(t)
		// A FAILED row is a different thing: it reached the recipient's
		// queue and then failed to paste. The sender is told either way;
		// only refusals never entered the queue at all.
		res, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "will fail", MaxRecipientQueue: 10,
		})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
		// Direct UPDATE, not MarkFailed: that method transitions a row out of
		// StateDelivering, and driving a row through the full claim cycle
		// would test the mailman rather than this query's state filter.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE messages SET state = ?, error = ? WHERE public_id = ?`,
			StateFailed, "pane gone", res.PublicID); err != nil {
			t.Fatalf("mark row failed: %v", err)
		}
		if got, _ := s.RefusedInbound(ctx, "bob"); got.Total != 0 {
			t.Errorf("Total = %d, want 0 — a FAILED row is not a REFUSED row", got.Total)
		}
	})
}

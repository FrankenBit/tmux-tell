package store

import (
	"context"
	"testing"
	"time"
)

func TestDeleteMessages_ByState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "2"})
	m, _ := s.ClaimNext(ctx, "b")
	_ = s.MarkDelivered(ctx, m.PublicID)

	n, err := s.DeleteMessages(ctx, "", []State{StateQueued})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	rest, _ := s.ListMessages(ctx, ListFilter{})
	if len(rest) != 1 {
		t.Errorf("remaining = %d, want 1", len(rest))
	}
}

func TestDeleteMessages_ByStateAndAgent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "c", Body: "2"})

	n, err := s.DeleteMessages(ctx, "b", []State{StateQueued})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1", n)
	}
	cMsgs, _ := s.ListMessages(ctx, ListFilter{ToAgent: "c"})
	if len(cMsgs) != 1 {
		t.Errorf("c untouched, but got %d", len(cMsgs))
	}
}

func TestDeleteMessages_MultipleStates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "1"})
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "a", ToAgent: "b", Body: "2"})
	_, _ = s.ClaimNext(ctx, "b")

	n, err := s.DeleteMessages(ctx, "", []State{StateQueued, StateDelivering})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}
}

func TestDeleteMessages_AgentsTableUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "1"})

	_, _ = s.DeleteMessages(ctx, "", []State{StateQueued})

	list, _ := s.ListAgents(ctx)
	if len(list) != 2 {
		t.Errorf("agents = %d, want 2 (untouched)", len(list))
	}
}

func TestDeleteMessages_RequiresStates(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.DeleteMessages(context.Background(), "", nil); err == nil {
		t.Error("want error for empty states")
	}
}

// TestDeleteMessagesBefore_SameDayCutoff pins that the schema's T-format
// created_at compares correctly with a T-format cutoff on the same day.
// The schema uses strftime('%Y-%m-%dT%H:%M:%fZ','now') — same format as the
// cutoff produced by strandedTimeFormat — so lexicographic comparison is
// consistent and there is no SPACE-vs-T ordering anomaly.
func TestDeleteMessagesBefore_SameDayCutoff(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	// Seed one "old" row (before base) and one "new" row (after base) using
	// the schema's T-format directly, so the test exercises the real comparison
	// path rather than relying on the implicit "all inserted = before future".
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"old1", "a", "b", "x", "message", string(StateDelivered),
		base.Add(-1*time.Hour).UTC().Format(sqliteTimeFormat))
	if err != nil {
		t.Fatalf("seed old: %v", err)
	}
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO messages (public_id, from_agent, to_agent, body, kind, state, created_at)
		 VALUES (?,?,?,?,?,?,?)`,
		"new1", "a", "b", "y", "message", string(StateDelivered),
		base.Add(1*time.Hour).UTC().Format(sqliteTimeFormat))
	if err != nil {
		t.Fatalf("seed new: %v", err)
	}

	cutoff := base.UTC().Format(sqliteTimeFormat)
	n, err := s.DeleteMessagesBefore(ctx, "", cutoff, []State{StateDelivered})
	if err != nil {
		t.Fatalf("DeleteMessagesBefore: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only the pre-base row)", n)
	}
	rest, _ := s.ListMessages(ctx, ListFilter{})
	if len(rest) != 1 || rest[0].PublicID != "new1" {
		t.Errorf("remaining = %v, want [new1]", rest)
	}
}

package store

import (
	"context"
	"testing"
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

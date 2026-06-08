package store

import (
	"context"
	"testing"
)

func TestMarkAcknowledged_HappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	res, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "hello"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	if err := s.MarkAcknowledged(ctx, "bob", res.PublicID); err != nil {
		t.Fatalf("MarkAcknowledged: %v", err)
	}

	// The message should no longer appear in the queued view.
	msgs, err := s.ListMessages(ctx, ListFilter{ToAgent: "bob", State: StateQueued})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("queued after ack = %d, want 0", len(msgs))
	}

	// The message should appear in the acknowledged view.
	acked, err := s.ListMessages(ctx, ListFilter{ToAgent: "bob", State: StateAcknowledged})
	if err != nil {
		t.Fatalf("list acknowledged: %v", err)
	}
	if len(acked) != 1 || acked[0].PublicID != res.PublicID {
		t.Errorf("acknowledged = %v, want [%s]", acked, res.PublicID)
	}
}

func TestMarkAcknowledged_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	res, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x"})

	if err := s.MarkAcknowledged(ctx, "bob", res.PublicID); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	// Second call must not error.
	if err := s.MarkAcknowledged(ctx, "bob", res.PublicID); err != nil {
		t.Errorf("second ack (idempotent): %v", err)
	}
}

func TestMarkAcknowledged_AuthScope(t *testing.T) {
	// carol cannot ack a message addressed to bob.
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, "carol", "%3"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	res, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "y"})

	err := s.MarkAcknowledged(ctx, "carol", res.PublicID)
	if err == nil {
		t.Errorf("expected error acking another agent's message, got nil")
	}
}

func TestMarkAcknowledgedBatch_HappyPath(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Insert 3 backlog messages then 1 new arrival.
	var backlogIDs []int64
	for i := 0; i < 3; i++ {
		res, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "old"})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		// Retrieve the internal id via GetMessage.
		m, err := s.GetMessage(ctx, res.PublicID)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		backlogIDs = append(backlogIDs, m.ID)
	}
	epoch := backlogIDs[len(backlogIDs)-1] // highest backlog id

	// Insert a "new arrival" that must NOT be acked.
	newRes, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "new"})
	if err != nil {
		t.Fatalf("insert new: %v", err)
	}

	n, err := s.MarkAcknowledgedBatch(ctx, "bob", epoch)
	if err != nil {
		t.Fatalf("MarkAcknowledgedBatch: %v", err)
	}
	if n != 3 {
		t.Errorf("rows acked = %d, want 3", n)
	}

	// The new arrival must remain queued.
	msgs, err := s.ListMessages(ctx, ListFilter{ToAgent: "bob", State: StateQueued})
	if err != nil {
		t.Fatalf("list queued: %v", err)
	}
	if len(msgs) != 1 || msgs[0].PublicID != newRes.PublicID {
		t.Errorf("queued after batch ack = %v, want only [%s]", msgs, newRes.PublicID)
	}
}

func TestMarkAcknowledgedBatch_NoEpoch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "alice", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.UpsertAgent(ctx, "bob", "%2"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	_, _ = s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "x"})

	// epochID = 0 → no-op.
	n, err := s.MarkAcknowledgedBatch(ctx, "bob", 0)
	if err != nil {
		t.Fatalf("MarkAcknowledgedBatch(0): %v", err)
	}
	if n != 0 {
		t.Errorf("rows acked = %d, want 0", n)
	}
}

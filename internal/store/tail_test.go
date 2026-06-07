package store

import (
	"context"
	"testing"
)

func TestTailRows_AfterIDAndFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ins := func(from, to, kind string) string {
		r, err := s.InsertMessage(ctx, InsertParams{FromAgent: from, ToAgent: to, Body: "b", Kind: Kind(kind)})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return r.PublicID
	}
	ins("alice", "bob", "message")   // id 1
	ins("bob", "carol", "message")   // id 2
	ins("alice", "carol", "control") // id 3

	// afterID 0 → all three, id-asc.
	all, err := s.TailRows(ctx, 0, TailFilter{}, 0)
	if err != nil {
		t.Fatalf("TailRows: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all = %d, want 3", len(all))
	}
	if all[0].ID >= all[1].ID || all[1].ID >= all[2].ID {
		t.Errorf("rows not id-asc: %d %d %d", all[0].ID, all[1].ID, all[2].ID)
	}

	// afterID = first id → only the later two (rowid-poll cursor).
	rest, _ := s.TailRows(ctx, all[0].ID, TailFilter{}, 0)
	if len(rest) != 2 || rest[0].ID != all[1].ID {
		t.Errorf("after-cursor = %d rows starting %d, want 2 starting %d", len(rest), rest[0].ID, all[1].ID)
	}

	// from filter.
	fromAlice, _ := s.TailRows(ctx, 0, TailFilter{From: "alice"}, 0)
	if len(fromAlice) != 2 {
		t.Errorf("from=alice = %d, want 2", len(fromAlice))
	}
	// to + kind compose (AND).
	carolMsgs, _ := s.TailRows(ctx, 0, TailFilter{To: "carol", Kind: "message"}, 0)
	if len(carolMsgs) != 1 || carolMsgs[0].FromAgent != "bob" {
		t.Errorf("to=carol kind=message = %+v, want 1 (bob→carol)", carolMsgs)
	}
}

func TestMessagesByIDs_ReReadsState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	r, err := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "b"})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, _ := s.TailRows(ctx, 0, TailFilter{}, 0)
	id := rows[0].ID

	// Initially queued.
	got, err := s.MessagesByIDs(ctx, []int64{id})
	if err != nil {
		t.Fatalf("MessagesByIDs: %v", err)
	}
	if len(got) != 1 || got[0].State != StateQueued {
		t.Fatalf("initial = %+v, want queued", got)
	}

	// Transition to delivered → re-read reflects it (same id). Real lifecycle:
	// queued → ClaimNext (delivering) → MarkDelivered.
	if m, err := s.ClaimNext(ctx, "bob"); err != nil || m == nil {
		t.Fatalf("ClaimNext: m=%v err=%v", m, err)
	}
	if err := s.MarkDelivered(ctx, r.PublicID); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	got, _ = s.MessagesByIDs(ctx, []int64{id})
	if got[0].State != StateDelivered {
		t.Errorf("after MarkDelivered = %s, want delivered", got[0].State)
	}
	if !got[0].DeliveredAt.Valid {
		t.Errorf("delivered_at not set after MarkDelivered")
	}

	// Empty ids → no query, empty result.
	if r, err := s.MessagesByIDs(ctx, nil); err != nil || r != nil {
		t.Errorf("empty ids = (%v, %v), want (nil, nil)", r, err)
	}
}

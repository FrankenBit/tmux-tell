package store

import (
	"context"
	"testing"
)

// TestStaleQueued_CountAndAck pins the #390 pre-flip-orphan primitives:
// CountStaleQueued / AckStaleQueued act on queued, non-deferred rows for the
// agent — and EXCLUDE promoted-deferred rows (state=queued but deliver_after set)
// because those bypass the backlog floor in ClaimNext and deliver regardless of a
// delivery_mode flip. The deliver_after IS NULL clause is the load-bearing part.
func TestStaleQueued_CountAndAck(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, a := range []string{"lookout", "other"} {
		if err := s.UpsertAgent(ctx, a, ""); err != nil {
			t.Fatalf("upsert %s: %v", a, err)
		}
	}

	// Two normal queued rows for lookout — the orphan set.
	for _, b := range []string{"m1", "m2"} {
		if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: b}); err != nil {
			t.Fatalf("insert %s: %v", b, err)
		}
	}
	// A deferred row (state=deferred) — excluded by the state filter.
	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "bosun", ToAgent: "lookout", Body: "deferred", DeliverAfter: "resume"}); err != nil {
		t.Fatalf("insert deferred: %v", err)
	}
	// Promote it → state=queued WITH deliver_after set — excluded by deliver_after IS NULL.
	if _, err := s.PromoteDeferred(ctx, "lookout", "resume"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// A queued row for a DIFFERENT agent — excluded by to_agent.
	if _, err := s.InsertMessage(ctx, InsertParams{FromAgent: "bosun", ToAgent: "other", Body: "x"}); err != nil {
		t.Fatalf("insert other: %v", err)
	}

	n, err := s.CountStaleQueued(ctx, "lookout")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("CountStaleQueued = %d, want 2 (the promoted-deferred + cross-agent rows excluded)", n)
	}

	acked, err := s.AckStaleQueued(ctx, "lookout")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked != 2 {
		t.Fatalf("AckStaleQueued = %d, want 2", acked)
	}

	// After ack: nothing stale remains for lookout.
	if n2, _ := s.CountStaleQueued(ctx, "lookout"); n2 != 0 {
		t.Errorf("post-ack CountStaleQueued = %d, want 0", n2)
	}
	// The promoted-deferred row must SURVIVE (still queued, not acked).
	promoted, err := s.ListMessages(ctx, ListFilter{ToAgent: "lookout", State: StateQueued})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(promoted) != 1 || !promoted[0].DeliverAfter.Valid {
		t.Errorf("expected 1 surviving queued promoted-deferred row, got %d: %+v", len(promoted), promoted)
	}
	// The other agent's row is untouched.
	if n3, _ := s.CountStaleQueued(ctx, "other"); n3 != 1 {
		t.Errorf("other agent CountStaleQueued = %d, want 1 (untouched)", n3)
	}
}

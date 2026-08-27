package store

import (
	"context"
	"testing"
	"time"
)

// The AGE must measure the oldest REAL deliverable, not a synthetic notice.
// A pile of auto-generated nudges must not report itself as old backlog —
// the same notice-loop discipline RecipientOldestPendingAt already applies.
func TestBacklogAgeAndAnnounces_AgeIgnoresSyntheticNotices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	insertQueuedTo(t, s, "gina", 1)

	// A notice BACKDATED far older than the real message.
	//
	// The backdating is what makes this arm live. An earlier version inserted
	// the notice AFTER the real row — so MIN(created_at) was the real row
	// either way, and a mutant that dropped the kind filter still passed. The
	// hazardous ingredient was present and the expected answer coincided with
	// the broken one. Verified by mutation: this fixture reddens, the previous
	// one did not.
	n, err := s.InsertNotice(ctx, InsertParams{
		FromAgent: "gina", ToAgent: "gina", Kind: KindBacklogAnnounce, Body: "nudge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE messages SET created_at = ? WHERE public_id = ?`,
		"2020-01-01T00:00:00.000Z", n.PublicID); err != nil {
		t.Fatal(err)
	}

	// now = an hour after the real row, so a correct age is ~1h and an age
	// keyed on the notice would be ~0.
	future := time.Now().UTC().Add(time.Hour)
	age, _, ok, err := s.BacklogAgeAndAnnounces(ctx, "gina", future)
	if err != nil || !ok {
		t.Fatalf("age not determined: ok=%v err=%v", ok, err)
	}
	// Correct: ~1h (the real row). Broken: ~5 years (the backdated notice).
	if age > 2*time.Hour {
		t.Errorf("age=%v — it keyed on the backdated synthetic notice rather than the real row", age)
	}
	if age < 55*time.Minute {
		t.Errorf("age=%v — too small to be the real row", age)
	}
}

// The COUNT must include only DELIVERED announces. An announce that FAILED
// told nobody, so counting it claims the chamber was warned when it was not.
// That is cabinboy's third state and is explicitly not "announced and unread".
func TestBacklogAgeAndAnnounces_CountsOnlyDeliveredAnnounces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	insertQueuedTo(t, s, "gina", 1)

	mk := func(state State) {
		r, err := s.InsertNotice(ctx, InsertParams{
			FromAgent: "gina", ToAgent: "gina", Kind: KindBacklogAnnounce, Body: "nudge",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE messages SET state = ? WHERE public_id = ?`, state, r.PublicID); err != nil {
			t.Fatal(err)
		}
	}
	mk(StateDelivered)
	mk(StateDelivered)
	mk(StateFailed) // told nobody
	mk(StateQueued) // has not told anyone YET

	_, announces, _, err := s.BacklogAgeAndAnnounces(ctx, "gina", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if announces != 2 {
		t.Errorf("announces=%d, want 2 — failed and still-queued announces warned nobody", announces)
	}
}

// No pending real deliverable => ok=false, never a zero age. "oldest 0s" would
// assert the backlog just arrived, which is the opposite of could-not-tell.
func TestBacklogAgeAndAnnounces_NoPendingIsNotZeroAge(t *testing.T) {
	s := newTestStore(t)
	_, _, ok, err := s.BacklogAgeAndAnnounces(context.Background(), "empty", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok=true with no pending deliverable — a zero age would read as brand-new backlog")
	}
}

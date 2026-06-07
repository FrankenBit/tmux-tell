package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// insertQueuedTo inserts n queued messages to `to` and returns their numeric
// ids in insertion (ascending) order, so callers can reason about the
// id-ordinal claim-floor (#204). ids[len-1] is always the highest id.
func insertQueuedTo(t *testing.T, s *Store, to string, n int) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		r, err := s.InsertMessage(ctx, InsertParams{
			FromAgent: "sender", ToAgent: to, Body: fmt.Sprintf("m%d", i),
		})
		if err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
		m, err := s.GetMessage(ctx, r.PublicID)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		ids = append(ids, m.ID)
	}
	return ids
}

func TestQueuedBacklogFloor_Announce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := insertQueuedTo(t, s, "bob", 3)

	// keepNewest = 0 is announce mode: skip the whole backlog, floor at the
	// highest id, skipped == M.
	floor, skipped, err := s.QueuedBacklogFloor(ctx, "bob", 0)
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if floor != ids[2] {
		t.Errorf("floor = %d, want max id %d", floor, ids[2])
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

func TestQueuedBacklogFloor_AutoDeliverWithinCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	insertQueuedTo(t, s, "bob", 3)

	// keepNewest (5) >= M (3): everything delivers. No floor, no nudge.
	floor, skipped, err := s.QueuedBacklogFloor(ctx, "bob", 5)
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if floor != 0 || skipped != 0 {
		t.Errorf("(floor, skipped) = (%d, %d), want (0, 0)", floor, skipped)
	}
}

func TestQueuedBacklogFloor_AutoDeliverExceedsCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := insertQueuedTo(t, s, "bob", 5)

	// keepNewest = 2, M = 5: keep the newest 2 (ids[4], ids[3]); floor lands
	// on the 3rd-highest id (ids[2]); skipped = M - keepNewest = 3.
	floor, skipped, err := s.QueuedBacklogFloor(ctx, "bob", 2)
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if floor != ids[2] {
		t.Errorf("floor = %d, want 3rd-highest id %d", floor, ids[2])
	}
	if skipped != 3 {
		t.Errorf("skipped = %d, want 3", skipped)
	}
}

func TestQueuedBacklogFloor_EmptyBacklog(t *testing.T) {
	s := newTestStore(t)
	floor, skipped, err := s.QueuedBacklogFloor(context.Background(), "bob", 0)
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if floor != 0 || skipped != 0 {
		t.Errorf("(floor, skipped) = (%d, %d), want (0, 0) on empty backlog", floor, skipped)
	}
}

func TestClaimNext_SkipsBacklogFloor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ids := insertQueuedTo(t, s, "bob", 5)

	// Stamp the floor at the 3rd-highest id: the mailman should deliver only
	// the newest two (ids[3], ids[4]) and never the skipped backlog below.
	if err := s.SetBacklogEpoch(ctx, "bob", ids[2]); err != nil {
		t.Fatalf("set epoch: %v", err)
	}

	var claimed []int64
	for {
		m, err := s.ClaimNext(ctx, "bob")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if m == nil {
			break
		}
		claimed = append(claimed, m.ID)
	}

	want := []int64{ids[3], ids[4]} // ascending — ClaimNext orders by id
	if len(claimed) != len(want) {
		t.Fatalf("claimed %v, want %v (only ids above the floor)", claimed, want)
	}
	for i := range want {
		if claimed[i] != want[i] {
			t.Errorf("claimed[%d] = %d, want %d", i, claimed[i], want[i])
		}
	}
	// The skipped rows stay queued (the #204 residue, drained later via #221).
	if d, _ := s.RecipientQueueDepth(ctx, "bob"); d != 3 {
		t.Errorf("post-claim queued depth = %d, want 3 (skipped backlog stays queued)", d)
	}
}

func TestClaimNext_NullEpochClaimsAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Agent registered but never stamped: backlog_epoch_id is NULL → the
	// COALESCE folds to 0 → every row claims, exactly as pre-#204.
	if err := s.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	insertQueuedTo(t, s, "bob", 3)

	n := 0
	for {
		m, err := s.ClaimNext(ctx, "bob")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if m == nil {
			break
		}
		n++
	}
	if n != 3 {
		t.Errorf("claimed %d, want 3 (NULL epoch claims all)", n)
	}
}

func TestClaimNext_PostEpochArrivalDelivers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	ids := insertQueuedTo(t, s, "bob", 2)
	// Announce: skip both, floor at the highest existing id.
	if err := s.SetBacklogEpoch(ctx, "bob", ids[1]); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	// A message arriving AFTER register gets a higher id than the floor and
	// must deliver normally — the floor only suppresses pre-existing backlog.
	newIDs := insertQueuedTo(t, s, "bob", 1)

	m, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if m == nil || m.ID != newIDs[0] {
		t.Fatalf("claimed %v, want the post-epoch arrival id %d", m, newIDs[0])
	}
	if m2, _ := s.ClaimNext(ctx, "bob"); m2 != nil {
		t.Errorf("second claim = id %d, want nil (backlog still suppressed)", m2.ID)
	}
}

func TestSetBacklogEpoch_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "bob", "%1"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Fresh agent: epoch is NULL (invalid).
	a, err := s.GetAgent(ctx, "bob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.BacklogEpoch.Valid {
		t.Errorf("fresh agent BacklogEpoch.Valid = true, want false (NULL)")
	}

	if err := s.SetBacklogEpoch(ctx, "bob", 42); err != nil {
		t.Fatalf("set epoch: %v", err)
	}
	a, err = s.GetAgent(ctx, "bob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !a.BacklogEpoch.Valid || a.BacklogEpoch.Int64 != 42 {
		t.Errorf("BacklogEpoch = %+v, want valid 42", a.BacklogEpoch)
	}

	// Stamping again advances (re-register semantics): later writes win.
	if err := s.SetBacklogEpoch(ctx, "bob", 99); err != nil {
		t.Fatalf("re-stamp: %v", err)
	}
	a, _ = s.GetAgent(ctx, "bob")
	if a.BacklogEpoch.Int64 != 99 {
		t.Errorf("BacklogEpoch = %d after re-stamp, want 99", a.BacklogEpoch.Int64)
	}
}

func TestSetBacklogEpoch_NotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetBacklogEpoch(context.Background(), "ghost", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

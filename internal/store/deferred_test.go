package store

import (
	"context"
	"testing"
)

// insertDeferred stores a deferred message from→to with the given trigger and
// returns its public_id.
func insertDeferred(t *testing.T, s *Store, from, to, trigger string) string {
	t.Helper()
	r, err := s.InsertMessage(context.Background(), InsertParams{
		FromAgent: from, ToAgent: to, Body: "staged", DeliverAfter: trigger,
	})
	if err != nil {
		t.Fatalf("insert deferred: %v", err)
	}
	return r.PublicID
}

func stateOf(t *testing.T, s *Store, publicID string) State {
	t.Helper()
	m, err := s.GetMessage(context.Background(), publicID)
	if err != nil {
		t.Fatalf("get %s: %v", publicID, err)
	}
	return m.State
}

// TestInsertDeferred_StateAndNotClaimable pins the substrate: a deferred insert
// lands in StateDeferred, carries its trigger, and is invisible to ClaimNext
// (the mailman never picks it up while deferred).
func TestInsertDeferred_StateAndNotClaimable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := insertDeferred(t, s, "bob", "bob", "resume")

	if got := stateOf(t, s, id); got != StateDeferred {
		t.Errorf("state = %q, want %q", got, StateDeferred)
	}
	m, _ := s.GetMessage(ctx, id)
	if !m.DeliverAfter.Valid || m.DeliverAfter.String != "resume" {
		t.Errorf("deliver_after = %+v, want resume", m.DeliverAfter)
	}
	claimed, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed != nil {
		t.Errorf("deferred row should NOT be claimable; got %s", claimed.PublicID)
	}
}

// TestPromoteDeferred_RoundTrip pins the core flush: a deferred row is not
// claimable until PromoteDeferred fires its trigger, after which it is queued
// and the mailman claims it.
func TestPromoteDeferred_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := insertDeferred(t, s, "bob", "bob", "resume")

	n, err := s.PromoteDeferred(ctx, "bob", "resume")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if n != 1 {
		t.Fatalf("promoted = %d, want 1", n)
	}
	if got := stateOf(t, s, id); got != StateQueued {
		t.Errorf("post-promote state = %q, want %q", got, StateQueued)
	}
	claimed, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.PublicID != id {
		t.Errorf("promoted row should be claimable; got %v", claimed)
	}
}

// TestPromoteDeferred_Idempotent: a second flush with nothing left to promote
// is a no-op (0, nil), not an error — so a chamber can flush unconditionally.
func TestPromoteDeferred_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	insertDeferred(t, s, "bob", "bob", "resume")
	if n, _ := s.PromoteDeferred(ctx, "bob", "resume"); n != 1 {
		t.Fatalf("first promote = %d, want 1", n)
	}
	n, err := s.PromoteDeferred(ctx, "bob", "resume")
	if err != nil {
		t.Fatalf("second promote errored: %v", err)
	}
	if n != 0 {
		t.Errorf("second promote = %d, want 0 (idempotent no-op)", n)
	}
}

// TestPromoteDeferred_ScopedToAgent pins the authorization shape: an agent's
// flush promotes only rows addressed to itself; another agent's flush can't
// touch them.
func TestPromoteDeferred_ScopedToAgent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := insertDeferred(t, s, "bob", "bob", "resume")

	// carol flushing "resume" must not promote bob's deferred row.
	if n, _ := s.PromoteDeferred(ctx, "carol", "resume"); n != 0 {
		t.Errorf("cross-agent flush promoted %d rows, want 0", n)
	}
	if got := stateOf(t, s, id); got != StateDeferred {
		t.Errorf("bob's row state = %q after carol's flush, want still %q", got, StateDeferred)
	}
}

// TestPromoteDeferred_TriggerMismatch: only rows whose deliver_after matches the
// flushed trigger are promoted.
func TestPromoteDeferred_TriggerMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := insertDeferred(t, s, "bob", "bob", "resume")
	if n, _ := s.PromoteDeferred(ctx, "bob", "register"); n != 0 {
		t.Errorf("mismatched-trigger flush promoted %d, want 0", n)
	}
	if got := stateOf(t, s, id); got != StateDeferred {
		t.Errorf("state = %q after mismatched flush, want still deferred", got)
	}
}

// TestListMessages_DeferredVisibility: deferred rows are hidden from the
// default all-states view and surfaced only via the Deferred opt-in.
func TestListMessages_DeferredVisibility(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	deferredID := insertDeferred(t, s, "bob", "bob", "resume")
	queued, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "bob", ToAgent: "bob", Body: "live"})

	// Default (no state filter): deferred excluded.
	all, _ := s.ListMessages(ctx, ListFilter{ToAgent: "bob"})
	for _, m := range all {
		if m.PublicID == deferredID {
			t.Errorf("default ListMessages leaked deferred row %s", deferredID)
		}
	}
	if len(all) != 1 || all[0].PublicID != queued.PublicID {
		t.Errorf("default view = %d rows, want only the queued one", len(all))
	}

	// Opt-in: only deferred.
	def, _ := s.ListMessages(ctx, ListFilter{ToAgent: "bob", Deferred: true})
	if len(def) != 1 || def[0].PublicID != deferredID {
		t.Errorf("Deferred view = %v, want only the deferred row", def)
	}
}

// TestDeferred_BypassesClaimFloor is the #204 composition: a deferred row has
// an old id, so a floor set above it (e.g. a register between defer and flush)
// would skip it under the plain id>floor test. The deliver_after-marker
// exemption makes the promoted row claimable regardless of the floor.
func TestDeferred_BypassesClaimFloor(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "bob", "%3")

	// 1. Stage a deferred message (gets a low id).
	deferredID := insertDeferred(t, s, "bob", "bob", "resume")

	// 2. A burst of normal queued backlog arrives (higher ids).
	var lastQueued string
	for i := 0; i < 3; i++ {
		r, _ := s.InsertMessage(ctx, InsertParams{FromAgent: "alice", ToAgent: "bob", Body: "backlog"})
		lastQueued = r.PublicID
	}

	// 3. Register-time floor set to the top of the queued backlog (announce
	//    mode: skip all pre-existing queued). The deferred row's id is BELOW it.
	floor, skipped, err := s.QueuedBacklogFloor(ctx, "bob", 0)
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if skipped == 0 {
		t.Fatalf("expected a non-zero floor from the queued backlog")
	}
	if err := s.SetBacklogEpoch(ctx, "bob", floor); err != nil {
		t.Fatalf("set floor: %v", err)
	}

	// 4. Flush promotes the deferred row to queued (still a low id, below floor).
	if n, _ := s.PromoteDeferred(ctx, "bob", "resume"); n != 1 {
		t.Fatalf("promote = %d, want 1", n)
	}

	// 5. ClaimNext must return the promoted-deferred row FIRST (it bypasses the
	//    floor via its deliver_after marker), not skip it as "old backlog".
	claimed, err := s.ClaimNext(ctx, "bob")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.PublicID != deferredID {
		t.Errorf("claim = %v, want the promoted-deferred row %s (it must bypass the #204 floor); lastQueued=%s",
			claimed, deferredID, lastQueued)
	}
}

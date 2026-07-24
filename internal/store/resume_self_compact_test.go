package store

import (
	"context"
	"testing"
	"time"
)

// TestPromoteResumeOnSelfCompactIfNew_PromotesOncePerSignal pins the #846 store
// half: a deferred resume row promotes on a NEW self-compact edge, the watermark
// advances so the edge is not re-consumed, and a repeat with no new signal is a
// no-op.
//
// Mutation anchors:
//   - weakening the `last_self_compact_at > resume_promoted_at` guard to `>=`
//     makes the no-new-signal call re-promote → the "no-op" assertion flips red;
//   - dropping the `resume_promoted_at = last_self_compact_at` advance makes every
//     eligible call re-promote the same edge → same red;
//   - sharing self_compact_counted_at instead of the own column would let
//     CountSelfCompactIfNew's advance suppress this promote — caught by
//     TestPromoteResumeOnSelfCompactIfNew_IndependentOfShrinkWatermark below.
func TestPromoteResumeOnSelfCompactIfNew_PromotesOncePerSignal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// A resume row is staged, sitting deferred.
	id := insertDeferred(t, s, "bosun", "engineer", "resume")
	if got := stateOf(t, s, id); got != StateDeferred {
		t.Fatalf("staged row state = %q, want %q", got, StateDeferred)
	}

	// No self-compact signal yet → nothing promotes, row stays deferred.
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || promoted || n != 0 {
		t.Fatalf("no-signal promote = (%v,%d,%v), want (false,0,nil)", promoted, n, err)
	}
	if got := stateOf(t, s, id); got != StateDeferred {
		t.Fatalf("row state after no-signal = %q, want still %q", got, StateDeferred)
	}

	// First self-compact edge → the row promotes, watermark advances to the signal.
	if err := s.setSelfCompactSignalAt(ctx, "engineer", base); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || !promoted || n != 1 {
		t.Fatalf("first promote = (%v,%d,%v), want (true,1,nil)", promoted, n, err)
	}
	if got := stateOf(t, s, id); got != StateQueued {
		t.Errorf("promoted row state = %q, want %q", got, StateQueued)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.ResumePromotedAt != a.LastSelfCompactAt || a.ResumePromotedAt == "" {
		t.Errorf("watermark=%q not advanced to signal=%q", a.ResumePromotedAt, a.LastSelfCompactAt)
	}

	// No new signal → no-op (edge already consumed). A fresh deferred row stays put.
	id2 := insertDeferred(t, s, "bosun", "engineer", "resume")
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || promoted || n != 0 {
		t.Errorf("repeat with no new signal = (%v,%d,%v), want (false,0,nil)", promoted, n, err)
	}
	if got := stateOf(t, s, id2); got != StateDeferred {
		t.Errorf("row staged after edge = %q, want still %q (no new compact)", got, StateDeferred)
	}

	// A newer self-compact edge → the second row promotes.
	if err := s.setSelfCompactSignalAt(ctx, "engineer", base.Add(time.Minute)); err != nil {
		t.Fatalf("second signal: %v", err)
	}
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || !promoted || n != 1 {
		t.Errorf("second promote = (%v,%d,%v), want (true,1,nil)", promoted, n, err)
	}
	if got := stateOf(t, s, id2); got != StateQueued {
		t.Errorf("second row state = %q, want %q", got, StateQueued)
	}
}

// TestPromoteResumeOnSelfCompactIfNew_IndependentOfShrinkWatermark is the
// load-bearing #846 design test: the resume promote and the #285 respawn counter
// must NOT consume each other's compaction edge. They read the SAME
// last_self_compact_at signal but own SEPARATE watermarks (resume_promoted_at vs
// self_compact_counted_at), so a single self-compact must feed BOTH — counting a
// shrink AND promoting a resume row, in either order, off one signal.
//
// Mutation anchor: point PromoteResumeOnSelfCompactIfNew at self_compact_counted_at
// (the tempting reuse the tracker warned against) and whichever consumer runs
// second sees the edge already consumed by the first → one of the two assertions
// below flips red.
func TestPromoteResumeOnSelfCompactIfNew_IndependentOfShrinkWatermark(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Opt the agent into respawn counting so CountSelfCompactIfNew is live too.
	if err := s.SetRespawnAfterShrinks(ctx, "engineer", 3); err != nil {
		t.Fatalf("set respawn: %v", err)
	}
	id := insertDeferred(t, s, "bosun", "engineer", "resume")
	if err := s.setSelfCompactSignalAt(ctx, "engineer", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("signal: %v", err)
	}

	// Count the shrink FIRST — this advances self_compact_counted_at.
	if counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer"); err != nil || !counted || n != 1 {
		t.Fatalf("count = (%v,%d,%v), want (true,1,nil)", counted, n, err)
	}
	// The SAME edge must still promote the resume row — it would not if the two
	// shared a watermark, because the count above would have consumed it.
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || !promoted || n != 1 {
		t.Fatalf("promote after count = (%v,%d,%v), want (true,1,nil) — edge must feed both", promoted, n, err)
	}
	if got := stateOf(t, s, id); got != StateQueued {
		t.Errorf("row state = %q, want %q", got, StateQueued)
	}
}

// TestPromoteResumeOnSelfCompactIfNew_UnknownAgentNoOp: a vanished agent is a
// silent no-op, not an error — the mailman polls this every eligible iteration.
func TestPromoteResumeOnSelfCompactIfNew_UnknownAgentNoOp(t *testing.T) {
	promoted, n, err := newTestStore(t).PromoteResumeOnSelfCompactIfNew(context.Background(), "ghost", "resume")
	if err != nil || promoted || n != 0 {
		t.Errorf("promote on missing agent = (%v,%d,%v), want (false,0,nil)", promoted, n, err)
	}
}

// TestPromoteResumeOnSelfCompactIfNew_TriggerScoped: the promote is scoped to the
// passed trigger, so a self-compact edge does not drain a register-deferred row.
func TestPromoteResumeOnSelfCompactIfNew_TriggerScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	reg := insertDeferred(t, s, "bosun", "engineer", "register")
	if err := s.setSelfCompactSignalAt(ctx, "engineer", time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if promoted, n, err := s.PromoteResumeOnSelfCompactIfNew(ctx, "engineer", "resume"); err != nil || !promoted || n != 0 {
		t.Fatalf("promote = (%v,%d,%v), want (true,0,nil) — edge claimed, but no resume rows", promoted, n, err)
	}
	if got := stateOf(t, s, reg); got != StateDeferred {
		t.Errorf("register row state = %q, want still %q (resume promote must not touch it)", got, StateDeferred)
	}
}

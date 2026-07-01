package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A freshly-registered agent has respawn_after_shrinks = 0 (disabled) and
// respawn_shrink_count = 0 — the migration defaults. SetRespawnAfterShrinks
// round-trips the threshold through GetAgent and ListAgents, and 0 disables.
func TestSetRespawnAfterShrinks_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	a, err := s.GetAgent(ctx, "pilot")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.RespawnAfterShrinks != 0 || a.RespawnShrinkCount != 0 {
		t.Errorf("fresh agent respawn_after_shrinks=%d shrink_count=%d, want both 0",
			a.RespawnAfterShrinks, a.RespawnShrinkCount)
	}

	if err := s.SetRespawnAfterShrinks(ctx, "pilot", 3); err != nil {
		t.Fatalf("SetRespawnAfterShrinks(3): %v", err)
	}
	a, _ = s.GetAgent(ctx, "pilot")
	if a.RespawnAfterShrinks != 3 {
		t.Errorf("GetAgent respawn_after_shrinks = %d, want 3", a.RespawnAfterShrinks)
	}

	// The listing path carries it too.
	list, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, ag := range list {
		if ag.Name == "pilot" {
			found = true
			if ag.RespawnAfterShrinks != 3 {
				t.Errorf("ListAgents respawn_after_shrinks = %d, want 3", ag.RespawnAfterShrinks)
			}
		}
	}
	if !found {
		t.Fatal("pilot not in ListAgents")
	}

	// 0 disables (round-trips back).
	if err := s.SetRespawnAfterShrinks(ctx, "pilot", 0); err != nil {
		t.Fatalf("SetRespawnAfterShrinks(0): %v", err)
	}
	a, _ = s.GetAgent(ctx, "pilot")
	if a.RespawnAfterShrinks != 0 {
		t.Errorf("respawn_after_shrinks = %d after disable, want 0", a.RespawnAfterShrinks)
	}
}

// Negative thresholds are rejected. Mutation anchor: dropping the `n < 0` guard
// in SetRespawnAfterShrinks flips this red (a negative threshold would persist
// and, being < any count, could never fire — a silently-broken config).
func TestSetRespawnAfterShrinks_RejectsNegative(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetRespawnAfterShrinks(ctx, "pilot", -1); err == nil {
		t.Error("SetRespawnAfterShrinks(-1) accepted, want rejected")
	}
	// And the row was never written (still the default 0).
	a, _ := s.GetAgent(ctx, "pilot")
	if a.RespawnAfterShrinks != 0 {
		t.Errorf("respawn_after_shrinks = %d after rejected write, want 0", a.RespawnAfterShrinks)
	}
}

func TestSetRespawnAfterShrinks_UnknownAgent(t *testing.T) {
	s := newTestStore(t)
	err := s.SetRespawnAfterShrinks(context.Background(), "ghost", 3)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// IncrementRespawnShrinkCount returns the monotonically-increasing post-increment
// count and persists it; ResetRespawnShrinkCount clears it back to 0; and a
// fresh increment after reset starts the cycle again at 1.
func TestRespawnShrinkCount_IncrementResetCycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	for want := 1; want <= 3; want++ {
		got, err := s.IncrementRespawnShrinkCount(ctx, "engineer")
		if err != nil {
			t.Fatalf("increment: %v", err)
		}
		if got != want {
			t.Errorf("IncrementRespawnShrinkCount returned %d, want %d", got, want)
		}
		a, _ := s.GetAgent(ctx, "engineer")
		if a.RespawnShrinkCount != want {
			t.Errorf("persisted shrink_count = %d, want %d", a.RespawnShrinkCount, want)
		}
	}

	if err := s.ResetRespawnShrinkCount(ctx, "engineer"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.RespawnShrinkCount != 0 {
		t.Errorf("shrink_count = %d after reset, want 0", a.RespawnShrinkCount)
	}

	// The counter resumes from 1 after a reset (mutation anchor: a reset that
	// set the count to anything but 0 flips this).
	got, err := s.IncrementRespawnShrinkCount(ctx, "engineer")
	if err != nil {
		t.Fatalf("increment after reset: %v", err)
	}
	if got != 1 {
		t.Errorf("first increment after reset = %d, want 1", got)
	}
}

func TestIncrementRespawnShrinkCount_UnknownAgent(t *testing.T) {
	_, err := newTestStore(t).IncrementRespawnShrinkCount(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// ResetRespawnShrinkCount is a no-op (not an error) against a missing agent —
// best-effort bookkeeping bracketing the respawn pathway.
func TestResetRespawnShrinkCount_UnknownAgentNoError(t *testing.T) {
	if err := newTestStore(t).ResetRespawnShrinkCount(context.Background(), "ghost"); err != nil {
		t.Errorf("reset on missing agent = %v, want nil (no-op)", err)
	}
}

// #285 PR2. A fresh agent has an empty self-compact signal + watermark;
// SetSelfCompactSignal stamps last_self_compact_at (surfaced via GetAgent).
func TestSetSelfCompactSignal_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	a, _ := s.GetAgent(ctx, "pilot")
	if a.LastSelfCompactAt != "" || a.SelfCompactCountedAt != "" {
		t.Errorf("fresh agent signal=%q counted=%q, want both empty", a.LastSelfCompactAt, a.SelfCompactCountedAt)
	}
	if err := s.SetSelfCompactSignal(ctx, "pilot"); err != nil {
		t.Fatalf("SetSelfCompactSignal: %v", err)
	}
	a, _ = s.GetAgent(ctx, "pilot")
	if a.LastSelfCompactAt == "" {
		t.Error("last_self_compact_at empty after SetSelfCompactSignal, want stamped")
	}
	// The signal alone never touches the watermark or the counter (mailman-owned).
	if a.SelfCompactCountedAt != "" || a.RespawnShrinkCount != 0 {
		t.Errorf("signal wrote watermark=%q count=%d, want the hook to touch NEITHER",
			a.SelfCompactCountedAt, a.RespawnShrinkCount)
	}
}

func TestSetSelfCompactSignal_UnknownAgent(t *testing.T) {
	err := newTestStore(t).SetSelfCompactSignal(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// CountSelfCompactIfNew edge-detects a fresh signal exactly once: it counts when
// last_self_compact_at is newer than the watermark, advances the watermark to it,
// and no-ops until a newer signal arrives. Mutation anchors: (a) weakening the
// `last_self_compact_at > self_compact_counted_at` guard to `>=` makes the
// second (no-new-signal) call re-count → the "no-op" assertion flips red;
// (b) dropping the `self_compact_counted_at = last_self_compact_at` advance makes
// every call re-count the same signal → same red.
func TestCountSelfCompactIfNew_CountsOncePerSignal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	// No signal yet → nothing to count.
	if counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer"); err != nil || counted || n != 0 {
		t.Fatalf("no-signal count = (%v,%d,%v), want (false,0,nil)", counted, n, err)
	}

	// First signal → counts once (count 1), watermark advances to the signal.
	if err := s.setSelfCompactSignalAt(ctx, "engineer", base); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer"); err != nil || !counted || n != 1 {
		t.Fatalf("first count = (%v,%d,%v), want (true,1,nil)", counted, n, err)
	}
	a, _ := s.GetAgent(ctx, "engineer")
	if a.SelfCompactCountedAt != a.LastSelfCompactAt || a.SelfCompactCountedAt == "" {
		t.Errorf("watermark=%q not advanced to signal=%q", a.SelfCompactCountedAt, a.LastSelfCompactAt)
	}

	// No new signal → no-op (the edge already consumed).
	if counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer"); err != nil || counted || n != 0 {
		t.Errorf("repeat count with no new signal = (%v,%d,%v), want (false,0,nil)", counted, n, err)
	}

	// A newer signal → counts again (count 2).
	if err := s.setSelfCompactSignalAt(ctx, "engineer", base.Add(time.Minute)); err != nil {
		t.Fatalf("second signal: %v", err)
	}
	if counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer"); err != nil || !counted || n != 2 {
		t.Errorf("second count = (%v,%d,%v), want (true,2,nil)", counted, n, err)
	}
}

// Burst coalescing (documented, accepted semantics): multiple signals since the
// last count collapse to ONE increment — CountSelfCompactIfNew sees only the
// latest last_self_compact_at. Deterministic via injected instants.
func TestCountSelfCompactIfNew_BurstCoalesces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "engineer", "%4"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Three signals land before the mailman observes even once.
	for i := 0; i < 3; i++ {
		if err := s.setSelfCompactSignalAt(ctx, "engineer", base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("signal %d: %v", i, err)
		}
	}
	counted, n, err := s.CountSelfCompactIfNew(ctx, "engineer")
	if err != nil || !counted || n != 1 {
		t.Errorf("burst count = (%v,%d,%v), want (true,1,nil) — a burst coalesces to one", counted, n, err)
	}
}

// A missing agent is a silent no-op (not an error): the mailman polls this every
// eligible iteration, so a vanished agent must not spam errors.
func TestCountSelfCompactIfNew_UnknownAgentNoOp(t *testing.T) {
	counted, n, err := newTestStore(t).CountSelfCompactIfNew(context.Background(), "ghost")
	if err != nil || counted || n != 0 {
		t.Errorf("count on missing agent = (%v,%d,%v), want (false,0,nil)", counted, n, err)
	}
}

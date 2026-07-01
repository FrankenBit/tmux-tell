package store

import (
	"context"
	"errors"
	"testing"
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

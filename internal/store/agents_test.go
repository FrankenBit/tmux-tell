package store

import (
	"context"
	"errors"
	"testing"
)

// TestSetSessionID_SideEffectFree is the load-bearing #644 invariant: the
// session-id backfill setter writes ONLY the session_id column and leaves
// attention_state (#224) and stuck_reason (#298) untouched. register clears
// those on (self-)register; an on-behalf backfill must NOT, or it would erase
// a stale chamber's real signals.
//
// Mutation-pin: make the SetSessionID UPDATE in agents.go also clear
// attention_state (or stuck_reason) and the SideEffectFree subtest fails —
// exactly the regression this pins against.
func TestSetSessionID_SideEffectFree(t *testing.T) {
	newSeeded := func(t *testing.T) *Store {
		t.Helper()
		s := newTestStore(t)
		ctx := context.Background()
		if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
			t.Fatalf("seed agent: %v", err)
		}
		return s
	}

	t.Run("Backfill_EmptyToSet", func(t *testing.T) {
		s := newSeeded(t)
		ctx := context.Background()
		// A freshly-registered agent has an empty session_id (AC#4 backfill case).
		if a, _ := s.GetAgent(ctx, "pilot"); a.SessionID != "" {
			t.Fatalf("precondition: session_id = %q, want empty", a.SessionID)
		}
		if err := s.SetSessionID(ctx, "pilot", "uuid-aaa"); err != nil {
			t.Fatalf("SetSessionID: %v", err)
		}
		if a, _ := s.GetAgent(ctx, "pilot"); a.SessionID != "uuid-aaa" {
			t.Errorf("session_id = %q, want uuid-aaa", a.SessionID)
		}
	})

	t.Run("Refresh_OverwritesExisting", func(t *testing.T) {
		s := newSeeded(t)
		ctx := context.Background()
		// AC#4 refresh case: an existing session_id is overwritten in place.
		if err := s.SetSessionID(ctx, "pilot", "uuid-old"); err != nil {
			t.Fatalf("seed session_id: %v", err)
		}
		if err := s.SetSessionID(ctx, "pilot", "uuid-new"); err != nil {
			t.Fatalf("SetSessionID refresh: %v", err)
		}
		if a, _ := s.GetAgent(ctx, "pilot"); a.SessionID != "uuid-new" {
			t.Errorf("session_id = %q, want uuid-new", a.SessionID)
		}
	})

	t.Run("SideEffectFree_PreservesAttentionAndStuck", func(t *testing.T) {
		s := newSeeded(t)
		ctx := context.Background()
		// Seed the exact signals register would clear: a chamber parked at
		// awaiting_operator with a stuck mailman.
		if err := s.SetAttentionState(ctx, "pilot", AttentionStateAwaitingOperator); err != nil {
			t.Fatalf("seed attention_state: %v", err)
		}
		if err := s.SetStuck(ctx, "pilot", StuckReasonPaneNotFound); err != nil {
			t.Fatalf("seed stuck_reason: %v", err)
		}

		if err := s.SetSessionID(ctx, "pilot", "uuid-backfill"); err != nil {
			t.Fatalf("SetSessionID: %v", err)
		}

		a, _ := s.GetAgent(ctx, "pilot")
		if a.SessionID != "uuid-backfill" {
			t.Errorf("session_id = %q, want uuid-backfill", a.SessionID)
		}
		if a.AttentionState != AttentionStateAwaitingOperator {
			t.Errorf("attention_state = %q, want %q (backfill must NOT clear it)",
				a.AttentionState, AttentionStateAwaitingOperator)
		}
		if a.StuckReason != StuckReasonPaneNotFound {
			t.Errorf("stuck_reason = %q, want %q (backfill must NOT clear it)",
				a.StuckReason, StuckReasonPaneNotFound)
		}
	})

	t.Run("UnknownAgent_ErrNotFound", func(t *testing.T) {
		s := newSeeded(t)
		if err := s.SetSessionID(context.Background(), "ghost", "uuid-x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})
}

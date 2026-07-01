package cli

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestSetSessionID_Core exercises the shared backfill core's contract: a named
// target round-trips, and the two guards (empty target, empty session id) fail
// loud rather than writing a meaningless / clearing value.
func TestSetSessionID_Core(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // best-effort close in test
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatal(err)
	}

	// Round-trips to the store for a named target.
	res, err := setSessionID(ctx, s, "pilot", "uuid-123", false)
	if err != nil {
		t.Fatalf("setSessionID: %v", err)
	}
	if !res.OK || res.Agent != "pilot" || res.SessionID != "uuid-123" || res.Discovered {
		t.Errorf("result = %+v, want {OK:true Agent:pilot SessionID:uuid-123 Discovered:false}", res)
	}
	if a, _ := s.GetAgent(ctx, "pilot"); a.SessionID != "uuid-123" {
		t.Errorf("persisted session_id = %q, want uuid-123", a.SessionID)
	}

	// Empty target is rejected at the surface, before the store.
	if _, err := setSessionID(ctx, s, "", "uuid-123", false); err == nil {
		t.Error("empty target accepted, want an error")
	}

	// Empty session id is rejected — a backfill must POPULATE, never silently
	// clear a prior value (which would itself be a side effect).
	if _, err := setSessionID(ctx, s, "pilot", "", false); err == nil {
		t.Error("empty session id accepted, want an error")
	}

	// A missing target surfaces ErrNotFound from the store.
	if _, err := setSessionID(ctx, s, "ghost", "uuid-123", false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestMCP_SetSessionID_DoesNotClearAttentionOrStuck pins the #224/#298 escape
// at the MCP surface (#644): backfilling a stale chamber's session id via
// tmux-tell.set_session_id must populate session_id WITHOUT clearing the
// attention_state / stuck_reason register would clear. This is the whole point
// of the field-specific backfill — an orchestrator migrating a parked chamber
// on-behalf must not erase its real signals.
func TestMCP_SetSessionID_DoesNotClearAttentionOrStuck(t *testing.T) {
	s := newCmdTestStore(t, "pilot")
	ctx := context.Background()
	if err := s.SetAttentionState(ctx, "pilot", store.AttentionStateAwaitingOperator); err != nil {
		t.Fatalf("seed attention_state: %v", err)
	}
	if err := s.SetStuck(ctx, "pilot", store.StuckReasonPaneNotFound); err != nil {
		t.Fatalf("seed stuck_reason: %v", err)
	}

	got := callMCPTool(t, s, "tmux-tell.set_session_id", map[string]any{
		"name":       "pilot",
		"session_id": "uuid-onbehalf",
	})
	if got["ok"] != true {
		t.Fatalf("ok = %v; got=%v", got["ok"], got)
	}
	if got["session_id"] != "uuid-onbehalf" {
		t.Errorf("session_id = %v, want uuid-onbehalf", got["session_id"])
	}

	a, _ := s.GetAgent(ctx, "pilot")
	if a.SessionID != "uuid-onbehalf" {
		t.Errorf("persisted session_id = %q, want uuid-onbehalf", a.SessionID)
	}
	if a.AttentionState != store.AttentionStateAwaitingOperator {
		t.Errorf("attention_state = %q, want %q (MCP backfill must NOT clear it)",
			a.AttentionState, store.AttentionStateAwaitingOperator)
	}
	if a.StuckReason != store.StuckReasonPaneNotFound {
		t.Errorf("stuck_reason = %q, want %q (MCP backfill must NOT clear it)",
			a.StuckReason, store.StuckReasonPaneNotFound)
	}
}

// TestMCP_SetSessionID_RequiresSessionID pins that the MCP surface fails loud
// when session_id is omitted (it does NOT self-discover — the server's own pane
// is not the target's), rather than writing an empty/clearing value.
func TestMCP_SetSessionID_RequiresSessionID(t *testing.T) {
	s := newCmdTestStore(t, "pilot")

	got := callMCPTool(t, s, "tmux-tell.set_session_id", map[string]any{
		"name": "pilot",
	})
	if got["_isError"] != true {
		t.Errorf("missing session_id should error; got=%v", got)
	}
	// The prior (empty) session_id must be untouched by the rejected call.
	if a, _ := s.GetAgent(context.Background(), "pilot"); a.SessionID != "" {
		t.Errorf("session_id = %q, want empty (rejected call must not write)", a.SessionID)
	}
}

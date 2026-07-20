package cli

import (
	"strings"
	"testing"
)

// strictRolloutTools is the #753 phase-2 set: every args-bearing MCP tool that
// now decodes through decodeStrictArgs (send + ask are covered by phase-1 tests).
// validArgs carries EVERY property in the tool's registered inputSchema, typed —
// so the completeness sub-test below drives the exact documented surface.
var strictRolloutTools = []struct {
	name      string
	validArgs map[string]any
}{
	{"tmux-tell.ping", map[string]any{"agent": "bob", "timeout_seconds": 1}},
	{"tmux-tell.agent_state", map[string]any{"agent": "bob"}},
	{"tmux-tell.set_pane_name", map[string]any{"name": "Alice"}},
	{"tmux-tell.set_metabolism", map[string]any{"value": "warming"}},
	{"tmux-tell.set_session_id", map[string]any{"name": "bob", "session_id": "11111111-1111-1111-1111-111111111111"}},
	{"tmux-tell.flush_deferred", map[string]any{"trigger": "resume"}},
	{"tmux-tell.wait_for_reply", map[string]any{"ask_id": "x", "timeout_ms": 1}},
	{"tmux-tell.check_replies", map[string]any{"ask_id": "x", "since": 0}},
	{"tmux-tell.resend", map[string]any{"id": "x", "force": true}},
	{"tmux-tell.message_status", map[string]any{"id": "x"}},
	{"tmux-tell.get", map[string]any{"id": "x"}},
	{"tmux-tell.control", map[string]any{"to": "alice", "command": "compact", "resume_with": "", "for_task": "", "force_rate_limited": false}},
	{"tmux-tell.agents", map[string]any{"available_only": true}},
	{"tmux-tell.inbox", map[string]any{"state": "queued", "limit": 10, "ack_ids": []string{}, "ack_all": false, "unanswered": false}},
	{"tmux-tell.register", map[string]any{"name": "carol", "pane": "%9", "start_mailman": false, "force": true, "alias": "", "delivery_mode": "mailbox-only"}},
	{"tmux-tell.unregister", map[string]any{"name": "bob", "purge_queue": false, "force": true}},
	{"tmux-tell.flag_operator", map[string]any{"body": "help"}},
}

// TestMCP_StrictRollout_Completeness is the executable completeness gate for the
// #753 phase-2 rollout: for each tool it sends EVERY documented schema property,
// then asserts the call did not fail with "unknown parameter". That is exactly
// the way a struct that fails to cover its schema would break a VALID call — so
// an incomplete input struct reds this tool's row, independent of any manual or
// survey reading of the structs. The tool may still error for other reasons
// (missing pane, unknown id, tmux exec) — only an unknown-parameter rejection is
// a failure here.
func TestMCP_StrictRollout_Completeness(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	for _, tc := range strictRolloutTools {
		t.Run(tc.name, func(t *testing.T) {
			s := newCmdTestStore(t, "alice", "bob")
			got := callMCPTool(t, s, tc.name, tc.validArgs)
			if text, _ := got["_text"].(string); strings.Contains(text, "unknown parameter") {
				t.Errorf("%s: strict decode rejected a DOCUMENTED param — struct is missing a schema key: %q", tc.name, text)
			}
		})
	}
}

// TestMCP_StrictRollout_FailsLoud is the other direction: every rolled-out tool
// must reject an unknown top-level parameter (naming it), where before phase-2 it
// silently vanished. Sends the tool's valid args plus one bogus key.
func TestMCP_StrictRollout_FailsLoud(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	for _, tc := range strictRolloutTools {
		t.Run(tc.name, func(t *testing.T) {
			s := newCmdTestStore(t, "alice", "bob")
			args := map[string]any{"__nope__": true}
			for k, v := range tc.validArgs {
				args[k] = v
			}
			got := callMCPTool(t, s, tc.name, args)
			if got["_isError"] != true {
				t.Fatalf("%s: unknown param must fail loud (ok:false), got=%v", tc.name, got)
			}
			text, _ := got["_text"].(string)
			if !strings.Contains(text, `unknown parameter "__nope__"`) {
				t.Errorf("%s: error %q should name the unknown key __nope__", tc.name, text)
			}
		})
	}
}

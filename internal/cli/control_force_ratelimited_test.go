package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// #573 pins the control-surface arm of the #558 force-rate-limited escape-hatch.
// The enforcement (gate bypass + #105 IsPasteUnsafeForced) is already covered by
// #558's serve tests; what's NEW here is purely input-surface plumbing, so these
// tests assert the force_rate_limited marker REACHES every row each control path
// emits. The marker is read back via ClaimNext — the claim path is the column's
// sole consumer (#558).

// claimForce claims the next deliverable row for agent and returns its
// ForceRateLimited marker. Fails the test if no row is claimable.
func claimForce(t *testing.T, s *store.Store, agent string) bool {
	t.Helper()
	m, err := s.ClaimNext(context.Background(), agent)
	if err != nil {
		t.Fatalf("claim %s: %v", agent, err)
	}
	if m == nil {
		t.Fatalf("claim %s: no row queued", agent)
	}
	return m.ForceRateLimited
}

// TestControl_ForceRateLimited_PlainCommand: the plain path (single control row,
// no macro, no resume) carries the marker. `sleep` on self with no resume_with
// resolves to /compact via path 3 (plain InsertMessage).
func TestControl_ForceRateLimited_PlainCommand(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	res, err := doControl(context.Background(), s, controlParams{
		From: "alice", To: "alice", Command: "sleep", ForceRateLimited: true,
	})
	if err != nil {
		t.Fatalf("doControl plain: %v", err)
	}
	if res.Macro != "" {
		t.Fatalf("macro = %q, want plain (empty)", res.Macro)
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("plain control row not forced — --force-rate-limited dropped on the plain path")
	}
}

// TestControl_ForceRateLimited_RestartMacro_BothRows is the #573 decision pin:
// force applies to BOTH rows of the restart InsertMessagePair, not just the
// primary. Mutation anchor: drop `ForceRateLimited: p.ForceRateLimited` from
// enableP in control.go → the second assertion fails (the re-enable row would
// defer on the same banner the disable just punched through, re-creating the
// half-actioned state #29's atomic insert prevents).
func TestControl_ForceRateLimited_RestartMacro_BothRows(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	res, err := doControl(context.Background(), s, controlParams{
		From: "alice", To: "alice", Command: "mcp-restart-tmux-tell",
		ForceRateLimited: true,
	})
	if err != nil {
		t.Fatalf("doControl restart: %v", err)
	}
	if res.Macro != "restart" {
		t.Fatalf("macro = %q, want restart", res.Macro)
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("restart row 1 (disable) not forced")
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("restart row 2 (enable) not forced — force must ride BOTH rows or the re-enable defers half-actioned (#573)")
	}
}

// TestControl_ForceRateLimited_ResumeWith_BothRows is the second #573 decision
// pin: force applies to BOTH the /compact (control) row and the resume
// (message) row of the sleep+resume InsertMessagePair. Mutation anchor: drop
// `ForceRateLimited: p.ForceRateLimited` from resumeP → the resume assertion
// fails (the continuation would defer, leaving the chamber slept-but-dormant).
func TestControl_ForceRateLimited_ResumeWith_BothRows(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	res, err := doControl(context.Background(), s, controlParams{
		From: "alice", To: "alice", Command: "sleep",
		ResumeWith: "carry on with #573", ForceRateLimited: true,
		MaxBody: 16 * 1024,
	})
	if err != nil {
		t.Fatalf("doControl sleep+resume: %v", err)
	}
	if res.Macro != "resume" {
		t.Fatalf("macro = %q, want resume", res.Macro)
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("sleep row 1 (/compact) not forced")
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("sleep row 2 (resume) not forced — force must ride BOTH rows or the continuation defers slept-but-dormant (#573)")
	}
}

// TestControl_NoForce_DefaultsFalse guards against a hardcoded-true regression:
// without the flag, every row defaults to unforced (so all existing control
// macros keep their normal deferral semantics).
func TestControl_NoForce_DefaultsFalse(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	if _, err := doControl(context.Background(), s, controlParams{
		From: "alice", To: "alice", Command: "mcp-restart-tmux-tell",
	}); err != nil {
		t.Fatalf("doControl restart: %v", err)
	}
	// Claim both rows separately (each call consumes a distinct row) — not via
	// `||`, which would short-circuit past the second row and skip the check.
	row1Forced := claimForce(t, s, "alice")
	row2Forced := claimForce(t, s, "alice")
	if row1Forced || row2Forced {
		t.Errorf("unforced restart macro produced a forced row (row1=%v row2=%v) — default must be false",
			row1Forced, row2Forced)
	}
}

// TestControlCLI_ForceRateLimited_FlagPlumbs drives the CLI surface end-to-end:
// the --force-rate-limited flag must reach the row. Guards the flag→controlParams
// wiring (a missing field there would silently drop the operator's force).
func TestControlCLI_ForceRateLimited_FlagPlumbs(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	t.Setenv("TMUX_TELL_DB", ":memory:")
	t.Cleanup(func() { _ = s.Close() })

	var stdout, stderr bytes.Buffer
	exit := runControlCLI(
		[]string{"--to", "alice", "--command", "sleep", "--force-rate-limited"},
		&stdout, &stderr,
	)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("CLI --force-rate-limited did not reach the row")
	}
}

// TestMCP_Control_ForceRateLimited_PlumbsFlag drives the MCP handler: the
// force_rate_limited JSON field must reach the row. The MCP input struct + schema
// is a distinct surface from the CLI flag, so it needs its own guard.
func TestMCP_Control_ForceRateLimited_PlumbsFlag(t *testing.T) {
	s := newCmdTestStore(t, "alice")
	t.Setenv("TMUX_AGENT_NAME", "alice")
	handler := mcpControlHandler(s)
	_, err := handler(context.Background(), json.RawMessage(
		`{"to":"alice","command":"sleep","force_rate_limited":true}`))
	if err != nil {
		t.Fatalf("mcp control: %v", err)
	}
	if !claimForce(t, s, "alice") {
		t.Errorf("MCP force_rate_limited did not reach the row")
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// installFakeChamberState wires a tmuxio fake runner that returns the
// given pane content on every capture-pane call. The temporal-delta is
// collapsed to a microsecond so tests don't pay the 200ms production
// wait. Cleanup restores both.
func installFakeChamberState(t *testing.T, captureContent string) {
	t.Helper()
	prevDelta := tmuxio.SetChamberStateTemporalDeltaForTest(time.Microsecond)
	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(captureContent), nil
		}
		return nil, nil
	})
	t.Cleanup(func() {
		tmuxio.SetTmuxRunner(prevRunner)
		tmuxio.SetChamberStateTemporalDeltaForTest(prevDelta)
	})
}

// TestStateCLI_HappyPathJSON pins the JSON output shape for the
// idle-classification case. The schema is the durable shape that
// Binnacle's M6b will consume verbatim per cli-semaphore#74.
func TestStateCLI_HappyPathJSON(t *testing.T) {
	installFakeChamberState(t, "history\n──── Chamber ──\n❯ \n────────\n  status\n")
	s := newCmdTestStore(t, "bosun")

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "bosun", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}

	var res chamberStateResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v; stdout=%s", err, stdout.String())
	}
	if res.Agent != "bosun" {
		t.Errorf("agent = %q, want bosun", res.Agent)
	}
	if res.State != "idle" {
		t.Errorf("state = %q, want idle", res.State)
	}
	if !res.Evidence.PromptEmpty {
		t.Errorf("evidence.prompt_empty should be true for the Idle branch")
	}
	if res.Evidence.Reason == "" {
		t.Errorf("evidence.reason should always be populated")
	}
	if res.CapturedAt == "" {
		t.Errorf("captured_at should always be populated")
	}
}

// TestStateCLI_HappyPathText pins the text-format output structure
// (the labeled-columns shape sibling to `claude-msg config show`).
func TestStateCLI_HappyPathText(t *testing.T) {
	installFakeChamberState(t, "history\n❯ \n  status\n")
	s := newCmdTestStore(t, "bosun")

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "bosun", "text", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"AGENT\tbosun", "STATE\tidle", "EVIDENCE\t", "CAPTURED\t"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in text output:\n%s", want, out)
		}
	}
}

// TestStateCLI_AgentNotRegistered pins the error path for an unknown
// agent: the result still emits with State=unknown + a descriptive
// Evidence.Reason; exit is non-zero so scripts can branch.
func TestStateCLI_AgentNotRegistered(t *testing.T) {
	s := newCmdTestStore(t) // empty registry

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "ghost", "json", &stdout, &stderr)
	if exit != exitInternal {
		t.Fatalf("exit = %d, want exitInternal; stderr=%s", exit, stderr.String())
	}

	var res chamberStateResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Agent != "ghost" {
		t.Errorf("agent = %q, want ghost", res.Agent)
	}
	if res.State != "unknown" {
		t.Errorf("state = %q, want unknown (agent-not-registered)", res.State)
	}
	if !strings.Contains(res.Evidence.Reason, "not registered") {
		t.Errorf("evidence.reason should mention 'not registered'; got %q", res.Evidence.Reason)
	}
}

// TestStateCLI_AgentHasNoPane pins the error path for a registered
// agent with no pane: same shape as agent-not-found — result emitted,
// State=unknown, descriptive Reason, non-zero exit.
func TestStateCLI_AgentHasNoPane(t *testing.T) {
	s := newCmdTestStore(t)
	// Register an agent without a pane.
	if err := s.UpsertAgent(context.Background(), "stranded", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "stranded", "json", &stdout, &stderr)
	if exit != exitInternal {
		t.Fatalf("exit = %d, want exitInternal", exit)
	}

	var res chamberStateResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.State != "unknown" {
		t.Errorf("state = %q, want unknown (no-pane)", res.State)
	}
	if !strings.Contains(res.Evidence.Reason, "no pane") {
		t.Errorf("evidence.reason should mention 'no pane'; got %q", res.Evidence.Reason)
	}
}

// TestStateCLI_UnknownFormat pins the validation guard for an
// unrecognized --format value.
func TestStateCLI_UnknownFormat(t *testing.T) {
	installFakeChamberState(t, "❯ \n")
	s := newCmdTestStore(t, "bosun")

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "bosun", "yaml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage", exit)
	}
}

// TestMCP_ChamberState pins the MCP tool's happy-path response. The
// schema MUST match the CLI's JSON output byte-for-byte (modulo
// timestamps) — Binnacle consumes one schema, not two.
func TestMCP_ChamberState(t *testing.T) {
	installFakeChamberState(t, "history\n❯ \n  status\n")
	s := newCmdTestStore(t, "bosun")

	got := callMCPTool(t, s, "semaphore.chamber_state", map[string]any{"agent": "bosun"})
	if got["agent"] != "bosun" {
		t.Errorf("agent = %v, want bosun", got["agent"])
	}
	if got["state"] != "idle" {
		t.Errorf("state = %v, want idle", got["state"])
	}
	if _, ok := got["evidence"]; !ok {
		t.Errorf("evidence field missing from MCP response")
	}
	if _, ok := got["captured_at"]; !ok {
		t.Errorf("captured_at field missing from MCP response")
	}
}

// TestMCP_ChamberState_RequiresAgent pins the input validation: empty
// agent → error response. callMCPTool marks error responses with the
// `_isError` field so consumers can branch on it.
func TestMCP_ChamberState_RequiresAgent(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	got := callMCPTool(t, s, "semaphore.chamber_state", map[string]any{})
	if got["_isError"] != true {
		t.Errorf("expected isError=true for missing agent; got %v", got)
	}
}

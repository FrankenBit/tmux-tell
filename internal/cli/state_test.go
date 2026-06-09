package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// installFakeAgentState wires a tmuxio fake runner that returns the
// given pane content on every capture-pane call. The temporal-delta is
// collapsed to a microsecond so tests don't pay the 200ms production
// wait. Cleanup restores both.
func installFakeAgentState(t *testing.T, captureContent string) {
	t.Helper()
	prevDelta := tmuxio.SetAgentStateTemporalDeltaForTest(time.Microsecond)
	prevRunner := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(captureContent), nil
		}
		return nil, nil
	})
	t.Cleanup(func() {
		tmuxio.SetTmuxRunner(prevRunner)
		tmuxio.SetAgentStateTemporalDeltaForTest(prevDelta)
	})
}

// TestStateCLI_HappyPathJSON pins the JSON output shape for the
// idle-classification case. The schema is the durable shape that
// Binnacle's M6b will consume verbatim per #74.
func TestStateCLI_HappyPathJSON(t *testing.T) {
	installFakeAgentState(t, "history\n──── Agent ──\n❯\u00a0\n────────\n  status\n")
	s := newCmdTestStore(t, "bosun")

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "bosun", "json", &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}

	var res agentStateResult
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
	installFakeAgentState(t, "history\n❯\u00a0\n  status\n")
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

	var res agentStateResult
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

	var res agentStateResult
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
	installFakeAgentState(t, "❯\u00a0\n")
	s := newCmdTestStore(t, "bosun")

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "bosun", "yaml", &stdout, &stderr)
	if exit != exitUsage {
		t.Errorf("exit = %d, want exitUsage", exit)
	}
}

// TestMCP_AgentState pins the MCP tool's happy-path response. The
// schema MUST match the CLI's JSON output byte-for-byte (modulo
// timestamps) — Binnacle consumes one schema, not two.
func TestMCP_AgentState(t *testing.T) {
	installFakeAgentState(t, "history\n❯\u00a0\n  status\n")
	s := newCmdTestStore(t, "bosun")

	got := callMCPTool(t, s, "tmux-msg.agent_state", map[string]any{"agent": "bosun"})
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

// TestMCP_AgentState_RequiresAgent pins the input validation: empty
// agent → error response. callMCPTool marks error responses with the
// `_isError` field so consumers can branch on it.
func TestMCP_AgentState_RequiresAgent(t *testing.T) {
	s := newCmdTestStore(t, "bosun")
	got := callMCPTool(t, s, "tmux-msg.agent_state", map[string]any{})
	if got["_isError"] != true {
		t.Errorf("expected isError=true for missing agent; got %v", got)
	}
}

// TestStateCLI_MailboxOnlyAgent_ShortCircuitsToIdle pins the #116
// chrome short-circuit: a mailbox-only agent has no Claude TUI to
// probe, so AgentState classification would always fall to Unknown.
// Short-circuit to Idle gives consumers (mailman gate, operator probe)
// a useful answer with zero capture-pane calls.
func TestStateCLI_MailboxOnlyAgent_ShortCircuitsToIdle(t *testing.T) {
	// installFakeAgentState would normally satisfy the AgentState
	// probe; here we DON'T install it, so any call to tmuxio.AgentState
	// would fail. The short-circuit must bypass the probe entirely.
	s := newCmdTestStore(t, "alice")
	if err := s.SetDeliveryMode(context.Background(), "alice",
		store.DeliveryModeMailboxOnly); err != nil {
		t.Fatalf("set delivery_mode: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exit := runStateWithStore(context.Background(), s, "alice", "json",
		&stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	var res agentStateResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		t.Fatalf("decode: %v; stdout=%s", err, stdout.String())
	}
	if res.State != "idle" {
		t.Errorf("state = %q, want idle (mailbox-only short-circuit)", res.State)
	}
	if !strings.Contains(res.Evidence.Reason, "mailbox-only") {
		t.Errorf("evidence.reason should mention mailbox-only; got %q", res.Evidence.Reason)
	}
}

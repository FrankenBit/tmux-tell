// Discipline pins for the cmd/claude-msg package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the three orthogonal grep handles
// for the discipline.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/testpin"
)

// PIN: wire shape is single source-of-truth — JSON-tag-driven, no
// manual map construction. The omitempty contract holds on
// trackResult's optional fields so empty-state-dependent fields
// never appear in serialised JSON, and populated ones always do.
// Surveyor #31 Q(d) follow-up: the CLI/MCP byte-identity test
// verifies wire equivalence; this pin verifies the omitempty
// invariant itself.
func TestPin_WireShapeSingleSoT_OmitemptyContract(t *testing.T) {
	testpin.Triage(t, "WireShapeSingleSoT",
		"wire shape is single source-of-truth — JSON-tag-driven, no manual map construction")
	res := &trackResult{
		OK:        true,
		ID:        "abcd",
		From:      "alice",
		To:        "bob",
		State:     "queued",
		Kind:      "message",
		CreatedAt: "2026-05-30T11:00:00Z",
		// DeliveredAt, Error, ReplyTo intentionally empty
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, banned := range []string{"delivered_at", "error", "reply_to"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("omitempty contract: empty %q field should not appear in:\n%s",
				banned, raw)
		}
	}

	// Inverse pin: when all three are non-empty, they must appear.
	res.DeliveredAt = "2026-05-30T11:01:00Z"
	res.Error = "boom"
	res.ReplyTo = "1234"
	raw, _ = json.Marshal(res)
	for _, required := range []string{"delivered_at", "error", "reply_to"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("populated %q field should appear in:\n%s", required, raw)
		}
	}
}

// PIN: wire shape is single source-of-truth — the CLI's --format json
// and the MCP tool's response must serialise byte-identical. Pins the
// single-SoT invariant: if the two callers diverged, one of them is
// constructing the wire shape manually rather than from the shared
// JSON-tagged struct. Surveyor's Q3 carry-over.
func TestPin_WireShapeSingleSoT_CLIAndMCPByteIdentity(t *testing.T) {
	testpin.Triage(t, "WireShapeSingleSoT",
		"CLI --format json and MCP tool response must serialise byte-identical")
	s := newCmdTestStore(t, "alice", "bob")
	id := seedMessage(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Setenv("CLAUDE_AGENT_NAME", "alice")

	// CLI shape.
	var stdout bytes.Buffer
	if exit := runTrackCLI([]string{"--format", "json", id}, &stdout, &bytes.Buffer{}); exit != exitOK {
		t.Fatalf("cli exit = %d", exit)
	}
	cliMap := parseJSONResult(t, stdout.Bytes())

	// MCP shape.
	mcpMap := callMCPTool(t, s, "tmux-msg.message_status", map[string]any{"id": id})

	// Strip MCP-private fields injected by the test harness.
	delete(mcpMap, "_text")
	delete(mcpMap, "_isError")

	cliJSON, _ := json.Marshal(cliMap)
	mcpJSON, _ := json.Marshal(mcpMap)
	if string(cliJSON) != string(mcpJSON) {
		t.Errorf("wire-shape drift:\n CLI: %s\n MCP: %s", cliJSON, mcpJSON)
	}
}

// TestPin_OperatorInputRowGate_QuickProbeSkippedWhenSentinelPromotes —
// REMOVED 2026-06-04 (tmux-msg #92). The asymmetric gate
// composition this pin guarded (PromptSentinelGate → QuickPresenceProbe
// → WaitForQuietPane) was retired when the probe-and-watch substrate
// was replaced with the observe-only-with-one-named-visibility-side-
// effect ObserveGate (the 📫 typing-notification per #95 is the
// side-effect; opt-out via notify-emoji-disabled). The
// "sentinel-first-cheap promotes, QuickPresenceProbe skipped"
// commitment from PR #67 no longer exists at this layer — there's no
// composition order to preserve because there's only one gate.
//
// See tmux-msg #91 (investigation) + #92 (redesign) for the
// migration story. The discipline that produced this pin —
// perf-skip-when-cheaper-path-decides — is general and could resurface
// under a different concrete pin; this PIN_ slot stays empty pending
// such a re-instantiation.

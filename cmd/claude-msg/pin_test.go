// Discipline pins for the cmd/claude-msg package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the three orthogonal grep handles
// for the discipline.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/testpin"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
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
	mcpMap := callMCPTool(t, s, "semaphore.message_status", map[string]any{"id": id})

	// Strip MCP-private fields injected by the test harness.
	delete(mcpMap, "_text")
	delete(mcpMap, "_isError")

	cliJSON, _ := json.Marshal(cliMap)
	mcpJSON, _ := json.Marshal(mcpMap)
	if string(cliJSON) != string(mcpJSON) {
		t.Errorf("wire-shape drift:\n CLI: %s\n MCP: %s", cliJSON, mcpJSON)
	}
}

// PIN: the asymmetric gate's composition order is sentinel-first
// (cheap, ~5ms, read-only) then QuickPresenceProbe (~50ms, write+
// observe), AND the cheap path wins by skipping the expensive one
// when it promotes. The `!runFullGate` guard at serve.go:473 is what
// enforces the skip; removing it would make both gates run
// unconditionally — a perf regression (not a correctness break)
// flagged in PR #66's mutation-experiment table and tracked as #67.
//
// Mechanism: drive a fake tmuxRunner where the first capture-pane
// returns a Claude Code input row with the prompt sentinel followed
// by non-whitespace content. InputRowHasContent classifies that as
// DeltaInputActivity, the sentinel block promotes runFullGate=true,
// the QuickPresenceProbe block's `!runFullGate` guard MUST skip.
//
// Probe-counting subtlety: QuietOpts.MaxWait is set to a microsecond
// so the full gate (which DOES run because sentinel promoted) caps
// quickly, but the cap check fires at the TOP of each iteration —
// the first iteration runs to completion BEFORE the cap trips on the
// second iteration's top. WaitForQuietPane therefore contributes
// exactly 2 probes regardless of perf-skip state. The load-bearing
// signal is the DELTA: when perf-skip works, total probes = 2
// (WaitForQuietPane only). When the `!runFullGate` guard is dropped,
// QuickPresenceProbe ALSO runs to completion, contributing 2 more
// → total probes = 4. The assertion pins probeCount == 2.
func TestPin_OperatorInputRowGate_QuickProbeSkippedWhenSentinelPromotes(t *testing.T) {
	testpin.Triage(t, "OperatorInputRowGate",
		"asymmetric gate composition: sentinel-first-cheap promotes, QuickPresenceProbe skipped (perf-skip property from PR #66 mutation table)")

	var (
		mu             sync.Mutex
		probeCount     int
		loadBufferUsed bool
		firstCapture   bool
	)
	prev := tmuxio.SetTmuxRunner(func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		switch args[0] {
		case "capture-pane":
			if !firstCapture {
				firstCapture = true
				// Sentinel + non-whitespace draft → classifyInputRow
				// returns DeltaInputActivity → sentinel block promotes
				// runFullGate=true.
				return []byte("conversation context\n" + tmuxio.PromptSentinel + "operator's draft text\n"), nil
			}
			// Subsequent captures (verify-token probes). Returning
			// content with the public_id keeps the delivery on the
			// happy path so loadBufferUsed flips true.
			return []byte("conversation context\nid TEST receipt\n"), nil
		case "display-message":
			return []byte("1\n"), nil
		case "send-keys":
			for i, a := range args {
				if a == "-l" && i+1 < len(args) && args[i+1] == tmuxio.QuietProbe {
					probeCount++
					return nil, nil
				}
			}
		case "load-buffer":
			loadBufferUsed = true
		}
		return nil, nil
	})
	t.Cleanup(func() { tmuxio.SetTmuxRunner(prev) })

	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	if _, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "test body",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE messages SET public_id='TEST' WHERE id=?`, 1); err != nil {
		t.Fatalf("rewrite public_id: %v", err)
	}

	opts := fastOpts("bob")
	// Default-skip-full-gate path with both opt-in pre-checks enabled.
	opts.QuietDisabled = true
	opts.PromptSentinelGate = true
	opts.QuickPresenceProbe = true
	// Microsecond budget so the full gate returns ErrCapExceeded
	// before injecting any of its own probes — keeps the perf-skip
	// assertion clean from WaitForQuietPane probe contamination.
	opts.QuietOpts = tmuxio.QuietOpts{
		ObserveWindow:        time.Microsecond,
		InputActivityBackoff: time.Microsecond,
		MaxWait:              time.Microsecond,
	}

	stop, wait, _ := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		all, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(all) >= 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	mu.Lock()
	defer mu.Unlock()

	// Load-bearing signal: WaitForQuietPane contributes 2 probes
	// (one iteration before the microsecond MaxWait trips on the next
	// iteration's cap check). QuickPresenceProbe MUST NOT add 2 more.
	// Green-state total = 2; perf-skip-broken total = 4.
	if probeCount != 2 {
		t.Errorf("perf-skip regression: QuickPresenceProbe ran after sentinel promoted (probe injections = %d, want 2 — only WaitForQuietPane's single iteration). The `!runFullGate` guard on cmd/claude-msg/serve.go's QuickPresenceProbe block is no longer enforcing the composition order this pin asserts. See #67.", probeCount)
	}
	// Delivery still happens after the cap-exceeded path through the
	// full gate. Without this assertion, a pure no-op pre-check
	// scaffold could pass the perf-skip check without actually
	// exercising the gate composition.
	if !loadBufferUsed {
		t.Errorf("delivery never reached load-buffer; the gate-composition path may have failed before delivery, weakening the pin's perf-skip signal")
	}
}

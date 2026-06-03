package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// State classifies a chamber's current activity from the cli-semaphore
// vantage point. Five values, per the cli-semaphore#69 verdict
// (bus id `d47f`, 2026-06-04). The zero value is StateUnknown so a
// caller that forgets to initialize gets the safer-default-on-
// uncertainty behaviour automatically.
//
// Consumer convention: treat StateUnknown as advisory-not-authoritative
// per the cli-semaphore#65 playbook's substrate-class-of-claim shape.
// Don't roll up an unknown classification into a known state silently;
// gate the consumer's action until the probe substantiates better data.
type State int

const (
	// StateUnknown means the probe couldn't substantiate a known state —
	// pane unreachable, capture-pane errored, or the pane is stable in
	// some non-prompt non-menu UI state the heuristic doesn't recognize.
	// Always the zero value.
	StateUnknown State = iota
	// StateIdle means the chamber is waiting for input. The pane is
	// stable across the temporal-delta window AND the PromptSentinel is
	// painted with no content past it.
	StateIdle
	// StateWorking means the chamber is actively processing — streaming
	// output, spinner ticking, or any other substantive pane-content
	// change across the temporal-delta window.
	StateWorking
	// StateAtRestInCompaction means the chamber is mid-`/compact`
	// sequence. Detection relies on CompactionMarker; per cli-semaphore
	// #70 the marker is currently a placeholder pending empirical
	// capture of the compaction-in-progress UI.
	StateAtRestInCompaction
	// StateAwaitingOperator means the chamber is paused on an
	// AskUserQuestion popup or other operator-input-required UI —
	// structurally distinct from idle: the chamber has an open turn
	// awaiting human response, and the next bus message can't drive the
	// turn forward without first being treated by the operator.
	// Detection relies on AwaitingOperatorMarker; per #70 the marker is
	// currently a placeholder pending empirical capture.
	StateAwaitingOperator
)

// String returns the wire-format name of the state — the same string
// the CLI / MCP surfaces emit in their `state` field. Stable across
// implementations so consumers can switch on it without recompilation.
// Names match cli-semaphore#69's accepted vocabulary verbatim.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateWorking:
		return "working"
	case StateAtRestInCompaction:
		return "at-rest-in-compaction"
	case StateAwaitingOperator:
		return "awaiting-operator"
	default:
		return "unknown"
	}
}

// Evidence carries the observation that led to the State classification.
// Fields are populated per state; consumers should treat unset fields as
// "not applicable to this state's detection path." Reason is always
// populated and intended for human-readable display (the CLI `text`
// format prints it; the JSON format includes it verbatim).
type Evidence struct {
	// Reason is a one-line explanation of the classification, suitable
	// for display in the CLI text format or surfaced through the MCP
	// tool's response. Always populated.
	Reason string `json:"reason"`
	// PromptEmpty is true when the StateIdle classification was made
	// because the prompt sentinel was found with no content past it.
	PromptEmpty bool `json:"prompt_empty,omitempty"`
	// ChangedLineCount is the number of differing lines between the two
	// temporal-delta captures, populated for StateWorking.
	ChangedLineCount int `json:"changed_line_count,omitempty"`
	// Marker is the matched substring for StateAtRestInCompaction or
	// StateAwaitingOperator.
	Marker string `json:"marker,omitempty"`
}

// CompactionMarker is the substring that identifies a chamber in
// StateAtRestInCompaction via pane-capture inspection.
//
// PLACEHOLDER per cli-semaphore#70 — empirical capture of the
// compaction-in-progress UI is pending. While this constant is empty,
// the StateAtRestInCompaction branch is effectively disabled (the
// emptiness-guard in ChamberState skips the check). When #70 lands the
// fixture with the actual paint format, populate this constant in the
// same commit that adds the corresponding state-AtRestInCompaction
// test fixture.
//
// FORWARD-WATCH (same shape as PromptSentinel): Claude-Code-version-
// dependent. If the compaction UI paint changes across a Claude Code
// version update, this constant needs re-verification + the
// corresponding test fixture updated.
const CompactionMarker = ""

// AwaitingOperatorMarker is the substring that identifies a chamber in
// StateAwaitingOperator (AskUserQuestion popups, selection menus, …).
//
// PLACEHOLDER per cli-semaphore#70 — same shape + same forward-watch
// as CompactionMarker. While empty, the StateAwaitingOperator branch
// in ChamberState is disabled.
const AwaitingOperatorMarker = ""

// chamberStateTemporalDelta is the wait between the two capture-pane
// calls in ChamberState. 200ms is long enough to catch typical
// streaming-output changes + spinner animations (most Claude Code
// spinners tick at ~80-100ms intervals) and short enough that probing
// a working chamber doesn't add meaningful latency to the caller's
// flow. False-negatives on chambers running long-running tools whose
// only paint is a 1Hz spinner counter are an accepted risk for v1 —
// the cli-semaphore prompt-sentinel gate (PR #66) catches a working-
// pane mis-classified as idle at the delivery layer if it matters.
var chamberStateTemporalDelta = 200 * time.Millisecond

// SetChamberStateTemporalDeltaForTest swaps the temporal-delta wait
// for tests so the suite doesn't pay 200ms per ChamberState call.
// Returns the previous value for cleanup restoration. Sibling to
// SetSettleDelayForTest.
func SetChamberStateTemporalDeltaForTest(d time.Duration) time.Duration {
	prev := chamberStateTemporalDelta
	chamberStateTemporalDelta = d
	return prev
}

// ChamberState classifies the receiving pane's current activity by
// inspecting two consecutive capture-pane snapshots taken
// chamberStateTemporalDelta apart and applying a precedence-ordered
// heuristic.
//
// Substrate-class: read-only-observe. Exactly two capture-pane calls,
// zero send-keys, zero pane mutation. This is the load-bearing property
// that distinguishes ChamberState from QuickPresenceProbe and
// WaitForQuietPane (write+observe via probe-and-watch). Pinned by
// TestChamberState_NoPaneMutation in the test suite. "Knock at the door
// without waking the inhabitant" per the cli-semaphore#69 framing.
//
// Heuristic v1 (per cli-semaphore#71's algorithm spec):
//
//  1. If either capture fails → StateUnknown + the wrapped error.
//  2. If CompactionMarker is non-empty AND found in capture B →
//     StateAtRestInCompaction. (Constant empty in v1 pending #70.)
//  3. If capture A != capture B → StateWorking. Any substantive change
//     across the temporal-delta window means the chamber is painting.
//  4. If capture B has the PromptSentinel with no content past it →
//     StateIdle. (Reuses InputRowHasContent's classification logic via
//     the shared classifyInputRow helper.)
//  5. If AwaitingOperatorMarker is non-empty AND found in capture B →
//     StateAwaitingOperator. (Constant empty in v1 pending #70.)
//  6. Otherwise → StateUnknown.
//
// The (B) /proc-inspection hybrid named on #69 is NOT implemented in
// v1; if (A)-only proves insufficient empirically, (B) lands as a
// follow-up sub-issue.
//
// Substrate-reuse: the Idle branch consumes classifyInputRow which is
// the parse-only sibling of PR #66's InputRowHasContent. PromptSentinel
// is the Claude-Code-version-pinned constant; same forward-watch as
// documented there.
//
// Errors: capture-pane failures propagate via the error return value
// paired with StateUnknown — the safer-default-on-uncertainty contract
// from the cli-semaphore#65 playbook applied at the detection layer.
// Callers should treat any error as advisory (the unknown classification
// is honest about probe-failure rather than rolling up to a known state
// silently).
func ChamberState(ctx context.Context, pane string) (State, Evidence, error) {
	if pane == "" {
		return StateUnknown, Evidence{Reason: "pane required"},
			errors.New("tmuxio: pane required")
	}

	capA, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("first capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: chamber-state capture #1: %w: %s",
				err, strings.TrimSpace(string(capA)))
	}

	select {
	case <-time.After(chamberStateTemporalDelta):
	case <-ctx.Done():
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("context cancelled during temporal-delta wait: %v", ctx.Err())},
			ctx.Err()
	}

	capB, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("second capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: chamber-state capture #2: %w: %s",
				err, strings.TrimSpace(string(capB)))
	}

	capAStr := string(capA)
	capBStr := string(capB)

	// Precedence 1: compaction marker.
	if CompactionMarker != "" && strings.Contains(capBStr, CompactionMarker) {
		return StateAtRestInCompaction,
			Evidence{
				Reason: fmt.Sprintf("compaction marker found: %q", CompactionMarker),
				Marker: CompactionMarker,
			}, nil
	}

	// Precedence 2: working (any substantive change across the window).
	if capAStr != capBStr {
		return StateWorking,
			Evidence{
				Reason:           "pane content changed across temporal-delta window",
				ChangedLineCount: countChangedLines(capAStr, capBStr),
			}, nil
	}

	// Precedence 3: idle (prompt sentinel + empty input row + pane stable).
	if classifyInputRow(capBStr) == DeltaQuiet {
		return StateIdle,
			Evidence{
				Reason:      "prompt sentinel found with empty input row + pane stable",
				PromptEmpty: true,
			}, nil
	}

	// Precedence 4: awaiting-operator marker.
	if AwaitingOperatorMarker != "" && strings.Contains(capBStr, AwaitingOperatorMarker) {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("awaiting-operator marker found: %q", AwaitingOperatorMarker),
				Marker: AwaitingOperatorMarker,
			}, nil
	}

	// Default: unknown. Pane is stable but neither the prompt sentinel
	// nor any marker fired — chamber is in some non-prompt non-menu UI
	// state the v1 heuristic doesn't classify.
	return StateUnknown,
		Evidence{Reason: "pane stable + no recognized marker + no prompt sentinel"},
		nil
}

// countChangedLines returns the number of lines that differ between
// the two captures. Cheap line-by-line walk; used only to populate
// Evidence.ChangedLineCount for the StateWorking branch — not load-
// bearing for classification (the byte-equality check above is the
// authoritative test).
func countChangedLines(a, b string) int {
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")
	max := len(aLines)
	if len(bLines) > max {
		max = len(bLines)
	}
	diff := 0
	for i := 0; i < max; i++ {
		var aLine, bLine string
		if i < len(aLines) {
			aLine = aLines[i]
		}
		if i < len(bLines) {
			bLine = bLines[i]
		}
		if aLine != bLine {
			diff++
		}
	}
	return diff
}

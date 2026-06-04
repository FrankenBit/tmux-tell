package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	// Detection relies on AwaitingOperatorMarker; per cli-semaphore#79 the marker is
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
// Empirically captured 2026-06-04 from a Quartermaster pane displaying
// a live AskUserQuestion popup (cli-semaphore#79). The captured pane
// content is frozen as testdata/golden_quartermaster_askuserquestion_
// 2026-06-04.txt so future Claude Code UI drift surfaces as a golden-
// match failure on the canary test in probe_test.go.
//
// The substring is the popup's bottom-line keybinding hint —
// "↑/↓ to navigate · Esc to cancel" — combined with the middle-dot
// separator. The combination is structurally unique to Claude Code's
// popup UI: regular chat / response text never emits keybinding hints
// with U+00B7 middle-dot separators. Catches both single-select and
// multi-select popup variants because both end with the same footer.
//
// FORWARD-WATCH (same shape as PromptSentinel + CompactionMarker):
// Claude-Code-version-dependent. If the popup UI's footer changes
// across a Claude Code version update, this constant + the golden
// fixture both need re-verification. The canary test surfaces the
// drift loudly.
const AwaitingOperatorMarker = "↑/↓ to navigate · Esc to cancel"

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
// inspecting two consecutive capture-pane snapshots + the tmux cursor
// position and applying a precedence-ordered heuristic.
//
// Substrate-class: read-only-observe. Exactly two capture-pane calls,
// one display-message call, zero send-keys, zero pane mutation. This
// is the load-bearing property that distinguishes ChamberState from
// QuickPresenceProbe and WaitForQuietPane (write+observe via probe-
// and-watch). Pinned by TestChamberState_NoPaneMutation in the test
// suite. "Knock at the door without waking the inhabitant" per
// cli-semaphore#69's framing — all three tmux calls are read-only
// (capture-pane reads the visible buffer; display-message reads
// tmux's internal pane state).
//
// Heuristic v2 (cli-semaphore#69 smoke test surfaced the v1 gap on
// cursor-less classification; operator's design call 2026-06-04
// resolved it via cursor-position awareness):
//
//  1. If either capture fails → StateUnknown + the wrapped error.
//  2. If CompactionMarker is non-empty AND found in capture B →
//     StateAtRestInCompaction. (Constant empty in v1 pending #70.)
//  3. If capture A != capture B → StateWorking. Any substantive change
//     across the temporal-delta window means the chamber is painting.
//  4. **Cursor-position-aware input-row classification** (the v2 gap-fix):
//     query the cursor position via display-message; identify the row
//     the cursor sits on; if that row starts with PromptSentinel:
//     - Cursor at sentinel position (col == sentinel-width): the
//     input area's input position. If row is empty past the
//     sentinel → StateIdle (clean prompt). If row has content past
//     the sentinel → StateIdle as well (Claude Code auto-suggestion
//     ghost text; operator hasn't engaged — cursor would have moved
//     past the content if they had).
//     - Cursor past sentinel position: operator is mid-typing →
//     StateAwaitingOperator (chamber blocked on operator finishing
//     their draft).
//     - Cursor before sentinel position: unusual; treat as Unknown.
//  5. If AwaitingOperatorMarker is non-empty AND found in capture B →
//     StateAwaitingOperator. (Backup detection for non-`❯`-painting
//     UIs — AskUserQuestion popups, search dialogs, etc. Constant
//     empty in v1 pending #70.)
//  6. If the cursor query failed or the cursor row doesn't start with
//     PromptSentinel, fall back to the cursor-less heuristic
//     (classifyInputRow == DeltaQuiet → Idle; else Unknown). This
//     preserves the v1 behavior when the cursor substrate is
//     unreachable.
//  7. Otherwise → StateUnknown with an accurate reason naming the
//     sub-case that fired (vs the v1's misleading "no prompt sentinel"
//     blanket message).
//
// Substrate-reuse: the cursor-less fallback path consumes
// classifyInputRow which is the parse-only sibling of PR #66's
// InputRowHasContent. PromptSentinel is the Claude-Code-version-
// pinned constant; same forward-watch as documented there.
//
// Errors: capture-pane failures propagate via the error return value
// paired with StateUnknown — the safer-default-on-uncertainty contract
// from the cli-semaphore#65 playbook applied at the detection layer.
// Cursor query failures are non-fatal (the heuristic gracefully
// degrades to the cursor-less path); only capture-pane failures bubble
// up as errors.
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

	// Cursor-position-aware classification (the v2 substrate per
	// cli-semaphore#69 operator's design call 2026-06-04). Query the
	// cursor; if it sits on a row that starts with PromptSentinel,
	// distinguish auto-suggestion (cursor at sentinel) from operator-
	// drafting (cursor past sentinel) — the two cases the v1 heuristic
	// conflated as "non-empty input row".
	cursorX, cursorY, cursorErr := chamberCursor(ctx, pane)
	if cursorErr == nil {
		lines := strings.Split(capBStr, "\n")
		if cursorY >= 0 && cursorY < len(lines) {
			row := lines[cursorY]
			rest, hasSentinel := strings.CutPrefix(row, PromptSentinel)
			if hasSentinel {
				sentinelCol := utf8.RuneCountInString(PromptSentinel)
				switch {
				case cursorX == sentinelCol:
					// Cursor right after `❯ ` — either a clean idle
					// prompt or an auto-suggestion ghost-text. Both
					// classify as Idle because the operator hasn't
					// engaged (cursor would have moved past content
					// if they had been typing).
					ev := Evidence{
						Reason:      "cursor at prompt sentinel position; pane stable",
						PromptEmpty: strings.TrimSpace(rest) == "",
					}
					if !ev.PromptEmpty {
						ev.Reason = fmt.Sprintf("cursor at prompt sentinel position with auto-suggestion ghost-text (%q); pane stable", strings.TrimSpace(rest))
					}
					return StateIdle, ev, nil
				case cursorX > sentinelCol:
					// Cursor past the sentinel — operator is mid-
					// typing. Chamber is blocked on operator finishing
					// the draft (or clearing it). Same consumer-side
					// semantics as AskUserQuestion popup: don't
					// dispatch into this state.
					return StateAwaitingOperator,
						Evidence{
							Reason: fmt.Sprintf("cursor past prompt sentinel (col %d > %d); operator mid-typing", cursorX, sentinelCol),
						}, nil
				}
				// Cursor before sentinel position on the sentinel row
				// is unusual; fall through to marker / unknown checks.
			}
		}
	}

	// Precedence 5: awaiting-operator marker (backup for non-`❯`
	// painting UIs — AskUserQuestion popups, search dialogs, etc.).
	if AwaitingOperatorMarker != "" && strings.Contains(capBStr, AwaitingOperatorMarker) {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("awaiting-operator marker found: %q", AwaitingOperatorMarker),
				Marker: AwaitingOperatorMarker,
			}, nil
	}

	// Cursor-less fallback (cursor query failed or cursor row doesn't
	// have the sentinel): the v1 heuristic via classifyInputRow. Used
	// when display-message isn't available or the cursor is somewhere
	// other than the input row (e.g., chamber paused mid-spinner).
	if classifyInputRow(capBStr) == DeltaQuiet {
		return StateIdle,
			Evidence{
				Reason:      "prompt sentinel found with empty input row (cursor-less fallback); pane stable",
				PromptEmpty: true,
			}, nil
	}

	// Default: unknown. Distinguish two sub-cases for accurate evidence:
	//   - sentinel found with content past it (but cursor not at input row)
	//   - sentinel not found at all
	hasSentinelInPane := strings.Contains(capBStr, PromptSentinel)
	cursorNote := ""
	if cursorErr != nil {
		cursorNote = fmt.Sprintf(" (cursor query failed: %v)", cursorErr)
	}
	if hasSentinelInPane {
		return StateUnknown,
			Evidence{Reason: "pane stable; prompt sentinel found but cursor not at input row + no recognized marker" + cursorNote},
			nil
	}
	return StateUnknown,
		Evidence{Reason: "pane stable; prompt sentinel not found in any row + no recognized marker" + cursorNote},
		nil
}

// chamberCursor queries the tmux cursor position for the pane. Returns
// (cursorX, cursorY, error). tmux's cursor_x is column 0-indexed,
// cursor_y is row 0-indexed from the top of the visible pane. A single
// display-message call returns both values as "X/Y" for parse-once
// efficiency.
//
// Errors here are non-fatal at the ChamberState layer — the algorithm
// gracefully degrades to the cursor-less heuristic when the cursor
// substrate is unreachable.
func chamberCursor(ctx context.Context, pane string) (int, int, error) {
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{cursor_x}/#{cursor_y}")
	if err != nil {
		return 0, 0, fmt.Errorf("tmuxio: chamber-state cursor query: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("tmuxio: chamber-state cursor parse: unexpected format %q", string(out))
	}
	x, errX := strconv.Atoi(parts[0])
	y, errY := strconv.Atoi(parts[1])
	if errX != nil || errY != nil {
		return 0, 0, fmt.Errorf("tmuxio: chamber-state cursor parse: %q", string(out))
	}
	return x, y, nil
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

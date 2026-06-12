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

// PromptSentinel is the Claude Code TUI's input-row prefix — U+276F
// (Heavy Right-Pointing Angle Quotation Mark Ornament) followed by
// U+00A0 (NO-BREAK SPACE). Empirically verified across all six
// agents on 2026-06-04, hex `e2 9d af c2 a0`.
//
// The string-literal uses `\u00a0` so the NBSP is explicit in the
// source code (mixing a visually-identical NBSP into the literal
// would silently fool future readers into thinking it's a regular
// space).
//
// FORWARD-WATCH: this constant is Claude-Code-version-dependent. If
// the Claude Code TUI's prompt character changes (theme update,
// version bump, customization), the cursor-aware AgentState branch
// silently degrades to "cursor not at input row". Re-verify the
// constant during any major Claude Code version update via
// `tmux capture-pane | od -An -tx1` on the input row; the canary
// tests in state_canary_test.go (golden + byte-encoding) catch
// drift loudly.
const PromptSentinel = "❯\u00a0"

// State classifies a agent's current activity from the tmux-msg
// vantage point. Five values, per the #69 verdict
// (bus id `d47f`, 2026-06-04). The zero value is StateUnknown so a
// caller that forgets to initialize gets the safer-default-on-
// uncertainty behaviour automatically.
//
// Consumer convention: treat StateUnknown as advisory-not-authoritative
// per the #65 playbook's substrate-class-of-claim shape.
// Don't roll up an unknown classification into a known state silently;
// gate the consumer's action until the probe substantiates better data.
type State int

const (
	// StateUnknown means the probe couldn't substantiate a known state —
	// pane unreachable, capture-pane errored, or the pane is stable in
	// some non-prompt non-menu UI state the heuristic doesn't recognize.
	// Always the zero value.
	StateUnknown State = iota
	// StateIdle means the agent is waiting for input. The pane is
	// stable across the temporal-delta window AND the PromptSentinel is
	// painted with no content past it.
	StateIdle
	// StateWorking means the agent is actively processing — streaming
	// output, spinner ticking, or any other substantive pane-content
	// change across the temporal-delta window.
	StateWorking
	// StateAtRestInCompaction means the agent is mid-`/compact`
	// sequence. Detection relies on CompactionMarker, an empirically-
	// captured substring of Claude Code's compaction-in-progress UI
	// (#70, PR #88). Lit up 2026-06-04 from two operator-
	// coordinated captures of the Quartermaster pane at distinct
	// progress points (8% and 68%) across the same /compact event;
	// canary + classification pins in state_canary_test.go and
	// state_test.go protect the substring against Claude Code UI
	// drift.
	StateAtRestInCompaction
	// StateAwaitingOperator means the agent is paused on an
	// AskUserQuestion popup or other operator-input-required UI —
	// structurally distinct from idle: the agent has an open turn
	// awaiting human response, and the next bus message can't drive the
	// turn forward without first being treated by the operator.
	// Detection relies on AwaitingOperatorMarker, an empirically-captured
	// substring of Claude Code's popup footer (#79, PR #87).
	// Lit up 2026-06-04 from an operator-coordinated AskUserQuestion
	// capture; canary + classification pins in state_canary_test.go
	// and state_test.go protect the substring against Claude Code UI
	// drift.
	StateAwaitingOperator
)

// String returns the wire-format name of the state — the same string
// the CLI / MCP surfaces emit in their `state` field. Stable across
// implementations so consumers can switch on it without recompilation.
// Names match #69's accepted vocabulary verbatim.
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

// IsPasteUnsafe reports whether a paste-and-Enter delivery to a pane in
// this state risks corrupting operator-visible content. Three states qualify:
//
//   - StateAwaitingOperator: the operator is typing OR a popup is open
//     consuming keystrokes. A paste into either case destroys what's
//     visible (operator's draft / popup interpretation of pasted bytes
//     as keystrokes).
//   - StateUnknown: the classifier couldn't substantiate a known state.
//     The popup-as-Unknown failure mode (#105) is exactly the case
//     where pasting is destructive — if we can't substantiate, we
//     can't paste safely.
//   - StateAtRestInCompaction: paste-into-compaction is consumed by
//     the /compact slash-command parser as additional commands —
//     destructive. The PostCompactPause machinery prevents this at a
//     SCHEDULING layer when the mailman just delivered /compact, but
//     leaves a coverage gap when the agent is in Compaction for an
//     UNRELATED reason (operator-initiated /compact). Returning true
//     here gives defense-in-depth at the safety-check layer per
//     Surveyor PR #134 S2.
//
// StateIdle and StateWorking are paste-safe (idle by definition;
// working buffers mid-turn keystrokes per Claude Code TUI behavior).
//
// Used by the mailman's pre-paste safety check (#105 Half 2): even if
// the observe-gate decides to flush, a final state probe before the
// actual paste-and-Enter aborts the delivery when this returns true.
func IsPasteUnsafe(s State) bool {
	return s == StateAwaitingOperator || s == StateUnknown || s == StateAtRestInCompaction
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

// CompactionMarker is the substring that identifies a agent in
// StateAtRestInCompaction via pane-capture inspection.
//
// Empirically captured 2026-06-04 from a Quartermaster pane mid-
// `/compact` (#70). Two captures from the same compaction
// event — at 8% and 68% progress — are frozen as
// testdata/golden_quartermaster_compaction_2026-06-04.txt and
// testdata/golden_quartermaster_compaction_advanced_2026-06-04.txt so
// future Claude Code UI drift surfaces as a golden-match failure on the
// canary test in state_canary_test.go.
//
// The substring intentionally EXCLUDES the leading spinner glyph: the
// 8% capture shows `✻ Compacting conversation…` (U+273B six-pointed
// black star) while the 68% capture shows `✢ Compacting conversation…`
// (U+2722 four teardrops-spoked asterisk). The glyph cycles across
// spinner frames; the trailing phrase is the stable load-bearing
// substring. The ellipsis is U+2026, painted as a single codepoint.
//
// Precedence in AgentState: this check runs BEFORE the pane-equality
// "working" check (precedence 1 vs 2) so a agent mid-compaction — a
// pane whose spinner is animating across the temporal-delta window and
// would otherwise classify as Working — is correctly identified as
// AtRestInCompaction. The two captures at different progress points
// pin this precedence in TestAgentState_AtRestInCompactionOnGolden.
//
// FORWARD-WATCH (same shape as PromptSentinel + AwaitingOperatorMarker):
// Claude-Code-version-dependent. If the compaction UI's phrase changes
// across a Claude Code version update, this constant + both golden
// fixtures need re-verification. The canary test surfaces the drift
// loudly.
const CompactionMarker = "Compacting conversation…"

// AwaitingOperatorMarker is the substring that identifies a agent in
// StateAwaitingOperator (AskUserQuestion popups, selection menus, …).
//
// Empirically captured 2026-06-04 from a Quartermaster pane displaying
// a live AskUserQuestion popup (#79). The captured pane
// content is frozen as testdata/golden_quartermaster_askuserquestion_
// 2026-06-04.txt so future Claude Code UI drift surfaces as a golden-
// match failure on the canary test in state_canary_test.go.
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

// agentStateTemporalDelta is the wait between the two capture-pane
// calls in AgentState. 200ms is long enough to catch typical
// streaming-output changes + spinner animations (most Claude Code
// spinners tick at ~80-100ms intervals) and short enough that probing
// a working agent doesn't add meaningful latency to the caller's
// flow. False-negatives on agents running long-running tools whose
// only paint is a 1Hz spinner counter are an accepted risk for v1 —
// the ObserveGate's poll loop catches a working-pane mis-classified
// as idle at the delivery layer via subsequent iterations.
var agentStateTemporalDelta = 200 * time.Millisecond

// SetAgentStateTemporalDeltaForTest swaps the temporal-delta wait
// for tests so the suite doesn't pay 200ms per AgentState call.
// Returns the previous value for cleanup restoration. Sibling to
// SetSettleDelayForTest.
func SetAgentStateTemporalDeltaForTest(d time.Duration) time.Duration {
	prev := agentStateTemporalDelta
	agentStateTemporalDelta = d
	return prev
}

// AgentState classifies the receiving pane's current activity by
// inspecting two consecutive capture-pane snapshots + the tmux cursor
// position and applying a precedence-ordered heuristic.
//
// Substrate-class: read-only-observe. Exactly two capture-pane calls,
// one display-message call, zero send-keys, zero pane mutation.
// Pinned by TestAgentState_NoPaneMutation in the test suite. "Knock
// at the door without waking the inhabitant" per #69's
// framing — all three tmux calls are read-only (capture-pane reads
// the visible buffer; display-message reads tmux's internal pane
// state).
//
// Heuristic v2 (#69 smoke test surfaced the v1 gap on
// cursor-less classification; operator's design call 2026-06-04
// resolved it via cursor-position awareness):
//
//  1. If either capture fails → StateUnknown + the wrapped error.
//  2. If CompactionMarker is non-empty AND found in capture B →
//     StateAtRestInCompaction. Lit up 2026-06-04 (#70, PR #88). This
//     precedence over working is load-bearing: a agent mid-compaction
//     is animating (spinner glyph cycles, percentage ticks) so capA
//     != capB; without the marker check firing first, the agent
//     would mis-classify as Working.
//  3. If capture A != capture B → StateWorking. Any substantive change
//     across the temporal-delta window means the agent is painting.
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
//     StateAwaitingOperator (agent blocked on operator finishing
//     their draft).
//     - Cursor before sentinel position: unusual; treat as Unknown.
//  5. If AwaitingOperatorMarker is non-empty AND found in capture B →
//     StateAwaitingOperator. (Backup detection for non-`❯`-painting
//     UIs — AskUserQuestion popups, search dialogs, etc.)
//  6. If the cursor query failed or the cursor row doesn't start with
//     PromptSentinel, fall back to the cursor-less heuristic
//     (isInputRowQuiet returns true → Idle; else Unknown). This
//     preserves classification when the cursor substrate is
//     unreachable.
//  7. Otherwise → StateUnknown with an accurate reason naming the
//     sub-case that fired (sentinel found vs not, cursor query failure
//     vs cursor-not-on-input-row).
//
// PromptSentinel is the Claude-Code-version-pinned constant; see its
// doc-comment for the forward-watch on Claude Code TUI changes.
//
// Errors: capture-pane failures propagate via the error return value
// paired with StateUnknown — the safer-default-on-uncertainty contract
// from the #65 playbook applied at the detection layer.
// Cursor query failures are non-fatal (the heuristic gracefully
// degrades to the cursor-less path); only capture-pane failures bubble
// up as errors.
func AgentState(ctx context.Context, pane string) (State, Evidence, error) {
	if pane == "" {
		return StateUnknown, Evidence{Reason: "pane required"},
			errors.New("tmuxio: pane required")
	}

	capA, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("first capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: agent-state capture #1: %w: %s",
				err, strings.TrimSpace(string(capA)))
	}

	select {
	case <-time.After(agentStateTemporalDelta):
	case <-ctx.Done():
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("context cancelled during temporal-delta wait: %v", ctx.Err())},
			ctx.Err()
	}

	capB, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("second capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: agent-state capture #2: %w: %s",
				err, strings.TrimSpace(string(capB)))
	}

	capAStr := string(capA)
	capBStr := string(capB)

	// Precedence 1: compaction marker (from the active PaneProfile; empty
	// disables the check for an adapter with no compaction UI).
	if m := activeProfile.CompactionMarker; m != "" && strings.Contains(capBStr, m) {
		return StateAtRestInCompaction,
			Evidence{
				Reason: fmt.Sprintf("compaction marker found: %q", m),
				Marker: m,
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
	// #69 operator's design call 2026-06-04). Query the
	// cursor; if it sits on a row that starts with PromptSentinel,
	// distinguish auto-suggestion (cursor at sentinel) from operator-
	// drafting (cursor past sentinel) — the two cases the v1 heuristic
	// conflated as "non-empty input row".
	sentinel := activeProfile.PromptSentinel
	cursorX, cursorY, cursorErr := agentCursor(ctx, pane)
	if cursorErr == nil && sentinel != "" {
		lines := strings.Split(capBStr, "\n")
		if cursorY >= 0 && cursorY < len(lines) {
			row := lines[cursorY]
			rest, hasSentinel := strings.CutPrefix(row, sentinel)
			if hasSentinel {
				sentinelCol := utf8.RuneCountInString(sentinel)
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
					// typing. Agent is blocked on operator finishing
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

	// Precedence 5: awaiting-operator marker (backup for non-sentinel-
	// painting UIs — AskUserQuestion popups, search dialogs, etc.). From the
	// active PaneProfile; empty disables the backup check.
	if m := activeProfile.AwaitingOperatorMarker; m != "" && strings.Contains(capBStr, m) {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("awaiting-operator marker found: %q", m),
				Marker: m,
			}, nil
	}

	// Cursor-less fallback (cursor query failed or cursor row doesn't
	// have the sentinel): parse the pane for a sentinel-row with no
	// content past it. Used when display-message isn't available or
	// the cursor is somewhere other than the input row (e.g., agent
	// paused mid-spinner).
	if isInputRowQuiet(capBStr) {
		return StateIdle,
			Evidence{
				Reason:      "prompt sentinel found with empty input row (cursor-less fallback); pane stable",
				PromptEmpty: true,
			}, nil
	}

	// Default: unknown. Distinguish two sub-cases for accurate evidence:
	//   - sentinel found with content past it (but cursor not at input row)
	//   - sentinel not found at all
	hasSentinelInPane := sentinel != "" && strings.Contains(capBStr, sentinel)
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

// agentCursor queries the tmux cursor position for the pane. Returns
// (cursorX, cursorY, error). tmux's cursor_x is column 0-indexed,
// cursor_y is row 0-indexed from the top of the visible pane. A single
// display-message call returns both values as "X/Y" for parse-once
// efficiency.
//
// Errors here are non-fatal at the AgentState layer — the algorithm
// gracefully degrades to the cursor-less heuristic when the cursor
// substrate is unreachable.
func agentCursor(ctx context.Context, pane string) (int, int, error) {
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{cursor_x}/#{cursor_y}")
	if err != nil {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor query: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor parse: unexpected format %q", string(out))
	}
	x, errX := strconv.Atoi(parts[0])
	y, errY := strconv.Atoi(parts[1])
	if errX != nil || errY != nil {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor parse: %q", string(out))
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

// isInputRowQuiet walks the captured pane and returns true when at
// least one row starts with PromptSentinel AND that row has only
// whitespace past the sentinel. False otherwise (no sentinel row
// found, or sentinel row has non-whitespace content). Used by the
// cursor-less fallback path in AgentState when display-message
// fails or the cursor isn't on the input row.
func isInputRowQuiet(paneContent string) bool {
	sentinel := activeProfile.PromptSentinel
	if sentinel == "" {
		return false
	}
	sawSentinel := false
	for _, row := range strings.Split(paneContent, "\n") {
		rest, found := strings.CutPrefix(row, sentinel)
		if !found {
			continue
		}
		sawSentinel = true
		if strings.TrimSpace(rest) != "" {
			return false
		}
	}
	return sawSentinel
}

// bottomInputRowContent returns the text past the sentinel on the
// BOTTOM-most sentinel-prefixed row of the captured pane — the live input
// row. ok is false when no sentinel is configured or no sentinel row is
// present in the capture.
//
// Bottom-most (not first, not any) because an adapter whose prompt sentinel
// also prefixes transcript turns — codex paints every submitted user turn
// as `› [Pasted Content]` / `› text`, the same glyph as its live input —
// would otherwise be read off a historical row. Only the bottom-most
// sentinel row is the editable input. (Claude's `❯ ` is unique to the live
// input, so bottom-most and only-one coincide there; the bottom-most rule
// is the adapter-general form.)
//
// This is the anchor for the input-emptied delivery-verify signal (#336):
// a paste that submits leaves this row empty (Claude clears it in place;
// codex opens a fresh empty input block below), so a non-empty→empty
// transition is the submit signal — robust to paste-collapse, which masks
// the verify token but not the emptiness of the input row.
func bottomInputRowContent(capture string) (content string, ok bool) {
	sentinel := activeProfile.PromptSentinel
	if sentinel == "" {
		return "", false
	}
	lines := strings.Split(capture, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, found := strings.CutPrefix(lines[i], sentinel); found {
			return rest, true
		}
	}
	return "", false
}

// inputRowCleared reports whether the captured pane shows the live input
// row (bottom-most sentinel row) present AND empty past the sentinel.
// anchored is false when the input row can't be located (no sentinel
// configured, or no sentinel row in the capture) — the caller then falls
// back to the legacy token-match verify signal.
func inputRowCleared(capture string) (cleared, anchored bool) {
	rest, ok := bottomInputRowContent(capture)
	if !ok {
		return false, false
	}
	return strings.TrimSpace(rest) == "", true
}

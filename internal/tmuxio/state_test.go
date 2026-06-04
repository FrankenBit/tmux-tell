package tmuxio

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// --- State.String() ---

func TestState_String_AllValues(t *testing.T) {
	cases := []struct {
		s    State
		want string
	}{
		{StateUnknown, "unknown"},
		{StateIdle, "idle"},
		{StateWorking, "working"},
		{StateAtRestInCompaction, "at-rest-in-compaction"},
		{StateAwaitingOperator, "awaiting-operator"},
		{State(99), "unknown"}, // out-of-range defaults to "unknown" (safer)
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

// --- ChamberState integration tests ---

// fastTemporalDelta installs a microsecond temporal-delta so tests
// don't pay the 200ms production wait. Cleanup restores production.
func fastTemporalDelta(t *testing.T) {
	t.Helper()
	prev := SetChamberStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { SetChamberStateTemporalDeltaForTest(prev) })
}

// TestChamberState_IdleWhenSentinelEmpty pins the happy-path Idle
// classification: pane is stable across the temporal-delta window
// AND shows the PromptSentinel with no content past it. Reuses the
// classifyInputRow helper from PR #66's substrate.
func TestChamberState_IdleWhenSentinelEmpty(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history line A\nhistory line B\n──── Chamber ──\n❯\u00a0\n────────\n  status\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (sentinel empty + pane stable)", state)
	}
	if !ev.PromptEmpty {
		t.Errorf("Evidence.PromptEmpty = false, want true (the Idle branch sets it)")
	}
	if ev.Reason == "" {
		t.Errorf("Evidence.Reason should always be populated; got empty")
	}
}

// TestChamberState_WorkingWhenPaneChanges pins the Working
// classification: the two captures differ → chamber is painting →
// working. ChangedLineCount is populated in Evidence for observability.
func TestChamberState_WorkingWhenPaneChanges(t *testing.T) {
	fastTemporalDelta(t)
	paneA := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 5s\n❯\u00a0\n  status\n"
	paneB := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 6s\n❯\u00a0\n  status\n"
	fr := newFakeProbeRunner([]string{paneA, paneB})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateWorking {
		t.Errorf("state = %v, want StateWorking (pane content changed)", state)
	}
	if ev.ChangedLineCount < 1 {
		t.Errorf("Evidence.ChangedLineCount = %d, want >= 1 (the spinner-counter line changed)", ev.ChangedLineCount)
	}
}

// TestChamberState_UnknownWhenStableNonPromptUI pins the safer-default
// branch: pane is stable across the temporal-delta window but neither
// shows the PromptSentinel nor matches any marker. The chamber is in
// some non-recognized UI state and the heuristic refuses to silently
// roll up to a known classification.
func TestChamberState_UnknownWhenStableNonPromptUI(t *testing.T) {
	fastTemporalDelta(t)
	// Pane shows streaming output with no `❯\u00a0` row in view + no marker.
	pane := "● Some response line\n  ⎿  Tool output line 1\n  ⎿  Tool output line 2\n  status line\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown (no sentinel + no marker + pane stable)", state)
	}
	if ev.Reason == "" {
		t.Errorf("Evidence.Reason should be populated for the Unknown branch too")
	}
}

// TestChamberState_PaneRequired pins the input-validation guard: empty
// pane returns an error and StateUnknown without firing any tmux
// calls. Mirrors the InputRowHasContent / QuickPresenceProbe
// validation discipline.
func TestChamberState_PaneRequired(t *testing.T) {
	fr := newFakeProbeRunner([]string{"ctx\n"})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := ChamberState(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty pane, got nil")
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown when pane is empty", state)
	}
	if len(fr.calls) != 0 {
		t.Errorf("tmux was called %d times, want 0 (validation should reject before any tmux call)", len(fr.calls))
	}
}

// TestChamberState_NoPaneMutation pins the substrate-class property:
// ChamberState is read-only-observe. A successful classification makes
// EXACTLY 2 capture-pane calls + 1 display-message call (the cursor
// query added in cli-semaphore#69's v2 algorithm per operator's
// design call 2026-06-04) and ZERO send-keys. All three calls are
// read-only at the tmux layer — capture-pane reads the visible
// buffer; display-message reads internal pane state. This is the
// load-bearing claim that ChamberState honors the "knock at the door
// without waking" framing from #69; the v2 substrate-class extension
// from PR #75's 2-call shape preserves the no-mutation property
// while gaining cursor-position awareness.
func TestChamberState_NoPaneMutation(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Chamber ──\n❯\u00a0\n────────\n  status\n"
	fr := newChamberStateRunner([]string{pane, pane}, 2, 5) // cursor at sentinel position
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, _, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fr.calls) != 3 {
		t.Errorf("tmux call count = %d, want 3 (read-only-observe = 2 capture-pane + 1 display-message)", len(fr.calls))
	}
	capturePaneCount := 0
	displayMessageCount := 0
	for i, call := range fr.calls {
		switch {
		case strings.HasPrefix(call, "capture-pane"):
			capturePaneCount++
		case strings.HasPrefix(call, "display-message"):
			displayMessageCount++
		default:
			t.Errorf("call[%d] = %q, want capture-pane or display-message prefix (no send-keys in read-only-observe)", i, call)
		}
	}
	if capturePaneCount != 2 {
		t.Errorf("capture-pane count = %d, want 2", capturePaneCount)
	}
	if displayMessageCount != 1 {
		t.Errorf("display-message count = %d, want 1", displayMessageCount)
	}
	if fr.probeChars != 0 {
		t.Errorf("probe chars sent = %d, want 0 (no inject)", fr.probeChars)
	}
	if fr.backspaces != 0 {
		t.Errorf("backspaces = %d, want 0 (no cleanup needed in read-only-observe)", fr.backspaces)
	}
}

// chamberStateRunner extends fakeProbeRunner with cursor-position
// responses for the display-message call ChamberState makes in v2.
// Returns capture-pane content from the captures slice + cursor
// position as "X/Y" for display-message.
type chamberStateRunner struct {
	*fakeProbeRunner
	cursorX int
	cursorY int
}

func newChamberStateRunner(captures []string, cursorX, cursorY int) *chamberStateRunner {
	return &chamberStateRunner{
		fakeProbeRunner: newFakeProbeRunner(captures),
		cursorX:         cursorX,
		cursorY:         cursorY,
	}
}

func (c *chamberStateRunner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	// Intercept display-message; delegate everything else to the
	// underlying fakeProbeRunner.
	c.mu.Lock()
	c.calls = append(c.calls, strings.Join(args, " "))
	c.mu.Unlock()
	if args[0] == "display-message" {
		return []byte(fmt.Sprintf("%d/%d\n", c.cursorX, c.cursorY)), nil
	}
	// Re-dispatch but skip the call-recording in the underlying runner
	// (we already recorded above to avoid double-counting).
	c.mu.Lock()
	// Temporarily pop the call we just added so the underlying runner
	// can re-add it via its own path. (Cleaner alternative: skip the
	// add and call the underlying run directly.)
	if len(c.calls) > 0 {
		c.calls = c.calls[:len(c.calls)-1]
	}
	c.mu.Unlock()
	return c.fakeProbeRunner.run(ctx, stdin, args...)
}

// TestChamberState_IdleWhenCursorAtSentinelEmpty pins the cursor-aware
// happy path for the clean-prompt case: cursor at the position right
// after `❯\u00a0` AND empty content past it → StateIdle with
// Evidence.PromptEmpty=true. v2 algorithm per cli-semaphore#69
// operator's design call 2026-06-04.
func TestChamberState_IdleWhenCursorAtSentinelEmpty(t *testing.T) {
	fastTemporalDelta(t)
	// Cursor row (index 3, 0-indexed) is `❯\u00a0` with no content past it.
	pane := "history\n──── Chamber ──\n  recap line\n❯\u00a0\n────────\n  status\n"
	// cursorX=2 (right after "❯\u00a0"); cursorY=3 (the ❯\u00a0row)
	fr := newChamberStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (cursor at sentinel + empty)", state)
	}
	if !ev.PromptEmpty {
		t.Errorf("Evidence.PromptEmpty should be true for the clean-prompt case")
	}
	if !strings.Contains(ev.Reason, "cursor at prompt sentinel") {
		t.Errorf("Evidence.Reason should mention cursor at sentinel; got %q", ev.Reason)
	}
}

// TestChamberState_IdleWhenCursorAtSentinelWithAutoSuggestion pins the
// v2 fix for the smoke-test gap: when the input row is `❯\u00a0<content>`
// but the cursor is still at the sentinel position (col == sentinel
// width), the content is Claude Code's auto-suggested ghost-text and
// the operator hasn't engaged. Classify as StateIdle with
// Evidence.PromptEmpty=false + descriptive Reason.
//
// Empirical fixture: Pilot's pane in the 2026-06-04 smoke test showed
// `❯\u00a0/nimbus-board` with cursor at col 2 — Claude Code's slash-command
// auto-suggestion. Operator had not typed the suggestion; it was a
// ghost-text proposal.
func TestChamberState_IdleWhenCursorAtSentinelWithAutoSuggestion(t *testing.T) {
	fastTemporalDelta(t)
	// Row 3 (0-indexed): `❯\u00a0/nimbus-board` — Claude Code auto-suggested.
	pane := "history\n──── Chamber ──\n  recap line\n❯\u00a0/nimbus-board\n────────\n  status\n"
	// cursorX=2 (right after "❯\u00a0", before "/"); cursorY=3 (the ❯\u00a0row).
	// Operator has NOT engaged — cursor would be past the suggestion if they had.
	fr := newChamberStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (cursor at sentinel + auto-suggestion ghost-text)", state)
	}
	if ev.PromptEmpty {
		t.Errorf("Evidence.PromptEmpty should be false (there IS content, just not operator-typed)")
	}
	if !strings.Contains(ev.Reason, "auto-suggestion") {
		t.Errorf("Evidence.Reason should mention auto-suggestion; got %q", ev.Reason)
	}
}

// TestChamberState_AwaitingOperatorWhenCursorPastSentinel pins the
// operator-mid-typing case: cursor is past the sentinel position,
// meaning content past `❯\u00a0` is operator-typed (not ghost-text).
// Chamber is blocked on operator finishing the draft → return
// StateAwaitingOperator so consumers (Bosun) gate their dispatch.
func TestChamberState_AwaitingOperatorWhenCursorPastSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Row 3 (0-indexed): `❯\u00a0Thank you for handling this and ` (#63 reproduction shape).
	pane := "history\n──── Chamber ──\n  recap line\n❯\u00a0Thank you for handling this and \n────────\n  status\n"
	// cursorX=37 (past the typed content); cursorY=3.
	fr := newChamberStateRunner([]string{pane, pane}, 37, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (cursor past sentinel = operator mid-typing)", state)
	}
	if !strings.Contains(ev.Reason, "operator mid-typing") {
		t.Errorf("Evidence.Reason should mention operator mid-typing; got %q", ev.Reason)
	}
}

// TestChamberState_FallbackWhenCursorRowNotSentinel pins the cursor-
// less fallback path: when the cursor sits on a row that doesn't start
// with `❯\u00a0` (e.g., chamber is mid-spinner and cursor is on the spinner
// row), the v2 algorithm falls back to v1's classifyInputRow heuristic.
//
// Smoke evidence: Surveyor pane during PR review showed cursor at the
// title-separator row, not the ❯\u00a0input row (the chamber was working).
// The fallback lets the algorithm still classify cleanly when cursor
// position doesn't help.
func TestChamberState_FallbackWhenCursorRowNotSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Cursor at row 1 (not the ❯\u00a0row at row 3). Pane is otherwise stable
	// + has the sentinel with empty content; fallback to v1 heuristic
	// → StateIdle.
	pane := "history\n──── Chamber ──\n  recap line\n❯\u00a0\n────────\n  status\n"
	fr := newChamberStateRunner([]string{pane, pane}, 0, 1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (cursor-less fallback finds empty sentinel)", state)
	}
	if !strings.Contains(ev.Reason, "cursor-less fallback") {
		t.Errorf("Evidence.Reason should mention cursor-less fallback; got %q", ev.Reason)
	}
}

// TestChamberState_UnknownWithAccurateReason_SentinelFoundCursorOff pins
// the accurate-reason cleanup (the "C" item from the operator's
// 2026-06-04 discussion): when the sentinel IS in the pane but the
// cursor isn't at the input row AND the cursor-less fallback didn't
// match (DeltaInputActivity on the input row), the Unknown branch
// reports "sentinel found but cursor not at input row" rather than the
// misleading v1 "no prompt sentinel" message.
func TestChamberState_UnknownWithAccurateReason_SentinelFoundCursorOff(t *testing.T) {
	fastTemporalDelta(t)
	// Pane has the sentinel but with content past it. Cursor on row 1
	// (a non-sentinel row). classifyInputRow returns DeltaInputActivity
	// (sentinel + content) → not DeltaQuiet → fallback doesn't classify
	// as Idle. Falls through to Unknown — and the reason should name
	// the actual situation, not "no prompt sentinel".
	pane := "history\n  spinner-ish content\n❯\u00a0<agent-narration>\n────────\n  status\n"
	fr := newChamberStateRunner([]string{pane, pane}, 0, 1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown", state)
	}
	if strings.Contains(ev.Reason, "no prompt sentinel") {
		t.Errorf("Evidence.Reason should NOT say 'no prompt sentinel' when sentinel IS in pane; got %q", ev.Reason)
	}
	if !strings.Contains(ev.Reason, "sentinel found") {
		t.Errorf("Evidence.Reason should accurately name that sentinel was found; got %q", ev.Reason)
	}
}

// TestChamberState_ContextCancelledDuringTemporalDelta pins the
// cancellation contract: a context cancelled between the two captures
// returns StateUnknown + ctx.Err() rather than racing the second
// capture or silently waiting out the delta.
func TestChamberState_ContextCancelledDuringTemporalDelta(t *testing.T) {
	// Use the production temporal delta here so the cancellation has
	// time to fire mid-wait. (Microsecond delta would race the cancel.)
	prev := SetChamberStateTemporalDeltaForTest(100 * time.Millisecond)
	t.Cleanup(func() { SetChamberStateTemporalDeltaForTest(prev) })

	pane := "history\n❯\u00a0\n  status\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prevRunner := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prevRunner) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	state, _, err := ChamberState(ctx, "%5")
	if err == nil {
		t.Fatal("expected error from ctx cancellation, got nil")
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown when ctx cancelled mid-wait", state)
	}
	// Exactly one capture should have happened — the cancellation cuts
	// the temporal-delta short before the second capture.
	if len(fr.calls) != 1 {
		t.Errorf("tmux call count = %d, want 1 (cancellation aborts before the second capture-pane)", len(fr.calls))
	}
}

// TestCountChangedLines_DiffShape pins the helper's behavior on the
// shapes ChamberState cares about: same content → 0; trailing line
// added → 1; spinner-counter line changed → 1; complete rewrite → all
// lines.
func TestCountChangedLines_DiffShape(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want int
	}{
		{"identical", "a\nb\nc\n", "a\nb\nc\n", 0},
		{"one line changed", "a\nb\nc\n", "a\nX\nc\n", 1},
		{"trailing line added", "a\nb\n", "a\nb\nc\n", 1},
		{"all lines different", "a\nb\nc\n", "x\ny\nz\n", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := countChangedLines(c.a, c.b); got != c.want {
				t.Errorf("countChangedLines(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
			}
		})
	}
}

// TestChamberState_AwaitingOperatorOnAskUserQuestionGolden pins the
// end-to-end classification for the AskUserQuestion popup scenario
// (cli-semaphore#79). Loads the capture-derived golden fixture as
// both capture-pane responses (the pane is stable across the temporal
// delta — operator is reading the popup; nothing's animating), and
// asserts ChamberState returns StateAwaitingOperator with the marker
// surfaced in Evidence.
//
// The state.go classifier reaches the AwaitingOperatorMarker check
// (precedence 5) because:
//   - No CompactionMarker match (capture is a popup, not compaction)
//   - capA == capB (stable pane)
//   - Cursor lookup finds no PromptSentinel row (the popup overlays
//     the input area), so the cursor-aware classification falls
//     through to the marker check
//   - AwaitingOperatorMarker matches the popup footer
//
// Without this pin, a future refactor could silently break the
// AwaitingOperatorMarker path while the canary in probe_test.go
// (which only checks the substring is in the golden) would still pass.
// This test pins the load-bearing *classification*, not just the
// constant-vs-golden alignment.
func TestChamberState_AwaitingOperatorOnAskUserQuestionGolden(t *testing.T) {
	fastTemporalDelta(t)
	golden, err := os.ReadFile("testdata/golden_quartermaster_askuserquestion_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	pane := string(golden)
	fr := newChamberStateRunner([]string{pane, pane}, 0, 0)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (popup golden + non-empty AwaitingOperatorMarker should hit precedence 5)",
			state)
	}
	if ev.Marker == "" {
		t.Errorf("Evidence.Marker is empty; expected the matched AwaitingOperatorMarker substring")
	}
	if ev.Marker != AwaitingOperatorMarker {
		t.Errorf("Evidence.Marker = %q, want %q", ev.Marker, AwaitingOperatorMarker)
	}
	if ev.Reason == "" {
		t.Errorf("Evidence.Reason should name the awaiting-operator marker match")
	}
}

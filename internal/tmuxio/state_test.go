package tmuxio

import (
	"context"
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
	pane := "history line A\nhistory line B\n──── Chamber ──\n❯ \n────────\n  status\n"
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
	paneA := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 5s\n❯ \n  status\n"
	paneB := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 6s\n❯ \n  status\n"
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
	// Pane shows streaming output with no `❯ ` row in view + no marker.
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
// EXACTLY 2 tmux calls (both capture-pane) and ZERO send-keys. This is
// the distinguishing property vs QuickPresenceProbe (write+observe)
// and is the load-bearing claim that ChamberState honors the "knock at
// the door without waking" framing from cli-semaphore#69.
func TestChamberState_NoPaneMutation(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Chamber ──\n❯ \n────────\n  status\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, _, err := ChamberState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Errorf("tmux call count = %d, want 2 (read-only-observe = exactly two capture-pane calls)", len(fr.calls))
	}
	for i, call := range fr.calls {
		if !strings.HasPrefix(call, "capture-pane") {
			t.Errorf("call[%d] = %q, want capture-pane prefix (no send-keys in read-only-observe)", i, call)
		}
	}
	if fr.probeChars != 0 {
		t.Errorf("probe chars sent = %d, want 0 (no inject)", fr.probeChars)
	}
	if fr.backspaces != 0 {
		t.Errorf("backspaces = %d, want 0 (no cleanup needed in read-only-observe)", fr.backspaces)
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

	pane := "history\n❯ \n  status\n"
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

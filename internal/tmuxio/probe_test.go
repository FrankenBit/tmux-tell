package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- PromptSentinel encoding canary tests ---
//
// These tests anchor the PromptSentinel constant to the actual byte
// encoding Claude Code emits in production pane output. Per
// cli-semaphore#69 substrate-discovery 2026-06-04: PR #66 + PR #77
// shipped with the regular-space variant ("❯ "), but tmux + Claude
// Code actually paint with NBSP (U+00A0, hex c2 a0). The bug was
// invisible to unit tests because the test fixtures themselves used
// the regular-space variant — a spec-derived fixture rather than a
// capture-derived one (Surveyor's O69 discipline-class). These canary
// tests close that gap.

// TestPromptSentinel_BytesMatchNBSP pins the byte-level encoding of
// PromptSentinel against the empirically-captured production bytes.
// If a future contributor changes the constant to use a regular space
// (U+0020) instead of NBSP (U+00A0), this test catches it before
// merge.
func TestPromptSentinel_BytesMatchNBSP(t *testing.T) {
	// The Claude Code TUI emits ❯ (U+276F, hex e2 9d af) followed by
	// NBSP (U+00A0, hex c2 a0). Empirically captured across all 6
	// chambers on 2026-06-04 via `tmux capture-pane | od -An -tx1`.
	want := []byte{0xe2, 0x9d, 0xaf, 0xc2, 0xa0}
	got := []byte(PromptSentinel)
	if !bytesEqual(got, want) {
		t.Errorf("PromptSentinel bytes = % x, want % x (❯ + U+00A0 NBSP)", got, want)
	}
}

// bytesEqual is a tiny helper so the canary test doesn't import
// "bytes" just for one Equal call.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPromptSentinel_MatchesGoldenCapture pins PromptSentinel against
// a real `tmux capture-pane` output frozen as testdata. This is the
// capture-derived (vs spec-derived) anchor per Surveyor's O69
// discipline-class — if Claude Code's emission encoding changes
// (theme update, terminal switch, version bump), the golden fixture
// stops matching and surfaces the divergence loudly.
//
// Forward-watch: re-capture the golden file when Claude Code TUI
// changes, or when this test fails after a Claude Code version bump.
// The capture command is documented in PromptSentinel's doc-comment.
func TestPromptSentinel_MatchesGoldenCapture(t *testing.T) {
	golden, err := os.ReadFile("testdata/golden_bosun_idle_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read golden capture: %v", err)
	}
	found := false
	for _, line := range strings.Split(string(golden), "\n") {
		if strings.HasPrefix(line, PromptSentinel) {
			found = true
			t.Logf("matched sentinel on golden line: %q", line[:min(50, len(line))])
			break
		}
	}
	if !found {
		t.Errorf("golden capture has NO line starting with PromptSentinel %q (% x) — Claude Code emission encoding may have drifted; re-verify via tmux capture-pane | od -An -tx1 on a live chamber + update PromptSentinel + re-capture the golden fixture", PromptSentinel, []byte(PromptSentinel))
	}
}

// --- analyzeDelta unit tests ---

func TestAnalyzeDelta_Quiet_TwoTrailingProbes(t *testing.T) {
	before := "response line A\nresponse line B\n> hello\nstatus line\n"
	after := "response line A\nresponse line B\n> hello──\nstatus line\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (two probes appended cleanly)", v)
	}
}

func TestAnalyzeDelta_InputActivity_OperatorTypedAfterProbes(t *testing.T) {
	// Operator typed 'x' after our two probes landed. Input row ends
	// with 'x', not probes → strip-2 fails → DeltaInputActivity.
	before := "context\n\n> \n"
	after := "context\n\n> ──x\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (operator typed after probes)", v)
	}
}

func TestAnalyzeDelta_InputActivity_OperatorRemovedProbe(t *testing.T) {
	// Operator deleted one of our two probes. Input row has only 1
	// trailing probe → strip-2 fails (not enough probes) →
	// DeltaInputActivity.
	before := "context\n\n> \n"
	after := "context\n\n> ─\n" // only 1 probe (operator removed one)
	if v := analyzeDelta(before, after, "─", 2); v != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (operator removed a probe)", v)
	}
}

func TestAnalyzeDelta_InputActivity_OperatorTypedBetweenProbes(t *testing.T) {
	// Operator typed 'x' between our two probes — input row ends with
	// `─x─` not `──`. The trailing-2-probes strip works, but the
	// resulting "stripped" content (`> ─x`) doesn't match before
	// (`> `) → DeltaInputActivity.
	before := "context\n\n> \n"
	after := "context\n\n> ─x─\n"
	// Note: this ends with one trailing probe, then `x`, then a probe.
	// stripTrailingProbes wants exactly 2 trailing. The character
	// before the last probe is `x` (non-probe), so we can only strip
	// one trailing probe before hitting a non-probe... wait, let me
	// re-check. Last char is `─`, before that is `x` (non-probe).
	// stripTrailingProbes(─x─, ─, 2) — first iter strips one probe →
	// `> ─x`; second iter wants suffix `─` but suffix is `x` → fails
	// → returns false. Correctly classified as InputActivity.
	if v := analyzeDelta(before, after, "─", 2); v != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (operator typed between probes)", v)
	}
}

func TestAnalyzeDelta_Quiet_ConversationStreamingAboveInputRow(t *testing.T) {
	// Mode-2 regression: the conversation area above the input row is
	// streaming (extra lines, content changes). The input row is
	// stable except for our two probes. Should still deliver — the
	// new design doesn't gate on conversation-area changes.
	before := "old line 1\nold line 2\n> \n"
	after := "old line 1\nNEW STREAMED LINE\nold line 2\n> ──\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (conversation streaming above input is ignored)", v)
	}
}

func TestAnalyzeDelta_Quiet_PrevAccumulatedProbes(t *testing.T) {
	// A prior iteration backed off with 2 probes accumulated in the
	// input row. This iteration adds 2 more — input row now ends with
	// 4 probes. Strip the 2 we just added → matches before.
	before := "ctx\n> ──\n"
	after := "ctx\n> ────\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (accumulated probes + 2 new)", v)
	}
}

func TestAnalyzeDelta_InputActivity_SeparatorRowDoesntFalseQuiet(t *testing.T) {
	// A pure-dash separator row exists. It ends with the probe, but
	// stripping 2 leaves a row of pure dashes — should NOT match
	// because (a) the character before the stripped suffix is itself
	// a probe (failing the stripTrailingProbes guard), and (b) the
	// before-separator is identical so even if stripped to a different
	// length it wouldn't match. The real input row is what produces
	// DeltaQuiet here, not the separator.
	before := "msg\n──────────\n> \n"
	after := "msg\n──────────\n> ──\n"
	if v := analyzeDelta(before, after, "─", 2); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (separator should not interfere)", v)
	}
}

func TestStripTrailingProbes_AcceptsAllDashRowAtFaceValue(t *testing.T) {
	// Pure-dash rows (separators) get stripped at face value; the
	// safety against false-matching them lives one level up — the
	// stripped result won't match the (unchanged) separator in
	// `before`. Verified by TestAnalyzeDelta_InputActivity_SeparatorRowDoesntFalseQuiet.
	got, ok := stripTrailingProbes("──────────", "─", 2)
	if !ok {
		t.Errorf("stripTrailingProbes should accept any row with >= n trailing probes")
	}
	if got != "────────" {
		t.Errorf("got %q, want %q (10 dashes minus 2)", got, "────────")
	}
}

func TestStripTrailingProbes_AcceptsExactlyNTrailingProbes(t *testing.T) {
	got, ok := stripTrailingProbes("> ──", "─", 2)
	if !ok {
		t.Fatalf("stripTrailingProbes should accept exactly n trailing probes")
	}
	if got != "> " {
		t.Errorf("got %q, want %q", got, "> ")
	}
}

func TestStripTrailingProbes_RejectsFewerThanNTrailingProbes(t *testing.T) {
	_, ok := stripTrailingProbes("> ─", "─", 2)
	if ok {
		t.Errorf("stripTrailingProbes should reject when fewer than n trailing probes")
	}
}

// --- WaitForQuietPane integration tests ---

// fakeProbeRunner is a scripted tmux runner. Captures are consumed in
// order as the loop progresses. Probe injections (`send-keys -l ─`)
// are counted; backspaces are counted separately.
type fakeProbeRunner struct {
	mu         sync.Mutex
	captures   []string
	captureIdx int
	probeChars int
	backspaces int
	calls      []string
}

func newFakeProbeRunner(captures []string) *fakeProbeRunner {
	return &fakeProbeRunner{captures: captures}
}

func (f *fakeProbeRunner) run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "capture-pane":
		if f.captureIdx >= len(f.captures) {
			return []byte(f.captures[len(f.captures)-1]), nil
		}
		out := f.captures[f.captureIdx]
		f.captureIdx++
		return []byte(out), nil
	case "send-keys":
		for i, a := range args {
			if a == "-l" && i+1 < len(args) && args[i+1] == QuietProbe {
				f.probeChars++
				return nil, nil
			}
			if a == "BSpace" {
				f.backspaces++
				return nil, nil
			}
		}
		return nil, nil
	}
	return nil, nil
}

// quickOpts gives a QuickPresenceProbe paint-wait that completes
// within microseconds for tests.
func quickOpts() QuickPresenceOpts {
	return QuickPresenceOpts{PaintWait: 1 * time.Millisecond}
}

// TestQuickPresenceProbe_QuietWhenIdle pins the speed-win common case:
// pane is idle, two probes land cleanly, analyzeDelta returns
// DeltaQuiet, both probes get backspaced before return.
func TestQuickPresenceProbe_QuietWhenIdle(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"ctx\n> \n",   // before — empty input row
		"ctx\n> ──\n", // after — two probes appended cleanly
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := QuickPresenceProbe(context.Background(), "%5", quickOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (probes landed cleanly on idle pane)", verdict)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe chars sent = %d, want 2", fr.probeChars)
	}
	if fr.backspaces != 2 {
		t.Errorf("backspaces = %d, want 2 (probes must always be cleaned up)", fr.backspaces)
	}
}

// TestQuickPresenceProbe_DetectsActiveTyping pins the safety case:
// operator types during the probe window, probes don't land cleanly
// (operator's keystroke landed after the probes), analyzeDelta
// returns DeltaInputActivity. Cleanup still backspaces the probes.
func TestQuickPresenceProbe_DetectsActiveTyping(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"ctx\n> \n",    // before
		"ctx\n> ──x\n", // after — operator typed 'x' after our probes
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := QuickPresenceProbe(context.Background(), "%5", quickOpts())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (typing detected)", verdict)
	}
	if fr.backspaces != 2 {
		t.Errorf("backspaces = %d, want 2 (probes cleaned up on activity branch too)", fr.backspaces)
	}
}

// TestQuickPresenceProbe_PaneRequired pins the input-validation guard:
// empty pane returns an error WITHOUT firing any tmux calls.
func TestQuickPresenceProbe_PaneRequired(t *testing.T) {
	fr := newFakeProbeRunner([]string{"ctx\n", "ctx\n"})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, err := QuickPresenceProbe(context.Background(), "", quickOpts())
	if err == nil {
		t.Fatal("expected error for empty pane, got nil")
	}
	if len(fr.calls) != 0 {
		t.Errorf("tmux was called %d times, want 0 (validation should reject before any tmux call)", len(fr.calls))
	}
}

// TestInputRowHasContent_QuietWhenSentinelEmpty pins the happy path:
// pane shows the prompt sentinel followed by an empty input row →
// DeltaQuiet, single capture-pane call, zero pane mutations.
func TestInputRowHasContent_QuietWhenSentinelEmpty(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"history line A\nhistory line B\n──── Chamber ──\n❯\u00a0\n────────\n  status line\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (empty input row past sentinel)", verdict)
	}
	if fr.probeChars != 0 {
		t.Errorf("probe chars sent = %d, want 0 (no inject in read-only-observe)", fr.probeChars)
	}
	if fr.backspaces != 0 {
		t.Errorf("backspaces = %d, want 0 (no cleanup needed in read-only-observe)", fr.backspaces)
	}
}

// TestInputRowHasContent_DetectsOperatorDraftSitting pins the headline
// #63 case: the operator's draft is sitting in the input row when a
// delivery would land. A bus delivery's trailing Enter would chain the
// draft + the delivery body as one submission. Heuristic returns
// DeltaInputActivity so the gate is engaged.
func TestInputRowHasContent_DetectsOperatorDraftSitting(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"history\n──── Chamber ──\n❯\u00a0Thank you for handling this and \n────────\n  status\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (draft sitting in input row)", verdict)
	}
}

// TestInputRowHasContent_DetectsAgentNarration pins the worked-example
// from the cli-semaphore#63 Part 2 design pass: Surveyor's pane showed
// `❯\u00a0<Silence — standing by ...>` (agent-side narration), and the
// heuristic correctly classifies it as DeltaInputActivity because the
// text IS in the input buffer (Enter would submit it). Non-typed
// content in the input row is substrate-equivalent to operator-typed
// content for gate purposes.
func TestInputRowHasContent_DetectsAgentNarration(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"history\n──── Surveyor ──\n❯\u00a0<Silence — standing by per close-out.>\n────────\n  status\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (agent narration in input row is gate-equivalent)", verdict)
	}
}

// TestInputRowHasContent_NoSentinelFallsBackToGate pins the safer
// default: when the pane has no PromptSentinel row at all (mid-stream
// output, menu overlay, search dialog, …), the heuristic returns
// DeltaInputActivity so the asymmetric gate falls back to the full
// gate rather than delivering into an unknown UI state.
func TestInputRowHasContent_NoSentinelFallsBackToGate(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"streaming output line 1\nstreaming output line 2\n[cursor mid-stream]\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity (no PromptSentinel found → safer default)", verdict)
	}
}

// TestInputRowHasContent_PaneRequired pins the input-validation guard:
// empty pane returns an error WITHOUT firing any tmux calls. Mirrors
// the QuickPresenceProbe / WaitForQuietPane validation discipline.
func TestInputRowHasContent_PaneRequired(t *testing.T) {
	fr := newFakeProbeRunner([]string{"ctx\n"})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, err := InputRowHasContent(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty pane, got nil")
	}
	if len(fr.calls) != 0 {
		t.Errorf("tmux was called %d times, want 0 (validation should reject before any tmux call)", len(fr.calls))
	}
}

// TestInputRowHasContent_KnownLimitation_MultiLineContinuationFalseNegative
// pins the multi-line-continuation false-negative documented in
// InputRowHasContent's doc-comment. Today this returns DeltaQuiet even
// though the input buffer holds content on a continuation row — because
// the heuristic scans rows beginning with PromptSentinel and the
// continuation row lacks the sentinel prefix.
//
// This test exists as test-pinned contract for the documented limitation
// (sibling to the doc-comment claim). When the upgrade to a region-based
// scan lands (the input area is bounded by horizontal-rule separators
// per the empirical capture; scanning all rows between those separators
// catches continuation content regardless of sentinel prefix), THIS TEST
// WILL INTENTIONALLY FLIP — the expected verdict becomes
// DeltaInputActivity. At that point this test should be renamed / removed
// and the corresponding known-limitation paragraph in
// InputRowHasContent's doc-comment removed alongside, in the same commit
// that strengthens the heuristic. The intentional-flip is the regression-
// protection for "we said we'd fix this and we did" landing as a single
// atomic substrate-change.
func TestInputRowHasContent_KnownLimitation_MultiLineContinuationFalseNegative(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		// Row 1 has PromptSentinel + empty; row 2 is continuation
		// content with no sentinel prefix (indent-style continuation
		// is one plausible paint format Claude Code might use for
		// multi-line drafts).
		"history line\n──── Chamber ──\n❯\u00a0\n  continuation line with operator content\n────────\n  status\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	verdict, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if verdict != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (multi-line-continuation false-negative — known limitation per InputRowHasContent's doc-comment). If this assertion fails because the heuristic now correctly classifies continuation content, REMOVE this test and the corresponding known-limitation paragraph from InputRowHasContent's doc-comment in the same commit that strengthens the heuristic.", verdict)
	}
}

// TestInputRowHasContent_NoPaneMutationOnQuiet pins the substrate-class
// property: InputRowHasContent is read-only-observe. A successful
// classification (either DeltaQuiet or DeltaInputActivity) makes
// exactly ONE tmux call — a capture-pane — and no send-keys for either
// probes or cleanup. This is the distinguishing property vs the
// QuickPresenceProbe (write+observe) and is the reason this gate is
// safe to call before every delivery without operational footprint.
func TestInputRowHasContent_NoPaneMutationOnQuiet(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"history\n──── Chamber ──\n❯\u00a0\n────────\n  status\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, err := InputRowHasContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fr.calls) != 1 {
		t.Errorf("tmux call count = %d, want 1 (read-only-observe = single capture-pane)", len(fr.calls))
	}
	if !strings.HasPrefix(fr.calls[0], "capture-pane") {
		t.Errorf("first call = %q, want capture-pane (no send-keys in read-only-observe)", fr.calls[0])
	}
}

// shortOpts gives a probe-and-watch loop that completes within
// milliseconds — production defaults (3s/60s/5min) are unsuitable for
// tests.
func shortOpts() QuietOpts {
	return QuietOpts{
		ObserveWindow:        1 * time.Millisecond,
		InputActivityBackoff: 5 * time.Millisecond,
		MaxWait:              200 * time.Millisecond,
	}
}

func TestWaitForQuietPane_QuietFirstAttempt(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"ctx\n> \n",   // before
			"ctx\n> ──\n", // after — two probes appended cleanly
		},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe injections = %d, want 2 (two-dash design)", fr.probeChars)
	}
	if fr.backspaces != 2 {
		t.Errorf("backspaces on quiet exit = %d, want 2 (cleanup of both probes)", fr.backspaces)
	}
}

// Conversation streaming above the input row used to trigger
// DeltaTUINoise → 5min cap-hit (the 28ca incident on 2026-05-31). The
// new design ignores everything except the input row → DeltaQuiet on
// first attempt despite the streaming.
func TestWaitForQuietPane_ConversationStreamingStillDelivers(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"old line\n> \n",               // before
			"old line\nNEW STREAM\n> ──\n", // after — conversation grew but input row clean
		},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe injections = %d, want 2", fr.probeChars)
	}
}

// Operator typed mid-cycle → DeltaInputActivity → backoff → on retry
// operator cleared, leaving prior probes in the input row → DeltaQuiet.
// Verifies probe accumulation across iterations (4 total probes; no
// backspaces between iters).
func TestWaitForQuietPane_OperatorTypedThenQuiet(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			// Iter 1: operator typed 'x' after our two probes
			"ctx\n> \n",    // before #1
			"ctx\n> ──x\n", // after #1 — operator interfered
			// Iter 2: operator cleared their x, our 2 probes still there
			"ctx\n> ──\n",   // before #2 (sees the 2 accumulated probes)
			"ctx\n> ────\n", // after #2 (2 more probes pasted)
		},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 4 {
		t.Errorf("probe injections = %d, want 4 (two iters of two probes each)", fr.probeChars)
	}
	// Final cleanup backspaces all 4 accumulated probes.
	if fr.backspaces != 4 {
		t.Errorf("backspaces on quiet exit = %d, want 4", fr.backspaces)
	}
}

// Cap-exceeded path: operator keeps interfering past MaxWait. On exit,
// all accumulated probes are backspaced so delivery starts with a
// clean input. This is the visual-mess fix.
func TestWaitForQuietPane_CapExceededCleansAccumulatedProbes(t *testing.T) {
	// Every iteration: operator typed → InputActivity. Loop iterates
	// until MaxWait fires.
	captures := []string{}
	for i := 0; i < 30; i++ {
		captures = append(captures,
			fmt.Sprintf("ctx %d\n> \n", i),    // before
			fmt.Sprintf("ctx %d\n> ──x\n", i), // after — interfered
		)
	}
	fr := newFakeProbeRunner(captures)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.MaxWait = 25 * time.Millisecond
	err := WaitForQuietPane(context.Background(), "%1", opts)
	if !errors.Is(err, ErrCapExceeded) {
		t.Errorf("err = %v, want ErrCapExceeded", err)
	}
	if fr.backspaces == 0 {
		t.Errorf("cap-exceeded path should backspace accumulated probes")
	}
	if fr.backspaces != fr.probeChars {
		t.Errorf("backspaces (%d) should equal probe count (%d) on cap-exceeded — visual-mess invariant",
			fr.backspaces, fr.probeChars)
	}
}

// TestWaitForQuietPane_PingsDuringBackoff regression for the
// 2026-05-30 surveyor-mailman SIGABRT. Without periodic pings in
// internal sleeps, the systemd watchdog (WatchdogSec=30s) trips and
// kills the process during a long backoff.
func TestWaitForQuietPane_PingsDuringBackoff(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"ctx\n> \n",
			"ctx\n> ──x\n", // input activity → backoff
			"ctx\n> ──x\n",
			"ctx\n> ──x──\n", // x still there + 2 probes; stripped = `> ──x`; matches before. Quiet.
		},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	var pingCount int
	opts := QuietOpts{
		ObserveWindow:        1 * time.Millisecond,
		InputActivityBackoff: 12 * time.Millisecond,
		MaxWait:              500 * time.Millisecond,
		PingInterval:         3 * time.Millisecond,
		Ping:                 func() { pingCount++ },
	}
	if err := WaitForQuietPane(context.Background(), "%1", opts); err != nil {
		t.Fatalf("err: %v", err)
	}
	if pingCount < 3 {
		t.Errorf("ping count = %d; want >= 3 (watchdog stays alive across backoff)", pingCount)
	}
}

// Nil Ping is safe (preserves the "no watchdog wired" path).
func TestWaitForQuietPane_NilPingNoPanic(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{"ctx\n> \n", "ctx\n> ──\n"},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.Ping = nil
	if err := WaitForQuietPane(context.Background(), "%1", opts); err != nil {
		t.Errorf("nil Ping should be safe: %v", err)
	}
}

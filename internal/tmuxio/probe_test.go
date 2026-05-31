package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

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
			"old line\n> \n",                  // before
			"old line\nNEW STREAM\n> ──\n",    // after — conversation grew but input row clean
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
			"ctx\n> \n",     // before #1
			"ctx\n> ──x\n",  // after #1 — operator interfered
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
			fmt.Sprintf("ctx %d\n> \n", i),       // before
			fmt.Sprintf("ctx %d\n> ──x\n", i),    // after — interfered
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
			"ctx\n> ──x\n",  // input activity → backoff
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

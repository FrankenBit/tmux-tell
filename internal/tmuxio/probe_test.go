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

func TestAnalyzeDelta_Quiet(t *testing.T) {
	before := "response line A\nresponse line B\n> hello\nstatus line\n"
	after := "response line A\nresponse line B\n> hello─\nstatus line\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet", v)
	}
}

func TestAnalyzeDelta_InputActivity_OperatorTyped(t *testing.T) {
	// Operator typed 'x' after our probe landed. The input row
	// (somewhere — we don't tell analyzeDelta where) gained both
	// a probe AND an 'x', so no row matches the "+probe-only"
	// signature; pass 2 sees a row gained a probe → InputActivity.
	before := "context\n\n> \n"
	after := "context\n\n> ─x\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaInputActivity {
		t.Errorf("verdict = %v, want DeltaInputActivity", v)
	}
}

func TestAnalyzeDelta_OperatorDeletedProbe_ClassifiesAsProbeMissing(t *testing.T) {
	// The handshake: operator saw the probe and removed it. The
	// after-capture has no new probe characters anywhere → no row
	// gained a probe → DeltaProbeMissing. The mailman's policy treats
	// this as the same "safe back off" path as DeltaInputActivity,
	// so the operator-deleted-probe handshake still gets the long
	// backoff downstream.
	before := "context\n\n> \n"
	after := "context\n\n> \n"
	if v := analyzeDelta(before, after, "─"); v != DeltaProbeMissing {
		t.Errorf("verdict = %v, want DeltaProbeMissing (operator-removed-probe)", v)
	}
}

func TestAnalyzeDelta_TUINoise(t *testing.T) {
	// Status line changed but the input row is clean. The input row
	// (last line) signature matches; non-input rows differ → TUINoise.
	before := "line A\nthinking... 5s\n> \n"
	after := "line A\nthinking... 6s\n> ─\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaTUINoise {
		t.Errorf("verdict = %v, want DeltaTUINoise", v)
	}
}

func TestAnalyzeDelta_ProbeMissing(t *testing.T) {
	before := "ctx\n> hi\n"
	after := "ctx\n> hi\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaProbeMissing {
		t.Errorf("verdict = %v, want DeltaProbeMissing", v)
	}
}

func TestAnalyzeDelta_ProbeStrippedFromRightmost(t *testing.T) {
	// Before contains existing probe characters (chat header). Our
	// probe is the rightmost newly-added one. The chat header row
	// has the same probe count in before and after, so it gets
	// skipped; the input row gains the probe and matches.
	before := "─── header ───\nbody\n> already typing\n"
	after := "─── header ───\nbody\n> already typing─\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (rightmost-probe-stripped)", v)
	}
}

// 2026-05-31 regression: when Claude Code renders a tool call, the
// rendering cursor sits inside the response area (not the input box).
// The probe still lands in the input box because typed text always
// goes there. cursor_y-based identification would look at the wrong
// row and return ProbeMissing; the probe-position-based approach
// finds the input row by where the new probe character landed.
func TestAnalyzeDelta_RenderingCursorElsewhereStillFindsInputRow(t *testing.T) {
	// Multi-line response area + input box at the bottom. Imagine
	// cursor_y pointed at line 1 (response area). The old logic would
	// look at line 1 for the probe, miss it, return ProbeMissing.
	before := "tool output line\nmore output\n> \n"
	after := "tool output line\nmore output\n> ─\n"
	if v := analyzeDelta(before, after, "─"); v != DeltaQuiet {
		t.Errorf("verdict = %v, want DeltaQuiet (find input row by probe location, not cursor_y)", v)
	}
}

// --- WaitForQuietPane integration tests ---

// fakeProbeRunner is a scripted tmux runner. Captures and cursor_y
// values are consumed in turn as the loop progresses.
type fakeProbeRunner struct {
	mu         sync.Mutex
	captures   []string
	cursorYs   []int
	captureIdx int
	cursorIdx  int
	probeChars int
	backspaces int
	calls      []string
}

func newFakeProbeRunner(captures []string, cursorYs []int) *fakeProbeRunner {
	return &fakeProbeRunner{captures: captures, cursorYs: cursorYs}
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
	case "display-message":
		var cy int
		if f.cursorIdx < len(f.cursorYs) {
			cy = f.cursorYs[f.cursorIdx]
			f.cursorIdx++
		} else if len(f.cursorYs) > 0 {
			cy = f.cursorYs[len(f.cursorYs)-1]
		}
		return []byte(fmt.Sprintf("%d\n", cy)), nil
	}
	return nil, nil
}

// shortOpts gives a probe-and-watch loop that completes within
// milliseconds — production defaults (5s/60s/5min) are unsuitable
// for tests.
func shortOpts() QuietOpts {
	return QuietOpts{
		ObserveWindow:        5 * time.Millisecond,
		InputActivityBackoff: 5 * time.Millisecond,
		TUINoiseBackoff:      2 * time.Millisecond,
		MaxWait:              200 * time.Millisecond,
	}
}

func TestWaitForQuietPane_QuietFirstAttempt(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"ctx\n> \n",   // before
			"ctx\n> ─\n",  // after
		},
		[]int{1}, // cursor_y on the "> " line
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 1 {
		t.Errorf("probe injections = %d, want 1", fr.probeChars)
	}
	if fr.backspaces != 1 {
		t.Errorf("backspaces on quiet exit = %d, want 1", fr.backspaces)
	}
}

// TUI noise on the first iteration, quiet on the second.
// Verifies: short backoff between iterations, then accumulated probes
// (2 by now) are cleaned up on the quiet path.
func TestWaitForQuietPane_TUINoiseThenQuiet(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"thinking 1s\n> \n",   // round 1 before
			"thinking 2s\n> ─\n",  // round 1 after — TUI noise (status changed)
			"thinking 2s\n> ─\n",  // round 2 before (probe still in input)
			"thinking 2s\n> ──\n", // round 2 after — only input changed by new probe
		},
		[]int{1, 1}, // cursor_y stable across both rounds
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe injections = %d, want 2", fr.probeChars)
	}
	// Two probes were sent. The quiet exit backspaces both.
	if fr.backspaces != 2 {
		t.Errorf("backspaces = %d, want 2 (cleanup of accumulated probes)", fr.backspaces)
	}
}

// Input activity (operator typed) → long backoff → quiet.
func TestWaitForQuietPane_InputActivityThenQuiet(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"ctx\n> \n",
			"ctx\n> ─x\n", // operator typed 'x'
			"ctx\n> ─x\n", // round 2 before (probe + 'x' still there)
			"ctx\n> ─x─\n", // round 2 after — only new probe added
		},
		[]int{1, 1},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.calls)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe injections = %d, want 2", fr.probeChars)
	}
	// Both probes backspaced on quiet exit (could eat operator's 'x',
	// bounded annoyance acknowledged in the design notes).
	if fr.backspaces != 2 {
		t.Errorf("backspaces = %d, want 2", fr.backspaces)
	}
}

// Cap exceeded path: gate never finds quiet; on exit, accumulated
// probes are backspaced so delivery starts with a clean input. This
// is the visual-mess fix from 2026-05-30.
func TestWaitForQuietPane_CapExceededCleansAccumulatedProbes(t *testing.T) {
	// Every iteration shows TUI noise. The cap fires after some N
	// iterations, and the cleanup backspaces all probes sent.
	captures := []string{}
	for i := 0; i < 30; i++ {
		captures = append(captures,
			fmt.Sprintf("thinking %ds\n> \n", i),
			fmt.Sprintf("thinking %ds\n> ─\n", i+1), // status changes
		)
	}
	cursorYs := make([]int, 30)
	for i := range cursorYs {
		cursorYs[i] = 1
	}
	fr := newFakeProbeRunner(captures, cursorYs)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.MaxWait = 50 * time.Millisecond
	err := WaitForQuietPane(context.Background(), "%1", opts)
	if !errors.Is(err, ErrCapExceeded) {
		t.Errorf("err = %v, want ErrCapExceeded", err)
	}
	if fr.backspaces == 0 {
		t.Errorf("cap-exceeded path should backspace accumulated probes; got %d", fr.backspaces)
	}
	if fr.backspaces != fr.probeChars {
		t.Errorf("backspaces (%d) should match probe count (%d) on cap-exceeded",
			fr.backspaces, fr.probeChars)
	}
}

// TestWaitForQuietPane_PingsDuringBackoff (regression for the
// 2026-05-30 surveyor-mailman SIGABRT). Without periodic pings in the
// internal sleeps, the systemd watchdog (WatchdogSec=30s) trips and
// kills the process during a long backoff.
func TestWaitForQuietPane_PingsDuringBackoff(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{
			"ctx\n> typing\n",
			"ctx\n> typing─more\n", // input activity
			"ctx\n> typing─more\n",
			"ctx\n> typing─more─\n", // quiet
		},
		[]int{1, 1},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	var pingCount int
	opts := QuietOpts{
		ObserveWindow:        2 * time.Millisecond,
		InputActivityBackoff: 12 * time.Millisecond,
		TUINoiseBackoff:      2 * time.Millisecond,
		MaxWait:              500 * time.Millisecond,
		PingInterval:         3 * time.Millisecond,
		Ping:                 func() { pingCount++ },
	}
	if err := WaitForQuietPane(context.Background(), "%1", opts); err != nil {
		t.Fatalf("err: %v", err)
	}
	if pingCount < 3 {
		t.Errorf("ping count = %d; want >= 3", pingCount)
	}
}

// Nil Ping is safe (preserves the "no watchdog wired" path).
func TestWaitForQuietPane_NilPingNoPanic(t *testing.T) {
	fr := newFakeProbeRunner(
		[]string{"ctx\n> \n", "ctx\n> ─\n"},
		[]int{1},
	)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.Ping = nil
	if err := WaitForQuietPane(context.Background(), "%1", opts); err != nil {
		t.Errorf("nil Ping should be safe: %v", err)
	}
}

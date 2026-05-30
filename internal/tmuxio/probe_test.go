package tmuxio

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbeRunner builds a scripted tmux runner that simulates a pane
// going through a sequence of capture-pane responses. Each captureStep
// gives the body to return on capture-pane calls until updateAfter is
// reached; the runner advances through steps as time passes.
type fakeProbeRunner struct {
	mu          sync.Mutex
	captures    []string // sequence of capture-pane responses, in order
	captureIdx  int
	probeChars  int // running count of send-keys -l "─" calls
	backspaces  int // running count of send-keys BSpace calls
	probeCallLog []string
}

func newFakeProbeRunner(captures []string) *fakeProbeRunner {
	return &fakeProbeRunner{captures: captures}
}

func (f *fakeProbeRunner) run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probeCallLog = append(f.probeCallLog, strings.Join(args, " "))
	switch args[0] {
	case "capture-pane":
		if f.captureIdx >= len(f.captures) {
			return []byte(f.captures[len(f.captures)-1]), nil
		}
		out := f.captures[f.captureIdx]
		f.captureIdx++
		return []byte(out), nil
	case "send-keys":
		// args looks like: send-keys -t <pane> -l "─" OR send-keys -t <pane> BSpace
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
// milliseconds — the production defaults (5s/60s/30min) are unsuitable
// for tests.
func shortOpts() QuietOpts {
	return QuietOpts{
		ObserveWindow:   5 * time.Millisecond,
		BackoffInterval: 5 * time.Millisecond,
		MaxWait:         200 * time.Millisecond,
		CaptureLines:    5,
	}
}

func TestWaitForQuietPane_QuietFirstAttempt(t *testing.T) {
	// Before: empty input row. After: same row + our probe.
	fr := newFakeProbeRunner([]string{
		"> \n",       // before-probe capture
		"> ─\n",      // after-probe capture (probe added, nothing else)
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.probeCallLog)
	}
	if fr.probeChars != 1 {
		t.Errorf("probe injections = %d, want 1", fr.probeChars)
	}
	if fr.backspaces != 1 {
		t.Errorf("backspaces on quiet exit = %d, want 1", fr.backspaces)
	}
}

func TestWaitForQuietPane_ActivityThenQuiet(t *testing.T) {
	// Round 1: before "> hi", after "> hi─more" → activity (operator typed).
	// Backoff 5ms. NO backspace on activity per design.
	// Round 2: before "> hi─more", after "> hi─more─" → quiet (only probe added).
	fr := newFakeProbeRunner([]string{
		"> hi\n",
		"> hi─more\n",
		"> hi─more\n",
		"> hi─more─\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := WaitForQuietPane(context.Background(), "%1", shortOpts()); err != nil {
		t.Fatalf("err: %v; calls=%v", err, fr.probeCallLog)
	}
	if fr.probeChars != 2 {
		t.Errorf("probe injections = %d, want 2 (round 1 + round 2)", fr.probeChars)
	}
	if fr.backspaces != 1 {
		t.Errorf("backspaces = %d, want 1 (only after the quiet exit)", fr.backspaces)
	}
}

func TestWaitForQuietPane_CapExceeded(t *testing.T) {
	// Every capture pair shows activity — we never reach quiet.
	// MaxWait is 60ms; ObserveWindow + BackoffInterval = 10ms per round,
	// so we should hit the cap after ~6 rounds.
	captures := []string{}
	for i := 0; i < 30; i++ {
		captures = append(captures, "> typing\n", "> typing more\n")
	}
	fr := newFakeProbeRunner(captures)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.MaxWait = 60 * time.Millisecond
	err := WaitForQuietPane(context.Background(), "%1", opts)
	if !errors.Is(err, ErrCapExceeded) {
		t.Errorf("err = %v, want ErrCapExceeded", err)
	}
	if fr.backspaces != 0 {
		t.Errorf("cap-exceeded path should NOT backspace; got %d", fr.backspaces)
	}
}

func TestWaitForQuietPane_ProbeNeverLands(t *testing.T) {
	// after-probe capture doesn't contain QuietProbe — something is wrong
	// (e.g. tmux ate the keystroke, capture-pane lagged). isQuiet returns
	// false; we back off and retry. The retry exhausts the cap.
	fr := newFakeProbeRunner([]string{
		"> nothing\n",
		"> nothing\n", // no probe visible
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	opts := shortOpts()
	opts.MaxWait = 30 * time.Millisecond
	err := WaitForQuietPane(context.Background(), "%1", opts)
	if !errors.Is(err, ErrCapExceeded) {
		t.Errorf("err = %v, want ErrCapExceeded (probe-never-lands collapses to cap)", err)
	}
}

func TestWaitForQuietPane_ContextCancellation(t *testing.T) {
	fr := newFakeProbeRunner([]string{
		"> hi\n",
		"> hi changed\n", // activity → backoff → ctx cancellation hits
		"> hi changed\n",
		"> hi changed─\n",
	})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	opts := QuietOpts{
		ObserveWindow:   5 * time.Millisecond,
		BackoffInterval: 100 * time.Millisecond, // long enough to hit ctx cancel
		MaxWait:         time.Second,
		CaptureLines:    5,
	}
	err := WaitForQuietPane(ctx, "%1", opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

func TestIsQuiet_ProbeAtEnd(t *testing.T) {
	if !isQuiet("> hello", "> hello─", "─") {
		t.Error("appending probe to end should be quiet")
	}
}

func TestIsQuiet_NoProbe(t *testing.T) {
	if isQuiet("> hello", "> hello", "─") {
		t.Error("missing probe should NOT be quiet")
	}
}

func TestIsQuiet_ExtraContent(t *testing.T) {
	if isQuiet("> ", "> typed─", "─") {
		t.Error("operator typed before probe → NOT quiet")
	}
	if isQuiet("> ", "> ─typed", "─") {
		t.Error("operator typed after probe → NOT quiet")
	}
}

func TestIsQuiet_BeforeAlreadyHasProbeShape(t *testing.T) {
	// Chat header in `before` already contains the ─ character; only
	// the LAST occurrence (our actual probe) is stripped.
	before := "─── header ───\n> "
	after := "─── header ───\n> ─"
	if !isQuiet(before, after, "─") {
		t.Error("probe at end with header above should be quiet")
	}
}

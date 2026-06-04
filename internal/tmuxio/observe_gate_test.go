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

// observeGateRunner is a per-test fake that drives ChamberState's
// (capture-pane × 2, display-message × 1) call shape plus the
// observe-gate's extractInputContent capture-pane call. The script is
// a per-iteration tuple of (paneA, paneB, cursorX, cursorY,
// inputContent) where inputContent is what extractInputContent would
// produce if the pane were captured at that moment — separating the
// fixture's stateful capture from the parse logic so tests can pin
// the gate's decisions cleanly without crafting precise pane strings.
type observeGateRunner struct {
	mu     sync.Mutex
	steps  []observeStep
	cursor int
	calls  []string
}

type observeStep struct {
	paneA, paneB     string
	cursorX, cursorY int
	inputContent     string
}

func newObserveGateRunner(steps []observeStep) *observeGateRunner {
	return &observeGateRunner{steps: steps}
}

func (r *observeGateRunner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, strings.Join(args, " "))
	if len(r.steps) == 0 {
		return nil, errors.New("observeGateRunner: out of steps")
	}
	step := r.steps[0]
	switch args[0] {
	case "capture-pane":
		// ChamberState's two captures, then extractInputContent's one.
		// We cycle 3 capture-pane calls per logical iteration; advance
		// to the next step after the third.
		r.cursor++
		switch r.cursor % 3 {
		case 1:
			return []byte(step.paneA), nil
		case 2:
			return []byte(step.paneB), nil
		case 0:
			// Synthesize a pane whose only sentinel-row content is
			// step.inputContent so extractInputContent recovers it
			// verbatim. If empty, no sentinel row → empty content.
			// The 80-char ─ separator + ⏵⏵ status line below the
			// sentinel bound the walk-until-boundary capture per
			// #96 so the synthetic "footer" content isn't slurped
			// into the extracted input.
			out := step.paneB
			if step.inputContent != "" {
				out = "header\n" + PromptSentinel + step.inputContent + "\n" +
					strings.Repeat("─", 80) + "\n  ⏵⏵ chrome\n"
			}
			// Advance to next step after the third capture (closes one
			// iteration's worth of calls).
			if len(r.steps) > 1 {
				r.steps = r.steps[1:]
			}
			r.cursor = 0
			return []byte(out), nil
		}
	case "display-message":
		return []byte(formatCursor(step.cursorX, step.cursorY)), nil
	}
	return nil, nil
}

func formatCursor(x, y int) string {
	// tmux's display-message format "%d/%d" for cursor_x/cursor_y.
	return formatInt(x) + "/" + formatInt(y) + "\n"
}

func formatInt(i int) string {
	// strconv.Itoa via a single allocation; the std fmt would also do.
	if i == 0 {
		return "0"
	}
	var buf [16]byte
	n := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// fastObserveDelta installs a microsecond temporal-delta on ChamberState
// for the duration of the test. Without this, each iteration sleeps
// 200ms for the cursor-aware classification's stability check, and the
// observe-gate's loop would run real-time over tests.
func fastObserveDelta(t *testing.T) {
	t.Helper()
	prev := SetChamberStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { SetChamberStateTemporalDeltaForTest(prev) })
}

// TestObserveGate_FastPathIdle pins the idle-on-first-iteration happy
// path: when the receiver's pane shows the prompt sentinel with the
// cursor at the sentinel position and an empty input row, the gate
// returns StateIdle on the first poll with Stale=false.
func TestObserveGate_FastPathIdle(t *testing.T) {
	fastObserveDelta(t)
	pane := "history\n" + PromptSentinel + "\nfooter\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: pane, paneB: pane, cursorX: 2, cursorY: 1, inputContent: ""},
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.State != StateIdle {
		t.Errorf("State = %v, want StateIdle", outcome.State)
	}
	if outcome.Stale {
		t.Errorf("Stale = true, want false on fast-path idle")
	}
	if outcome.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (fast-path idle)", outcome.Iterations)
	}
}

// TestObserveGate_StaleFlush pins the abandoned-draft path: when the
// operator's input-row content remains hash-stable for longer than
// InputStaleThreshold, the gate returns Stale=true with InputContent
// populated so the caller can archive + clear + paste. This is the
// load-bearing behavior change from #92's design.
func TestObserveGate_StaleFlush(t *testing.T) {
	fastObserveDelta(t)
	draft := "Hello there, I started typing and then walked away"
	// Build a pane where the cursor is PAST the sentinel position
	// (operator-mid-typing classification). cursorX = sentinel-width
	// (2) + len(draft) makes the cursor sit at end-of-draft.
	pane := "history\n" + PromptSentinel + draft + "\nfooter\n"
	cursorX := 2 + len(draft)
	steps := make([]observeStep, 5)
	for i := range steps {
		steps[i] = observeStep{
			paneA:        pane,
			paneB:        pane,
			cursorX:      cursorX,
			cursorY:      1,
			inputContent: draft,
		}
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		BackoffFactor:       1.0, // disable backoff growth for predictable timing
		InputStaleThreshold: 50 * time.Millisecond,
		MaxWait:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !outcome.Stale {
		t.Errorf("Stale = false, want true (stable draft past threshold)")
	}
	if outcome.InputContent != draft {
		t.Errorf("InputContent = %q, want %q", outcome.InputContent, draft)
	}
	if outcome.State != StateAwaitingOperator {
		t.Errorf("State = %v, want StateAwaitingOperator (cursor past sentinel)", outcome.State)
	}
	if outcome.Iterations < 2 {
		t.Errorf("Iterations = %d, want >= 2 (need at least one stable-window check)", outcome.Iterations)
	}
}

// TestObserveGate_ContentChangeResetsStale pins the hash-change-resets
// behavior: when the operator's input keeps changing, the stale timer
// resets each iteration → the gate keeps waiting indefinitely (capped
// only by MaxWait). The receive-after-quiet path of operator-types-
// then-stops would also exercise this — for now, we pin the simpler
// case: operator types continuously through the test budget; the gate
// returns ErrMaxWaitExceeded with Stale=true and the LAST-seen content.
func TestObserveGate_ContentChangeResetsStale(t *testing.T) {
	fastObserveDelta(t)
	// Build sentinel-row panes whose draft grows each iteration so
	// ChamberState classifies as StateAwaitingOperator (cursor past
	// sentinel position) and extractInputContent recovers the draft.
	mkPane := func(draft string) (string, int) {
		return "history\n" + PromptSentinel + draft + "\nfooter\n", 2 + len(draft)
	}
	steps := make([]observeStep, 5)
	drafts := []string{"Hel", "Hello", "Hello t", "Hello the", "Hello there"}
	for i, draft := range drafts {
		pane, cursor := mkPane(draft)
		steps[i] = observeStep{
			paneA: pane, paneB: pane, cursorX: cursor, cursorY: 1, inputContent: draft,
		}
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		BackoffFactor:       1.0,
		InputStaleThreshold: time.Hour, // never fires inside this test
		MaxWait:             20 * time.Millisecond,
	})
	if !errors.Is(err, ErrMaxWaitExceeded) {
		t.Errorf("err = %v, want ErrMaxWaitExceeded", err)
	}
	if !outcome.Stale {
		t.Errorf("Stale = false, want true on MaxWait exceeded (caller can still proceed)")
	}
	if outcome.InputContent == "" {
		t.Errorf("InputContent empty; expected the last-seen content for archiving")
	}
}

// TestObserveGate_ContextCancellation pins the cancellation contract:
// a context cancelled during the gate's sleep returns ctx.Err() with
// Stale=false. The caller should propagate the cancellation (typically
// SIGTERM mid-loop).
func TestObserveGate_ContextCancellation(t *testing.T) {
	fastObserveDelta(t)
	// Drive a fake that never goes idle, never changes content (just a
	// stable operator-typing state). The gate would normally wait for
	// the stale threshold; we cancel mid-loop instead.
	pane := "history\n" + PromptSentinel + "drafting...\nfooter\n"
	steps := make([]observeStep, 5)
	for i := range steps {
		steps[i] = observeStep{
			paneA:        pane,
			paneB:        pane,
			cursorX:      14,
			cursorY:      1,
			inputContent: "drafting...",
		}
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	outcome, err := ObserveGate(ctx, "%5", ObserveGateOpts{
		PollIntervalMin:     30 * time.Millisecond, // long enough to land in the sleep
		PollIntervalMax:     30 * time.Millisecond,
		InputStaleThreshold: time.Hour,
		MaxWait:             time.Minute,
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if outcome.Stale {
		t.Errorf("Stale = true on cancellation; expected false (caller shouldn't archive on SIGTERM)")
	}
}

// TestObserveGate_PaneRequired pins the input-validation guard.
func TestObserveGate_PaneRequired(t *testing.T) {
	_, err := ObserveGate(context.Background(), "", ObserveGateOpts{})
	if err == nil {
		t.Fatal("expected error for empty pane")
	}
}

// TestObserveGate_PingFires pins the systemd-watchdog plumbing: the
// Ping callback must run at least once per iteration so the mailman's
// sdnotify.Watchdog keeps ticking during long observe loops.
func TestObserveGate_PingFires(t *testing.T) {
	fastObserveDelta(t)
	pane := "history\n" + PromptSentinel + "\nfooter\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: pane, paneB: pane, cursorX: 2, cursorY: 1, inputContent: ""},
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	pings := 0
	_, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             time.Second,
		Ping:                func() { pings++ },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pings == 0 {
		t.Errorf("Ping never invoked; expected at least once per iteration")
	}
}

// TestExtractInputContent_SentinelRowFound pins the parse: when the
// pane has a row starting with PromptSentinel, the content past it
// (trimmed of trailing spaces) is returned.
func TestExtractInputContent_SentinelRowFound(t *testing.T) {
	pane := "ctx\n" + PromptSentinel + "Hello there   \n" +
		strings.Repeat("─", 80) + "\n  ⏵⏵ chrome\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: pane, paneB: pane, cursorX: 0, cursorY: 0, inputContent: "Hello there"},
	})
	// extractInputContent uses tmuxRun → install the fake then call
	// directly (not through the gate so we bypass the 3-call cycle).
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(pane), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })
	_ = runner

	got, err := extractInputContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hello there" {
		t.Errorf("extracted = %q, want %q", got, "Hello there")
	}
}

// TestExtractInputContent_NoSentinelRow pins the empty-return case:
// when no row starts with PromptSentinel (e.g., chamber mid-spinner,
// popup overlay), the function returns "" with nil error rather than
// erroring.
func TestExtractInputContent_NoSentinelRow(t *testing.T) {
	pane := "ctx\n  spinner...\nfooter\n"
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(pane), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })
	got, err := extractInputContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("extracted = %q, want empty (no sentinel row)", got)
	}
}

// TestExtractInputContent_MultilineDraftCapturedToStatusBoundary pins
// the #96 fix: a multi-line operator draft (sentinel row + continuation
// rows) is captured in full, not truncated at the first sentinel-row.
// The walk stops at the below-input separator (20+ ─ chars) so the
// status line isn't slurped into the archived draft.
//
// Pre-#96, this test would have returned just "first line of draft"
// (the legacy single-row extraction); the silent-truncation gap was
// the (c) flush correctness bug the multi-line capture issue tracked.
func TestExtractInputContent_MultilineDraftCapturedToStatusBoundary(t *testing.T) {
	pane := "history line\n" +
		"───────────────────────── Chamber ──\n" +
		PromptSentinel + "first line of draft\n" +
		"  second line of draft\n" +
		"  third line of draft\n" +
		strings.Repeat("─", 80) + "\n" +
		"  ⏵⏵ bypass permissions on (shift+tab to cycle)\n"
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(pane), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	got, err := extractInputContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "first line of draft\n  second line of draft\n  third line of draft"
	if got != want {
		t.Errorf("multi-line extract = %q,\nwant %q", got, want)
	}
}

// TestExtractInputContent_StopsAtBelowInputSeparator pins the
// separator-recognizer half of the walk: a multi-line draft followed
// by the below-input ─-separator is correctly bounded — the separator
// row is NOT included in the captured content.
func TestExtractInputContent_StopsAtBelowInputSeparator(t *testing.T) {
	pane := PromptSentinel + "draft line 1\n" +
		"  draft line 2\n" +
		strings.Repeat("─", 80) + "\n" +
		"more chrome below\n"
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(pane), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	got, err := extractInputContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "draft line 1\n  draft line 2"
	if got != want {
		t.Errorf("extract = %q, want %q (separator row must not be included)", got, want)
	}
}

// TestExtractInputContent_StopsAtStatusLine pins the status-line
// recognizer: a sentinel row immediately followed by the ⏵⏵ status
// line (no below-input separator visible, e.g., narrow pane or
// scroll position) still bounds the input correctly.
func TestExtractInputContent_StopsAtStatusLine(t *testing.T) {
	pane := PromptSentinel + "single line draft\n" +
		"  ⏵⏵ bypass permissions on\n"
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte(pane), nil
		}
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	got, err := extractInputContent(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "single line draft" {
		t.Errorf("extract = %q, want %q", got, "single line draft")
	}
}

// TestIsInputAreaBoundary_RecognizerCases unit-tests the boundary
// detector. The walk's correctness depends on these recognitions, so
// pin them at the function-level so a future refactor that breaks the
// recognizer is caught even if the walk itself is fine.
func TestIsInputAreaBoundary_RecognizerCases(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"empty", "", false},
		{"plain text", "hello world", false},
		{"single dash", "─", false},
		{"twenty dashes (threshold)", strings.Repeat("─", 20), true},
		{"long separator", strings.Repeat("─", 80), true},
		{"separator with leading whitespace", "  " + strings.Repeat("─", 80), true},
		{"status line", "  ⏵⏵ bypass permissions on (shift+tab to cycle)", true},
		{"status line at line start", "⏵⏵ stuff", true},
		{"short run of dashes plus text", "── Title ──", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInputAreaBoundary(c.line); got != c.want {
				t.Errorf("isInputAreaBoundary(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

// TestSendCtrlU_SendsCorrectKeystroke pins the tmux call shape: a
// single send-keys with "C-u" target and no Enter follow-up.
func TestSendCtrlU_SendsCorrectKeystroke(t *testing.T) {
	var calls []string
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := SendCtrlU(context.Background(), "%5"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tmux call count = %d, want 1 (single send-keys C-u)", len(calls))
	}
	if !strings.Contains(calls[0], "send-keys") || !strings.Contains(calls[0], "C-u") {
		t.Errorf("expected send-keys with C-u; got %q", calls[0])
	}
	if strings.Contains(calls[0], "Enter") {
		t.Errorf("send-keys should NOT include Enter; got %q", calls[0])
	}
}

// TestSendCtrlU_PaneRequired pins input validation symmetric with the
// rest of the package.
func TestSendCtrlU_PaneRequired(t *testing.T) {
	if err := SendCtrlU(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty pane")
	}
}

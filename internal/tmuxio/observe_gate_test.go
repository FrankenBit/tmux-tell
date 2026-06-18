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

// observeGateRunner is a per-test fake that drives AgentState's
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
	// inCopyMode is what the #526 precedence-0 `#{pane_in_mode}` query
	// returns for this step: true → "1" (AgentState returns StateInCopyMode
	// and SKIPS the captures), false → "0" (normal capture-based path).
	inCopyMode bool
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
		// AgentState's two captures, then extractInputContent's one.
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
		// #526: distinguish the precedence-0 pane_in_mode query from the
		// cursor query by the format arg.
		if strings.Contains(args[len(args)-1], "pane_in_mode") {
			if step.inCopyMode {
				// AgentState returns StateInCopyMode on this call and skips
				// the captures, so advance the step HERE (the capture-driven
				// advance in the capture-pane case won't run this iteration).
				if len(r.steps) > 1 {
					r.steps = r.steps[1:]
				}
				r.cursor = 0
				return []byte("1\n"), nil
			}
			return []byte("0\n"), nil
		}
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

// fastObserveDelta installs a microsecond temporal-delta on AgentState
// for the duration of the test. Without this, each iteration sleeps
// 200ms for the cursor-aware classification's stability check, and the
// observe-gate's loop would run real-time over tests.
func fastObserveDelta(t *testing.T) {
	t.Helper()
	prev := SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { SetAgentStateTemporalDeltaForTest(prev) })
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

// TestObserveGate_CopyModePersistReverts pins the #526 amended-D4 core:
// when the pane stays in copy-mode past MaxWait, the gate returns
// ErrCopyModeUnsafe WITHOUT Stale=true — so the caller reverts to queued
// rather than delivering into a scrolled pane (which would reproduce 83b3).
// CopyModeWait is populated for the metric.
func TestObserveGate_CopyModePersistReverts(t *testing.T) {
	fastObserveDelta(t)
	runner := newObserveGateRunner([]observeStep{
		{inCopyMode: true}, // single step persists (runner keeps the last step)
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             20 * time.Millisecond,
	})
	if !errors.Is(err, ErrCopyModeUnsafe) {
		t.Fatalf("err = %v, want ErrCopyModeUnsafe", err)
	}
	if outcome.State != StateInCopyMode {
		t.Errorf("State = %v, want StateInCopyMode", outcome.State)
	}
	if outcome.Stale {
		t.Error("Stale = true, want false — copy-mode must NOT trigger deliver-anyway (would paste into a scrolled pane)")
	}
	if outcome.CopyModeWait <= 0 {
		t.Errorf("CopyModeWait = %v, want > 0 (the metric needs the hold duration)", outcome.CopyModeWait)
	}
}

// TestObserveGate_CopyModeExitDelivers pins the transition AC: copy-mode on
// the first poll, then the operator exits to a live idle prompt → the gate
// delivers (StateIdle, not Stale) and reports CopyModeWait > 0. This is the
// "fires automatically on return-to-live" behavior, for free via the poll.
func TestObserveGate_CopyModeExitDelivers(t *testing.T) {
	fastObserveDelta(t)
	idle := "history\n" + PromptSentinel + "\nfooter\n"
	runner := newObserveGateRunner([]observeStep{
		{inCopyMode: true}, // scrolled up
		{paneA: idle, paneB: idle, cursorX: 2, cursorY: 1}, // exited → idle
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.State != StateIdle {
		t.Errorf("State = %v, want StateIdle after copy-mode exit", outcome.State)
	}
	if outcome.Stale {
		t.Error("Stale = true, want false — clean delivery on return-to-live")
	}
	if outcome.CopyModeWait <= 0 {
		t.Errorf("CopyModeWait = %v, want > 0 (waited on copy-mode before delivering)", outcome.CopyModeWait)
	}
	if outcome.Iterations < 2 {
		t.Errorf("Iterations = %d, want >= 2 (copy-mode poll then idle poll)", outcome.Iterations)
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
	// AgentState classifies as StateAwaitingOperator (cursor past
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
// when no row starts with PromptSentinel (e.g., agent mid-spinner,
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
		"───────────────────────── Agent ──\n" +
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

// TestObserveGate_OnOperatorTyping_FiresOncePerDeliveryCycle pins #95's
// load-bearing property: the visibility-notification callback fires
// EXACTLY ONCE the first iteration the gate observes
// StateAwaitingOperator. Subsequent iterations in the same cycle skip
// the re-fire so the operator's input row doesn't accumulate emoji.
//
// Mechanism: drive a gate over a draft that grows iteration to
// iteration (StateAwaitingOperator throughout); count callback fires.
// Green-state count = 1.
func TestObserveGate_OnOperatorTyping_FiresOncePerDeliveryCycle(t *testing.T) {
	fastObserveDelta(t)
	// Three iterations of growing draft → StateAwaitingOperator each
	// time, callback should fire on iteration 1 and skip 2+3.
	steps := []observeStep{
		{paneA: "h", paneB: "h", cursorX: 5, cursorY: 1, inputContent: "Hel"},
		{paneA: "h", paneB: "h", cursorX: 7, cursorY: 1, inputContent: "Hello"},
		{paneA: "h", paneB: "h", cursorX: 9, cursorY: 1, inputContent: "Hello t"},
	}
	// Make panes structurally StateAwaitingOperator by including the
	// sentinel row with content past it + cursor past sentinel.
	for i, draft := range []string{"Hel", "Hello", "Hello t"} {
		pane := "history\n" + PromptSentinel + draft + "\nfooter\n"
		steps[i].paneA = pane
		steps[i].paneB = pane
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	notifyCount := 0
	_, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		BackoffFactor:       1.0,
		InputStaleThreshold: time.Hour, // never fires within test budget
		MaxWait:             50 * time.Millisecond,
		OnOperatorTyping:    func() { notifyCount++ },
	})
	if err == nil {
		// Expected ErrMaxWaitExceeded since stale threshold never hits.
		t.Fatalf("expected ErrMaxWaitExceeded, got nil")
	}
	if notifyCount != 1 {
		t.Errorf("OnOperatorTyping fired %d times, want exactly 1 (one-shot per delivery cycle)", notifyCount)
	}
}

// TestObserveGate_OnOperatorTyping_NotFiredOnIdleFastPath pins that
// the notification is suppressed when the gate's fast-path-idle
// branch fires — no point notifying the operator that mail is coming
// when it's about to land cleanly anyway.
func TestObserveGate_OnOperatorTyping_NotFiredOnIdleFastPath(t *testing.T) {
	fastObserveDelta(t)
	pane := "history\n" + PromptSentinel + "\nfooter\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: pane, paneB: pane, cursorX: 2, cursorY: 1, inputContent: ""},
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	notifyCount := 0
	_, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             time.Second,
		OnOperatorTyping:    func() { notifyCount++ },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if notifyCount != 0 {
		t.Errorf("OnOperatorTyping fired %d times on idle fast-path, want 0", notifyCount)
	}
}

// TestObserveGate_OnOperatorTyping_NilCallbackSafe pins that a nil
// callback is a valid no-op — used by callers (or tests) that don't
// want the notification.
func TestObserveGate_OnOperatorTyping_NilCallbackSafe(t *testing.T) {
	fastObserveDelta(t)
	pane := "history\n" + PromptSentinel + "drafting...\nfooter\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: pane, paneB: pane, cursorX: 14, cursorY: 1, inputContent: "drafting..."},
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Microsecond, // fires immediately so we return cleanly
		MaxWait:             time.Second,
		// OnOperatorTyping intentionally nil
	})
	if err != nil {
		t.Fatalf("unexpected error with nil callback: %v", err)
	}
}

// TestNotifyPendingMessage_SendsCorrectKeystroke pins the tmux call
// shape: a single send-keys -l with PendingMessageMarker as the
// literal payload. No Enter follow-up — the 📫 rides along in the
// operator's input row.
func TestNotifyPendingMessage_SendsCorrectKeystroke(t *testing.T) {
	var calls []string
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := NotifyPendingMessage(context.Background(), "%5"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tmux call count = %d, want 1 (single send-keys)", len(calls))
	}
	if !strings.Contains(calls[0], "send-keys") {
		t.Errorf("expected send-keys; got %q", calls[0])
	}
	if !strings.Contains(calls[0], "-l") {
		t.Errorf("expected -l (literal) flag; got %q", calls[0])
	}
	if !strings.Contains(calls[0], PendingMessageMarker) {
		t.Errorf("expected PendingMessageMarker %q in call; got %q", PendingMessageMarker, calls[0])
	}
	if strings.Contains(calls[0], "Enter") {
		t.Errorf("send-keys should NOT include Enter (📫 rides along); got %q", calls[0])
	}
}

// TestNotifyPendingMessage_PaneRequired pins input validation
// symmetric with the rest of the package.
func TestNotifyPendingMessage_PaneRequired(t *testing.T) {
	if err := NotifyPendingMessage(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty pane")
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

// TestClearInput_TwoPressesPerLine pins the #360 P3 fix: codex clears a
// multi-line draft in ~2 Ctrl+U presses per line (text-clear + line-join), so
// ClearInput sends clearPressesPerLine*lineCount presses (the prior one-per-
// line under-cleared codex and left residual lines for the paste to compound
// with — operator-substrate-witnessed in the #336 gate). Each press is a
// send-keys C-u with no Enter follow-up.
func TestClearInput_TwoPressesPerLine(t *testing.T) {
	var calls []string
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := ClearInput(context.Background(), "%5", 3); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := 3 * clearPressesPerLine
	if len(calls) != want {
		t.Fatalf("tmux call count = %d, want %d (clearPressesPerLine*lineCount)", len(calls), want)
	}
	for i, c := range calls {
		if !strings.Contains(c, "send-keys") || !strings.Contains(c, "C-u") {
			t.Errorf("call %d: expected send-keys C-u; got %q", i, c)
		}
		if strings.Contains(c, "Enter") {
			t.Errorf("call %d: send-keys should NOT include Enter; got %q", i, c)
		}
	}
}

// TestClearInput_MinimumOneLine pins the lineCount<1 clamp: a zero or negative
// count still clears one line's worth of presses (clearPressesPerLine), so a
// caller that mis-derives the count never sends zero presses.
func TestClearInput_MinimumOneLine(t *testing.T) {
	var calls int
	prev := SetTmuxRunner(func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls++
		return nil, nil
	})
	t.Cleanup(func() { SetTmuxRunner(prev) })

	if err := ClearInput(context.Background(), "%5", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != clearPressesPerLine {
		t.Fatalf("tmux call count = %d, want %d (lineCount<1 clamps to one line)", calls, clearPressesPerLine)
	}
}

// TestClearInput_PaneRequired pins input validation symmetric with the rest
// of the package.
func TestClearInput_PaneRequired(t *testing.T) {
	if err := ClearInput(context.Background(), "", 2); err == nil {
		t.Fatal("expected error for empty pane")
	}
}

// TestObserveGate_WorkingDeliverImmediately_FastPath pins the #106
// opt-in: when WorkingDeliverImmediately is true and the first poll
// classifies StateWorking (pane content changed across the temporal-
// delta window), the gate returns immediately rather than entering
// the backoff loop. Iterations==1 + State==StateWorking + Stale==false
// distinguishes the opt-in fast-path from the existing safer-default
// behavior.
func TestObserveGate_WorkingDeliverImmediately_FastPath(t *testing.T) {
	fastObserveDelta(t)
	// paneA != paneB triggers StateWorking via the stability check.
	paneA := "● Streaming response line 1\n  status\n"
	paneB := "● Streaming response line 2\n  status\n"
	runner := newObserveGateRunner([]observeStep{
		{paneA: paneA, paneB: paneB, cursorX: 2, cursorY: 1, inputContent: ""},
	})
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:           time.Microsecond,
		PollIntervalMax:           time.Microsecond,
		InputStaleThreshold:       time.Minute,
		MaxWait:                   time.Second,
		WorkingDeliverImmediately: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.State != StateWorking {
		t.Errorf("State = %v, want StateWorking (changed pane)", outcome.State)
	}
	if outcome.Stale {
		t.Errorf("Stale = true on fast-path; expected false (no draft to archive)")
	}
	if outcome.Iterations != 1 {
		t.Errorf("Iterations = %d, want 1 (fast-path on first poll)", outcome.Iterations)
	}
	if !strings.Contains(outcome.Reason, "immediate delivery") {
		t.Errorf("Reason = %q, want it to mention opt-in immediate delivery", outcome.Reason)
	}
}

// TestObserveGate_WorkingDeliverImmediately_Off_DefaultBackoff pins
// the default-off behavior: same StateWorking capture without the
// opt-in flag falls through to the safer-default wait, hits MaxWait,
// and surfaces ErrMaxWaitExceeded. This is the v0.3.0-through-v0.6.0
// contract that #106 doesn't change unless explicitly opted in.
//
// Note on the lastState assertion: this test does NOT pin a specific
// outcome.State value. The observeGateRunner fixture cycles 3 capture
// returns per its internal cursor counter (designed around
// observe-gate's read-only-observe + extractInputContent pattern),
// while AgentState makes only 2 capture-pane calls per gate iteration.
// Across multi-iteration runs, every third iteration's first capture
// hits the cursor-position-3 (input-content) return, which feeds paneB
// into AgentState as both capA and capB → classifies as non-Working
// (Unknown via the cursor-based fallback path). Which iteration MaxWait
// happens to interrupt determines whether lastState reads StateWorking
// or StateUnknown — both indicate non-fast-path. The contract being
// pinned here is "default-off does NOT fast-path" (err = MaxWait,
// Iterations >= 2), not the exact lastState classification.
//
// If this test fails, the cause is an intentional architectural rework
// (extending the opt-in to default-on, moving the opt-in check, or
// changing the fast-path semantics) — re-assert the per-state contract
// before changing the test.
func TestObserveGate_WorkingDeliverImmediately_Off_DefaultBackoff(t *testing.T) {
	fastObserveDelta(t)
	paneA := "● Streaming response line 1\n  status\n"
	paneB := "● Streaming response line 2\n  status\n"
	// Provide enough steps so the gate cycles a few times before MaxWait.
	steps := make([]observeStep, 10)
	for i := range steps {
		steps[i] = observeStep{paneA: paneA, paneB: paneB, cursorX: 2, cursorY: 1, inputContent: ""}
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:     time.Microsecond,
		PollIntervalMax:     time.Microsecond,
		InputStaleThreshold: time.Minute,
		MaxWait:             20 * time.Millisecond,
		// WorkingDeliverImmediately: false (default — explicit for clarity)
	})
	if !errors.Is(err, ErrMaxWaitExceeded) {
		t.Errorf("err = %v, want ErrMaxWaitExceeded (gate deferred until cap)", err)
	}
	if outcome.Iterations < 2 {
		t.Errorf("Iterations = %d, want >= 2 (gate looped before MaxWait)", outcome.Iterations)
	}
	// Non-fast-path assertion: any non-Idle classification is acceptable
	// (see test-level docstring for why lastState specifically isn't
	// pinned to StateWorking).
	if outcome.State == StateIdle {
		t.Errorf("State = %v, want non-Idle (any non-fast-path classification)", outcome.State)
	}
}

// TestObserveGate_WorkingDeliverImmediately_DoesNotApplyToAwaitingOperator
// pins the per-state eligibility contract: the opt-in is StateWorking-
// only. Even with the flag true, a pane classified as StateAwaitingOperator
// (cursor past sentinel = operator drafting) must NOT fast-path; the
// existing stale-flush path takes over after InputStaleThreshold.
//
// If this test fails, the cause is an intentional architectural rework
// (extending the opt-in to additional states, merging the case
// structure, or moving the opt-in check from per-case to a single
// pre-switch branch) — re-assert the per-state contract before changing
// the test. The case-structure makes accidental regression hard;
// failure here is a structural-reshape signal, not a slip-of-the-keyboard.
func TestObserveGate_WorkingDeliverImmediately_DoesNotApplyToAwaitingOperator(t *testing.T) {
	fastObserveDelta(t)
	draft := "operator started typing"
	// Stable pane with sentinel + cursor PAST the sentinel position →
	// classifies StateAwaitingOperator, not StateWorking.
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
		PollIntervalMin:           time.Microsecond,
		PollIntervalMax:           time.Microsecond,
		BackoffFactor:             1.0,
		InputStaleThreshold:       50 * time.Millisecond,
		MaxWait:                   5 * time.Second,
		WorkingDeliverImmediately: true, // flag ON
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome.State != StateAwaitingOperator {
		t.Errorf("State = %v, want StateAwaitingOperator (cursor past sentinel)", outcome.State)
	}
	// Operator-draft path: the gate should reach stale-flush
	// (Stale=true), NOT fast-path with reason mentioning "immediate".
	if strings.Contains(outcome.Reason, "immediate delivery") {
		t.Errorf("Reason = %q, want stale-flush reason (opt-in must NOT apply to AwaitingOperator)", outcome.Reason)
	}
	if outcome.Iterations < 2 {
		t.Errorf("Iterations = %d, want >= 2 (gate looped through stale threshold, not fast-path)", outcome.Iterations)
	}
}

// TestObserveGate_WorkingDeliverImmediately_DoesNotApplyToUnknown pins
// the second per-state eligibility contract: StateUnknown (no sentinel,
// no marker, stable pane = unrecognized UI) must NOT fast-path regardless
// of the opt-in. This is the discipline that #105 surfaced — fast-paste
// into an unrecognized state is the destructive case (popup-as-Unknown
// failure mode where the operator's draft gets paste-interpreted as
// keystrokes).
//
// If this test fails, the cause is an intentional architectural rework
// (extending the opt-in to Unknown, merging the case structure, or
// changing how StateUnknown is classified) — re-assert the per-state
// contract before changing the test. Like the AwaitingOperator pin,
// failure here is a structural-reshape signal, not a regression.
func TestObserveGate_WorkingDeliverImmediately_DoesNotApplyToUnknown(t *testing.T) {
	fastObserveDelta(t)
	// Stable pane with no sentinel + no marker → StateUnknown.
	pane := "● Some response\n  ⎿ Tool output\n  status\n"
	steps := make([]observeStep, 5)
	for i := range steps {
		steps[i] = observeStep{
			paneA:        pane,
			paneB:        pane,
			cursorX:      5,
			cursorY:      0,
			inputContent: "",
		}
	}
	runner := newObserveGateRunner(steps)
	prev := SetTmuxRunner(runner.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	outcome, err := ObserveGate(context.Background(), "%5", ObserveGateOpts{
		PollIntervalMin:           time.Microsecond,
		PollIntervalMax:           time.Microsecond,
		InputStaleThreshold:       time.Minute,
		MaxWait:                   20 * time.Millisecond,
		WorkingDeliverImmediately: true, // flag ON
	})
	if !errors.Is(err, ErrMaxWaitExceeded) {
		t.Errorf("err = %v, want ErrMaxWaitExceeded (opt-in must NOT apply to Unknown — gate stays deferred until cap)", err)
	}
	if outcome.State != StateUnknown {
		t.Errorf("State = %v, want StateUnknown (no sentinel, no marker)", outcome.State)
	}
	if strings.Contains(outcome.Reason, "immediate delivery") {
		t.Errorf("Reason = %q, want MaxWait reason (opt-in must NOT apply to Unknown)", outcome.Reason)
	}
}

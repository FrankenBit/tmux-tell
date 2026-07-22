package tmuxio

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbeRunner is a scripted tmux runner. Captures are consumed in
// order as the loop progresses. The `calls` slice records every tmux
// invocation; `TestAgentState_NoPaneMutation` asserts every
// recorded call is capture-pane or display-message (no send-keys),
// which catches any future change that reintroduces pane mutation
// against the recipient.
type fakeProbeRunner struct {
	mu         sync.Mutex
	captures   []string
	captureIdx int
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
	}
	return nil, nil
}

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
		{StateInCopyMode, "copy-mode"},
		{StateRateLimited, "rate-limited"},
		{StateUsageLimited, "usage-limited"},
		{State(99), "unknown"}, // out-of-range defaults to "unknown" (safer)
	}
	for _, c := range cases {
		if got := c.s.String(); got != c.want {
			t.Errorf("State(%d).String() = %q, want %q", int(c.s), got, c.want)
		}
	}
}

func TestAgentState_RateLimitedPatternWinsOverWorking(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, PaneProfile{
		PromptSentinel:   PromptSentinel,
		RateLimitPattern: `SYNTHETIC RATE LIMIT(?:.*?retry\s+after\s+(?P<retry_seconds>\d+(?:\.\d+)?s))?`,
	})
	paneA := "history\nSYNTHETIC RATE LIMIT retry after 10s\n❯\u00a0\n"
	paneB := "history\nSYNTHETIC RATE LIMIT retry after 9s\n❯\u00a0\n"
	fr := newFakeProbeRunner([]string{paneA, paneB})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateRateLimited {
		t.Errorf("state = %v, want StateRateLimited (pattern must beat working animation)", state)
	}
	if !strings.Contains(ev.Marker, "SYNTHETIC RATE LIMIT") {
		t.Errorf("Evidence.Marker = %q, want synthetic rate-limit text", ev.Marker)
	}
	if ev.RetryAfter != 9*time.Second {
		t.Errorf("Evidence.RetryAfter = %s, want 9s", ev.RetryAfter)
	}
}

func TestAgentState_RateLimitPatternDisabledWhenProfileEmpty(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, PaneProfile{PromptSentinel: PromptSentinel})
	pane := "history\nSYNTHETIC RATE LIMIT\n❯\u00a0\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateRateLimited {
		t.Fatal("state = StateRateLimited with empty profile pattern — production literals must remain sample-gated")
	}
}

func TestAgentState_UsageLimitedPatternWinsOverWorking(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, PaneProfile{
		PromptSentinel:    PromptSentinel,
		UsageLimitPattern: `■ You've hit your usage limit(?:.*?try again at (?P<retry_at>.+))?`,
	})
	paneA := "history\n■ You've hit your usage limit. Try again at 3:59 PM.\n❯\u00a0\n"
	paneB := "history\n■ You've hit your usage limit. Try again at 4:00 PM.\n❯\u00a0\n"
	fr := newFakeProbeRunner([]string{paneA, paneB})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateUsageLimited {
		t.Errorf("state = %v, want StateUsageLimited (pattern must beat working animation)", state)
	}
	if !strings.Contains(ev.Marker, "You've hit your usage limit") {
		t.Errorf("Evidence.Marker = %q, want usage-limit text", ev.Marker)
	}
}

func TestAgentState_UsageLimitPatternDisabledWhenProfileEmpty(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, PaneProfile{PromptSentinel: PromptSentinel})
	pane := "history\n■ You've hit your usage limit.\n❯\u00a0\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateUsageLimited {
		t.Fatal("state = StateUsageLimited with empty profile pattern — production literals must remain sample-gated")
	}
}

// TestCutPromptSentinel pins the #690 strip-tolerant sentinel match: a
// regular-space-terminated sentinel (Codex `› `) matches its right-trimmed
// captured form (bare `›`, capture-pane strips the trailing space when the
// composer is empty), while a normal `› <ghost>` row still yields the ghost as
// rest. Claude's NBSP sentinel is space-scoped-inert (NBSP isn't stripped, so a
// bare `❯` must NOT match — the tolerance must not widen Claude's matching).
func TestCutPromptSentinel(t *testing.T) {
	const codex = "› " // U+203A + regular 0x20 space
	for _, c := range []struct {
		row, wantRest string
		wantFound     bool
	}{
		{"› Explain this codebase", "Explain this codebase", true}, // normal ghost-text
		{"› ", "", true},   // sentinel with its space intact (capture kept it)
		{"›", "", true},    // #690: empty composer, trailing space stripped → bare glyph
		{"  ›", "", false}, // indented bare glyph: neither a prefix nor == trimmed sentinel
		{"no sentinel here", "", false},
	} {
		rest, found := cutPromptSentinel(c.row, codex)
		if found != c.wantFound || rest != c.wantRest {
			t.Errorf("cutPromptSentinel(%q, codex) = (%q,%v); want (%q,%v)", c.row, rest, found, c.wantRest, c.wantFound)
		}
	}

	const claude = "❯ " // U+276F + NBSP (U+00A0) — capture-pane does NOT strip NBSP
	if rest, found := cutPromptSentinel("❯", claude); found {
		t.Errorf("cutPromptSentinel(bare ❯, claude) = (%q,%v); want not-found (tolerance is space-scoped; NBSP unaffected)", rest, found)
	}
	if rest, found := cutPromptSentinel("❯ draft", claude); !found || rest != "draft" {
		t.Errorf("cutPromptSentinel(claude sentinel + draft) = (%q,%v); want (\"draft\",true)", rest, found)
	}
}

// --- AgentState integration tests ---

// fastTemporalDelta installs a microsecond temporal-delta so tests
// don't pay the 200ms production wait. Cleanup restores production.
func fastTemporalDelta(t *testing.T) {
	t.Helper()
	prev := SetAgentStateTemporalDeltaForTest(time.Microsecond)
	t.Cleanup(func() { SetAgentStateTemporalDeltaForTest(prev) })
}

// TestAgentState_IdleWhenSentinelEmpty pins the happy-path Idle
// classification: pane is stable across the temporal-delta window
// AND shows the PromptSentinel with no content past it. Reuses the
// classifyInputRow helper from PR #66's substrate.
func TestAgentState_IdleWhenSentinelEmpty(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history line A\nhistory line B\n──── Agent ──\n❯\u00a0\n────────\n  status\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_WorkingWhenPaneChanges pins the Working
// classification: the two captures differ → agent is painting →
// working. ChangedLineCount is populated in Evidence for observability.
func TestAgentState_WorkingWhenPaneChanges(t *testing.T) {
	fastTemporalDelta(t)
	paneA := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 5s\n❯\u00a0\n  status\n"
	paneB := "history\n● Bash(slow command)\n  ⎿ Running…\n✻ Crunched for 6s\n❯\u00a0\n  status\n"
	fr := newFakeProbeRunner([]string{paneA, paneB})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_UnknownWhenStableNonPromptUI pins the safer-default
// branch: pane is stable across the temporal-delta window but neither
// shows the PromptSentinel nor matches any marker. The agent is in
// some non-recognized UI state and the heuristic refuses to silently
// roll up to a known classification.
func TestAgentState_UnknownWhenStableNonPromptUI(t *testing.T) {
	fastTemporalDelta(t)
	// Pane shows streaming output with no `❯\u00a0` row in view + no marker.
	pane := "● Some response line\n  ⎿  Tool output line 1\n  ⎿  Tool output line 2\n  status line\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_PaneRequired pins the input-validation guard: empty
// pane returns an error and StateUnknown without firing any tmux
// calls. Mirrors the InputRowHasContent / QuickPresenceProbe
// validation discipline.
func TestAgentState_PaneRequired(t *testing.T) {
	fr := newFakeProbeRunner([]string{"ctx\n"})
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "")
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

// TestAgentState_NoPaneMutation pins the substrate-class property:
// AgentState is read-only-observe. A successful classification makes
// EXACTLY 2 capture-pane calls + 1 display-message call (the cursor
// query added in #69's v2 algorithm per operator's
// design call 2026-06-04) and ZERO send-keys. All three calls are
// read-only at the tmux layer — capture-pane reads the visible
// buffer; display-message reads internal pane state. This is the
// load-bearing claim that AgentState honors the "knock at the door
// without waking" framing from #69; the v2 substrate-class extension
// from PR #75's 2-call shape preserves the no-mutation property
// while gaining cursor-position awareness.
func TestAgentState_NoPaneMutation(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Agent ──\n❯\u00a0\n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 5) // cursor at sentinel position
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fr.calls) != 4 {
		t.Errorf("tmux call count = %d, want 4 (read-only-observe = 1 pane_in_mode + 2 capture-pane + 1 cursor display-message)", len(fr.calls))
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
	if displayMessageCount != 2 {
		t.Errorf("display-message count = %d, want 2 (pane_in_mode + cursor)", displayMessageCount)
	}
}

// TestAgentState_CopyModeDetectedSkipsCaptures pins the #526 load-bearing
// property: when pane_in_mode=1 (operator scrolled up), AgentState returns
// StateInCopyMode at precedence 0 and SKIPS the capture-pane snapshots — they
// would read the historical scrolled view and could misclassify as Idle (the
// 83b3 bug). Exactly one tmux call (the pane_in_mode query), zero capture-pane.
func TestAgentState_CopyModeDetectedSkipsCaptures(t *testing.T) {
	fastTemporalDelta(t)
	// A scrolled view that, if captured, shows an old `❯ ` prompt at top —
	// the exact shape that would misclassify as Idle without precedence-0.
	scrolled := "❯ \nhistory line\nmore history\n"
	fr := newAgentStateRunner([]string{scrolled, scrolled}, 2, 0)
	fr.inCopyMode = true
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateInCopyMode {
		t.Errorf("state = %v, want StateInCopyMode (pane_in_mode=1)", state)
	}
	if !strings.Contains(ev.Reason, "copy-mode") {
		t.Errorf("Evidence.Reason = %q, want it to mention copy-mode", ev.Reason)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("tmux call count = %d, want 1 (only the pane_in_mode query; captures skipped)", len(fr.calls))
	}
	for _, call := range fr.calls {
		if strings.HasPrefix(call, "capture-pane") {
			t.Errorf("capture-pane was called in copy-mode (%q) — MUST be skipped (it reads the scrolled view → 83b3)", call)
		}
	}
}

// TestAgentState_LivePaneNotCopyMode confirms pane_in_mode=0 (live prompt)
// falls through to the normal capture-based classification — a live idle pane
// is NOT deferred as copy-mode.
func TestAgentState_LivePaneNotCopyMode(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Agent ──\n❯ \n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2) // cursor at sentinel
	fr.inCopyMode = false
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateInCopyMode {
		t.Fatal("state = StateInCopyMode for a live pane (pane_in_mode=0) — false defer")
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (live, stable, cursor at sentinel)", state)
	}
}

// TestAgentState_CopyModeQueryError_DegradesAndFlags pins the #537 signal: a
// pane_in_mode query *error* must NOT abort AgentState — it degrades to the
// capture-based classifier (here a clean live idle pane) AND stamps
// Evidence.CopyModeQueryFailed so the gate loop can count consecutive failures.
// The classification is unchanged (StateIdle); only the flag rides along.
func TestAgentState_CopyModeQueryError_DegradesAndFlags(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n" + PromptSentinel + "\nfooter\n"   // sentinel row, empty past it → Idle
	fr := newAgentStateRunner([]string{pane, pane}, 2, 1) // cursor at sentinel position
	fr.copyModeQueryErr = true
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v — a pane_in_mode query error must degrade, not abort", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (degraded to the capture classifier)", state)
	}
	if !ev.CopyModeQueryFailed {
		t.Error("Evidence.CopyModeQueryFailed = false, want true (the gate needs this signal to bias a persistent failure toward defer)")
	}
}

// TestAgentState_CopyModeQueryOK_NoFlag is the negative companion: when the
// pane_in_mode query succeeds, CopyModeQueryFailed stays false so a single
// clean read resets the gate's consecutive-failure run.
func TestAgentState_CopyModeQueryOK_NoFlag(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n" + PromptSentinel + "\nfooter\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 1)
	fr.inCopyMode = false // query succeeds, pane_in_mode=0
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	_, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ev.CopyModeQueryFailed {
		t.Error("Evidence.CopyModeQueryFailed = true on a successful query, want false")
	}
}

// TestPaneInCopyMode_Parsing pins the helper's read of `#{pane_in_mode}`:
// "1" → true (in a mode), "0"/anything-else → false, runner error → error.
func TestPaneInCopyMode_Parsing(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"1\n", true},
		{"1", true},
		{"0\n", false},
		{"0", false},
		{"", false},
	}
	for _, c := range cases {
		fr := func(_ context.Context, _ io.Reader, _ ...string) ([]byte, error) {
			return []byte(c.out), nil
		}
		prev := SetTmuxRunner(fr)
		got, err := PaneInCopyMode(context.Background(), "%5")
		SetTmuxRunner(prev)
		if err != nil {
			t.Errorf("PaneInCopyMode(out=%q) unexpected error: %v", c.out, err)
		}
		if got != c.want {
			t.Errorf("PaneInCopyMode(out=%q) = %v, want %v", c.out, got, c.want)
		}
	}
}

// agentStateRunner extends fakeProbeRunner with cursor-position
// responses for the display-message call AgentState makes in v2.
// Returns capture-pane content from the captures slice + cursor
// position as "X/Y" for display-message.
type agentStateRunner struct {
	*fakeProbeRunner
	cursorX int
	cursorY int
	// inCopyMode is what the #526 precedence-0 `#{pane_in_mode}` query
	// returns: false → "0" (live prompt, the default for all pre-#526
	// tests), true → "1" (scrolled into copy-mode).
	inCopyMode bool
	// copyModeQueryErr makes the precedence-0 pane_in_mode query return an
	// error (#537), so AgentState degrades to the capture path and stamps
	// Evidence.CopyModeQueryFailed. The cursor query still succeeds.
	copyModeQueryErr bool
}

func newAgentStateRunner(captures []string, cursorX, cursorY int) *agentStateRunner {
	return &agentStateRunner{
		fakeProbeRunner: newFakeProbeRunner(captures),
		cursorX:         cursorX,
		cursorY:         cursorY,
	}
}

func (c *agentStateRunner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	// Intercept display-message; delegate everything else to the
	// underlying fakeProbeRunner.
	c.mu.Lock()
	c.calls = append(c.calls, strings.Join(args, " "))
	c.mu.Unlock()
	if args[0] == "display-message" {
		// #526: AgentState makes TWO display-message calls — the
		// precedence-0 pane_in_mode query (before the captures) and the
		// cursor query. Distinguish them by the format arg.
		if strings.Contains(args[len(args)-1], "pane_in_mode") {
			if c.copyModeQueryErr {
				return []byte("display-message error"), fmt.Errorf("tmuxio-test: pane_in_mode boom")
			}
			if c.inCopyMode {
				return []byte("1\n"), nil
			}
			return []byte("0\n"), nil
		}
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

// TestAgentState_IdleWhenCursorAtSentinelEmpty pins the cursor-aware
// happy path for the clean-prompt case: cursor at the position right
// after `❯\u00a0` AND empty content past it → StateIdle with
// Evidence.PromptEmpty=true. v2 algorithm per #69
// operator's design call 2026-06-04.
func TestAgentState_IdleWhenCursorAtSentinelEmpty(t *testing.T) {
	fastTemporalDelta(t)
	// Cursor row (index 3, 0-indexed) is `❯\u00a0` with no content past it.
	pane := "history\n──── Agent ──\n  recap line\n❯\u00a0\n────────\n  status\n"
	// cursorX=2 (right after "❯\u00a0"); cursorY=3 (the ❯\u00a0row)
	fr := newAgentStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_IdleWhenCursorAtSentinelWithAutoSuggestion pins the
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
func TestAgentState_IdleWhenCursorAtSentinelWithAutoSuggestion(t *testing.T) {
	fastTemporalDelta(t)
	// Row 3 (0-indexed): `❯\u00a0/nimbus-board` — Claude Code auto-suggested.
	pane := "history\n──── Agent ──\n  recap line\n❯\u00a0/nimbus-board\n────────\n  status\n"
	// cursorX=2 (right after "❯\u00a0", before "/"); cursorY=3 (the ❯\u00a0row).
	// Operator has NOT engaged — cursor would be past the suggestion if they had.
	fr := newAgentStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_AwaitingOperatorWhenCursorPastSentinel pins the
// operator-mid-typing case: cursor is past the sentinel position,
// meaning content past `❯\u00a0` is operator-typed (not ghost-text).
// Agent is blocked on operator finishing the draft → return
// StateAwaitingOperator so consumers (Bosun) gate their dispatch.
func TestAgentState_AwaitingOperatorWhenCursorPastSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Row 3 (0-indexed): `❯\u00a0Thank you for handling this and ` (#63 reproduction shape).
	pane := "history\n──── Agent ──\n  recap line\n❯\u00a0Thank you for handling this and \n────────\n  status\n"
	// cursorX=37 (past the typed content); cursorY=3.
	fr := newAgentStateRunner([]string{pane, pane}, 37, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_AwaitingOperatorWhenTypingChangesFrame is the #332
// regression pin: an operator ACTIVELY typing repaints the input row every
// keystroke, so the two temporal-delta captures DIFFER (capA != capB). The
// pre-#332 precedence returned StateWorking at P5 ("pane content changed")
// BEFORE the cursor-past-sentinel branch could fire — and StateWorking is
// paste-safe, so the mailman pasted into the half-typed draft (the
// 2026-06-12 operator-witnessed clobber on the Engineer Claude pane).
//
// With the fix, cursor-strictly-past-sentinel (operator drafting) wins over
// the frame-change classification → StateAwaitingOperator (paste-unsafe), so
// delivery is deferred. This is the case the existing
// TestAgentState_AwaitingOperatorWhenCursorPastSentinel does NOT cover: that
// one uses a STABLE frame (capA == capB), so P5 never fired and the bug was
// invisible. Here the frame CHANGES, which is what makes P5 the shadow.
func TestAgentState_AwaitingOperatorWhenTypingChangesFrame(t *testing.T) {
	fastTemporalDelta(t)
	// Operator is typing: input row (index 3) grows by one char between the
	// two captures — capA != capB, exactly as a live keystroke repaint does.
	capA := "history\n──── Agent ──\n  recap line\n❯\u00a0Thanks for handling thi\n────────\n  status\n"
	capB := "history\n──── Agent ──\n  recap line\n❯\u00a0Thanks for handling this\n────────\n  status\n"
	// cursorX=25 (past the typed content, well past sentinelCol=2); cursorY=3.
	fr := newAgentStateRunner([]string{capA, capB}, 25, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (operator typing must win over frame-change; #332)", state)
	}
	if !strings.Contains(ev.Reason, "operator mid-typing") {
		t.Errorf("Evidence.Reason should mention operator mid-typing; got %q", ev.Reason)
	}
}

// TestAgentState_WorkingWhenStreamingCursorAtSentinel is the #332 SAFETY pin
// — the measured no-regression case. Claude's streaming/spinner busy states
// change the frame (capA != capB) while keeping the cursor AT the sentinel
// column (col 2), never past it (measured live: 156/156 busy frame-changes).
// The fix hoists ONLY cursor-STRICTLY-past-sentinel above P5; a cursor at the
// sentinel on a changing frame must still classify StateWorking (paste-safe),
// NOT get mis-read as drafting or idle. If a future edit broadened the hoist
// to include the at-sentinel case, this pin turns red.
func TestAgentState_WorkingWhenStreamingCursorAtSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Streaming: the transcript region (above the input box) grows between
	// captures, but the input row stays a bare `❯ ` and the cursor is
	// parked at col 2.
	capA := "history\n──── Agent ──\n  streaming line one\n❯\u00a0\n────────\n  status\n"
	capB := "history\n──── Agent ──\n  streaming line one two\n❯\u00a0\n────────\n  status\n"
	// cursorX=2 (AT sentinelCol); cursorY=3 (the ❯ input row).
	fr := newAgentStateRunner([]string{capA, capB}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateWorking {
		t.Errorf("state = %v, want StateWorking (streaming with cursor AT sentinel must not be mis-read as drafting/idle; #332 safety)", state)
	}
}

// TestAgentState_AwaitingOperatorOnMcpPickerModal is the #719-B fixture-backed
// classification pin + mutation anchor. It feeds the REAL /mcp server-picker
// capture (frozen from Pilot's live pane %6, 2026-07-22) through the full
// classifier and asserts StateAwaitingOperator.
//
// Before the AwaitingOperatorMarker broadening, the /mcp footer
// ("↑/↓ to navigate · Enter to confirm · Esc to cancel") did NOT contain the
// old full-footer marker ("↑/↓ to navigate · Esc to cancel"), so the modal
// fell through every branch to StateUnknown — live-verified against pane %6
// (state=unknown, "prompt sentinel not found in any row + no recognized
// marker"). Unknown is paste-unsafe, so this was never a clobber; the fix is
// PRECISION — a healthy operator-interaction hold classifies as the specific
// StateAwaitingOperator instead of polluting the catch-all Unknown bucket that
// #719's freshness/alert layer reserves for genuinely-frozen panes.
//
// MUTATION ANCHOR: revert AwaitingOperatorMarker to the old full footer and
// this test fails with state=unknown — the exact live-probed pre-fix reading.
// The frame is stable (a static modal), so the marker branch is what decides
// the classification; the menu-selection row (`  ❯ bookstack …`, a REGULAR
// space after ❯, not the PromptSentinel's NBSP) is not a sentinel, so the
// cursor-at-sentinel idle branch cannot fire and short-circuit the marker.
func TestAgentState_AwaitingOperatorOnMcpPickerModal(t *testing.T) {
	fastTemporalDelta(t)
	golden, err := os.ReadFile("testdata/golden_pilot_mcp_modal_2026-07-22.txt")
	if err != nil {
		t.Fatalf("read /mcp modal golden: %v", err)
	}
	capture := string(golden)
	// Static modal: both temporal-delta captures identical (live-verified
	// stable across two capture-pane calls). Cursor at (2, 39) — the
	// highlighted `❯ bookstack` menu row, mid-pane, not the input row.
	fr := newAgentStateRunner([]string{capture, capture}, 2, 39)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%6")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (live /mcp picker modal must classify as a known operator-hold, not Unknown; #719-B)", state)
	}
	if !strings.Contains(ev.Reason, "awaiting-operator marker") {
		t.Errorf("Evidence.Reason should name the awaiting-operator marker; got %q", ev.Reason)
	}
}

// TestAgentState_FallbackWhenCursorRowNotSentinel pins the cursor-
// less fallback path: when the cursor sits on a row that doesn't start
// with `❯\u00a0` (e.g., agent is mid-spinner and cursor is on the spinner
// row), the v2 algorithm falls back to v1's classifyInputRow heuristic.
//
// Smoke evidence: Surveyor pane during PR review showed cursor at the
// title-separator row, not the ❯\u00a0input row (the agent was working).
// The fallback lets the algorithm still classify cleanly when cursor
// position doesn't help.
// --- #729: Win11 ASCII prompt-sentinel render-variant ---

// TestAgentState_IdleWhenCursorAtASCIIPromptSentinel is the #729 incident
// repro. A Claude CLI pane rendered under a Windows 11 terminal paints its
// prompt as plain ASCII `> ` (U+003E + regular space) instead of Linux's
// `❯ ` (U+276F + NBSP). Before the fix the Linux-calibrated PromptSentinel
// couldn't match, AgentState returned StateUnknown, and the pre-paste safety
// re-probe (serve.go) tripped pre_paste_safety_abort on every delivery.
//
// The empty composer's trailing space is stripped by `capture-pane -p`, so the
// captured input row is a bare `>` — this also exercises cutPromptSentinel's
// #690 space-strip tolerance on the variant. The cursor still sits at col 2
// (the terminal keeps the `> ` two-cell prompt; capture-pane stripping the text
// does not move the cursor).
func TestAgentState_IdleWhenCursorAtASCIIPromptSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Row 3 (0-indexed) is the Win11 ASCII prompt with an empty composer,
	// captured as bare `>` after the trailing-space strip.
	pane := "history\n──── Admin ──\n  recap line\n>\u00a0\n────────\n  status\n"
	// cursorX=2 (right after the `> ` two-cell prompt); cursorY=3.
	fr := newAgentStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%11")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (Win11 ASCII `> ` prompt, cursor at sentinel)", state)
	}
	if !ev.PromptEmpty {
		t.Errorf("Evidence.PromptEmpty should be true for the empty ASCII prompt")
	}
}

// TestAgentState_IdleWhenCursorAtASCIIPromptSentinelWithGhostText pins the
// Win11 auto-suggestion case: `> /compact` ghost-text with the cursor still at
// the sentinel column (col 2) — the operator has NOT engaged. StateIdle with
// PromptEmpty=false, mirroring the Linux ghost-text case.
func TestAgentState_IdleWhenCursorAtASCIIPromptSentinelWithGhostText(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Admin ──\n  recap line\n>\u00a0/compact\n────────\n  status\n"
	// cursorX=2 (right after `> `, before `/compact` ghost-text); cursorY=3.
	fr := newAgentStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%11")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (Win11 ASCII prompt + auto-suggestion ghost-text)", state)
	}
	if ev.PromptEmpty {
		t.Errorf("Evidence.PromptEmpty should be false (ghost-text present, not operator-typed)")
	}
	if !strings.Contains(ev.Reason, "auto-suggestion") {
		t.Errorf("Evidence.Reason should mention auto-suggestion; got %q", ev.Reason)
	}
}

// TestAgentState_AwaitingOperatorWhenCursorPastASCIISentinel pins that the
// variant's cursor-column comparison is width-correct: on a `> ` prompt the
// sentinel is 2 runes wide, so a cursor PAST col 2 is operator-mid-typing →
// StateAwaitingOperator (don't dispatch into a half-typed draft), exactly as
// the Linux path does.
func TestAgentState_AwaitingOperatorWhenCursorPastASCIISentinel(t *testing.T) {
	fastTemporalDelta(t)
	pane := "history\n──── Admin ──\n  recap line\n>\u00a0Thanks for handling \n────────\n  status\n"
	// cursorX=22 (past the operator-typed content); cursorY=3.
	fr := newAgentStateRunner([]string{pane, pane}, 22, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%11")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (cursor past ASCII sentinel = operator mid-typing)", state)
	}
	if !strings.Contains(ev.Reason, "operator mid-typing") {
		t.Errorf("Evidence.Reason should mention operator mid-typing; got %q", ev.Reason)
	}
}

// TestAgentState_ASCIIVariantNotHonoredCursorless is the SAFETY pin for the
// scoped design: the ASCII `> ` variant is trusted ONLY where the cursor
// anchors it to the live input row. Here a bare, empty-past `> ` row is present
// but the cursor sits OFF it (on a history row, col 0) — the cursor-aware path
// doesn't fire on the `> ` row, and the cursor-LESS fallback (isInputRowQuiet)
// keys on the PRIMARY sentinel only, so the pane must NOT classify Idle. If a
// future edit added the variant to the cursor-less scan, a stray `> ` blockquote
// or `> ` shell-continuation row would false-idle a non-input pane — this test
// reds the moment that happens.
func TestAgentState_ASCIIVariantNotHonoredCursorless(t *testing.T) {
	fastTemporalDelta(t)
	// The ONLY `> `-shaped row is an EMPTY one (row 2, captured as bare `>` after
	// the space-strip) — exactly the shape isInputRowQuiet would call "quiet" if
	// it honored the variant. The cursor is on the history row (0, col 0), which
	// has no sentinel, so the cursor-aware path can't classify it either. Correct
	// result: StateUnknown, NOT idle.
	pane := "history\n  some tool output\n>\u00a0\n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 0, 0)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%11")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateIdle {
		t.Errorf("state = StateIdle — the ASCII `> ` variant must NOT be honored in the cursor-less path (false-idle risk on a non-input `> ` row)")
	}
}

func TestAgentState_FallbackWhenCursorRowNotSentinel(t *testing.T) {
	fastTemporalDelta(t)
	// Cursor at row 1 (not the ❯\u00a0row at row 3). Pane is otherwise stable
	// + has the sentinel with empty content; fallback to v1 heuristic
	// → StateIdle.
	pane := "history\n──── Agent ──\n  recap line\n❯\u00a0\n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 0, 1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_UnknownWithAccurateReason_SentinelFoundCursorOff pins
// the accurate-reason cleanup (the "C" item from the operator's
// 2026-06-04 discussion): when the sentinel IS in the pane but the
// cursor isn't at the input row AND the cursor-less fallback didn't
// match (DeltaInputActivity on the input row), the Unknown branch
// reports "sentinel found but cursor not at input row" rather than the
// misleading v1 "no prompt sentinel" message.
func TestAgentState_UnknownWithAccurateReason_SentinelFoundCursorOff(t *testing.T) {
	fastTemporalDelta(t)
	// Pane has the sentinel but with content past it. Cursor on row 1
	// (a non-sentinel row). classifyInputRow returns DeltaInputActivity
	// (sentinel + content) → not DeltaQuiet → fallback doesn't classify
	// as Idle. Falls through to Unknown — and the reason should name
	// the actual situation, not "no prompt sentinel".
	pane := "history\n  spinner-ish content\n❯\u00a0<agent-narration>\n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 0, 1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestAgentState_ContextCancelledDuringTemporalDelta pins the
// cancellation contract: a context cancelled between the two captures
// returns StateUnknown + ctx.Err() rather than racing the second
// capture or silently waiting out the delta.
func TestAgentState_ContextCancelledDuringTemporalDelta(t *testing.T) {
	// Use the production temporal delta here so the cancellation has
	// time to fire mid-wait. (Microsecond delta would race the cancel.)
	prev := SetAgentStateTemporalDeltaForTest(100 * time.Millisecond)
	t.Cleanup(func() { SetAgentStateTemporalDeltaForTest(prev) })

	pane := "history\n❯\u00a0\n  status\n"
	fr := newFakeProbeRunner([]string{pane, pane})
	prevRunner := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prevRunner) })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	state, _, err := AgentState(ctx, "%5")
	if err == nil {
		t.Fatal("expected error from ctx cancellation, got nil")
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown when ctx cancelled mid-wait", state)
	}
	// Two calls should have happened — the #526 precedence-0 pane_in_mode
	// query, then the first capture-pane; the cancellation cuts the
	// temporal-delta short before the second capture.
	if len(fr.calls) != 2 {
		t.Errorf("tmux call count = %d, want 2 (pane_in_mode + first capture-pane; cancellation aborts before the second capture)", len(fr.calls))
	}
}

// TestCountChangedLines_DiffShape pins the helper's behavior on the
// shapes AgentState cares about: same content → 0; trailing line
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

// TestAgentState_AwaitingOperatorOnAskUserQuestionGolden pins the
// end-to-end classification for the AskUserQuestion popup scenario
// (#79). Loads the capture-derived golden fixture as
// both capture-pane responses (the pane is stable across the temporal
// delta — operator is reading the popup; nothing's animating), and
// asserts AgentState returns StateAwaitingOperator with the marker
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
// TestAgentState_AtRestInCompactionOnGolden pins the end-to-end
// classification for the /compact-in-progress scenario (tmux-msg
// #70). Loads BOTH capture-derived goldens — at 8% and 68% — and feeds
// them as capA and capB. This shape is load-bearing:
//
//   - The pane animates during compaction (spinner glyph cycles ✻↔✢,
//     percentage ticks, elapsed time changes), so capA != capB. Without
//     the CompactionMarker check at precedence 1, AgentState would
//     hit the precedence-2 "working" check and mis-classify.
//   - The marker matches BOTH captures despite the different spinner
//     glyphs, pinning the spinner-frame robustness end-to-end (the
//     canary in probe_test.go pins it at the substring level; this
//     test pins it at the classification level).
//
// The state.go classifier reaches the CompactionMarker check
// (precedence 1) on capture B, returns StateAtRestInCompaction with
// the marker surfaced in Evidence, and never reaches the working
// check that would otherwise fire on the animating pane.
//
// Without this pin, a future refactor that flipped precedence — or
// removed the spinner-cycling-aware substring scoping — would
// silently break the AtRestInCompaction path while the canary
// (substring-in-golden) still passed.
func TestAgentState_AtRestInCompactionOnGolden(t *testing.T) {
	fastTemporalDelta(t)
	earlyGolden, err := os.ReadFile("testdata/golden_quartermaster_compaction_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read early golden: %v", err)
	}
	advancedGolden, err := os.ReadFile("testdata/golden_quartermaster_compaction_advanced_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read advanced golden: %v", err)
	}
	// Sanity-check the spinner glyphs actually differ across the two
	// goldens — if a future capture re-frame normalizes them to the
	// same glyph this test's load-bearing claim about precedence-
	// over-working evaporates.
	if string(earlyGolden) == string(advancedGolden) {
		t.Fatalf("the two compaction goldens are byte-identical; the test's precedence-over-working claim requires capA != capB")
	}
	fr := newAgentStateRunner([]string{string(earlyGolden), string(advancedGolden)}, 0, 0)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAtRestInCompaction {
		t.Errorf("state = %v, want StateAtRestInCompaction (compaction marker found in capture B — precedence 1 should beat precedence-2-working even when capA != capB)",
			state)
	}
	if ev.Marker != CompactionMarker {
		t.Errorf("Evidence.Marker = %q, want %q", ev.Marker, CompactionMarker)
	}
	if ev.Reason == "" {
		t.Errorf("Evidence.Reason should name the compaction marker match")
	}
}

// TestAgentState_CompactionPhraseInTranscriptNotMidCompact pins #647: a chamber
// discussing compaction writes the bare phrase "Compacting conversation…" in a
// message; with the input idle, AgentState must NOT classify the pane as
// mid-/compact. The original bare-phrase whole-pane substring match did, and
// because StateAtRestInCompaction ∈ IsPasteUnsafe it deferred ALL inbound
// delivery (reproduced live: Engineer + Bosun panes both wedged while working on
// this very code). The fix requires the live-elapsed-timer parenthetical, which
// transcript prose lacks.
func TestAgentState_CompactionPhraseInTranscriptNotMidCompact(t *testing.T) {
	fastTemporalDelta(t)
	// Cursor row (index 3) is the empty prompt; the marker phrase sits in
	// transcript text above it WITHOUT the live-timer parenthetical.
	pane := "history\n  the wedge: \"Compacting conversation…\" matched transcript text\n────────\n❯ \n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateAtRestInCompaction {
		t.Fatalf("state = StateAtRestInCompaction; the bare phrase in transcript prose must NOT read as mid-/compact (#647)")
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (idle prompt; phrase only in transcript text)", state)
	}
}

// TestCapturedLiveCompaction pins the #647 discriminator directly: the live UI
// (marker + live-timer parenthetical, any spinner glyph) matches; transcript
// prose quoting the bare phrase — or the phrase with a non-timer parenthetical —
// does not.
func TestCapturedLiveCompaction(t *testing.T) {
	const marker = CompactionMarker
	cases := []struct {
		name    string
		capture string
		want    bool
	}{
		{"live UI early (✻ + 7s)", "✻ " + marker + " (7s · ↑ 2.9k tokens)", true},
		{"live UI advanced (✢ + 1m 42s)", "✢ " + marker + " (1m 42s · ↑ 2.9k tokens)", true},
		{"bare phrase quoted in prose", "the wedge: \"" + marker + "\" matched transcript text", false},
		{"phrase + non-timer parenthetical", marker + " (the marker)", false},
		{"phrase at end of a sentence, no paren", "discussing " + marker, false},
		{"phrase absent entirely", "nothing compaction-ish in this pane", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := capturedLiveCompaction(c.capture, marker); got != c.want {
				t.Errorf("capturedLiveCompaction(%q) = %v, want %v", c.capture, got, c.want)
			}
		})
	}
}

func TestAgentState_AwaitingOperatorOnAskUserQuestionGolden(t *testing.T) {
	fastTemporalDelta(t)
	golden, err := os.ReadFile("testdata/golden_quartermaster_askuserquestion_2026-06-04.txt")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	pane := string(golden)
	fr := newAgentStateRunner([]string{pane, pane}, 0, 0)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
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

// TestIsPasteUnsafe pins the per-state classification used by the
// mailman's pre-paste safety check (#105 Half 2): AwaitingOperator,
// Unknown, and AtRestInCompaction return true (paste-unsafe); Idle
// and Working return false. The Compaction case is intentional
// defense-in-depth per Surveyor PR #134 S2 — PostCompactPause handles
// the scheduling layer when the mailman just delivered /compact, but
// leaves a coverage gap when the agent is in Compaction for an
// unrelated reason (operator-initiated /compact). The safety-check
// layer covers that gap.
func TestIsPasteUnsafe(t *testing.T) {
	cases := map[State]bool{
		StateUnknown:            true,  // popup-as-Unknown failure mode
		StateAwaitingOperator:   true,  // operator typing or popup
		StateAtRestInCompaction: true,  // /compact slash-command parser destruction
		StateRateLimited:        true,  // upstream retry-after / cooldown
		StateUsageLimited:       true,  // quota exhausted / park-until-reset
		StateIdle:               false, // safe by definition
		StateWorking:            false, // Claude Code buffers mid-turn keystrokes
	}
	for state, want := range cases {
		if got := IsPasteUnsafe(state); got != want {
			t.Errorf("IsPasteUnsafe(%v) = %v, want %v", state, got, want)
		}
	}
}

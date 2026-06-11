package tmuxio

import (
	"context"
	"strings"
	"testing"
)

// setActivePaneProfileForTest swaps the package-global activeProfile for the
// duration of a test and restores it on cleanup. Sibling to SetTmuxRunner's
// save/restore idiom. Tests using it must NOT call t.Parallel() — activeProfile
// is a process-global the pane-observation readers consult directly.
func setActivePaneProfileForTest(t *testing.T, p PaneProfile) {
	t.Helper()
	prev := activeProfile
	activeProfile = p
	t.Cleanup(func() { activeProfile = prev })
}

// syntheticCodexLikeProfile is a deliberately non-Claude profile used to prove
// the #322 refactor threads the ACTIVE profile's snippets through the
// classifier rather than the hardcoded Claude constants. The sentinel is
// » (U+00BB) + NBSP — a clear stand-in, NOT codex's real sentinel (codex's
// exact bytes land in the adapter's PaneProfile after the mandated ANSI
// characterization, Phase 2). The point here is only that it DIFFERS from
// Claude's ❯ so the negative-space test below can show the Claude sentinel is
// inert under a non-Claude profile.
func syntheticCodexLikeProfile() PaneProfile {
	return PaneProfile{
		PromptSentinel:         "» ",
		CompactionMarker:       "",         // adapter has no compaction UI snippet (yet)
		AwaitingOperatorMarker: "",         // no popup-footer snippet (yet)
		StatusLineMarker:       "▸▸status", // distinct from Claude's ⏵⏵
	}
}

// TestClaudePaneProfile_MatchesConsts pins the constructor against the
// canary-pinned constants: ClaudePaneProfile() must assemble exactly the
// exported PromptSentinel / CompactionMarker / AwaitingOperatorMarker /
// StatusLineMarker values. If a future edit diverges the constructor from the
// constants (e.g. a copy-paste typo), the canary tests would still pass on the
// constants while the runtime silently read wrong values; this pin closes that
// gap.
func TestClaudePaneProfile_MatchesConsts(t *testing.T) {
	p := ClaudePaneProfile()
	if p.PromptSentinel != PromptSentinel {
		t.Errorf("PromptSentinel = %q, want const %q", p.PromptSentinel, PromptSentinel)
	}
	if p.CompactionMarker != CompactionMarker {
		t.Errorf("CompactionMarker = %q, want const %q", p.CompactionMarker, CompactionMarker)
	}
	if p.AwaitingOperatorMarker != AwaitingOperatorMarker {
		t.Errorf("AwaitingOperatorMarker = %q, want const %q", p.AwaitingOperatorMarker, AwaitingOperatorMarker)
	}
	if p.StatusLineMarker != StatusLineMarker {
		t.Errorf("StatusLineMarker = %q, want const %q", p.StatusLineMarker, StatusLineMarker)
	}
}

// TestSetActivePaneProfile_Installs pins the install/accessor round-trip and
// the package default (Claude). The default matters: in-package callers that
// never go through cli.Run (every existing tmuxio test) rely on activeProfile
// defaulting to the Claude profile.
func TestSetActivePaneProfile_Installs(t *testing.T) {
	if got := ActivePaneProfile(); got != ClaudePaneProfile() {
		t.Fatalf("default ActivePaneProfile() = %+v, want Claude default", got)
	}
	want := syntheticCodexLikeProfile()
	setActivePaneProfileForTest(t, want)
	if got := ActivePaneProfile(); got != want {
		t.Errorf("after install, ActivePaneProfile() = %+v, want %+v", got, want)
	}
}

// TestAgentState_RespectsActivePaneProfileSentinel proves the cursor-aware
// classification keys off the ACTIVE profile's sentinel. Under the synthetic
// non-Claude profile, a pane painted with the synthetic sentinel + cursor past
// it classifies as StateAwaitingOperator — exactly as the Claude sentinel does
// under the Claude profile, but driven by the installed snippet.
func TestAgentState_RespectsActivePaneProfileSentinel(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, syntheticCodexLikeProfile())

	// Row 3 (0-indexed): `» drafting a reply` — operator mid-typing under
	// the synthetic sentinel. cursorX=6 (past the 2-rune sentinel + content);
	// cursorY=3.
	pane := "history\n──── Agent ──\n  recap line\n» drafting a reply\n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 6, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateAwaitingOperator {
		t.Errorf("state = %v, want StateAwaitingOperator (cursor past the ACTIVE profile's sentinel)", state)
	}
	if !strings.Contains(ev.Reason, "operator mid-typing") {
		t.Errorf("Evidence.Reason should mention operator mid-typing; got %q", ev.Reason)
	}
}

// TestAgentState_ClaudeSentinelInertUnderNonClaudeProfile is the negative-space
// proof of the #322 refactor: under a non-Claude profile, a pane painted with
// the CLAUDE ❯ sentinel + cursor "past" it does NOT classify as
// StateAwaitingOperator, because the classifier no longer reads the hardcoded
// PromptSentinel constant — it reads activeProfile.PromptSentinel.
//
// Mutation-sensitivity: revert any of state.go's cursor-aware sentinel reads
// from `activeProfile.PromptSentinel` back to the bare `PromptSentinel` const
// and this test fails — the leaked const would match the ❯ row and return
// StateAwaitingOperator. That is the load-bearing invariant the refactor
// establishes: pane-observation is profile-driven, not Claude-hardcoded.
func TestAgentState_ClaudeSentinelInertUnderNonClaudeProfile(t *testing.T) {
	fastTemporalDelta(t)
	setActivePaneProfileForTest(t, syntheticCodexLikeProfile())

	// The pane uses Claude's ❯  sentinel with operator-typed content + a
	// cursor that WOULD be "past" it — under the Claude profile this is the
	// canonical StateAwaitingOperator shape (see
	// TestAgentState_AwaitingOperatorWhenCursorPastSentinel). Under the
	// synthetic profile the row doesn't start with the active `» `
	// sentinel, so the cursor-aware branch never fires; the pane is stable and
	// matches no marker → StateUnknown.
	pane := "history\n──── Agent ──\n  recap line\n❯ Thank you for handling this and \n────────\n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 37, 3)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state == StateAwaitingOperator {
		t.Fatalf("state = StateAwaitingOperator — the hardcoded Claude ❯ sentinel leaked through a non-Claude profile (the #322 refactor must read activeProfile.PromptSentinel, not the bare const)")
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown (Claude sentinel inert under synthetic profile; pane stable + no marker match)", state)
	}
}

// TestIsInputAreaBoundary_RespectsActiveStatusLineMarker pins that the input-
// area boundary recognizer reads the ACTIVE profile's StatusLineMarker, and
// that the adapter-universal ─×20 separator stays recognized regardless of
// profile. Negative-space arm: Claude's ⏵⏵ is inert under a profile whose
// StatusLineMarker differs.
func TestIsInputAreaBoundary_RespectsActiveStatusLineMarker(t *testing.T) {
	setActivePaneProfileForTest(t, syntheticCodexLikeProfile())

	cases := []struct {
		name string
		line string
		want bool
	}{
		{"active status marker", "  ▸▸status bypass on", true},
		{"claude status marker inert", "  ⏵⏵ bypass permissions on", false},
		{"universal separator still recognized", strings.Repeat("─", 25), true},
		{"plain content", "just a line of draft", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isInputAreaBoundary(c.line); got != c.want {
				t.Errorf("isInputAreaBoundary(%q) = %v, want %v", c.line, got, c.want)
			}
		})
	}
}

// TestIsInputAreaBoundary_EmptyStatusLineMarkerKeepsSeparator pins the
// empty-marker contract: a profile with no StatusLineMarker disables the
// status-line recognizer but the ─×20 separator recognizer still applies.
func TestIsInputAreaBoundary_EmptyStatusLineMarkerKeepsSeparator(t *testing.T) {
	setActivePaneProfileForTest(t, PaneProfile{PromptSentinel: "» ", StatusLineMarker: ""})

	if isInputAreaBoundary("  ⏵⏵ anything") {
		t.Error("empty StatusLineMarker should disable the status-line recognizer")
	}
	if !isInputAreaBoundary(strings.Repeat("─", 20)) {
		t.Error("the ─×20 separator recognizer must stay active regardless of StatusLineMarker")
	}
}

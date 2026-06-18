package tmuxio

import (
	"context"
	"reflect"
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

// TestCodexPromptSentinel_Bytes pins the byte-level encoding of the Codex
// sentinel against the empirically-captured production bytes (Lookout's %9,
// 2026-06-12): › (U+203A, hex e2 80 ba) followed by a REGULAR space (0x20).
// The sibling to TestPromptSentinel_BytesMatchNBSP — and the place a future
// Codex TUI change (glyph swap, or a switch to NBSP like Claude) surfaces
// loudly. The 0x20 (not c2 a0) is the load-bearing distinction.
func TestCodexPromptSentinel_Bytes(t *testing.T) {
	want := []byte{0xe2, 0x80, 0xba, 0x20}
	got := []byte(CodexPromptSentinel)
	if !bytesEqual(got, want) {
		t.Errorf("CodexPromptSentinel bytes = % x, want % x (› U+203A + regular space 0x20, NOT NBSP)", got, want)
	}
}

// TestCodexPaneProfile_Shape pins the Codex profile: the verified sentinel, and
// the intentionally-empty marker fields (their emptiness is a documented
// pending-characterization decision, not an oversight — see CodexPaneProfile).
func TestCodexPaneProfile_Shape(t *testing.T) {
	p := CodexPaneProfile()
	if p.PromptSentinel != CodexPromptSentinel {
		t.Errorf("PromptSentinel = %q, want %q", p.PromptSentinel, CodexPromptSentinel)
	}
	if p.CompactionMarker != "" || p.AwaitingOperatorMarker != "" || p.StatusLineMarker != "" {
		t.Errorf("Codex marker fields should be empty pending characterization; got compaction=%q awaiting=%q status=%q",
			p.CompactionMarker, p.AwaitingOperatorMarker, p.StatusLineMarker)
	}
}

// TestAgentState_ClassifiesCodexPane is the substrate-real regression pin for
// #322 observations 1+3: under the Codex profile, a Codex pane classifies
// correctly from the `› ` sentinel + cursor position, using the EXACT bytes and
// cursor coordinates captured from Lookout's %9 on 2026-06-12.
//
//   - idle/ghost-text: cursor at col 2 (== RuneCount("› ")) on the `› Write
//     tests for @filename` row → StateIdle (auto-suggestion ghost-text case).
//   - operator typing: cursor at col 18 (past the sentinel) on the `› Hello
//     Lookout, I` row → StateAwaitingOperator → paste would defer (no clobber).
//
// This is the substrate-verification of the "three-of-four-reduce-to-config"
// claim at the classification level (not just the byte level): the EXISTING
// cursor-aware classifier does the right thing under the Codex sentinel with
// zero per-adapter logic.
func TestAgentState_ClassifiesCodexPane(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())

	t.Run("idle_ghost_text_cursor_at_sentinel", func(t *testing.T) {
		fastTemporalDelta(t)
		// Row 2 (0-indexed): `› Write tests for @filename` — codex ghost-text.
		pane := "history\n  context\n› Write tests for @filename\n  gpt-5.5 default · /srv/codex/lookout\n"
		fr := newAgentStateRunner([]string{pane, pane}, 2, 2) // cursor_x=2 == RuneCount("› ")
		prev := SetTmuxRunner(fr.run)
		t.Cleanup(func() { SetTmuxRunner(prev) })

		state, ev, err := AgentState(context.Background(), "%9")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state != StateIdle {
			t.Errorf("state = %v, want StateIdle (cursor at codex sentinel + ghost-text)", state)
		}
		if ev.PromptEmpty {
			t.Errorf("PromptEmpty should be false (ghost-text present, not operator-typed)")
		}
	})

	t.Run("operator_typing_cursor_past_sentinel", func(t *testing.T) {
		fastTemporalDelta(t)
		// Row 2: `› Hello Lookout, I` — the real Path-A capture. cursor_x=18.
		pane := "history\n  context\n› Hello Lookout, I\n  gpt-5.5 default · /srv/codex/lookout\n"
		fr := newAgentStateRunner([]string{pane, pane}, 18, 2)
		prev := SetTmuxRunner(fr.run)
		t.Cleanup(func() { SetTmuxRunner(prev) })

		state, ev, err := AgentState(context.Background(), "%9")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if state != StateAwaitingOperator {
			t.Errorf("state = %v, want StateAwaitingOperator (cursor past codex sentinel = operator typing)", state)
		}
		if !strings.Contains(ev.Reason, "operator mid-typing") {
			t.Errorf("Reason should mention operator mid-typing; got %q", ev.Reason)
		}
	})
}

// TestSetActivePaneProfile_Installs pins the install/accessor round-trip and
// the package default (Claude). The default matters: in-package callers that
// never go through cli.Run (every existing tmuxio test) rely on activeProfile
// defaulting to the Claude profile.
func TestSetActivePaneProfile_Installs(t *testing.T) {
	if got := ActivePaneProfile(); !reflect.DeepEqual(got, ClaudePaneProfile()) {
		t.Fatalf("default ActivePaneProfile() = %+v, want Claude default", got)
	}
	want := syntheticCodexLikeProfile()
	setActivePaneProfileForTest(t, want)
	if got := ActivePaneProfile(); !reflect.DeepEqual(got, want) {
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

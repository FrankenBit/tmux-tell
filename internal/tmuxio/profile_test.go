package tmuxio

import (
	"context"
	"reflect"
	"regexp"
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
	SetActivePaneProfile(p)
	t.Cleanup(func() { SetActivePaneProfile(prev) })
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
	if !reflect.DeepEqual(p.PromptSentinelVariants, []string{ASCIIPromptSentinel}) {
		t.Errorf("PromptSentinelVariants = %q, want [%q] (Win11 ASCII render-variant, #729)", p.PromptSentinelVariants, ASCIIPromptSentinel)
	}
	if p.RateLimitPattern != "" {
		t.Errorf("RateLimitPattern = %q, want empty by default", p.RateLimitPattern)
	}
	if p.UsageLimitPattern != "" {
		t.Errorf("UsageLimitPattern = %q, want empty by default", p.UsageLimitPattern)
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

// TestASCIIPromptSentinel_Bytes pins the byte-level encoding of the Win11
// prompt render-variant (#729): `>` (U+003E, 0x3e) followed by a NO-BREAK SPACE
// (U+00A0, c2 a0) — hex `3e c2 a0`. Re-measured 2026-07-21 from three live
// Caymans Admin %11 captures; #786 pinned `3e 20` (regular space) from a
// mis-stated byte and never matched the live pane. Sibling to
// TestPromptSentinel_BytesMatchNBSP. The load-bearing distinction from the Linux
// `❯ ` (e2 9d af c2 a0): ONLY the ornament glyph differs (e2 9d af -> 3e); the
// NBSP trailer (c2 a0) is identical on both platforms, which is why matching on
// `>` + NBSP — not `>` + 0x20 — is what actually classifies a Win11 pane.
func TestASCIIPromptSentinel_Bytes(t *testing.T) {
	want := []byte{0x3e, 0xc2, 0xa0}
	got := []byte(ASCIIPromptSentinel)
	if !bytesEqual(got, want) {
		t.Errorf("ASCIIPromptSentinel bytes = % x, want % x (> U+003E + NBSP U+00A0)", got, want)
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
	if p.RateLimitPattern != "" {
		t.Errorf("Codex RateLimitPattern = %q, want empty pending characterization", p.RateLimitPattern)
	}
	if p.UsageLimitPattern != "" {
		t.Errorf("Codex UsageLimitPattern = %q, want empty pending characterization", p.UsageLimitPattern)
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

// TestAgentState_CodexWorkingMarker_BeatsFalseIdle pins the #590 fix. During an
// active codex turn the pane renders a persistent `◦ Working (Ns • esc to
// interrupt)` status row above the composer, while the composer below still
// shows the `› ` sentinel with the cursor parked at it (ghost-text) and the
// frame is stable across the temporal-delta window (the elapsed counter happens
// not to tick in this 200ms sample). Pre-fix that shape hit the
// cursor-at-sentinel branch and classified StateIdle — a false-idle that let the
// observe-gate treat a busy pane as paste-safe. The positive WorkingPattern
// marker (Precedence 4) must classify it StateWorking instead.
//
// Mutation anchor: blank CodexPaneProfile().WorkingPattern (or delete the
// Precedence-4 check in AgentState) and this reverts to StateIdle at the
// want-StateWorking assertion.
func TestAgentState_CodexWorkingMarker_BeatsFalseIdle(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)

	// Stable frame (capA == capB): active turn, working row present, composer
	// sentinel below with cursor parked at it — the exact #590 false-idle shape.
	pane := "  assistant is running a tool\n" +
		"◦ Working (5s • esc to interrupt)\n" +
		"› \n" +
		"  gpt-5.5 default · /srv/codex/lookout\n"
	// cursor at the composer sentinel row (index 2), col 2 == RuneCount("› ").
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateWorking {
		t.Errorf("state = %v, want StateWorking (codex working marker must beat the cursor-at-sentinel false-idle); evidence=%q", state, ev.Reason)
	}
	if !strings.Contains(ev.Reason, "working marker matched") {
		t.Errorf("Reason = %q, want it to name the matched working marker", ev.Reason)
	}
}

// TestAgentState_CodexIdleNoMarker_StaysIdle is the negative-space companion to
// the #590 fix: a genuinely idle codex pane (composer ghost-text, cursor at
// sentinel, NO working row) must still classify StateIdle. Pins that Precedence
// 4 is inert when the marker is absent — the fix is purely additive and does not
// regress the #322/#609 codex-idle path.
func TestAgentState_CodexIdleNoMarker_StaysIdle(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)

	pane := "history\n  context\n› Write tests for @filename\n  gpt-5.5 default · /srv/codex/lookout\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (no working marker → additive check inert)", state)
	}
}

// TestAgentState_CodexEmptyComposer_StrippedSentinel_Idle pins the #690 fix. An
// idle codex composer with NO ghost-text is `› ` (sentinel + trailing space,
// empty); `tmux capture-pane -p` strips the trailing space, so the captured row
// is a bare `›`. Pre-fix, CutPrefix(row, "› ") missed it → the cursor-aware
// branch was skipped and the pane fell through to StateUnknown ("prompt
// sentinel not found in any row") for a genuinely-idle pane, wedging delivery.
// cutPromptSentinel tolerates the strip → StateIdle, PromptEmpty=true.
//
// Mutation anchor: revert cutPromptSentinel to a plain strings.CutPrefix and
// this reverts to StateUnknown at the want-StateIdle assertion.
func TestAgentState_CodexEmptyComposer_StrippedSentinel_Idle(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)

	// Row 2 is the bare `›` — codex's empty composer after capture-pane strips
	// the sentinel's trailing space. Cursor at x=2 (the full "› " column; the
	// terminal cell wasn't stripped, only the captured string).
	pane := "history\n  context\n›\n  gpt-5.5 default · /srv/codex/lookout\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (empty composer, trailing space stripped); evidence=%q", state, ev.Reason)
	}
	if !ev.PromptEmpty {
		t.Errorf("PromptEmpty = false, want true (empty composer, no ghost-text)")
	}
}

// TestAgentState_CodexEmptyComposer_CursorLessFallback pins the SECOND #690
// enforcement site. When the cursor query doesn't land on the input row (here
// cursorY out of range — the shape a resize/transition can produce, which is
// exactly when the operator-witnessed wedge is most likely), the cursor-aware
// branch is skipped and the classifier drops to the cursor-less isInputRowQuiet
// fallback. That fallback must ALSO recognize the bare `›` (stripped empty
// composer) as StateIdle rather than falling through to StateUnknown — the
// second CutPrefix site cutPromptSentinel fixes. Mutation anchor: reverting the
// helper to plain CutPrefix flips this to StateUnknown too.
func TestAgentState_CodexEmptyComposer_CursorLessFallback(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)

	pane := "history\n  context\n›\n  gpt-5.5 default · /srv/codex/lookout\n"
	// cursorY = -1 is out of range, so the cursor-aware block is skipped and the
	// classifier falls to the cursor-less isInputRowQuiet path.
	fr := newAgentStateRunner([]string{pane, pane}, 2, -1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (bare `›` via cursor-less fallback); evidence=%q", state, ev.Reason)
	}
	if !strings.Contains(ev.Reason, "cursor-less fallback") {
		t.Errorf("Reason = %q, want it to name the cursor-less fallback path", ev.Reason)
	}
}

// TestCodexWorkingPattern_MatchesMarker pins the #590 codex working-marker regex
// against the empirical marker bytes and the negative cases it must NOT match.
// The pattern keys on the `Working (` … `esc to interrupt)` phrase pair on a
// single row; a future codex TUI change to that phrase surfaces here.
func TestCodexWorkingPattern_MatchesMarker(t *testing.T) {
	re := regexp.MustCompile(CodexWorkingPattern)

	// Positive: the live marker row across elapsed-counter values + surrounding
	// pane context. The leading ◦ glyph and the • separator are intentionally
	// not required by the pattern (drift-prone), so these all match.
	for _, s := range []string{
		"◦ Working (5s • esc to interrupt)",
		"◦ Working (12s • esc to interrupt)",
		"◦ Working (0s • esc to interrupt)",
		"  ◦ Working (3s • esc to interrupt)  ",
		"Working (7s - esc to interrupt)", // separator glyph drifted; still matches
		// Real captures from Lookout's 2026-07-01 sleep-12 persistence probe
		// (13/13 samples carried the pair). The leading glyph alternated ◦ ↔ •
		// across samples — glyph-only detection would be brittle; the pair holds:
		"◦ Working (14s • esc to interrupt)",
		"• Working (26s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close",
	} {
		if !re.MatchString(s) {
			t.Errorf("CodexWorkingPattern should match working row %q", s)
		}
	}

	// Negative: idle composer / ghost-text / status / unrelated prose must NOT
	// match (no `Working (` … `esc to interrupt)` pair on the row).
	for _, s := range []string{
		"› Write tests for @filename",
		"  gpt-5.5 default · /srv/codex/lookout",
		"history line about how the code is working",
		"press esc to interrupt is documented somewhere",
	} {
		if re.MatchString(s) {
			t.Errorf("CodexWorkingPattern should NOT match non-working row %q", s)
		}
	}

	// Cross-line guard: the phrase pair split across rows must NOT match — Go
	// regexp `.` excludes newline, so the marker cannot straddle lines (prevents
	// a stray `Working (` in scrollback pairing with a later `esc to interrupt)`).
	if re.MatchString("◦ Working (5s\nsome output esc to interrupt)") {
		t.Errorf("CodexWorkingPattern must not straddle newlines")
	}
}

// TestAgentState_Codex609PlaceholderClassifiesIdle is the #609 regression pin +
// substrate-of-record close. #609 reported codex chambers (Carpenter/Lookout)
// wedged at state=unknown "prompt sentinel not found in any row" when the
// composer showed the placeholder/typeahead text (e.g. `› Explain this
// codebase`). The 2026-07-01 survey found the issue body's root-cause was a
// hypothesis the code contradicts: the cursor-aware classifier has classified
// `› <ghost-text>` as StateIdle since 2026-06-04 (commit 1d6b4d4), well BEFORE
// #609 was filed (2026-06-20). Lookout byte-captured the exact live placeholder
// row — U+203A (`e2 80 ba`) + literal space (`20`) + text, cursor parked at
// column 2 (== RuneCount("› ")) — i.e. the literal `› ` sentinel IS present with
// the cursor at the sentinel. This test pins that exact reported state → Idle.
//
// The 2026-06-20 sprint-wedge was the #610 stacking downstream (a multi-message
// composer moves the cursor off the sentinel row / pushes the sentinel out of
// the captured input area → the "sentinel not found" / "cursor not at input row"
// unknown branches), which #610 (merged 2b1b56f) now prevents. #609 is
// resolved-by-#610; this pin guards the classifier's handling of the reported
// placeholder against regression. Distinct from the sibling ghost-text subtest
// above by naming #609's operator-witnessed capture as the substrate-of-record.
func TestAgentState_Codex609PlaceholderClassifiesIdle(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)
	// Lookout's exact live capture: `› Explain this codebase` as the bottom input
	// row. CodexPromptSentinel is `› ` (U+203A + space) — the `e2 80 ba 20` bytes
	// Lookout reported, pinned byte-exact by TestCodexPromptSentinel_Bytes.
	inputRow := CodexPromptSentinel + "Explain this codebase"
	pane := "some transcript\n  context line\n" + inputRow + "\n  gpt-5.5 default · /srv/codex/lookout\n"
	// cursor_x=2 == utf8.RuneCountInString("› "); cursor_y=2 (the input row).
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Fatalf("state = %v, want StateIdle — the #609-reported codex placeholder (cursor at sentinel, ghost-text after) is idle, not unknown; reason=%q", state, ev.Reason)
	}
	if ev.PromptEmpty {
		t.Errorf("PromptEmpty should be false: placeholder ghost-text follows the sentinel (not a clean-empty prompt)")
	}
	if !strings.Contains(ev.Reason, "ghost-text") {
		t.Errorf("Reason should identify the auto-suggestion ghost-text; got %q", ev.Reason)
	}
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

package tmuxio

import (
	"context"
	"strings"
	"testing"
)

// TestAgentStateWithProfile_ClaudeCallerCodexTarget pins the #827 fix from the
// caller-is-Claude, target-is-codex direction: even with the process-global
// activeProfile set to ClaudePaneProfile (the QM chamber's binary at
// classifier-search-time), AgentStateWithProfile passed CodexPaneProfile
// classifies a codex pane correctly. Anchored on the exact false-negative shape
// the tracker documented: `tmux-tell.tmux-tell_agent_state carpenter` from a
// claude-adapter MCP subprocess returning `state=unknown` with
// `"prompt sentinel not found in any row"`.
//
// The mutation anchor is at the bottom — same fixture through bare AgentState
// flips to StateUnknown, which is the pre-#827 bug.
func TestAgentStateWithProfile_ClaudeCallerCodexTarget(t *testing.T) {
	// Caller's binary is the Claude adapter: activeProfile stays Claude, the
	// state the MCP subprocess would observe.
	setActivePaneProfileForTest(t, ClaudePaneProfile())
	fastTemporalDelta(t)

	// Codex-shape pane: history + composer with the codex `› ` sentinel at the
	// bottom (composer empty). This is what carpenter's pane looks like when
	// idle — the exact shape the MCP false-negatived at.
	pane := "history\n  context\n› \n  gpt-5.5 default · /srv/codex/carpenter\n"
	// cursor at row 2 col 2 (RuneCount("› ") == 2), on the composer sentinel.
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentStateWithProfile(context.Background(), "%9", CodexPaneProfile())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (Claude-adapter caller + explicit Codex profile must classify codex pane correctly); evidence=%q",
			state, ev.Reason)
	}
	if !ev.PromptEmpty {
		t.Errorf("PromptEmpty should be true on an empty codex composer; got Reason=%q", ev.Reason)
	}
}

// TestAgentStateWithProfile_CodexCallerClaudeTarget is the symmetric case:
// caller's activeProfile is Codex, target is Claude. Same shape as the
// tracker's substrate-wide implication naming.
func TestAgentStateWithProfile_CodexCallerClaudeTarget(t *testing.T) {
	setActivePaneProfileForTest(t, CodexPaneProfile())
	fastTemporalDelta(t)

	// Claude-shape pane: history + composer with the Claude `❯ ` sentinel
	// (NBSP) at the bottom (composer empty).
	pane := "history\n──── Agent ──\n❯ \n────────\n  status\n"
	// cursor at row 2 col 2 (RuneCount("❯ ") == 2) — on the composer sentinel.
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentStateWithProfile(context.Background(), "%9", ClaudePaneProfile())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (Codex-adapter caller + explicit Claude profile must classify Claude pane correctly); evidence=%q",
			state, ev.Reason)
	}
	if !ev.PromptEmpty {
		t.Errorf("PromptEmpty should be true on an empty Claude composer; got Reason=%q", ev.Reason)
	}
}

// TestAgentState_BareUsesActiveProfile_MutationAnchor is the mutation anchor
// for #827: it reproduces the pre-fix false-negative by using BARE AgentState
// (the process-global path) on the same cross-adapter shape the two tests above
// classify correctly. If a future edit re-routed AgentState to a smarter
// per-pane-adapter classifier (e.g. by SetProvider peeking inside), this test
// would flip to StateIdle instead of StateUnknown and force the author to
// re-consider what the mailman fast path is doing.
//
// Concretely: with activeProfile=Claude and a codex-shape pane, bare AgentState
// searches for `❯ ` in the pane's rows, doesn't find it, and returns
// StateUnknown with "prompt sentinel not found in any row" — the exact
// pre-#827 behavior the tracker cited.
func TestAgentState_BareUsesActiveProfile_MutationAnchor(t *testing.T) {
	setActivePaneProfileForTest(t, ClaudePaneProfile())
	fastTemporalDelta(t)

	// Same codex-shape fixture as the first test above.
	pane := "history\n  context\n› \n  gpt-5.5 default · /srv/codex/carpenter\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 2)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, ev, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateUnknown {
		t.Errorf("state = %v, want StateUnknown (bare AgentState with activeProfile=Claude on a codex-shape pane MUST false-negative; the mutation anchor guards the reason #827 exists)", state)
	}
	if !strings.Contains(ev.Reason, "prompt sentinel not found in any row") {
		t.Errorf("Reason = %q, want the pre-#827 false-negative substring", ev.Reason)
	}
}

// TestAgentStateWithProfile_MailmanFastPathUnchanged confirms the process-global
// path is unchanged: AgentState (no profile arg) still classifies a Claude
// pane correctly when activeProfile=Claude — the mailman fast-path invariant.
// Same fixture the other _test files use, kept here to guard the refactor from
// silently breaking same-adapter classification if the classifier's algorithm
// drifts between (*classifier).agentState and any future re-inline into
// AgentState.
func TestAgentStateWithProfile_MailmanFastPathUnchanged(t *testing.T) {
	setActivePaneProfileForTest(t, ClaudePaneProfile())
	fastTemporalDelta(t)

	pane := "history\n❯ \n  status\n"
	fr := newAgentStateRunner([]string{pane, pane}, 2, 1)
	prev := SetTmuxRunner(fr.run)
	t.Cleanup(func() { SetTmuxRunner(prev) })

	state, _, err := AgentState(context.Background(), "%9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if state != StateIdle {
		t.Errorf("state = %v, want StateIdle (mailman fast-path: activeProfile=Claude on Claude pane must still classify idle)", state)
	}
}

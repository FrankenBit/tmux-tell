package tmuxio

// PaneProfile carries the per-adapter pane-observation snippets the substrate's
// pane-observation layer (AgentState, ObserveGate, extractInputContent) reads
// instead of historically-hardcoded Claude Code constants. It is the #322
// substrate-vs-adapter seam: the observation LOGIC stays unified and adapter-
// agnostic; only these visual snippets diverge per adapter, so a second TUI
// (codex's `›` rendering) is a config record, not a fork of the classifier.
//
// Per ADR-0009 the substrate is delivery-method-agnostic; #248/#280 introduced
// the Profile (BinaryName + DisplayLabel) and #323 added PasteCapable. This is
// the deeper seam #323's PasteCapable doc-comment anticipated ("the per-adapter
// PromptSentinel + Markers structure is #322's PaneProfile refactor"): a
// capability boolean kept the paste layer off panes it couldn't read; this
// teaches it HOW to read them.
//
// Field-by-field:
//   - PromptSentinel: the TUI input-row prefix the cursor-aware AgentState
//     classification keys off. Claude paints "❯ " (U+276F + NBSP). An
//     adapter SHOULD supply a non-empty sentinel — every interactive TUI paints
//     SOME input indicator, and an empty sentinel disables cursor-aware
//     classification entirely (AgentState degrades to the marker / cursor-less
//     fallback / unknown path).
//   - CompactionMarker: substring identifying an at-rest-in-compaction pane
//     (precedence-1 check in AgentState). Empty disables the check — correct for
//     an adapter with no compaction UI.
//   - AwaitingOperatorMarker: popup-footer backup substring for operator-input
//     UIs that don't paint the prompt sentinel (AskUserQuestion popups, search
//     dialogs). Empty disables the backup check.
//   - StatusLineMarker: the glyph marking the lower boundary of the input area
//     (Claude's "⏵⏵" status row), used by extractInputContent's multi-line
//     walk-until-boundary. Empty disables the status-line recognizer; the
//     adapter-universal ─×20 separator recognizer (isInputAreaBoundary) stays
//     active regardless, since box-drawing separators are a TUI-wide convention.
//
// Why PromptSentinel is a string, not the rune the #322 issue sketch proposed:
// Claude's sentinel is TWO codepoints (❯ U+276F followed by NBSP U+00A0), so a
// single rune cannot represent it. String is the honest type. (See PR body for
// the surfaced design fork.)
//
// A verify-strategy field is deliberately NOT present yet. #322 observation 2
// (slow codex verify-token) is gated on an ANSI-rendering investigation whose
// outcome decides whether the verify path stays config-only or needs an adapter
// seam; the field is held until that investigation lands rather than guessed.
type PaneProfile struct {
	PromptSentinel         string
	CompactionMarker       string
	AwaitingOperatorMarker string
	StatusLineMarker       string
}

// ClaudePaneProfile returns the Claude Code pane-observation profile — the
// historically-hardcoded constants assembled into a PaneProfile. The exported
// PromptSentinel / CompactionMarker / AwaitingOperatorMarker / StatusLineMarker
// constants remain the canary-pinned source of truth (state_canary_test.go);
// this constructor is the single place that reads them into the profile the
// runtime consumes, so the canary discipline still anchors the values.
func ClaudePaneProfile() PaneProfile {
	return PaneProfile{
		PromptSentinel:         PromptSentinel,
		CompactionMarker:       CompactionMarker,
		AwaitingOperatorMarker: AwaitingOperatorMarker,
		StatusLineMarker:       StatusLineMarker,
	}
}

// activeProfile is the process-global pane-observation profile, mirroring the
// internal/cli `active` Profile pattern: a CLI binary serves exactly one adapter
// for the life of the process, so a package-global set once at Run entry (via
// SetActivePaneProfile, called from cli.Run) is the pragmatic seam — versus
// threading PaneProfile through every pane-observation signature + its ~30 call
// sites and tests. It defaults to Claude so in-package tmuxio tests, which
// exercise AgentState / ObserveGate directly without going through cli.Run,
// observe the historical behavior unchanged.
var activeProfile = ClaudePaneProfile()

// SetActivePaneProfile installs the active pane-observation profile. Called once
// from cli.Run at process start with the adapter's Profile.Pane. Callers should
// supply a complete profile (non-empty PromptSentinel at minimum) — an empty
// profile disables cursor-aware classification. This is a start-of-process
// install, not a runtime toggle: it is NOT safe for concurrent use with the
// pane-observation readers, which is fine because Run sets it before any mailman
// goroutine starts observing.
func SetActivePaneProfile(p PaneProfile) { activeProfile = p }

// ActivePaneProfile returns the installed pane-observation profile — a read-only
// accessor for callers and tests that need to inspect the active snippets.
func ActivePaneProfile() PaneProfile { return activeProfile }

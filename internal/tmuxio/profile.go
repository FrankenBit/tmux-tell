package tmuxio

import "regexp"

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
//   - PromptSentinelVariants: additional render-variants of the prompt sentinel
//     the SAME TUI can paint under a different terminal/OS, tolerated ONLY in the
//     cursor-aware classification path (where the cursor pins the match to the
//     live input row). Claude under a Windows 11 terminal renders its prompt as
//     plain ASCII "> " (U+003E + regular space) — the ornament glyph U+276F
//     font-substitutes on Win11 — so an ssh-relayed or on-Win11 Claude pane that
//     the Linux-calibrated PromptSentinel can't match classified StateUnknown and
//     tripped pre_paste_safety_abort on every delivery (#729). A variant is
//     DELIBERATELY not consulted by the cursor-LESS scans (isInputRowQuiet,
//     extractInputContent): "> " is a common glyph (a markdown blockquote, a
//     "> " shell continuation prompt) that would false-idle a non-input row
//     without the cursor's corroboration — so it is trusted only where the
//     cursor anchors it. Empty (Codex, the synthetic profile) leaves the
//     single-sentinel behavior unchanged.
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
//   - PasteCollapseMarker: substring an adapter's TUI shows in the INPUT row for
//     a paste it collapsed (codex's `[Pasted Content N chars]`). Two uses, both
//     in the paste-delivery verify path (#401): (1) a definitive not-submitted
//     signal — while the marker is in the input area the paste has NOT submitted,
//     which OVERRIDES the cursor-anchor (that false-positives when a multi-line
//     collapsed paste parks the cursor on an empty sub-line); (2) the resubmit
//     trigger — codex's first Enter is absorbed while it is still ingesting the
//     bracketed paste, so the mailman re-sends Enter while the marker persists
//     (Enter-on-empty is a safe no-op, operator + Lookout confirmed). Empty
//     disables both — correct for Claude, which submits a collapsed paste on the
//     first Enter and needs no resubmit.
//   - RateLimitPattern: operator-configurable regex that identifies an
//     adapter's rate-limit pane (#504). Empty parks the detector. The regex is
//     validated at startup and may use named capture groups `retry_seconds`
//     (load-bearing in #504) plus future-extensible fields such as `retry_at`.
//   - UsageLimitPattern: operator-configurable regex that identifies an
//     adapter's usage-limit pane (#540). Empty parks the detector. This is a
//     distinct hard-stop sibling to rate-limit: the mailman parks until quota
//     reset rather than backing off exponentially.
//   - WorkingPattern: adapter-specific regex identifying a pane actively
//     processing a turn via a persistent status marker, checked independently of
//     (and before) the temporal-delta frame-change heuristic (#590). Empty parks
//     the positive-detection check — correct for Claude, whose animated spinner
//     the 200ms temporal-delta already catches. Codex renders a stable
//     "Working (…esc to interrupt)" status row whose only per-second change is an
//     elapsed counter, so a capture pair can read the frame as stable and the
//     cursor-aware logic false-idles the active turn; this positive marker
//     classifies it Working regardless of frame stability.
type PaneProfile struct {
	PromptSentinel         string
	PromptSentinelVariants []string
	CompactionMarker       string
	AwaitingOperatorMarker string
	StatusLineMarker       string
	PasteCollapseMarker    string
	RateLimitPattern       string
	UsageLimitPattern      string
	WorkingPattern         string
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
		PromptSentinelVariants: []string{ASCIIPromptSentinel},
		CompactionMarker:       CompactionMarker,
		AwaitingOperatorMarker: AwaitingOperatorMarker,
		StatusLineMarker:       StatusLineMarker,
	}
}

// CodexPromptSentinel is the OpenAI Codex CLI TUI's input-row prefix — U+203A
// (› SINGLE RIGHT-POINTING ANGLE QUOTATION MARK) followed by a REGULAR space
// (U+0020). Empirically captured 2026-06-12 from Lookout's codex pane (%9),
// hex `e2 80 ba 20` (idle ghost-text + operator-typing states).
//
// Contrast with the Claude PromptSentinel: Claude pairs ❯ with a NBSP (U+00A0)
// — a deliberate choice (likely to prevent terminal-side word-wrap on the
// sentinel); Codex uses a plain 0x20, the default fall-out rather than a design
// choice. A reader who knows Claude's NBSP might assume Codex matches — it does
// NOT; the trailing byte is a plain 0x20, byte-verified. If a future Codex TUI
// update changes the glyph or the
// space, the codex canary (TestCodexPromptSentinel_Bytes) surfaces the drift.
const CodexPromptSentinel = "› "

// CodexPasteCollapseMarker is the substring codex shows in the input row for a
// paste it collapsed — `[Pasted Content 1024 chars]` and similar. Matched as a
// prefix-substring (the char-count varies) and scoped to the input area, so a
// submitted paste's transcript entry doesn't trip it. Substrate-verified on
// Lookout `%8` (#401): a >1KB paste renders as `› [Pasted Content N chars]`.
// Load-bearing for the codex paste-submit fix — see PaneProfile.PasteCollapseMarker.
const CodexPasteCollapseMarker = "[Pasted Content"

// CodexWorkingPattern matches the OpenAI Codex TUI's active-turn status row,
// rendered immediately above the composer while a turn is processing — e.g.
// `◦ Working (12s • esc to interrupt)`. Empirically captured 2026-07-01 from
// Lookout's live codex v0.141.0 pane; `capture-pane -p` renders it as plain
// text (no ANSI escapes), byte-anchored `e2 97 a6 20` (◦ + space) + `Working (`
// + elapsed + ` ` + `e2 80 a2` (•) + ` esc to interrupt)`.
//
// The pattern keys on the phrase pair `Working (` … `esc to interrupt)` on a
// SINGLE row (Go regexp `.` excludes newline, so the pair cannot straddle
// lines) and deliberately omits both the leading `◦` (U+25E6) glyph and the
// elapsed-seconds format — the drift-prone parts. The `esc to interrupt`
// interrupt-hint is codex's active-turn-only affordance, so the same-row pair is
// a stable positive busy marker resilient to glyph/format churn. A future codex
// TUI change to the phrase surfaces via TestCodexWorkingPattern_MatchesMarker.
const CodexWorkingPattern = `Working \(.*esc to interrupt\)`

// CodexPaneProfile returns the OpenAI Codex CLI pane-observation profile.
// PromptSentinel is the substrate-verified `› ` (CodexPromptSentinel); under it
// the existing cursor-aware AgentState classifies Codex panes correctly (idle
// at the sentinel / ghost-text, awaiting-operator when the cursor moves past) —
// #322 observations 1 and 3, substrate-verified against real bytes.
//
// WorkingPattern is POPULATED (CodexWorkingPattern) — the #590 characterization
// of codex's active-turn status row, empirically captured 2026-07-01. Unlike the
// fields below it is live, not parked: it fixes the false-idle where a
// stable-frame active turn (mid-tool-run / mid-sleep) classified Idle because the
// temporal-delta saw no change and the cursor sat at the composer sentinel.
//
// The remaining marker fields are intentionally EMPTY pending characterization of
// Codex's other UIs (the 2026-06-12 capture only exercised idle + operator-typing):
//   - CompactionMarker / AwaitingOperatorMarker: empty disables those precedence
//     checks. Codex's compaction / popup equivalents (if any) aren't captured
//     yet; agent_state still classifies idle / working / awaiting-operator from
//     the sentinel + cursor without them.
//   - StatusLineMarker: empty — Codex's status row ("gpt-5.5 default · …") has no
//     stable leading glyph like Claude's ⏵⏵, so its input-area lower boundary
//     relies on the adapter-universal ─×20 separator. This only matters for
//     extractInputContent (the observe-gate stale-draft path), which is
//     unreachable while Codex is PasteCapable=false — so the gap is moot until
//     the verify-token-robustness work makes Codex paste-capable. Named here so
//     it is not a silent gap.
//   - RateLimitPattern: empty pending empirical Codex rate-limit captures
//     (#504). Do not add production literals without a real pane sample.
//   - UsageLimitPattern: empty pending empirical Codex usage-limit captures
//     (#540). Do not add production literals without a real pane sample.
func CodexPaneProfile() PaneProfile {
	return PaneProfile{
		PromptSentinel:      CodexPromptSentinel,
		PasteCollapseMarker: CodexPasteCollapseMarker,
		WorkingPattern:      CodexWorkingPattern,
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
var activeRateLimitRE *regexp.Regexp
var activeUsageLimitRE *regexp.Regexp
var activeWorkingRE *regexp.Regexp

// SetActivePaneProfile installs the active pane-observation profile. Called once
// from cli.Run at process start with the adapter's Profile.Pane. Callers should
// supply a complete profile (non-empty PromptSentinel at minimum) — an empty
// profile disables cursor-aware classification. This is a start-of-process
// install, not a runtime toggle: it is NOT safe for concurrent use with the
// pane-observation readers, which is fine because Run sets it before any mailman
// goroutine starts observing.
func SetActivePaneProfile(p PaneProfile) {
	activeProfile = p
	activeRateLimitRE = compileProfilePattern(p.RateLimitPattern)
	activeUsageLimitRE = compileProfilePattern(p.UsageLimitPattern)
	activeWorkingRE = compileProfilePattern(p.WorkingPattern)
}

// ActivePaneProfile returns the installed pane-observation profile — a read-only
// accessor for callers and tests that need to inspect the active snippets.
func ActivePaneProfile() PaneProfile { return activeProfile }

func compileProfilePattern(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil
	}
	return re
}

package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// PromptSentinel is the Claude Code TUI's input-row prefix — U+276F
// (Heavy Right-Pointing Angle Quotation Mark Ornament) followed by
// U+00A0 (NO-BREAK SPACE). Empirically verified across all six
// agents on 2026-06-04, hex `e2 9d af c2 a0`.
//
// The string-literal uses `\u00a0` so the NBSP is explicit in the
// source code (mixing a visually-identical NBSP into the literal
// would silently fool future readers into thinking it's a regular
// space).
//
// FORWARD-WATCH: this constant is Claude-Code-version-dependent. If
// the Claude Code TUI's prompt character changes (theme update,
// version bump, customization), the cursor-aware AgentState branch
// silently degrades to "cursor not at input row". Re-verify the
// constant during any major Claude Code version update via
// `tmux capture-pane | od -An -tx1` on the input row; the canary
// tests in state_canary_test.go (golden + byte-encoding) catch
// drift loudly.
const PromptSentinel = "❯\u00a0"

// State classifies a agent's current activity from the tmux-msg
// vantage point. Five values, per the #69 verdict
// (bus id `d47f`, 2026-06-04). The zero value is StateUnknown so a
// caller that forgets to initialize gets the safer-default-on-
// uncertainty behaviour automatically.
//
// Consumer convention: treat StateUnknown as advisory-not-authoritative
// per the #65 playbook's substrate-class-of-claim shape.
// Don't roll up an unknown classification into a known state silently;
// gate the consumer's action until the probe substantiates better data.
type State int

const (
	// StateUnknown means the probe couldn't substantiate a known state —
	// pane unreachable, capture-pane errored, or the pane is stable in
	// some non-prompt non-menu UI state the heuristic doesn't recognize.
	// Always the zero value.
	StateUnknown State = iota
	// StateIdle means the agent is waiting for input. The pane is
	// stable across the temporal-delta window AND the PromptSentinel is
	// painted with no content past it.
	StateIdle
	// StateWorking means the agent is actively processing — streaming
	// output, spinner ticking, or any other substantive pane-content
	// change across the temporal-delta window.
	StateWorking
	// StateAtRestInCompaction means the agent is mid-`/compact`
	// sequence. Detection relies on CompactionMarker, an empirically-
	// captured substring of Claude Code's compaction-in-progress UI
	// (#70, PR #88). Lit up 2026-06-04 from two operator-
	// coordinated captures of the Quartermaster pane at distinct
	// progress points (8% and 68%) across the same /compact event;
	// canary + classification pins in state_canary_test.go and
	// state_test.go protect the substring against Claude Code UI
	// drift.
	StateAtRestInCompaction
	// StateAwaitingOperator means the agent is paused on an
	// AskUserQuestion popup or other operator-input-required UI —
	// structurally distinct from idle: the agent has an open turn
	// awaiting human response, and the next bus message can't drive the
	// turn forward without first being treated by the operator.
	// Detection relies on AwaitingOperatorMarker, an empirically-captured
	// substring of Claude Code's popup footer (#79, PR #87).
	// Lit up 2026-06-04 from an operator-coordinated AskUserQuestion
	// capture; canary + classification pins in state_canary_test.go
	// and state_test.go protect the substring against Claude Code UI
	// drift.
	StateAwaitingOperator
	// StateInCopyMode means the operator has scrolled the pane up into
	// tmux copy-mode / view-mode (#526). Detected by `display-message
	// '#{pane_in_mode}'` at precedence 0 — BEFORE the capture-pane
	// snapshots — because `capture-pane -p` on a scrolled pane reads the
	// HISTORICAL view, not the live bottom: an old `❯ ` prompt scrolled
	// into frame would otherwise misclassify as StateIdle and the mailman
	// would paste into a scrolled pane (the 83b3 incident, 2026-06-17).
	// Paste-unsafe (see IsPasteUnsafe): a paste/Enter into copy-mode is
	// consumed as copy-mode navigation, and the underlying working/idle
	// state is genuinely UNOBSERVABLE while scrolled — so this state is
	// also what the `observed_state` self-probe honestly publishes during
	// a scroll-read (#448 cap counts it as not-working, self-healing on
	// exit).
	StateInCopyMode
	// StateRateLimited means the adapter pane matches the operator-configured
	// rate-limit regex (#504). Detection is marker-in-capture, same precedence
	// family as CompactionMarker (not copy-mode's pre-capture pane_in_mode
	// query): the pane content is still live, but it may be static or animate a
	// countdown, so the pattern must win before working/idle heuristics.
	// Paste-unsafe until the reactive layer decides when to retry.
	StateRateLimited
	// StateUsageLimited means the adapter pane matches the operator-configured
	// usage-limit regex (#540). This is the hard-stop sibling to rate-limit:
	// account quota is exhausted, so the mailman parks until quota reset rather
	// than backing off exponentially. Paste-unsafe while parked.
	StateUsageLimited
)

// String returns the wire-format name of the state — the same string
// the CLI / MCP surfaces emit in their `state` field. Stable across
// implementations so consumers can switch on it without recompilation.
// Names match #69's accepted vocabulary verbatim.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateWorking:
		return "working"
	case StateAtRestInCompaction:
		return "at-rest-in-compaction"
	case StateAwaitingOperator:
		return "awaiting-operator"
	case StateInCopyMode:
		return "copy-mode"
	case StateRateLimited:
		return "rate-limited"
	case StateUsageLimited:
		return "usage-limited"
	default:
		return "unknown"
	}
}

// IsPasteUnsafe reports whether a paste-and-Enter delivery to a pane in
// this state risks corrupting operator-visible content. Three states qualify:
//
//   - StateAwaitingOperator: the operator is typing OR a popup is open
//     consuming keystrokes. A paste into either case destroys what's
//     visible (operator's draft / popup interpretation of pasted bytes
//     as keystrokes).
//   - StateUnknown: the classifier couldn't substantiate a known state.
//     The popup-as-Unknown failure mode (#105) is exactly the case
//     where pasting is destructive — if we can't substantiate, we
//     can't paste safely.
//   - StateAtRestInCompaction: paste-into-compaction is consumed by
//     the /compact slash-command parser as additional commands —
//     destructive. The PostCompactPause machinery prevents this at a
//     SCHEDULING layer when the mailman just delivered /compact, but
//     leaves a coverage gap when the agent is in Compaction for an
//     UNRELATED reason (operator-initiated /compact). Returning true
//     here gives defense-in-depth at the safety-check layer per
//     Surveyor PR #134 S2.
//   - StateInCopyMode: the operator has scrolled the pane up (#526). A
//     paste/Enter is consumed as copy-mode navigation, not input, and
//     the verify-token can't surface from the scrolled view — the 83b3
//     failure. The observe-gate defers it, but this also covers the
//     post-gate-pass race where the operator scrolls up in the window
//     between an idle gate-pass and the actual paste.
//   - StateRateLimited: the adapter is showing a provider rate-limit banner
//     matched by the operator-configured regex (#504). Pasting more work at
//     this point deepens provider pressure and risks losing the delivery
//     behind an upstream cooldown; the reactive layer decides when to retry.
//   - StateUsageLimited: the adapter is showing an account usage-limit banner
//     matched by the operator-configured regex (#540). This is a hard-stop
//     quota event, not a temporary throttle; the mailman parks until reset.
//
// StateIdle and StateWorking are paste-safe (idle by definition;
// working buffers mid-turn keystrokes per Claude Code TUI behavior).
//
// Used by the mailman's pre-paste safety check (#105 Half 2): even if
// the observe-gate decides to flush, a final state probe before the
// actual paste-and-Enter aborts the delivery when this returns true.
//
// The set splits into two groups (#558): the rate-limit family
// (StateRateLimited, StateUsageLimited) is an operator-overridable
// *throttle/quota* signal, whereas the rest (awaiting-operator, unknown,
// compaction, copy-mode) is *content-corrupting* paste-unsafety that is NEVER
// overridable. IsPasteUnsafeForced is the second group alone — the predicate
// the #558 `--force-rate-limited` path uses so a forced delivery skips only the
// rate-limit family while every content-corrupting state still aborts the paste.
func IsPasteUnsafe(s State) bool {
	return IsPasteUnsafeForced(s) || s == StateRateLimited || s == StateUsageLimited
}

// IsPasteUnsafeForced reports paste-unsafety with the rate-limit family
// (StateRateLimited / StateUsageLimited) EXCLUDED — the content-corrupting
// states only. It is the pre-paste predicate for a #558 `--force-rate-limited`
// message: the operator chose to push past a rate-/usage-limit banner, but a
// paste into copy-mode, a popup/operator-typing, an unknown state, or an active
// compaction still corrupts operator-visible content and must abort regardless
// of the force flag. Keeping IsPasteUnsafe defined in terms of this function
// guarantees the two stay in lockstep: any future content-corrupting state
// added here is force-safe by construction; only the literal rate-limit family
// is ever forced through.
func IsPasteUnsafeForced(s State) bool {
	return s == StateAwaitingOperator || s == StateUnknown ||
		s == StateAtRestInCompaction || s == StateInCopyMode
}

// Evidence carries the observation that led to the State classification.
// Fields are populated per state; consumers should treat unset fields as
// "not applicable to this state's detection path." Reason is always
// populated and intended for human-readable display (the CLI `text`
// format prints it; the JSON format includes it verbatim).
type Evidence struct {
	// Reason is a one-line explanation of the classification, suitable
	// for display in the CLI text format or surfaced through the MCP
	// tool's response. Always populated.
	Reason string `json:"reason"`
	// PromptEmpty is true when the StateIdle classification was made
	// because the prompt sentinel was found with no content past it.
	PromptEmpty bool `json:"prompt_empty,omitempty"`
	// ChangedLineCount is the number of differing lines between the two
	// temporal-delta captures, populated for StateWorking.
	ChangedLineCount int `json:"changed_line_count,omitempty"`
	// RetryAfter is the parsed relative retry delay from the rate-limit regex,
	// when the adapter exposes a retry_seconds capture. Zero means no parseable
	// retry hint was available in the matched text.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
	// Marker is the matched substring for StateAtRestInCompaction or
	// StateAwaitingOperator.
	Marker string `json:"marker,omitempty"`
	// CopyModeQueryFailed is true when the precedence-0 `#{pane_in_mode}`
	// query (PaneInCopyMode) returned an *error* and AgentState degraded to
	// the capture-based classifier (#537). It does NOT change the returned
	// State — the classification still reflects the captures — but it tells
	// the gate loop that this poll's copy-mode determination is unreliable,
	// so the gate can bias a PERSISTENT run of such failures toward defer
	// rather than delivering on a possibly-stale capture (observe_gate.go).
	CopyModeQueryFailed bool `json:"copy_mode_query_failed,omitempty"`
}

// CompactionMarker is the substring that identifies a agent in
// StateAtRestInCompaction via pane-capture inspection.
//
// Empirically captured 2026-06-04 from a Quartermaster pane mid-
// `/compact` (#70). Two captures from the same compaction
// event — at 8% and 68% progress — are frozen as
// testdata/golden_quartermaster_compaction_2026-06-04.txt and
// testdata/golden_quartermaster_compaction_advanced_2026-06-04.txt so
// future Claude Code UI drift surfaces as a golden-match failure on the
// canary test in state_canary_test.go.
//
// The substring intentionally EXCLUDES the leading spinner glyph: the
// 8% capture shows `✻ Compacting conversation…` (U+273B six-pointed
// black star) while the 68% capture shows `✢ Compacting conversation…`
// (U+2722 four teardrops-spoked asterisk). The glyph cycles across
// spinner frames; the trailing phrase is the stable load-bearing
// substring. The ellipsis is U+2026, painted as a single codepoint.
//
// The phrase alone is NOT matched as a bare whole-pane substring: it is
// NOT structurally unique against transcript text — a chamber discussing
// compaction (or working on this code) writes "Compacting conversation…"
// in ordinary messages, which the original bare-substring match read as
// mid-/compact, deferring all inbound delivery (#647). The match
// (capturedLiveCompaction) therefore requires the marker's live-elapsed-
// timer parenthetical ("<marker> (<digit>…"), which survives the spinner
// animation and which prose-quotes of the phrase lack.
//
// Precedence in AgentState: this check runs BEFORE the pane-equality
// "working" check (precedence 1 vs 2) so a agent mid-compaction — a
// pane whose spinner is animating across the temporal-delta window and
// would otherwise classify as Working — is correctly identified as
// AtRestInCompaction. The two captures at different progress points
// pin this precedence in TestAgentState_AtRestInCompactionOnGolden.
//
// FORWARD-WATCH (same shape as PromptSentinel + AwaitingOperatorMarker):
// Claude-Code-version-dependent. If the compaction UI's phrase changes
// across a Claude Code version update, this constant + both golden
// fixtures need re-verification. The canary test surfaces the drift
// loudly.
const CompactionMarker = "Compacting conversation…"

// AwaitingOperatorMarker is the substring that identifies a agent in
// StateAwaitingOperator (AskUserQuestion popups, selection menus, …).
//
// Empirically captured 2026-06-04 from a Quartermaster pane displaying
// a live AskUserQuestion popup (#79). The captured pane
// content is frozen as testdata/golden_quartermaster_askuserquestion_
// 2026-06-04.txt so future Claude Code UI drift surfaces as a golden-
// match failure on the canary test in state_canary_test.go.
//
// The substring is the popup's bottom-line keybinding hint —
// "↑/↓ to navigate · Esc to cancel" — combined with the middle-dot
// separator. The combination is structurally unique to Claude Code's
// popup UI: regular chat / response text never emits keybinding hints
// with U+00B7 middle-dot separators. Catches both single-select and
// multi-select popup variants because both end with the same footer.
//
// FORWARD-WATCH (same shape as PromptSentinel + CompactionMarker):
// Claude-Code-version-dependent. If the popup UI's footer changes
// across a Claude Code version update, this constant + the golden
// fixture both need re-verification. The canary test surfaces the
// drift loudly.
const AwaitingOperatorMarker = "↑/↓ to navigate · Esc to cancel"

// agentStateTemporalDelta is the wait between the two capture-pane
// calls in AgentState. 200ms is long enough to catch typical
// streaming-output changes + spinner animations (most Claude Code
// spinners tick at ~80-100ms intervals) and short enough that probing
// a working agent doesn't add meaningful latency to the caller's
// flow. False-negatives on agents running long-running tools whose
// only paint is a 1Hz spinner counter are an accepted risk for v1 —
// the ObserveGate's poll loop catches a working-pane mis-classified
// as idle at the delivery layer via subsequent iterations.
var agentStateTemporalDelta = 200 * time.Millisecond

// SetAgentStateTemporalDeltaForTest swaps the temporal-delta wait
// for tests so the suite doesn't pay 200ms per AgentState call.
// Returns the previous value for cleanup restoration. Sibling to
// SetSettleDelayForTest.
func SetAgentStateTemporalDeltaForTest(d time.Duration) time.Duration {
	prev := agentStateTemporalDelta
	agentStateTemporalDelta = d
	return prev
}

// AgentState classifies the receiving pane's current activity by
// inspecting two consecutive capture-pane snapshots + the tmux cursor
// position and applying a precedence-ordered heuristic.
//
// Substrate-class: read-only-observe. Exactly two capture-pane calls,
// one display-message call, zero send-keys, zero pane mutation.
// Pinned by TestAgentState_NoPaneMutation in the test suite. "Knock
// at the door without waking the inhabitant" per #69's
// framing — all three tmux calls are read-only (capture-pane reads
// the visible buffer; display-message reads tmux's internal pane
// state).
//
// Heuristic v2 (#69 smoke test surfaced the v1 gap on
// cursor-less classification; operator's design call 2026-06-04
// resolved it via cursor-position awareness):
//
//  1. If either capture fails → StateUnknown + the wrapped error.
//  2. If CompactionMarker is non-empty AND found in capture B →
//     StateAtRestInCompaction. Lit up 2026-06-04 (#70, PR #88). This
//     precedence over working is load-bearing: a agent mid-compaction
//     is animating (spinner glyph cycles, percentage ticks) so capA
//     != capB; without the marker check firing first, the agent
//     would mis-classify as Working.
//  3. If the active UsageLimitPattern matches capture B →
//     StateUsageLimited. The regex is adapter-profile-owned and MUST be
//     validated against a real pane sample (#540). This is the hard-stop
//     sibling to rate-limit: it runs before rate-limit so a usage-limit pane
//     can't be shadowed by a broader cooldown banner.
//  4. If the active RateLimitPattern matches capture B → StateRateLimited.
//     The regex is adapter-profile-owned and MUST be validated against a
//     real pane sample (#504). This runs before Working because a rate-limit
//     pane may animate a countdown; it runs after compaction and usage-limit
//     because those are more specific local TUI modes.
//  5. If capture A != capture B → StateWorking. Any substantive change
//     across the temporal-delta window means the agent is painting.
//  6. **Cursor-position-aware input-row classification** (the v2 gap-fix):
//     query the cursor position via display-message; identify the row
//     the cursor sits on; if that row starts with PromptSentinel:
//     - Cursor at sentinel position (col == sentinel-width): the
//     input area's input position. If row is empty past the
//     sentinel → StateIdle (clean prompt). If row has content past
//     the sentinel → StateIdle as well (Claude Code auto-suggestion
//     ghost text; operator hasn't engaged — cursor would have moved
//     past the content if they had).
//     - Cursor past sentinel position: operator is mid-typing →
//     StateAwaitingOperator (agent blocked on operator finishing
//     their draft).
//     - Cursor before sentinel position: unusual; treat as Unknown.
//  7. If AwaitingOperatorMarker is non-empty AND found in capture B →
//     StateAwaitingOperator. (Backup detection for non-`❯`-painting
//     UIs — AskUserQuestion popups, search dialogs, etc.)
//  8. If the cursor query failed or the cursor row doesn't start with
//     PromptSentinel, fall back to the cursor-less heuristic
//     (isInputRowQuiet returns true → Idle; else Unknown). This
//     preserves classification when the cursor substrate is
//     unreachable.
//  9. Otherwise → StateUnknown with an accurate reason naming the
//     sub-case that fired (sentinel found vs not, cursor query failure
//     vs cursor-not-on-input-row).
//
// PromptSentinel is the Claude-Code-version-pinned constant; see its
// doc-comment for the forward-watch on Claude Code TUI changes.
//
// Errors: capture-pane failures propagate via the error return value
// paired with StateUnknown — the safer-default-on-uncertainty contract
// from the #65 playbook applied at the detection layer.
// Cursor query failures are non-fatal (the heuristic gracefully
// degrades to the cursor-less path); only capture-pane failures bubble
// up as errors.
func AgentState(ctx context.Context, pane string) (state State, ev Evidence, err error) {
	if pane == "" {
		return StateUnknown, Evidence{Reason: "pane required"},
			errors.New("tmuxio: pane required")
	}

	// #537: capture a precedence-0 pane_in_mode query *error* and stamp it onto
	// whatever Evidence the classification ultimately returns, via this deferred
	// decorator. Doing it once here — rather than at each of the ~7 classification
	// return sites — means every path (current and future) carries the flag
	// without a return site having to remember to set it. The named return `ev`
	// is what `return State, ev, nil` assigns to before this defer runs.
	var copyModeQueryFailed bool
	defer func() {
		if copyModeQueryFailed {
			ev.CopyModeQueryFailed = true
		}
	}()

	// Precedence 0: copy-mode / scroll-back (#526). Query pane_in_mode BEFORE
	// the capture-pane snapshots — capture-pane on a scrolled pane reads the
	// HISTORICAL view, so the heuristic below would read stale content (an old
	// `❯ ` scrolled into frame misclassifies as Idle → paste into a scrolled
	// pane, the 83b3 bug). The display-message query is cheap + authoritative
	// and reflects the live pane regardless of scroll position.
	//
	// QUERY ERROR (Surveyor #535 review, closed by #537): a query *error* here
	// falls through to the capture-based path, which is the pre-#526
	// 83b3-susceptible classifier — AND because the returned state is not
	// StateInCopyMode, the IsPasteUnsafe belt doesn't catch it either. So both
	// defense layers depend on this query succeeding. A *single* transient
	// display-message hiccup still degrades to that path (acceptable — the pane
	// is almost always fine, and reproducing 83b3 needs a 4-way conjunction:
	// scrolled pane + query error + an old `❯ ` scrolled into frame + a stable
	// two-capture window). The close for a *persistent* failure: stamp
	// Evidence.CopyModeQueryFailed (via the deferred decorator above) so the
	// gate loop can count *consecutive* failures and bias a genuinely-unreadable
	// pane toward defer — distinguishing a transient hiccup (degrade) from a pane
	// the query truly can't read (defer). See observe_gate.go's
	// copyModeQueryFailDeferThreshold.
	if inMode, merr := PaneInCopyMode(ctx, pane); merr != nil {
		copyModeQueryFailed = true
	} else if inMode {
		return StateInCopyMode,
			Evidence{Reason: "pane in copy-mode / scrolled up (pane_in_mode=1)"}, nil
	}

	capA, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("first capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: agent-state capture #1: %w: %s",
				err, strings.TrimSpace(string(capA)))
	}

	select {
	case <-time.After(agentStateTemporalDelta):
	case <-ctx.Done():
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("context cancelled during temporal-delta wait: %v", ctx.Err())},
			ctx.Err()
	}

	capB, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return StateUnknown,
			Evidence{Reason: fmt.Sprintf("second capture-pane failed: %v", err)},
			fmt.Errorf("tmuxio: agent-state capture #2: %w: %s",
				err, strings.TrimSpace(string(capB)))
	}

	capAStr := string(capA)
	capBStr := string(capB)

	// Precedence 1: compaction marker (from the active PaneProfile; empty
	// disables the check for an adapter with no compaction UI). The match
	// requires the marker's LIVE-timer parenthetical, not the bare phrase, so a
	// chamber writing "Compacting conversation…" in a message (e.g. one working
	// on this very code) doesn't false-positive as mid-/compact — the
	// transcript-sentinel wedge that deferred all inbound delivery (#647).
	if m := activeProfile.CompactionMarker; m != "" && capturedLiveCompaction(capBStr, m) {
		return StateAtRestInCompaction,
			Evidence{
				Reason: fmt.Sprintf("compaction marker found: %q", m),
				Marker: m,
			}, nil
	}

	// Precedence 2: empirical usage-limit regex (#540). The configured pattern
	// stays empty until real adapter pane output is captured; synthetic tests
	// can still exercise the mechanism without guessing production literals.
	if m := firstUsageLimitMatch(capBStr); m != "" {
		return StateUsageLimited,
			Evidence{
				Reason: fmt.Sprintf("usage-limit pattern matched: %q", m),
				Marker: m,
			}, nil
	}

	// Precedence 3: empirical rate-limit regex (#504). The configured pattern
	// stays empty until real adapter pane output is captured; synthetic tests
	// can still exercise the mechanism without guessing production literals.
	if m, retryAfter := firstRateLimitMatch(capBStr); m != "" {
		return StateRateLimited,
			Evidence{
				Reason:     fmt.Sprintf("rate-limit pattern matched: %q", m),
				Marker:     m,
				RetryAfter: retryAfter,
			}, nil
	}

	// Precedence 4: positive working marker (from the active PaneProfile; empty
	// parks the check). An adapter whose active turn renders a persistent status
	// marker is classified Working from that marker directly — BEFORE the
	// temporal-delta frame-change heuristic and the cursor-aware idle logic
	// below. Codex renders `◦ Working (Ns • esc to interrupt)` throughout a turn;
	// its only per-second delta is the elapsed counter, so a 200ms capture pair
	// can read the frame as stable and the cursor-at-sentinel branch would
	// false-idle the active turn (#590). Keying the marker here makes the
	// positive busy signal win over frame-stability. Additive: Claude's empty
	// WorkingPattern parks it, so the Claude path is unchanged.
	if m := firstWorkingMarkerMatch(capBStr); m != "" {
		return StateWorking,
			Evidence{
				Reason: fmt.Sprintf("working marker matched: %q", m),
				Marker: m,
			}, nil
	}

	// Precedence 5: working (any substantive change across the window).
	if capAStr != capBStr {
		return StateWorking,
			Evidence{
				Reason:           "pane content changed across temporal-delta window",
				ChangedLineCount: countChangedLines(capAStr, capBStr),
			}, nil
	}

	// Cursor-position-aware classification (the v2 substrate per
	// #69 operator's design call 2026-06-04). Query the
	// cursor; if it sits on a row that starts with PromptSentinel,
	// distinguish auto-suggestion (cursor at sentinel) from operator-
	// drafting (cursor past sentinel) — the two cases the v1 heuristic
	// conflated as "non-empty input row".
	sentinel := activeProfile.PromptSentinel
	cursorX, cursorY, cursorErr := agentCursor(ctx, pane)
	if cursorErr == nil && sentinel != "" {
		lines := strings.Split(capBStr, "\n")
		if cursorY >= 0 && cursorY < len(lines) {
			row := lines[cursorY]
			rest, hasSentinel := cutPromptSentinel(row, sentinel)
			if hasSentinel {
				sentinelCol := utf8.RuneCountInString(sentinel)
				switch {
				case cursorX == sentinelCol:
					// Cursor right after `❯ ` — either a clean idle
					// prompt or an auto-suggestion ghost-text. Both
					// classify as Idle because the operator hasn't
					// engaged (cursor would have moved past content
					// if they had been typing).
					idleEv := Evidence{
						Reason:      "cursor at prompt sentinel position; pane stable",
						PromptEmpty: strings.TrimSpace(rest) == "",
					}
					if !idleEv.PromptEmpty {
						idleEv.Reason = fmt.Sprintf("cursor at prompt sentinel position with auto-suggestion ghost-text (%q); pane stable", strings.TrimSpace(rest))
					}
					return StateIdle, idleEv, nil
				case cursorX > sentinelCol:
					// Cursor past the sentinel — operator is mid-
					// typing. Agent is blocked on operator finishing
					// the draft (or clearing it). Same consumer-side
					// semantics as AskUserQuestion popup: don't
					// dispatch into this state.
					return StateAwaitingOperator,
						Evidence{
							Reason: fmt.Sprintf("cursor past prompt sentinel (col %d > %d); operator mid-typing", cursorX, sentinelCol),
						}, nil
				}
				// Cursor before sentinel position on the sentinel row
				// is unusual; fall through to marker / unknown checks.
			}
		}
	}

	// Precedence 7: awaiting-operator marker (backup for non-sentinel-
	// painting UIs — AskUserQuestion popups, search dialogs, etc.). From the
	// active PaneProfile; empty disables the backup check.
	if m := activeProfile.AwaitingOperatorMarker; m != "" && strings.Contains(capBStr, m) {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("awaiting-operator marker found: %q", m),
				Marker: m,
			}, nil
	}

	// Cursor-less fallback (cursor query failed or cursor row doesn't
	// have the sentinel): parse the pane for a sentinel-row with no
	// content past it. Used when display-message isn't available or
	// the cursor is somewhere other than the input row (e.g., agent
	// paused mid-spinner).
	if isInputRowQuiet(capBStr) {
		return StateIdle,
			Evidence{
				Reason:      "prompt sentinel found with empty input row (cursor-less fallback); pane stable",
				PromptEmpty: true,
			}, nil
	}

	// Default: unknown. Distinguish two sub-cases for accurate evidence:
	//   - sentinel found with content past it (but cursor not at input row)
	//   - sentinel not found at all
	hasSentinelInPane := sentinel != "" && strings.Contains(capBStr, sentinel)
	cursorNote := ""
	if cursorErr != nil {
		cursorNote = fmt.Sprintf(" (cursor query failed: %v)", cursorErr)
	}
	if hasSentinelInPane {
		return StateUnknown,
			Evidence{Reason: "pane stable; prompt sentinel found but cursor not at input row + no recognized marker" + cursorNote},
			nil
	}
	return StateUnknown,
		Evidence{Reason: "pane stable; prompt sentinel not found in any row + no recognized marker" + cursorNote},
		nil
}

func firstRateLimitMatch(capture string) (string, time.Duration) {
	if activeRateLimitRE == nil {
		return "", 0
	}
	matches := activeRateLimitRE.FindStringSubmatch(capture)
	if len(matches) == 0 {
		return "", 0
	}
	matched := matches[0]
	if idx := activeRateLimitRE.SubexpIndex("retry_seconds"); idx >= 0 && idx < len(matches) {
		if d, err := parseRetrySeconds(matches[idx]); err == nil {
			return matched, d
		}
	}
	return matched, 0
}

func firstUsageLimitMatch(capture string) string {
	if activeUsageLimitRE == nil {
		return ""
	}
	matches := activeUsageLimitRE.FindStringSubmatch(capture)
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// firstWorkingMarkerMatch returns the active profile's working-marker match in
// the capture, or "" when the profile parks the check (empty WorkingPattern, as
// Claude does) or the marker is absent. Positive in-turn busy detection (#590).
func firstWorkingMarkerMatch(capture string) string {
	if activeWorkingRE == nil {
		return ""
	}
	return activeWorkingRE.FindString(capture)
}

func parseRetrySeconds(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty retry_seconds")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	s = strings.TrimSuffix(s, "s")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(f * float64(time.Second)), nil
}

// PaneInCopyMode reports whether the pane is currently in a tmux mode
// (copy-mode / view-mode) — i.e. the operator has scrolled up off the live
// prompt (#526). Queries `display-message -p '#{pane_in_mode}'`, which tmux
// renders as "1" when the pane is in ANY mode and "0" otherwise; the boolean
// covers copy-mode, copy-mode-vi, and view-mode without enumerating
// mode-name variants that differ by tmux config. Read-only: one
// display-message call, zero pane mutation.
//
// Used at AgentState precedence 0 (it MUST run before the capture-pane
// snapshots — capture-pane on a scrolled pane reads the historical view) and
// by the inbox/status surface to live-derive the pane_in_copy_mode deferral
// reason.
func PaneInCopyMode(ctx context.Context, pane string) (bool, error) {
	if pane == "" {
		return false, errors.New("tmuxio: pane required")
	}
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{pane_in_mode}")
	if err != nil {
		return false, fmt.Errorf("tmuxio: pane-in-mode query: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// agentCursor queries the tmux cursor position for the pane. Returns
// (cursorX, cursorY, error). tmux's cursor_x is column 0-indexed,
// cursor_y is row 0-indexed from the top of the visible pane. A single
// display-message call returns both values as "X/Y" for parse-once
// efficiency.
//
// Errors here are non-fatal at the AgentState layer — the algorithm
// gracefully degrades to the cursor-less heuristic when the cursor
// substrate is unreachable.
func agentCursor(ctx context.Context, pane string) (int, int, error) {
	out, err := tmuxRun(ctx, nil, "display-message", "-p", "-t", pane, "#{cursor_x}/#{cursor_y}")
	if err != nil {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor query: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor parse: unexpected format %q", string(out))
	}
	x, errX := strconv.Atoi(parts[0])
	y, errY := strconv.Atoi(parts[1])
	if errX != nil || errY != nil {
		return 0, 0, fmt.Errorf("tmuxio: agent-state cursor parse: %q", string(out))
	}
	return x, y, nil
}

// countChangedLines returns the number of lines that differ between
// the two captures. Cheap line-by-line walk; used only to populate
// Evidence.ChangedLineCount for the StateWorking branch — not load-
// bearing for classification (the byte-equality check above is the
// authoritative test).
func countChangedLines(a, b string) int {
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")
	max := len(aLines)
	if len(bLines) > max {
		max = len(bLines)
	}
	diff := 0
	for i := 0; i < max; i++ {
		var aLine, bLine string
		if i < len(aLines) {
			aLine = aLines[i]
		}
		if i < len(bLines) {
			bLine = bLines[i]
		}
		if aLine != bLine {
			diff++
		}
	}
	return diff
}

// isInputRowQuiet walks the captured pane and returns true when at
// least one row starts with PromptSentinel AND that row has only
// whitespace past the sentinel. False otherwise (no sentinel row
// found, or sentinel row has non-whitespace content). Used by the
// cursor-less fallback path in AgentState when display-message
// fails or the cursor isn't on the input row.
func isInputRowQuiet(paneContent string) bool {
	sentinel := activeProfile.PromptSentinel
	if sentinel == "" {
		return false
	}
	sawSentinel := false
	for _, row := range strings.Split(paneContent, "\n") {
		rest, found := cutPromptSentinel(row, sentinel)
		if !found {
			continue
		}
		sawSentinel = true
		if strings.TrimSpace(rest) != "" {
			return false
		}
	}
	return sawSentinel
}

// cutPromptSentinel splits a captured row on the active prompt sentinel,
// tolerating `tmux capture-pane -p`'s trailing-whitespace strip. A sentinel
// ending in a REGULAR space (Codex's `› `) is captured as its right-trimmed
// form (bare `›`) when the composer is EMPTY — no ghost-text or content follows
// the space, so capture-pane strips it — and a literal CutPrefix(row, "› ")
// then misses the empty-idle composer, falling through to StateUnknown
// ("prompt sentinel not found in any row") for a genuinely-idle pane (#690,
// operator-witnessed). When the row equals the right-trimmed sentinel, treat it
// as the sentinel with empty rest.
//
// Scoped to space-terminated sentinels: Claude's sentinel ends in NBSP
// (U+00A0), which capture-pane does NOT strip, so `TrimRight(sentinel, " ")`
// leaves it unchanged and this tolerance is inert for Claude — its empty
// composer keeps the NBSP and matches via the plain CutPrefix above. (That
// NBSP, chosen to avoid sentinel word-wrap, incidentally immunizes Claude
// against this strip; Codex's plain 0x20 is the vulnerable case.)
func cutPromptSentinel(row, sentinel string) (rest string, found bool) {
	if rest, ok := strings.CutPrefix(row, sentinel); ok {
		return rest, true
	}
	if trimmed := strings.TrimRight(sentinel, " "); trimmed != sentinel && row == trimmed {
		return "", true
	}
	return "", false
}

// inputRowCleared reports whether the captured pane shows the live input
// row empty, anchored on the CURSOR position (#336 cursor-anchor fix).
//
// The cursor is the only reliable empty-input signal for adapters that
// paint placeholder / auto-suggestion ghost-text into an EMPTY composer.
// Codex renders a dim example prompt (e.g. "Improve documentation in
// @filename") into its empty input row — the "idle ghost-text" state
// profile.go documents — which a plain-text emptiness scan of
// `capture-pane -p` misreads as a POPULATED input, false-negativing the
// verify (the exact `delivered_in_input_box verified=0` failure #336 set
// out to fix). A plain-text scan cannot tell dim ghost-text from a real
// buffered paste; the cursor can — it stays at the sentinel column when the
// input is genuinely empty and moves past it once content (a buffered
// paste, operator typing) is present. This is the same discriminator
// AgentState's cursor-aware idle classification uses (#69 v2 substrate);
// the verify signal now reuses it rather than AgentState's cursor-LESS
// fallback (which the original #336 floor adopted as its primary check —
// the regression this fix corrects).
//
// Using the cursor's row (not a bottom-most scan) also subsumes the
// transcript-sentinel problem the bottom-most rule was guarding against:
// codex paints every submitted turn with the same `› ` glyph, but the
// cursor sits only on the live input, so cursorY anchors the editable row
// directly.
//
// anchored is false when the cursor can't anchor the input row — cursor
// query failed (cursorOK false), no sentinel configured, the cursor row is
// outside the capture's range, or that row doesn't start with the sentinel.
// The caller then falls back to the legacy token-match verify signal.
func inputRowCleared(capture string, cursorX, cursorY int, cursorOK bool) (cleared, anchored bool) {
	sentinel := activeProfile.PromptSentinel
	if !cursorOK || sentinel == "" {
		return false, false
	}
	lines := strings.Split(capture, "\n")
	if cursorY < 0 || cursorY >= len(lines) {
		return false, false
	}
	if _, ok := strings.CutPrefix(lines[cursorY], sentinel); !ok {
		return false, false
	}
	// Cursor at the sentinel column ⇒ empty input (ghost-text doesn't move
	// the cursor). Cursor past it ⇒ a buffered paste or an operator draft.
	return cursorX == utf8.RuneCountInString(sentinel), true
}

// capturedLiveCompaction reports whether capture shows Claude Code's LIVE
// compaction UI rather than transcript prose that merely quotes the marker
// phrase. The live UI renders the marker with an animated spinner-glyph prefix
// AND a live-elapsed-timer parenthetical:
//
//	✻ Compacting conversation… (7s · ↑ 2.9k tokens)
//	✢ Compacting conversation… (1m 42s · ↑ 2.9k tokens)
//
// The spinner glyph animates across a set we don't enumerate (✻ U+273B, ✢
// U+2722, …), so the marker deliberately excludes it; but the bare phrase alone
// is NOT structurally unique — a chamber discussing compaction (or working on
// this code) writes "Compacting conversation…" in ordinary message text, which
// the old whole-pane substring match read as mid-/compact → IsPasteUnsafe → all
// inbound delivery deferred (#647, reproduced live).
//
// The live-timer parenthetical is the structural anchor: it survives the
// spinner animation (always present once compaction starts) and prose-quotes of
// the phrase lack it. Requiring "<marker> (<digit>" — the phrase, a space, the
// open paren, then the elapsed-timer's leading digit — admits the live UI while
// rejecting prose like "Compacting conversation… (the marker)". marker is the
// profile's CompactionMarker; the caller's m != "" guard disables the check for
// adapters with no compaction UI (codex).
//
// Residual: a message quoting the FULL live line (phrase + "(7s …") would still
// match — far rarer than quoting the bare phrase, so this collapses the
// false-positive surface rather than eliminating it.
func capturedLiveCompaction(capture, marker string) bool {
	for i := 0; ; {
		j := strings.Index(capture[i:], marker)
		if j < 0 {
			return false
		}
		rest := capture[i+j+len(marker):]
		if strings.HasPrefix(rest, " (") && len(rest) > 2 && rest[2] >= '0' && rest[2] <= '9' {
			return true
		}
		i += j + len(marker)
	}
}

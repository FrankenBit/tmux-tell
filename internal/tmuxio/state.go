package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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

// ASCIIPromptSentinel is the Windows-11 render-variant of the Claude Code
// prompt — `>` (U+003E) followed by a NO-BREAK SPACE (U+00A0), hex `3e c2 a0`.
// A Claude CLI session running under a Windows 11 terminal paints its prompt
// this way rather than the Linux `❯ `: **only the ornament glyph
// substitutes** (U+276F `❯` -> U+003E `>`); the NBSP trailer is IDENTICAL on
// both platforms. That was measured wrong the first time — #786 shipped this as
// `>` + a REGULAR space (`3e 20`), inheriting a mis-stated byte from the #729
// investigation, and it NEVER matched a live pane: capture-pane preserves the
// NBSP, so a `3e 20` literal cannot match a `3e c2 a0` row. Corrected against
// three live captures on 2026-07-21 (Caymans Admin pane %11 composer row, cursor
// at col 2: `3e c2 a0` + ghost-text; Bosun's + Admin's independent hexdumps
// agreed). ssh is a byte-transparent tunnel, so the variance is render-side, not
// transport — ANY Win11-hosted Claude pane hits this, not only ssh-relayed ones.
//
// Carried in the Claude PaneProfile's PromptSentinelVariants and matched ONLY in
// the cursor-aware AgentState path — see that field's doc for why a `>` match is
// trusted only with cursor corroboration. Like the Linux primary, its NBSP
// trailer survives `capture-pane -p` (which strips regular trailing whitespace
// but not NBSP), so it matches via a plain prefix cut — the #690 space-strip
// tolerance is inert for it, exactly as for the primary sentinel.
//
// FORWARD-WATCH: like PromptSentinel this is Claude-Code-render-dependent; the
// byte canary TestASCIIPromptSentinel_Bytes pins `3e c2 a0` so a future drift
// surfaces loudly.
const ASCIIPromptSentinel = ">\u00a0"

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
	// StateErrored means the adapter pane is showing Claude Code's TERMINAL
	// API-error chrome (#719) — an `● API Error: <code> …` line left painted in
	// the current-turn region after a request gave up, with the composer `❯`
	// prompt row preserved below it. That preserved prompt is exactly why this
	// is a FALSE-IDLE: the cursor-at-sentinel branch (P6) reads the pane as Idle,
	// the observe-gate flushes, and the mailman pastes into a DEAD turn — the
	// paste lands as ghost text nobody submits (the #719 anchor). This is an
	// ALERT-class state, NOT a healthy hold: unlike StateAwaitingOperator (an
	// operator-interaction hold) the turn has FAILED and needs attention/retry.
	//
	// Detection is live-scoped (capturedLiveErrorChrome): the error line must
	// sit in the CURRENT-turn region — strictly between the bottom-most
	// transcript `❯`-prompt and the composer — so error phrasing quoted in deep
	// scrollback does not false-fire, and an active 529-retry (which still shows
	// `esc to interrupt` in its footer) is deliberately SUPPRESSED so it stays
	// Working. Paste-unsafe/content-corrupting (IsPasteUnsafeForced): pasting
	// into the dead turn corrupts operator-visible content and is never
	// force-overridable.
	StateErrored
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
	case StateErrored:
		return "errored"
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
//   - StateErrored: the adapter is showing terminal API-error chrome (#719) —
//     a failed turn whose composer prompt is preserved (the false-idle). A
//     paste lands in the DEAD turn as unsubmitted ghost text; content-
//     corrupting and never force-overridable (IsPasteUnsafeForced).
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
		s == StateAtRestInCompaction || s == StateInCopyMode ||
		s == StateErrored
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

// AwaitingOperatorMarker is the substring that identifies an agent in
// StateAwaitingOperator (AskUserQuestion popups, the /mcp server picker,
// selection menus — any arrow-navigation modal, …).
//
// Empirically captured 2026-06-04 from a Quartermaster pane displaying a
// live AskUserQuestion popup (#79), re-confirmed 2026-07-22 against a live
// /mcp server-picker on Pilot's pane %6 (#719-B). Both captures are frozen
// under testdata/ so future Claude Code UI drift surfaces as a golden-match
// failure on the canary test in state_canary_test.go.
//
// The substring is the picker footer's NAVIGATION-HINT CORE —
// "↑/↓ to navigate ·" — the arrow keys + "to navigate" + the U+00B7
// middle-dot separator. That combination is structurally unique to Claude
// Code's picker chrome: regular chat / response text never combines ↑/↓ +
// "to navigate" + a middle-dot. The middle-dot is retained deliberately as
// the uniqueness anchor — dropping it would risk the #647-class prose-quote
// false-match (a chamber discussing this very code writes "↑/↓ to navigate"
// in ordinary prose; the trailing " ·" separator is what prose lacks).
//
// BROADENED 2026-07-22 (#719-B) from the full AskUserQuestion footer
// "↑/↓ to navigate · Esc to cancel" to just this core. The /mcp server
// picker's footer is "↑/↓ to navigate · Enter to confirm · Esc to cancel" —
// the "· Enter to confirm ·" wedged between "navigate" and "Esc" made the old
// full-footer substring fail to match, so a live /mcp modal fell through every
// branch to StateUnknown (live-probed 2026-07-22: state=unknown, "prompt
// sentinel not found in any row + no recognized marker"). Unknown is already
// paste-unsafe (IsPasteUnsafeForced), so this was never a clobber; the value
// is PRECISION — reclassifying a healthy operator-interaction hold out of the
// catch-all Unknown bucket into the specific StateAwaitingOperator, so Unknown
// stays reserved for genuinely-suspect / frozen panes (what #719's
// freshness/alert layer needs to trust).
//
// The broadening is MONOTONIC: a paste-UNSAFE marker can only ADD
// awaiting-operator classifications (deferrals), never remove one, so it
// cannot introduce a new clobber by construction (same safety shape as the
// #332 hoist). It still matches every AskUserQuestion footer (both select
// variants end with "↑/↓ to navigate · …"), so the #79/#133 canaries stay
// green.
//
// SCOPE: this marker covers arrow-navigation pickers only. #719's primary
// anchor — Claude Code restart-mode ("connection lost, y to restart") — has
// no arrow chrome and is semantically an ERROR state that should ALERT, not a
// healthy hold; it wants its own marker/state and remains fixture-gated in
// #719 (no restart fixture captured yet — do not fold it in blind).
//
// FORWARD-WATCH (same shape as PromptSentinel + CompactionMarker):
// Claude-Code-version-dependent. If the picker UI's footer changes across a
// Claude Code version update, this constant + the golden fixtures need
// re-verification. The canary test surfaces the drift loudly.
const AwaitingOperatorMarker = "↑/↓ to navigate ·"

// APIErrorMarker is the substring that anchors the StateErrored classification
// (#719) — Claude Code's terminal API-error line, e.g.
//
//	● API Error: 529 Overloaded. This is a server-side issue, usually temporary…
//
// Empirically captured 2026-07-22 from Bosun's live pane (%…) at the moment a
// 529 gave up — frozen as testdata/golden_bosun_api_error_529_2026-07-22.txt so
// future Claude Code UI drift surfaces as a golden-match failure on
// TestAPIErrorMarker_MatchesGoldenCapture.
//
// This bare "API Error:" substring is NOT matched whole-pane: like
// CompactionMarker it is not structurally unique (a chamber discussing an API
// error — or working on this code — writes "API Error:" in ordinary text). The
// live-scope + regex discipline lives in capturedLiveErrorChrome, which (a)
// scopes the match to the CURRENT-turn region (strictly between the bottom-most
// transcript `❯`-prompt and the composer, so scrollback prose can't fire it),
// (b) requires a 3-digit status code via apiErrorLineRE (`API Error: \d`, so
// codeless prose is rejected), and (c) suppresses on `esc to interrupt` in the
// footer (an active retry stays Working, not Errored).
//
// FORWARD-WATCH (same shape as PromptSentinel + CompactionMarker +
// AwaitingOperatorMarker): Claude-Code-version-dependent. If the API-error line
// phrasing changes across a Claude Code version update, this constant + the
// golden fixture need re-verification. The canary test surfaces the drift
// loudly.
const APIErrorMarker = "API Error:"

// ResumeModalMarker is the keybind-legend footer of Claude Code's session-resume
// choice modal (#719) — the full-screen picker shown when `claude` is launched
// with more than one resumable session and no `--resume <id>`. The
// chamber-claude.sh wrapper's UUID-bypass normally suppresses it; a raw `claude`
// launch surfaces it (which is how the fixture was captured). Empirically
// captured 2026-07-24 from Pilot's pane at %15 (operator triggered it via raw
// claude; Bosun byte-captured), frozen as
// testdata/golden_pilot_resume_modal_2026-07-24.txt so Claude Code UI drift
// surfaces as a golden-match failure on TestResumeModalMarker_MatchesGoldenCapture.
//
// The modal consumes paste-and-enter deliveries as its search-box input (Enter
// selects the highlighted session), so a chamber sitting at it must classify
// paste-UNSAFE (StateAwaitingOperator) or the mailman resumes the wrong session /
// drops the delivery silently — the multi-hour-silence failure #719 tracks.
//
// This legend is NOT matched as a bare whole-pane substring: like
// CompactionMarker / APIErrorMarker it is prose-collidable — a chamber quoting
// the modal (the #719 dispatch itself does) writes it in ordinary message text,
// which a whole-pane match would read as a live modal → defer ALL inbound
// delivery (the #647 / #852 class). MEASURED 2026-07-24: this legend + the header
// "Resume session" + the ⌕ glyph co-occur in ordinary bus prose (2 messages at
// first measure, growing as the marker is discussed), so even a header+footer
// co-occurrence rule false-fires. The discipline that closes it lives in
// capturedResumeModal — footer legend + a whitespace-gated search widget +
// live-scope. The DURABLE guards are the footer and live-scope; see that function
// for why the widget alone is not prose-proof.
//
// FORWARD-WATCH (same shape as PromptSentinel / CompactionMarker /
// AwaitingOperatorMarker / APIErrorMarker): Claude-Code-version-dependent. If the
// modal's footer legend is reworded across a Claude Code version update, this
// constant + the golden fixture need re-verification. The canary surfaces it.
const ResumeModalMarker = "Type to Search · Enter to select · Esc to clear"

// resumeModalSearchGlyph (⌕ U+2315) is the magnifier Claude Code paints inside
// the resume modal's search field; resumeModalBoxVertical (│ U+2502) is that
// field's rounded-box side border. The field renders as "│ ⌕ …" — a vertical,
// WHITESPACE, then the glyph — and capturedResumeModal requires that whitespace
// gap (see there). The gap is load-bearing: without it the gate matched the prose
// shorthand "│+⌕" (`+` is 0x2b, not whitespace) that chambers type when they
// discuss this detector. MEASURED 2026-07-24: a same-row │…⌕ went from 0 to 4 of
// ~19400 messages DURING the #854 review — the self-referential-marker
// temporal-extension landing on this very widget. Requiring the whitespace keeps
// the golden and excludes that shorthand — but a FAITHFUL paste of the modal row
// still reproduces "│ ⌕", so the widget narrows the surface without being
// independently prose-proof. The durable guards are the footer legend + live-scope
// (capturedResumeModal facts 1 and 3).
const (
	resumeModalSearchGlyph = "⌕" // U+2315
	resumeModalBoxVertical = "│" // U+2502
)

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
//     The cursor is queried once (via display-message) at this point, and
//     its row/column feed both 5a and 6 below.
//     5a. **Operator-drafting wins over frame-change** (#332): if the cursor
//     sits STRICTLY past the sentinel on the input row, the operator is
//     mid-typing → StateAwaitingOperator, BEFORE the frame-change check.
//     A drafting operator repaints the input row each keystroke (capA !=
//     capB), but Claude's busy states (streaming/spinner) keep the cursor
//     AT the sentinel (measured 156/156), so past-sentinel is unambiguous
//     drafting and must not be swallowed by 5's StateWorking (else the
//     mailman pastes into the half-typed draft — the witnessed clobber).
//  5. If capture A != capture B → StateWorking. Any substantive change
//     across the temporal-delta window means the agent is painting. Runs
//     after 5a so a drafting operator isn't mis-read as painting; a cursor
//     AT the sentinel on a changing frame is streaming and correctly lands
//     here.
//  6. **Cursor-at-sentinel on a STABLE frame → Idle** (the v2 gap-fix):
//     using the cursor queried at step 5, if the cursor row starts with
//     PromptSentinel and the cursor is AT the sentinel position (col ==
//     sentinel-width): StateIdle whether the row is empty past the sentinel
//     (clean prompt) or has content past it (Claude Code auto-suggestion
//     ghost text; operator hasn't engaged — cursor would have moved past the
//     content if they had). Below 5 so a cursor parked at the sentinel while
//     the frame streams is caught as Working, not mis-idled. Cursor before
//     the sentinel position is unusual; falls through to marker / unknown.
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
//
// AgentState reads the process-global activeProfile installed by cli.Run at
// startup. It is the correct entry point for the mailman fast path, whose
// pane probes are always same-adapter by construction (each mailman serves
// its own chamber's pane from its own adapter's binary). For cross-adapter
// probes — the MCP handler for tmux-tell.agent_state and the CLI `state
// --agent NAME` sibling when the target's adapter differs from the caller's
// — use AgentStateWithProfile (defined in classifier.go, #827). The two share
// the same algorithm via the private classifier type; only the profile source
// differs.
func AgentState(ctx context.Context, pane string) (state State, ev Evidence, err error) {
	return newClassifierFromActive().agentState(ctx, pane)
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

// isInputRowQuiet was extracted to a method on (*classifier) in classifier.go
// (#827) so cross-adapter MCP callers key on the target's sentinel, not the
// caller's activeProfile. See (*classifier).isInputRowQuiet for the full doc
// on codex-vs-Claude semantics + the orthogonal-discriminators invariant.

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

// matchCursorRowSentinel matches the cursor's row against the primary prompt
// sentinel first, then any tolerated render-variants, returning the sentinel
// that actually matched so the caller can compute ITS rune-width for the
// cursor-column comparison (variants can differ in width from the primary).
//
// It is the shared consumer of PromptSentinelVariants for cursor-anchored
// classification and post-paste verification. Variants (Claude's Win11 ASCII
// `> `, #729/#787) are honored here — and deliberately NOWHERE cursor-less —
// because a permissive glyph like `> ` is safe to treat as an input prompt only
// when the cursor pins it to the live input row; the cursor-less scans
// (isInputRowQuiet, extractInputContent) stay on the primary sentinel so a
// stray `> ` transcript/blockquote row can't be read as an idle prompt. See
// PaneProfile.PromptSentinelVariants.
func matchCursorRowSentinel(row, primary string, variants []string) (rest, matched string, found bool) {
	if primary != "" {
		if rest, ok := cutPromptSentinel(row, primary); ok {
			return rest, primary, true
		}
	}
	for _, v := range variants {
		if v == "" {
			continue
		}
		if rest, ok := cutPromptSentinel(row, v); ok {
			return rest, v, true
		}
	}
	return "", "", false
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
	if !cursorOK || activeProfile.PromptSentinel == "" {
		return false, false
	}
	lines := strings.Split(capture, "\n")
	if cursorY < 0 || cursorY >= len(lines) {
		return false, false
	}
	_, sentinel, ok := matchCursorRowSentinel(
		lines[cursorY], activeProfile.PromptSentinel, activeProfile.PromptSentinelVariants,
	)
	if !ok {
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

// apiErrorLineRE matches Claude Code's terminal API-error line — the marker
// phrase followed by a 3-digit-family status code's leading digit. Keyed on
// `API Error: \d` (a digit immediately after the ": ") rather than the specific
// 529 so the classifier is family-general (500/503/529/…) while still rejecting
// codeless prose like "hit an API Error: check the logs". Package-level
// MustCompile (fixed literal) mirrors the other pkg regexes.
var apiErrorLineRE = regexp.MustCompile(`API Error: \d`)

// escToInterruptHint is Claude Code's active-turn interrupt affordance. Its
// presence in the footer (at/below the composer) means the turn is still
// running — an active 529-retry shows it while retrying — so capturedLiveErrorChrome
// SUPPRESSES StateErrored while it is present, leaving the pane Working. The
// terminal (given-up) capture LACKS it (grep -c = 0), which is the load-bearing
// discriminant between a terminal error and an in-flight retry (#719 facts).
const escToInterruptHint = "esc to interrupt"

// promptGlyphs returns the ornament glyphs that begin a transcript-prompt row —
// the FIRST rune of the profile's PromptSentinel plus the first rune of each
// PromptSentinelVariant. Claude's composer sentinel is `❯`+NBSP and a transcript
// prompt is `❯`+regular-space; both share the U+276F ornament, so the bare glyph
// is the anchor for the current-turn region's upper bound. The Win11 variant
// contributes `>`; matching it too only ever SHRINKS the region (moves the upper
// bound down), which biases toward a false-NEGATIVE (a missed error, no
// regression) rather than a false-idle clobber — the safe direction.
func promptGlyphs(p PaneProfile) []string {
	var glyphs []string
	add := func(s string) {
		if s == "" {
			return
		}
		r, _ := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError {
			return
		}
		g := string(r)
		for _, existing := range glyphs {
			if existing == g {
				return
			}
		}
		glyphs = append(glyphs, g)
	}
	add(p.PromptSentinel)
	for _, v := range p.PromptSentinelVariants {
		add(v)
	}
	return glyphs
}

// capturedLiveErrorChrome reports whether capture shows Claude Code's TERMINAL
// API-error chrome (#719) — an `API Error: <code>` line left painted in the
// CURRENT-turn region after a request gave up — and returns the matched line.
//
// It is the live-scoped sibling of capturedLiveCompaction: a bare whole-pane
// grep for "API Error:" would false-fire on error phrasing quoted in deep
// scrollback (the mandatory Lookout negative test), so the match is bounded to
// the current turn and gated on two structural facts:
//
//  1. REGION. The composer is the bottom-most row matching the profile's
//     PromptSentinel (`❯`+NBSP, NBSP-exact via cutPromptSentinel). The region's
//     upper bound is the bottom-most transcript-prompt row strictly above the
//     composer — a row whose left-trimmed content begins with a prompt ornament
//     glyph (`❯`, or a variant's `>`). The error line must sit STRICTLY between
//     that upper bound and the composer. Error phrasing above the last
//     transcript prompt (scrollback) is out of region and does not fire.
//     (No transcript prompt above the composer → upper bound is -1 → the region
//     is rows[0:composer], the whole pane above the composer.)
//  2. CODE. The in-region line must match apiErrorLineRE (`API Error: \d`) — a
//     digit right after the marker — so codeless prose is rejected.
//  3. NOT-RETRYING. `esc to interrupt` must be ABSENT from the footer (rows
//     AT/BELOW the composer). An active 529-retry still paints that interrupt
//     hint; its presence means the turn is live, so StateErrored is suppressed
//     and the pane stays Working. This is the belt that discriminates a terminal
//     error from an in-flight retry even when both show the error line.
//
// marker gating (the caller's `m != ""` guard on profile.APIErrorMarker)
// disables the check for adapters with no API-error UI (codex). Returns
// (matchedLine, true) only when an in-region coded error line is present AND the
// interrupt hint is absent.
func capturedLiveErrorChrome(capture string, p PaneProfile) (string, bool) {
	sentinel := p.PromptSentinel
	if sentinel == "" {
		return "", false
	}
	rows := strings.Split(capture, "\n")

	// 1. Composer: bottom-most row matching the NBSP-exact composer sentinel.
	composer := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if _, found := cutPromptSentinel(rows[i], sentinel); found {
			composer = i
			break
		}
	}
	if composer < 0 {
		return "", false
	}

	// 3. NOT-RETRYING guard: an active retry keeps `esc to interrupt` in the
	// footer at/below the composer. Suppress while it is present.
	for i := composer; i < len(rows); i++ {
		if strings.Contains(rows[i], escToInterruptHint) {
			return "", false
		}
	}

	// 1 (cont.). Region upper bound: bottom-most transcript-prompt row strictly
	// above the composer (left-trimmed content begins with a prompt glyph).
	glyphs := promptGlyphs(p)
	upper := -1
	for i := composer - 1; i >= 0; i-- {
		trimmed := strings.TrimLeft(rows[i], " \t\u00a0") // spaces, tabs, NBSP
		for _, g := range glyphs {
			if strings.HasPrefix(trimmed, g) {
				upper = i
				break
			}
		}
		if upper >= 0 {
			break
		}
	}

	// 2. Coded error line strictly inside the region.
	for i := upper + 1; i < composer; i++ {
		if apiErrorLineRE.MatchString(rows[i]) {
			return strings.TrimSpace(rows[i]), true
		}
	}
	return "", false
}

// capturedResumeModal reports whether capture shows Claude Code's LIVE
// session-resume choice modal (#719) rather than transcript prose that quotes
// its marker strings. It is the live-scoped sibling of capturedLiveCompaction
// and capturedLiveErrorChrome, and it exists for the same reason: the modal's
// footer legend (marker) is prose-collidable — the #719 dispatch itself quotes
// it — so a bare whole-pane strings.Contains would classify any pane displaying
// such a message paste-unsafe and defer ALL inbound delivery (the #647 / #852
// class). The match is gated on three facts, layered so that neither prose
// quoting the modal's text nor a casual shorthand of its chrome fires:
//
//  1. FOOTER. The bottom-most row carrying the marker (ResumeModalMarker, the
//     keybind legend). This is the version-pinned semantic anchor; empty marker
//     (the caller's `m != ""` guard) disables the check for adapters with no
//     resume UI (codex).
//  2. SEARCH WIDGET. A row ABOVE the footer holding the box-drawn search field —
//     a box-vertical (│ U+2502), WHITESPACE, then the search glyph (⌕ U+2315):
//     "│ ⌕ …". The whitespace gap is required and load-bearing: the prose
//     shorthand "│+⌕" that chambers type when discussing this detector is NOT
//     whitespace-gapped, and a same-row │…⌕ grew 0→4 of ~19400 messages DURING
//     the #854 review (the self-referential-marker temporal-extension). This
//     narrows the surface but is NOT independently prose-proof — a faithful paste
//     of the modal row reproduces "│ ⌕" — so it is the WEAKEST of the three;
//     facts 1 and 3 are the durable guards.
//  3. LIVE-SCOPE. No composer prompt sentinel (profile.PromptSentinel, the
//     NBSP-exact ❯) strictly BELOW the footer. A LIVE modal is a full-screen
//     takeover (the fixture has ~30 blank rows below the footer, no ❯); a bus
//     message merely QUOTING the modal sits ABOVE the chamber's live composer,
//     so a sentinel below the footer means this is a scrollback quote. This is
//     the DURABLE guard: unlike a corpus count it does not decay as the marker
//     is discussed (the #719/#852 temporal-extension — the false-positive corpus
//     grows monotonically with discussion; a structural invariant does not).
//     Empty PromptSentinel skips the belt (no composer to locate).
//
// The header ("Resume session") is deliberately NOT keyed on: MEASURED it
// already appears in 9 live bus messages (self-referential — the dispatch
// specifying the marker contains it), and it grew 6→9 during the arc that
// discussed it. The footer legend + live-scope (facts 1 and 3) are the durable
// guards; the widget (fact 2) only excludes the casual "│+⌕" shorthand.
func capturedResumeModal(capture string, p PaneProfile) bool {
	marker := p.ResumeModalMarker
	if marker == "" {
		return false
	}
	rows := strings.Split(capture, "\n")

	// 1. Footer: bottom-most row carrying the keybind legend.
	footer := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if strings.Contains(rows[i], marker) {
			footer = i
			break
		}
	}
	if footer < 0 {
		return false
	}

	// 2. Search widget: a box-vertical, a WHITESPACE RUN (any length ≥1), then the
	// search glyph, on a single row strictly above the footer — i.e. "│\s+⌕".
	// Matching the run rather than a literal single space stays robust to
	// alignment drift ("│  ⌕") while still excluding the prose shorthand "│+⌕"
	// (`+` is not whitespace) and "│ text ⌕" (text is not whitespace) that
	// chambers type when discussing this detector. g > 0 requires ≥1 char in the
	// gap (rejects an adjacent "│⌕"); the all-whitespace gap is the discriminator.
	widget := false
	for i := 0; i < footer; i++ {
		v := strings.Index(rows[i], resumeModalBoxVertical)
		if v < 0 {
			continue
		}
		after := rows[i][v+len(resumeModalBoxVertical):]
		g := strings.Index(after, resumeModalSearchGlyph)
		if g > 0 && strings.TrimSpace(after[:g]) == "" {
			widget = true
			break
		}
	}
	if !widget {
		return false
	}

	// 3. Live-scope: no composer sentinel below the footer (else it is a
	// scrollback quote above a live prompt, not the live full-screen modal).
	if s := p.PromptSentinel; s != "" {
		for i := footer + 1; i < len(rows); i++ {
			if strings.Contains(rows[i], s) {
				return false
			}
		}
	}
	return true
}

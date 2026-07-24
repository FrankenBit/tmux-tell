package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// classifier bundles a pane-observation PaneProfile with its derived
// precompiled patterns. Introduced by #827 so the pane-classification algorithm
// can be called with an EXPLICIT profile from cross-adapter MCP callers
// (`tmux-tell.agent_state` on a codex target from a Claude-adapter binary and
// vice versa), without swapping the process-global activeProfile — which is a
// single-writer-at-Run-startup contract, unsafe for per-call toggle against
// concurrent mailman-owned observers.
//
// Exported entry points, both defined in this file:
//   - AgentStateWithProfile(ctx, pane, p) — cross-adapter path; explicit profile
//   - AgentState(ctx, pane) — process-global path (mailman fast path); defined
//     in state.go as a thin wrapper that delegates here via newClassifierFromActive
//
// The classifier algorithm itself is the (*classifier).agentState method below;
// its precedence rules are documented on the AgentState wrapper in state.go.
type classifier struct {
	profile      PaneProfile
	rateLimitRE  *regexp.Regexp
	usageLimitRE *regexp.Regexp
	workingRE    *regexp.Regexp
}

// newClassifier builds a classifier from an explicit PaneProfile, compiling
// each pattern fresh. Used by AgentStateWithProfile (cross-adapter MCP path);
// low-QPS so per-call regex compile is acceptable overhead.
func newClassifier(p PaneProfile) *classifier {
	return &classifier{
		profile:      p,
		rateLimitRE:  compileProfilePattern(p.RateLimitPattern),
		usageLimitRE: compileProfilePattern(p.UsageLimitPattern),
		workingRE:    compileProfilePattern(p.WorkingPattern),
	}
}

// newClassifierFromActive builds a classifier from the process-global
// activeProfile, reusing its already-compiled regexes to keep the mailman
// hot-path allocation-light. Callers that DON'T need the process-global (i.e.
// cross-adapter MCP probes) should use newClassifier(p) instead.
func newClassifierFromActive() *classifier {
	return &classifier{
		profile:      activeProfile,
		rateLimitRE:  activeRateLimitRE,
		usageLimitRE: activeUsageLimitRE,
		workingRE:    activeWorkingRE,
	}
}

// AgentStateWithProfile classifies a pane using an EXPLICIT PaneProfile — the
// cross-adapter entry point for MCP callers. #827: the tmux-tell.agent_state
// MCP handler runs inside the caller's adapter binary, so without an explicit
// profile it would search a codex pane for Claude's `❯ ` sentinel (or
// vice versa) and produce a systematic false-negative StateUnknown ("prompt
// sentinel not found in any row"). CLI-side `state --agent NAME` uses the same
// path so an operator running `tmux-tell-claude state --agent <codex>` also
// classifies correctly.
//
// Mailman-owned probes are always same-adapter by construction (each mailman
// serves its own chamber's pane from its own binary), so they should keep
// using AgentState — same behavior, cheaper (no per-call regex compile).
//
// Semantics + precedence rules identical to AgentState — see that function's
// doc block in state.go.
func AgentStateWithProfile(ctx context.Context, pane string, p PaneProfile) (State, Evidence, error) {
	return newClassifier(p).agentState(ctx, pane)
}

// firstRateLimitMatch — classifier-scoped variant of the pre-#827 free
// function. Empty pattern (parked check, adapter with no rate-limit UI)
// returns zero-value.
func (c *classifier) firstRateLimitMatch(capture string) (string, time.Duration) {
	if c.rateLimitRE == nil {
		return "", 0
	}
	matches := c.rateLimitRE.FindStringSubmatch(capture)
	if len(matches) == 0 {
		return "", 0
	}
	matched := matches[0]
	if idx := c.rateLimitRE.SubexpIndex("retry_seconds"); idx >= 0 && idx < len(matches) {
		if d, err := parseRetrySeconds(matches[idx]); err == nil {
			return matched, d
		}
	}
	return matched, 0
}

// firstUsageLimitMatch — classifier-scoped variant of the pre-#827 free
// function. Empty pattern returns "".
func (c *classifier) firstUsageLimitMatch(capture string) string {
	if c.usageLimitRE == nil {
		return ""
	}
	matches := c.usageLimitRE.FindStringSubmatch(capture)
	if len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// firstWorkingMarkerMatch returns the profile's working-marker match in the
// capture, or "" when the profile parks the check (empty WorkingPattern, as
// Claude does) or the marker is absent. Positive in-turn busy detection
// (#590). Classifier-scoped variant of the pre-#827 free function.
func (c *classifier) firstWorkingMarkerMatch(capture string) string {
	if c.workingRE == nil {
		return ""
	}
	return c.workingRE.FindString(capture)
}

// isInputRowQuiet reports whether the pane's composer — the BOTTOM-MOST row
// starting with the classifier's PromptSentinel — has only whitespace past
// the sentinel. Returns false when no sentinel row is found or the composer
// has non-whitespace content. Used by the cursor-less fallback path in
// (*classifier).agentState when the cursor query fails or the cursor isn't
// on the input row.
//
// Bottom-most anchoring (not every-row) is what makes this codex-safe (#756
// Bug 2). Codex renders every submitted turn's chrome with a `› [Sender · ts]
// message content` prefix, so a healthy codex pane routinely carries multiple
// sentinel rows — the transcript ones with content past the sentinel, the
// composer at the bottom with only ghost-text or nothing. The pre-#756 walk
// disqualified the whole pane the moment ANY sentinel row had content past
// it, so an idle codex composer with any submitted turn in scrollback fell
// through to StateUnknown and the mailman's pre_paste_safety_abort refused
// delivery in a loop. The composer is always the bottom-most sentinel row
// (input area is anchored below all rendered content in both codex + Claude
// TUIs), so keying on the bottom-most row is both codex-correct and Claude-
// safe: for Claude, the transcript rows carry `❯`+regular-space (`e2 9d af
// 20`) while the composer carries `❯`+NBSP (`e2 9d af c2 a0`); cutPromptSentinel
// is NBSP-exact so transcript rows return found=false, leaving the composer
// as the only cutPromptSentinel-matched row → bottom-most == only-sentinel-row
// and the observable answer is unchanged. Different discriminator per adapter
// (Claude: NBSP-exactness; codex: positional-anchor); combined per-adapter to
// yield the same observable outcome. See PR#834 review and
// feedback_adapter_agnostic_via_orthogonal_discriminators in QM memory for the
// coupling detail — the Claude-safety rests on cutPromptSentinel staying
// NBSP-exact.
//
// The `pane_in_mode=1` copy-mode gate in agentState (P0) means this only runs
// on live-view panes where the composer is in frame; a scrolled-up pane never
// reaches here.
func (c *classifier) isInputRowQuiet(paneContent string) bool {
	sentinel := c.profile.PromptSentinel
	if sentinel == "" {
		return false
	}
	rows := strings.Split(paneContent, "\n")
	for i := len(rows) - 1; i >= 0; i-- {
		rest, found := cutPromptSentinel(rows[i], sentinel)
		if !found {
			continue
		}
		return strings.TrimSpace(rest) == ""
	}
	return false
}

// agentState is the classifier-scoped implementation of pane classification —
// the single copy of the algorithm shared by AgentState (process-global) and
// AgentStateWithProfile (explicit profile, #827 cross-adapter path). See
// AgentState in state.go for the precedence-rule doc.
func (c *classifier) agentState(ctx context.Context, pane string) (state State, ev Evidence, err error) {
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

	// Precedence 1: compaction marker (from the classifier's PaneProfile; empty
	// disables the check for an adapter with no compaction UI). The match
	// requires the marker's LIVE-timer parenthetical, not the bare phrase, so a
	// chamber writing "Compacting conversation…" in a message (e.g. one working
	// on this very code) doesn't false-positive as mid-/compact — the
	// transcript-sentinel wedge that deferred all inbound delivery (#647).
	if m := c.profile.CompactionMarker; m != "" && capturedLiveCompaction(capBStr, m) {
		return StateAtRestInCompaction,
			Evidence{
				Reason: fmt.Sprintf("compaction marker found: %q", m),
				Marker: m,
			}, nil
	}

	// Precedence 1b (#719): Claude Code's session-resume choice modal. Placed
	// BEFORE the P5 frame-change check for the SAME reason as compaction above —
	// the modal's relative-time entries ("17 seconds ago") can tick across the
	// 200ms temporal-delta window, so capA != capB, and P5 would classify it
	// StateWorking. Working is paste-SAFE (see IsPasteUnsafeForced), so the
	// mailman would then paste-and-Enter INTO the modal — the delivery consumed
	// as search input, Enter selecting a session (the #719 clobber / multi-hour
	// silence). Reaching this check first classifies it StateAwaitingOperator
	// (paste-unsafe) regardless of whether the frame ticked. The match is
	// structural (capturedResumeModal: box-drawing search widget + live-scope) —
	// a whole-pane substring on the footer legend would defer all delivery on any
	// pane merely quoting the modal (#647 / #852 class). Empty ResumeModalMarker
	// disables it (codex parks it).
	if m := c.profile.ResumeModalMarker; m != "" && capturedResumeModal(capBStr, c.profile) {
		return StateAwaitingOperator,
			Evidence{
				Reason: "resume-session choice modal (paste-unsafe); operator selection required",
				Marker: m,
			}, nil
	}

	// Precedence 2: empirical usage-limit regex (#540). The configured pattern
	// stays empty until real adapter pane output is captured; synthetic tests
	// can still exercise the mechanism without guessing production literals.
	if m := c.firstUsageLimitMatch(capBStr); m != "" {
		return StateUsageLimited,
			Evidence{
				Reason: fmt.Sprintf("usage-limit pattern matched: %q", m),
				Marker: m,
			}, nil
	}

	// Precedence 3: empirical rate-limit regex (#504). The configured pattern
	// stays empty until real adapter pane output is captured; synthetic tests
	// can still exercise the mechanism without guessing production literals.
	if m, retryAfter := c.firstRateLimitMatch(capBStr); m != "" {
		return StateRateLimited,
			Evidence{
				Reason:     fmt.Sprintf("rate-limit pattern matched: %q", m),
				Marker:     m,
				RetryAfter: retryAfter,
			}, nil
	}

	// Precedence 4: positive working marker (from the classifier's PaneProfile;
	// empty parks the check). An adapter whose active turn renders a persistent
	// status marker is classified Working from that marker directly — BEFORE the
	// temporal-delta frame-change heuristic and the cursor-aware idle logic
	// below. Codex renders `◦ Working (Ns • esc to interrupt)` throughout a turn;
	// its only per-second delta is the elapsed counter, so a 200ms capture pair
	// can read the frame as stable and the cursor-at-sentinel branch would
	// false-idle the active turn (#590). Keying the marker here makes the
	// positive busy signal win over frame-stability. Additive: Claude's empty
	// WorkingPattern parks it, so the Claude path is unchanged.
	if m := c.firstWorkingMarkerMatch(capBStr); m != "" {
		return StateWorking,
			Evidence{
				Reason: fmt.Sprintf("working marker matched: %q", m),
				Marker: m,
			}, nil
	}

	// Cursor-position-aware classification (the v2 substrate per #69
	// operator's design call 2026-06-04). Query the cursor ONCE here — BEFORE
	// the P5 frame-change check — because the operator-drafting sub-case
	// (#332) must win over "pane content changed". An operator actively typing
	// repaints the input row every keystroke, so capA != capB, but the cursor
	// sits PAST the sentinel. Claude's streaming/spinner busy states never do
	// that: measured on the live Claude adapter, 156/156 busy frame-changes
	// kept the cursor AT the sentinel column (col 2), zero past it. So
	// cursor-strictly-past-sentinel is an unambiguous "operator is drafting"
	// signal. Were P5 to run first it would classify the drafting pane as
	// paste-safe StateWorking and the mailman would paste into the half-typed
	// draft — the 2026-06-12 operator-witnessed clobber this issue tracks.
	sentinel := c.profile.PromptSentinel
	cursorX, cursorY, cursorErr := agentCursor(ctx, pane)
	var (
		cursorRowRest        string
		sentinelCol          int
		cursorAtSentinel     bool
		cursorPastSentinel   bool
		cursorRowHasSentinel bool
	)
	if cursorErr == nil && sentinel != "" {
		lines := strings.Split(capBStr, "\n")
		if cursorY >= 0 && cursorY < len(lines) {
			row := lines[cursorY]
			// Match the primary sentinel or any render-variant (Claude's Win11
			// ASCII `> `, #729); use the MATCHED sentinel's rune-width for the
			// cursor-column comparison so a differently-sized variant still
			// anchors idle-vs-drafting correctly.
			rest, matched, hasSentinel := matchCursorRowSentinel(row, sentinel, c.profile.PromptSentinelVariants)
			if hasSentinel {
				cursorRowHasSentinel = true
				cursorRowRest = rest
				sentinelCol = utf8.RuneCountInString(matched)
				cursorAtSentinel = cursorX == sentinelCol
				cursorPastSentinel = cursorX > sentinelCol
			}
		}
	}

	// Precedence 5a (#332): operator mid-typing wins over the frame-change
	// working-classification. Cursor STRICTLY past the sentinel is the
	// drafting signal and fires whether or not the frame changed. A cursor AT
	// the sentinel on a changing frame is streaming — left to P5 below.
	if cursorPastSentinel {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("cursor past prompt sentinel (col %d > %d); operator mid-typing", cursorX, sentinelCol),
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

	// Precedence 5b (#719): terminal API-error chrome → StateErrored. Placed
	// AFTER P5 frame-change and BEFORE P6 cursor-at-sentinel→Idle, and the
	// ordering is load-bearing on both sides:
	//
	//   - After P5: an active 529-RETRY animates its spinner
	//     (`✻ 529 Overloaded · Retrying…`), so capA != capB and P5 catches it as
	//     Working before it reaches here. A TERMINAL error is a stable frame
	//     (the request gave up; nothing animates), so it falls through P5 to
	//     here. The esc-to-interrupt guard inside capturedLiveErrorChrome is the
	//     BELT for the residual case — a retry whose 200ms window happened to be
	//     stable still carries `esc to interrupt` in its footer and is suppressed.
	//   - Before P6: the terminal error PRESERVES the composer `❯` prompt row, so
	//     the cursor sits at the sentinel and P6 would classify it Idle — the
	//     false-idle this whole state exists to correct (the mailman then pastes
	//     into the dead turn). Running before P6 reclassifies it Errored first.
	//
	// The check is disabled (empty APIErrorMarker) for adapters with no
	// API-error chrome — codex parks it, so the codex path is unchanged.
	if m := c.profile.APIErrorMarker; m != "" {
		if line, ok := capturedLiveErrorChrome(capBStr, c.profile); ok {
			return StateErrored,
				Evidence{
					Reason: fmt.Sprintf("terminal API-error chrome (false-idle): %q", line),
					Marker: m,
				}, nil
		}
	}

	// Precedence 6: cursor AT the sentinel on a STABLE frame → Idle. This
	// stays BELOW P5 so a cursor parked at the sentinel while the frame
	// streams is caught as Working above, not mis-idled.
	if cursorRowHasSentinel && cursorAtSentinel {
		// Cursor right after `❯ ` — either a clean idle prompt or an
		// auto-suggestion ghost-text. Both classify as Idle because the
		// operator hasn't engaged (cursor would have moved past content
		// if they had been typing).
		idleEv := Evidence{
			Reason:      "cursor at prompt sentinel position; pane stable",
			PromptEmpty: strings.TrimSpace(cursorRowRest) == "",
		}
		if !idleEv.PromptEmpty {
			idleEv.Reason = fmt.Sprintf("cursor at prompt sentinel position with auto-suggestion ghost-text (%q); pane stable", strings.TrimSpace(cursorRowRest))
		}
		return StateIdle, idleEv, nil
	}
	// Cursor before sentinel position on the sentinel row is unusual; fall
	// through to marker / unknown checks.

	// Precedence 7: awaiting-operator marker (backup for non-sentinel-
	// painting UIs — AskUserQuestion popups, the /mcp picker, search dialogs).
	// From the classifier's PaneProfile; empty disables the backup check.
	//
	// The match is LIVE-SCOPED (capturedLiveAwaitingOperator), NOT a bare
	// whole-pane strings.Contains(capBStr, m): the marker "↑/↓ to navigate ·" is
	// prose-collidable — 18 live bus messages quote it (2026-07-24, growing as
	// this fix is discussed) — and a whole-pane match classified any pane merely
	// displaying such a message paste-unsafe, deferring ALL inbound delivery (the
	// #647 outage class, here intermittent: reachable on a stable frame with the
	// cursor not cleanly at the sentinel, e.g. a cursor-query hiccup skipping P6).
	// The live-scope helper requires the marker on a footer with no live composer
	// below it (a real picker replaces the composer; a scrollback quote sits above
	// it), mirroring capturedLiveCompaction / capturedLiveErrorChrome /
	// capturedResumeModal. Reached after P6, so a cleanly-idle pane with a
	// colliding message in scrollback classifies StateIdle there and never gets
	// here (#852).
	if m := c.profile.AwaitingOperatorMarker; m != "" && capturedLiveAwaitingOperator(capBStr, c.profile) {
		return StateAwaitingOperator,
			Evidence{
				Reason: fmt.Sprintf("live arrow-navigation picker (awaiting-operator marker on live footer): %q", m),
				Marker: m,
			}, nil
	}

	// Cursor-less fallback (cursor query failed or cursor row doesn't
	// have the sentinel): parse the pane for a sentinel-row with no
	// content past it. Used when display-message isn't available or
	// the cursor is somewhere other than the input row (e.g., agent
	// paused mid-spinner).
	if c.isInputRowQuiet(capBStr) {
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

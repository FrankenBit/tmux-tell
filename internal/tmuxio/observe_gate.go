package tmuxio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ObserveGateOpts configures the observe-only-with-one-named-visibility-
// side-effect delivery gate introduced by tmux-msg #92 (the substrate
// itself is read-only-observe; the one side-effect is the optional 📫
// nudge fired via OnOperatorTyping when first observing
// StateAwaitingOperator per #95 — opt-out via OnOperatorTyping=nil OR
// notify-emoji-disabled in the mailman config). With OnOperatorTyping
// nil-or-disabled the gate is strictly read-only. Defaults are filled
// in by ObserveGate when fields are zero-valued.
type ObserveGateOpts struct {
	// PollIntervalMin is the initial sleep between observe iterations.
	// Default 3s.
	PollIntervalMin time.Duration
	// PollIntervalMax caps the per-iteration sleep after backoff.
	// Default 15s.
	PollIntervalMax time.Duration
	// BackoffFactor is the multiplicative growth on PollInterval per
	// iteration when the agent is not yet ready (StateAwaitingOperator,
	// StateWorking, StateUsageLimited, StateAtRestInCompaction,
	// StateUnknown). Default 1.5.
	// The interval resets to PollIntervalMin when the input-row content
	// changes (operator typed something new) so a fresh observation
	// cadence kicks in.
	BackoffFactor float64
	// InputStaleThreshold is how long the operator's input-row content
	// must remain unchanged before the gate decides the draft is
	// abandoned and returns Stale=true so the caller can archive +
	// flush. Default 2m per the operator's design call 2026-06-04
	// (#92 comment).
	InputStaleThreshold time.Duration
	// MaxWait is the total safety cap on the gate's loop. On hitting
	// it, the gate returns ErrMaxWaitExceeded; the caller can still
	// deliver the message (the cap is a fail-loud rather than fail-
	// stop). Default 5m.
	MaxWait time.Duration
	// Ping is an optional callback the gate invokes on each iteration
	// so the caller can keep external timers (notably systemd's
	// WatchdogSec) alive during long observe loops. May be nil.
	// Mailman wires this to sdnotify.Watchdog at startup.
	Ping func()
	// OnOperatorTyping is an optional callback fired ONCE per delivery
	// cycle the first time the gate observes StateAwaitingOperator
	// (cursor past sentinel = operator drafting). Mailman wires this
	// to NotifyPendingMessage so the operator sees a 📫 emoji land in
	// their input row as a visibility signal that a bus message is
	// waiting (#95). Subsequent iterations in the same cycle do not
	// re-fire; once the gate returns and a new delivery cycle starts,
	// a fresh ObserveGate call gets a fresh callback opportunity.
	// May be nil — when nil, the gate is a no-op on the visibility
	// concern.
	OnOperatorTyping func()
	// WorkingDeliverImmediately, when true, opts the gate's
	// StateWorking branch out of the safer-default wait and into the
	// same fast-path return as StateIdle (#106). Default false (defer
	// on Working — the existing v0.3.0-through-v0.6.0 behavior).
	//
	// Rationale: Claude Code's TUI buffers keystrokes that arrive
	// mid-turn — a paste during StateWorking lands in the input row
	// and is read as the next operator turn after the current one
	// completes. The gate's StateWorking-defers-uniformly behavior
	// is structurally conservative; opting in trades a slightly
	// busier-looking recipient pane (the body text appears in the
	// input row while Claude is still streaming) for a real cadence
	// win (1s instead of 3-57s under backoff).
	//
	// Eligibility is StateWorking ONLY. StateAwaitingOperator (paste
	// would destroy the operator's draft), StateAtRestInCompaction
	// (immediate paste races the /compact slash-command parser), and
	// StateUnknown (the popup-state failure mode #105 surfaced — an
	// immediate paste into an unrecognized state is the destructive
	// case) all stay hard-deferred regardless of this flag. The
	// verify-token retry + delivered_in_input_box notice is the
	// load-bearing safety net for the small race-window between
	// observing StateWorking and the paste actually landing.
	WorkingDeliverImmediately bool
}

// GateOutcome reports the gate's decision and the evidence that
// produced it. The caller (mailman serve loop) uses Stale +
// InputContent to decide whether to archive + flush before pasting.
type GateOutcome struct {
	// Reason is a one-line human-readable explanation of how the gate
	// decided to return. Suitable for verbose mailman logging.
	Reason string
	// State is the AgentState classification that produced the
	// outcome. StateIdle for the happy path; StateAwaitingOperator for
	// stale-flush; potentially other states on MaxWait or context
	// cancellation.
	State State
	// Iterations is the number of AgentState polls before the gate
	// decided to return (1 = fast-path idle on first poll).
	Iterations int
	// Stale is true when the gate decided to proceed despite operator-
	// typed content in the input row, because the content was stable
	// for at least InputStaleThreshold. The caller should archive
	// InputContent as kind=stranded_draft (cap-bypass), then Ctrl+U
	// the input, then paste. On archive failure, the (a) fallback
	// kicks in (paste-and-Enter without clearing; produces a compound
	// message but doesn't strand the delivery).
	Stale bool
	// InputContent is the captured operator-typed content past the
	// PromptSentinel on the input row at the moment the gate decided
	// to return. Non-empty only when Stale=true. The hash of this
	// content is what stayed stable across InputStaleThreshold.
	InputContent string
	// CopyModeWait is the wall-clock from the FIRST StateInCopyMode
	// observation in THIS gate cycle until the gate returned (#526). Zero
	// when the pane was never in copy-mode. The caller feeds it to the
	// tmux_tell_copymode_defer_wait_seconds histogram. Populated on every
	// return path that saw copy-mode (the idle delivery path AND the
	// ErrCopyModeUnsafe revert path).
	//
	// Grain (Surveyor #535 review): this is a PER-GATE-CYCLE wait, not a
	// per-message total. A single-cycle read (exits copy-mode within MaxWait)
	// records the full hold in one sample — the common case. A read that
	// outlasts MaxWait reverts-and-retries, so it records N samples of ~MaxWait
	// across N cycles rather than one cumulative total. For a true
	// first-defer-to-final-delivery total, thread the first-copy-mode timestamp
	// across cycles via a serve-side per-publicID map (the #507 deferStart
	// shape) — deferred; per-cycle is the honest grain for v1.
	CopyModeWait time.Duration
	// RetryAfter is the parsed retry hint surfaced by the rate-limit regex.
	// Zero means the banner did not expose a parseable retry_seconds capture;
	// the caller falls back to exponential backoff.
	RetryAfter time.Duration
}

// ErrMaxWaitExceeded is returned when ObserveGate's safety cap fires
// without reaching either the idle path or the stale-flush path.
var ErrMaxWaitExceeded = errors.New("tmuxio: observe-gate MaxWait exceeded")

// ErrCopyModeUnsafe is returned when the gate's MaxWait fires while the pane
// is STILL in copy-mode (#526). Unlike ErrMaxWaitExceeded — whose caller
// delivers-anyway because the blocking state (working/awaiting) resolves into
// a deliverable pane — copy-mode persists until the operator exits scroll
// mode, so delivering-anyway would paste into a still-scrolled pane and
// reproduce the 83b3 bug this gate exists to prevent. The caller must NOT
// deliver on this error: revert the message to queued and retry. The
// within-gate poll detects copy-mode exit within one interval (~3-15s), so a
// retry delivers promptly once the operator returns to the live prompt.
var ErrCopyModeUnsafe = errors.New("tmuxio: observe-gate copy-mode persisted past MaxWait")

// ErrRateLimited is returned when the pane is visibly rate-limited. The caller
// should revert the message to queued and retry after the parsed retry hint or
// an exponential backoff.
var ErrRateLimited = errors.New("tmuxio: observe-gate rate-limited")

// ErrUsageLimited is returned when the pane is visibly usage-limited. The
// caller should revert the message to queued and park until quota reset.
var ErrUsageLimited = errors.New("tmuxio: observe-gate usage-limited")

// sinceIfSet returns the elapsed time since t, or 0 when t is the zero value
// (the event never happened). Used to populate GateOutcome.CopyModeWait only
// when the pane was actually observed in copy-mode during the cycle.
func sinceIfSet(t time.Time) time.Duration {
	if t.IsZero() {
		return 0
	}
	return time.Since(t)
}

// ObserveGate observes the receiver pane via repeated read-only
// AgentState calls until the agent is ready to receive a paste
// delivery. The substrate-class is observe-only-with-one-named-
// visibility-side-effect: the AgentState probe itself is strictly
// read-only (two capture-pane reads + one display-message query, zero
// send-keys), and the gate fires at most ONE optional pane mutation
// per delivery cycle — the 📫 nudge via OnOperatorTyping when first
// observing StateAwaitingOperator (#95, opt-out via
// OnOperatorTyping=nil OR notify-emoji-disabled in the mailman
// config). With OnOperatorTyping nil-or-disabled the gate is strictly
// read-only.
//
// Replaced the probe-and-watch gate per #92 (shipped in v0.3.0; the
// legacy primitives were swept out in v0.4.0 / #94). The algorithm:
//
//  1. Poll AgentState; on StateIdle (cursor at sentinel — empty or
//     auto-suggestion ghost-text), return immediately with Stale=false.
//  2. On StateAwaitingOperator (cursor past sentinel = operator
//     drafting), capture the input row's content past the
//     PromptSentinel and hash it. Track the time-since-first-stable.
//     If the hash remains stable for at least InputStaleThreshold,
//     return Stale=true with InputContent populated so the caller can
//     archive + Ctrl+U + paste.
//  3. On StateWorking / StateUsageLimited / StateRateLimited /
//     StateAtRestInCompaction / StateUnknown, treat as a safer-default wait
//     or reactive defer — defer and re-poll; for usage-limited return
//     ErrUsageLimited so the caller can park until reset, and for rate-limited
//     return ErrRateLimited so the caller can back off with the parsed retry
//     hint.
//  4. On each non-idle iteration, sleep PollInterval (starting at
//     PollIntervalMin, growing by BackoffFactor up to PollIntervalMax)
//     before the next poll. Reset to PollIntervalMin when the
//     operator-typed content changes (fresh activity → fresh cadence).
//  5. On total wall-clock exceeding MaxWait, return ErrMaxWaitExceeded
//     with Stale=true and the last-seen InputContent so the caller can
//     still proceed via the archive-or-fallback path.
//
// The race between observe-decides-idle and caller-pastes is unchanged
// from the legacy gate — covered by the verify-token + delivered_
// unverified safety net (the load-bearing post-hoc detector per the
// quiet-disabled help text). The observe-gate doesn't try to eliminate
// the race; it eliminates the pane mutation that the probe-and-watch
// gate inflicted on the operator during the gate's own observation.
func ObserveGate(ctx context.Context, pane string, opts ObserveGateOpts) (GateOutcome, error) {
	if pane == "" {
		return GateOutcome{}, errors.New("tmuxio: pane required")
	}
	if opts.PollIntervalMin <= 0 {
		opts.PollIntervalMin = 3 * time.Second
	}
	if opts.PollIntervalMax <= 0 {
		opts.PollIntervalMax = 15 * time.Second
	}
	if opts.PollIntervalMax < opts.PollIntervalMin {
		opts.PollIntervalMax = opts.PollIntervalMin
	}
	if opts.BackoffFactor < 1 {
		opts.BackoffFactor = 1.5
	}
	if opts.InputStaleThreshold <= 0 {
		opts.InputStaleThreshold = 2 * time.Minute
	}
	if opts.MaxWait <= 0 {
		opts.MaxWait = 5 * time.Minute
	}

	started := time.Now()
	deadline := started.Add(opts.MaxWait)
	pollInterval := opts.PollIntervalMin

	var (
		hashSeen         string
		hashSeenAt       time.Time
		lastContent      string
		iterations       int
		lastState        State
		notifiedOfTyping bool
		firstCopyModeAt  time.Time // #526: first StateInCopyMode this cycle (zero = never)
	)

	for {
		iterations++
		if opts.Ping != nil {
			opts.Ping()
		}
		state, ev, err := AgentState(ctx, pane)
		lastState = state
		if err != nil {
			return GateOutcome{
				Reason:     fmt.Sprintf("AgentState error: %v", err),
				State:      state,
				Iterations: iterations,
			}, fmt.Errorf("tmuxio: observe-gate: %w", err)
		}

		switch state {
		case StateIdle:
			return GateOutcome{
				Reason:       "idle: " + ev.Reason,
				State:        state,
				Iterations:   iterations,
				CopyModeWait: sinceIfSet(firstCopyModeAt),
			}, nil

		case StateAwaitingOperator:
			// Fire the operator-typing notification ONCE per delivery
			// cycle (#95). The callback (when wired by mailman) sends a
			// 📫 emoji into the operator's input row so they have a
			// visible signal that a bus message is pending. Subsequent
			// iterations in the same cycle skip the re-fire.
			if !notifiedOfTyping && opts.OnOperatorTyping != nil {
				opts.OnOperatorTyping()
				notifiedOfTyping = true
			}
			content, cerr := extractInputContent(ctx, pane)
			if cerr != nil {
				// Capture failure on input-content extraction is non-
				// fatal; treat the iteration as "still uncertain" and
				// continue. The next iteration may succeed.
				content = ""
			}
			hash := hashContent(content)
			now := time.Now()
			if hashSeen == "" || hash != hashSeen {
				hashSeen = hash
				hashSeenAt = now
				pollInterval = opts.PollIntervalMin // reset on fresh activity
			}
			stableFor := now.Sub(hashSeenAt)
			lastContent = content
			if stableFor >= opts.InputStaleThreshold {
				return GateOutcome{
					Reason: fmt.Sprintf("stale: input unchanged for %s past PromptSentinel",
						stableFor.Round(time.Second)),
					State:        state,
					Iterations:   iterations,
					Stale:        true,
					InputContent: content,
				}, nil
			}

		case StateWorking:
			if opts.WorkingDeliverImmediately {
				// #106: opt-in fast-path for StateWorking. Claude Code
				// buffers the paste; recipient sees the body in the input
				// row while still streaming, and reads it as the next
				// turn after the current one completes. Eligibility is
				// StateWorking only — see WorkingDeliverImmediately's
				// doc-comment for why AwaitingOperator / Compaction /
				// Unknown stay hard-deferred.
				return GateOutcome{
					Reason:       "working: immediate delivery (opt-in)",
					State:        state,
					Iterations:   iterations,
					CopyModeWait: sinceIfSet(firstCopyModeAt),
				}, nil
			}
			// Safer-default wait. The agent is busy; defer + re-poll.
		case StateRateLimited:
			return GateOutcome{
				Reason:     "rate-limited: " + ev.Reason,
				State:      state,
				Iterations: iterations,
				RetryAfter: ev.RetryAfter,
			}, ErrRateLimited
		case StateUsageLimited:
			return GateOutcome{
				Reason:     "usage-limited: " + ev.Reason,
				State:      state,
				Iterations: iterations,
			}, ErrUsageLimited
		case StateInCopyMode:
			// #526: operator scrolled the pane up into copy-mode. Safer-
			// default wait — defer + re-poll; a paste here is consumed as
			// copy-mode navigation and the verify-token can't surface from
			// the scrolled view (the 83b3 bug). Stamp the first observation
			// so CopyModeWait measures the full hold. The MaxWait branch
			// below returns ErrCopyModeUnsafe (NOT deliver-anyway) when the
			// pane is still scrolled at the cap.
			if firstCopyModeAt.IsZero() {
				firstCopyModeAt = time.Now()
			}
		case StateAtRestInCompaction, StateUnknown:
			// Safer-default wait. The agent is compacting / in an
			// unrecognized state; defer + re-poll.
		}

		// Check the safety cap before sleeping for the next iteration.
		if time.Now().After(deadline) {
			// #526: copy-mode is the one state where deliver-anyway is NOT
			// safe — the pane is still scrolled, so a paste reproduces the
			// 83b3 bug. Return ErrCopyModeUnsafe (no Stale) so the caller
			// reverts to queued and retries instead of delivering. The
			// within-gate poll catches the operator's exit within one
			// interval, so the retry delivers promptly on return-to-live.
			if lastState == StateInCopyMode {
				return GateOutcome{
					Reason: fmt.Sprintf("copy-mode active past MaxWait %s; reverting to queued (not pasting into a scrolled pane)",
						opts.MaxWait),
					State:        StateInCopyMode,
					Iterations:   iterations,
					CopyModeWait: sinceIfSet(firstCopyModeAt),
				}, ErrCopyModeUnsafe
			}
			return GateOutcome{
				Reason: fmt.Sprintf("MaxWait %s exceeded; last state=%s",
					opts.MaxWait, lastState),
				State:        lastState,
				Iterations:   iterations,
				Stale:        true,
				InputContent: lastContent,
				CopyModeWait: sinceIfSet(firstCopyModeAt),
			}, ErrMaxWaitExceeded
		}

		// Sleep until next poll, with respect to context cancellation
		// and the absolute deadline.
		remaining := time.Until(deadline)
		sleep := pollInterval
		if sleep > remaining {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
			return GateOutcome{
				Reason:     fmt.Sprintf("context cancelled: %v", ctx.Err()),
				State:      lastState,
				Iterations: iterations,
			}, ctx.Err()
		case <-time.After(sleep):
		}

		// Grow the interval (multiplicative, capped). Reset on hash
		// change is already handled in the StateAwaitingOperator branch
		// above. For other states (Working, Unknown), we just back off.
		next := time.Duration(float64(pollInterval) * opts.BackoffFactor)
		if next > opts.PollIntervalMax {
			next = opts.PollIntervalMax
		}
		pollInterval = next
	}
}

// extractInputContent captures the receiver pane and returns the
// operator's full multi-line input content. Walks from the first
// sentinel-prefixed row downward, joining each continuation row with
// "\n", until it hits a row recognized as outside the input area
// (the below-input separator or the status line). Returns "" with
// nil error when no sentinel row is found (e.g., agent paused
// mid-spinner — the input area isn't visible).
//
// Multi-line handling per #96: the legacy implementation captured
// only the first sentinel-row's content, so multi-line drafts got
// silently truncated at flush time (Ctrl+U cleared everything but
// the archived stranded_draft only held line 1). The walk-until-
// boundary shape matches Claude Code's TUI layout:
//
//	─────── <agent title> ──     ← title separator (above input)
//	❯ first line of draft           ← sentinel row + content
//	  continuation row              ← continuation (no sentinel prefix)
//	  another continuation row
//	─────────────────────────…      ← below-input separator (boundary)
//	  ⏵⏵ bypass permissions on …    ← status line (boundary)
//
// Boundary detection (isInputAreaBoundary): a row whose trimmed
// content starts with ⏵⏵ (U+23F5, the status-line marker) OR
// contains 20+ consecutive ─ (U+2500) characters. Edge case
// acknowledged: an operator who literally types 20+ ─ characters
// into their input would have it treated as a boundary. Vanishingly
// rare in practice.
//
// Read-only: one capture-pane call, zero pane mutation. Used by the
// observe-gate to compute the hash for stale-detection and to surface
// the cleared content to the caller for archiving.
func extractInputContent(ctx context.Context, pane string) (string, error) {
	out, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return "", fmt.Errorf("tmuxio: observe-gate input-content capture: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	sentinel := activeProfile.PromptSentinel
	if sentinel == "" {
		// No sentinel configured → no input-row anchor to walk from; the
		// gate's StateAwaitingOperator path is unreachable for this adapter
		// anyway (it needs a sentinel for cursor-aware classification).
		return "", nil
	}
	var inputLines []string
	inInput := false
	for _, line := range strings.Split(string(out), "\n") {
		if !inInput {
			if rest, ok := strings.CutPrefix(line, sentinel); ok {
				inputLines = append(inputLines, strings.TrimRight(rest, " "))
				inInput = true
			}
			continue
		}
		if isInputAreaBoundary(line) {
			break
		}
		inputLines = append(inputLines, strings.TrimRight(line, " "))
	}
	if len(inputLines) == 0 {
		return "", nil
	}
	return strings.Join(inputLines, "\n"), nil
}

// pasteStillInInput reports whether the active adapter's paste-collapse marker
// (codex `[Pasted Content`) is present in the LIVE input — i.e. a collapsed
// paste is sitting unsubmitted. Returns false when the adapter has no collapse
// marker (Claude) or no prompt sentinel, so it is codex-specific by config (#401).
//
// The live input is scoped to the BOTTOM-MOST prompt sentinel (the last line
// starting with the sentinel) through the end of the capture. This is what
// distinguishes a STUCK paste from a SUBMITTED one in codex's post-submit
// dual-prompt layout: when codex submits, the paste lingers as a transcript
// entry (`› [Pasted Content N]`) ABOVE a NEW empty input prompt — scoping to the
// FIRST sentinel would grab the lingering transcript copy and false-positive
// "stuck", but the LAST sentinel is the new empty input, so the marker is
// correctly absent. A genuinely stuck paste, by contrast, IS the bottom-most
// sentinel block, so the marker is present.
//
// Multi-block (#443 Obs2, operator-witnessed probe 2026-06-15): when several
// collapsed pastes are staged before submit, codex renders them ALL on one
// logical input row after a SINGLE sentinel (`› [Pasted Content N][Pasted
// Content N] #2[Pasted Content N] #3`), NOT one sentinel per block. So the
// bottom-most-sentinel scope still contains every staged marker — this detector
// reports stuck for an N-block composer exactly as it does for one block, with
// no early false-negative. That is what lets the #401 settle-until-empty-input
// resubmit loop handle N blocks unchanged: a single Enter on a ready composer
// submits the whole frame in one model turn (two-phase readiness, not
// placeholder-count == Enter-count), and the loop stops the instant the marker
// clears — so it never over-sends a blank follow-up after submit.
func pasteStillInInput(capture string) bool {
	marker := activeProfile.PasteCollapseMarker
	sentinel := activeProfile.PromptSentinel
	if marker == "" || sentinel == "" {
		return false
	}
	lines := strings.Split(capture, "\n")
	lastSentinel := -1
	for i, line := range lines {
		if strings.HasPrefix(line, sentinel) {
			lastSentinel = i
		}
	}
	if lastSentinel < 0 {
		return false
	}
	return strings.Contains(strings.Join(lines[lastSentinel:], "\n"), marker)
}

// StatusLineMarker is the glyph on the status row that bounds the BOTTOM of the
// input area — ⏵⏵ (U+23F5 ×2), present on the bottom row of every Claude Code
// pane in production ("⏵⏵ bypass permissions on (shift+tab to cycle)"). It is
// the per-adapter input-area boundary snippet #322 lifts into PaneProfile;
// extractInputContent's walk-until-boundary stops when it sees this glyph (via
// the active profile). An adapter with a different status row supplies its own;
// an empty value disables the status-line recognizer (the ─×20 separator
// recognizer in isInputAreaBoundary still applies).
//
// FORWARD-WATCH (same shape as PromptSentinel + CompactionMarker +
// AwaitingOperatorMarker): Claude-Code-version-dependent. If the status row's
// leading glyph changes across a Claude Code version update, this constant needs
// re-verification; the recognizer-case test in observe_gate_test.go surfaces the
// drift on the Claude default.
const StatusLineMarker = "⏵⏵"

// isInputAreaBoundary reports whether a captured row marks the
// boundary between the input area and the chrome below it (the
// below-input separator or the status line). Used by
// extractInputContent's walk-until-boundary multi-line capture (#96).
//
// Two recognizers:
//   - status-line marker: the active PaneProfile's StatusLineMarker (Claude:
//     ⏵⏵ U+23F5) — present on the bottom row of every Claude Code pane in
//     production. Skipped when the active profile leaves it empty.
//   - separator detection: 20+ consecutive ─ (U+2500) characters —
//     covers the below-input separator. Adapter-universal (box-drawing
//     separators are a TUI-wide convention), so it is NOT profile-gated. The
//     threshold is tuned to avoid false-positives on operator-typed content (an
//     operator who types 20 box-drawing horizontals in a row is doing something
//     unusual).
func isInputAreaBoundary(line string) bool {
	trimmed := strings.TrimSpace(line)
	if m := activeProfile.StatusLineMarker; m != "" && strings.HasPrefix(trimmed, m) {
		return true
	}
	if strings.Contains(trimmed, strings.Repeat("─", 20)) {
		return true
	}
	return false
}

// hashContent returns a short hex SHA-256 prefix of the content. Used
// by the observe-gate's stale-detection loop to compare iterations
// without holding the full content in memory across the loop. The
// prefix is collision-safe enough for "did the operator type since
// last check" semantics (32 bits of distinguishing entropy).
func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}

// PendingMessageMarker is the single character (or short string) the
// mailman injects into the operator's input row as a one-shot
// visibility signal that a bus message is queued and the gate is
// waiting (#95). Default is 📫 (U+1F4EB Closed Mailbox with Raised
// Flag) per the operator's 2026-06-04 design call — readable at small
// font sizes and structurally unique against typical Claude Code
// chrome / status content.
//
// The marker is a load-bearing operator-facing affordance: it rides
// along into the bus message if the operator doesn't notice / delete
// it. Recipients seeing 📫 in a message body know what it means: "the
// sender saw a pending bus message land while they were typing."
const PendingMessageMarker = "📫"

// NotifyPendingMessage injects PendingMessageMarker into the
// receiver's input row via a single `tmux send-keys -l` call. Used by
// the mailman serve loop as a one-shot visibility signal (#95) when
// the observe-gate first detects StateAwaitingOperator — the
// operator's input row gains one extra character so they have a
// visible indication that a bus message is pending.
//
// One-shot semantics (no follow-up Enter, no cleanup). Operator
// either notices and deletes it via Backspace (gate keeps waiting),
// or notices and finishes typing (📫 rides along into the sent
// message), or doesn't notice at all (same as the previous case). The
// mailman does NOT track or remove the marker — operator-deletes-or-
// it-rides-along is the intentional design, sibling to the (b)-
// rejected-style honesty that informs the (c) flush.
//
// Idempotency is enforced at the gate layer (ObserveGate's
// OnOperatorTyping callback fires at most once per delivery cycle);
// this helper does no caller-tracking of its own.
func NotifyPendingMessage(ctx context.Context, pane string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if out, err := tmuxRun(ctx, nil, "send-keys", "-t", pane, "-l", PendingMessageMarker); err != nil {
		return fmt.Errorf("tmuxio: send-keys notify-pending: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// clearPressesPerLine is how many Ctrl+U presses ClearInput sends per visual
// line of input content. Codex's TUI clears a multi-line draft in TWO presses
// per line — one to clear the line's TEXT, a second to JOIN it onto the line
// above (deleting the newline) — so a single press per line under-clears and
// leaves residual lines that the subsequent paste then compounds with
// (operator-substrate-witnessed in the #336 live-probe gate; banked in the
// codex-paste-substrate findings). An N-line codex draft needs ~2N-1 presses
// (N text-clears + N-1 joins); sending 2N over-clears by at most one harmless
// press. Claude clears the whole input on the FIRST Ctrl+U, so for it every
// press after the first lands on an already-empty line and is a no-op. So 2N
// is adapter-agnostic-by-over-clear — it satisfies codex's per-line cost and
// stays harmless for Claude, with no per-adapter branch.
//
// Why deterministic over-clear and not a cursor-loop-until-empty: a loop that
// reads the cursor after each press to stop when the row is empty would be
// "exact", but it reintroduces the slow-render timing fragility the #336
// probes exposed — codex renders large input slowly, so a cursor read taken
// right after a Ctrl+U can observe stale pre-clear state and either stop early
// or spin. Deterministic 2N sidesteps pane-timing entirely. See the PR body
// for the rejected-alternative reasoning.
const clearPressesPerLine = 2

// sendCtrlU sends exactly one Ctrl+U keystroke to pane via a single send-keys
// call (no Enter follow-up). The shared primitive under SendCtrlU / ClearInput.
func sendCtrlU(ctx context.Context, pane string) error {
	if out, err := tmuxRun(ctx, nil, "send-keys", "-t", pane, "C-u"); err != nil {
		return fmt.Errorf("tmuxio: send-keys C-u: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ClearInput clears the recipient's input row, the InputControl-axis clear
// gesture (#336). It sends clearPressesPerLine (2) Ctrl+U presses per line of
// current input content (minimum one line): codex needs ~2 presses per line to
// fully clear a multi-line draft (text-clear + line-join), and Claude's extra
// presses land on an already-empty line and are harmless — see
// clearPressesPerLine for the adapter-agnostic over-clear rationale.
//
// lineCount is the number of (visual) lines in the content being cleared —
// e.g. strings.Count(content, "\n")+1 for extractInputContent's output, which
// captures visual rows, matching codex's per-visual-line clear. Values < 1 are
// treated as 1.
//
// Like the single-press form it does NOT follow up with Enter — the caller
// owns the subsequent paste + Enter via Deliver.
func ClearInput(ctx context.Context, pane string, lineCount int) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if lineCount < 1 {
		lineCount = 1
	}
	presses := lineCount * clearPressesPerLine
	for i := 0; i < presses; i++ {
		if err := sendCtrlU(ctx, pane); err != nil {
			return fmt.Errorf("tmuxio: clear-input (%d/%d): %w", i+1, presses, err)
		}
	}
	return nil
}

// SendCtrlU sends a single Ctrl+U keystroke to the receiver pane. Unlike
// ClearInput it sends exactly ONE press regardless of adapter — the one-line
// form retained for callers clearing a known single-line input (where codex's
// line-join cost does not apply: a single line has nothing above to join onto,
// so one press clears it). Prefer ClearInput(ctx, pane, lineCount) when the
// content may be multi-line.
func SendCtrlU(ctx context.Context, pane string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	return sendCtrlU(ctx, pane)
}

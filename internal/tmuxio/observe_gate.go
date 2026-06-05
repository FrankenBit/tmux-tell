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

// ObserveGateOpts configures the read-only-observe-only delivery gate
// introduced by tmux-msg #92. Defaults are filled in by
// ObserveGate when fields are zero-valued.
type ObserveGateOpts struct {
	// PollIntervalMin is the initial sleep between observe iterations.
	// Default 3s.
	PollIntervalMin time.Duration
	// PollIntervalMax caps the per-iteration sleep after backoff.
	// Default 15s.
	PollIntervalMax time.Duration
	// BackoffFactor is the multiplicative growth on PollInterval per
	// iteration when the chamber is not yet ready (StateAwaitingOperator,
	// StateWorking, StateAtRestInCompaction, StateUnknown). Default 1.5.
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
}

// GateOutcome reports the gate's decision and the evidence that
// produced it. The caller (mailman serve loop) uses Stale +
// InputContent to decide whether to archive + flush before pasting.
type GateOutcome struct {
	// Reason is a one-line human-readable explanation of how the gate
	// decided to return. Suitable for verbose mailman logging.
	Reason string
	// State is the ChamberState classification that produced the
	// outcome. StateIdle for the happy path; StateAwaitingOperator for
	// stale-flush; potentially other states on MaxWait or context
	// cancellation.
	State State
	// Iterations is the number of ChamberState polls before the gate
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
}

// ErrMaxWaitExceeded is returned when ObserveGate's safety cap fires
// without reaching either the idle path or the stale-flush path.
var ErrMaxWaitExceeded = errors.New("tmuxio: observe-gate MaxWait exceeded")

// ObserveGate observes the receiver pane via repeated read-only
// ChamberState calls until the chamber is ready to receive a paste
// delivery. The substrate-class is read-only-observe: zero pane
// mutation, zero probe injection, zero send-keys before the caller's
// own Deliver call (or the caller's Ctrl+U on Stale flush).
//
// Replaced the probe-and-watch gate per #92 (shipped in v0.3.0; the
// legacy primitives were swept out in v0.4.0 / #94). The algorithm:
//
//  1. Poll ChamberState; on StateIdle (cursor at sentinel — empty or
//     auto-suggestion ghost-text), return immediately with Stale=false.
//  2. On StateAwaitingOperator (cursor past sentinel = operator
//     drafting), capture the input row's content past the
//     PromptSentinel and hash it. Track the time-since-first-stable.
//     If the hash remains stable for at least InputStaleThreshold,
//     return Stale=true with InputContent populated so the caller can
//     archive + Ctrl+U + paste.
//  3. On StateWorking / StateAtRestInCompaction / StateUnknown, treat
//     as a safer-default wait — defer and re-poll.
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
		hashSeen           string
		hashSeenAt         time.Time
		lastContent        string
		iterations         int
		lastState          State
		notifiedOfTyping   bool
	)

	for {
		iterations++
		if opts.Ping != nil {
			opts.Ping()
		}
		state, ev, err := ChamberState(ctx, pane)
		lastState = state
		if err != nil {
			return GateOutcome{
				Reason:     fmt.Sprintf("ChamberState error: %v", err),
				State:      state,
				Iterations: iterations,
			}, fmt.Errorf("tmuxio: observe-gate: %w", err)
		}

		switch state {
		case StateIdle:
			return GateOutcome{
				Reason:     "idle: " + ev.Reason,
				State:      state,
				Iterations: iterations,
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

		case StateWorking, StateAtRestInCompaction, StateUnknown:
			// Safer-default wait. The chamber is busy / compacting /
			// in an unrecognized state; defer + re-poll.
		}

		// Check the safety cap before sleeping for the next iteration.
		if time.Now().After(deadline) {
			return GateOutcome{
				Reason: fmt.Sprintf("MaxWait %s exceeded; last state=%s",
					opts.MaxWait, lastState),
				State:        lastState,
				Iterations:   iterations,
				Stale:        true,
				InputContent: lastContent,
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
// nil error when no sentinel row is found (e.g., chamber paused
// mid-spinner — the input area isn't visible).
//
// Multi-line handling per #96: the legacy implementation captured
// only the first sentinel-row's content, so multi-line drafts got
// silently truncated at flush time (Ctrl+U cleared everything but
// the archived stranded_draft only held line 1). The walk-until-
// boundary shape matches Claude Code's TUI layout:
//
//	─────── <chamber title> ──     ← title separator (above input)
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
	var inputLines []string
	inInput := false
	for _, line := range strings.Split(string(out), "\n") {
		if !inInput {
			if rest, ok := strings.CutPrefix(line, PromptSentinel); ok {
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

// isInputAreaBoundary reports whether a captured row marks the
// boundary between the input area and the chrome below it (the
// below-input separator or the status line). Used by
// extractInputContent's walk-until-boundary multi-line capture (#96).
//
// Two recognizers:
//   - status-line marker: ⏵⏵ (U+23F5) — present on the bottom row
//     of every Claude Code pane in production
//   - separator detection: 20+ consecutive ─ (U+2500) characters —
//     covers the below-input separator. The threshold is tuned to
//     avoid false-positives on operator-typed content (an operator
//     who types 20 box-drawing horizontals in a row is doing
//     something unusual).
func isInputAreaBoundary(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "⏵⏵") {
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

// SendCtrlU sends a single Ctrl+U keystroke to the receiver pane,
// clearing the current input line in Claude Code's TUI. Does NOT
// follow up with Enter — the caller is responsible for the subsequent
// paste + Enter via Deliver.
//
// Used by the mailman serve loop after a GateOutcome.Stale=true
// outcome, AFTER the cleared content has been successfully archived
// as kind=stranded_draft. On clear failure, the caller falls back to
// the (a) compound-delivery path per the #92 design.
func SendCtrlU(ctx context.Context, pane string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if out, err := tmuxRun(ctx, nil, "send-keys", "-t", pane, "C-u"); err != nil {
		return fmt.Errorf("tmuxio: send-keys C-u: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

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
// introduced by cli-semaphore #92. Defaults are filled in by
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
	// stop), matching WaitForQuietPane's quiet-cap semantics. Default
	// 5m, same as the legacy quiet-max-wait default.
	MaxWait time.Duration
	// Ping is an optional callback the gate invokes on each iteration
	// so the caller can keep external timers (notably systemd's
	// WatchdogSec) alive during long observe loops. May be nil.
	// Sibling to QuietOpts.Ping; mailman wires this to sdnotify
	// Watchdog at startup.
	Ping func()
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
// Sibling to ErrCapExceeded (the WaitForQuietPane equivalent).
var ErrMaxWaitExceeded = errors.New("tmuxio: observe-gate MaxWait exceeded")

// ObserveGate observes the receiver pane via repeated read-only
// ChamberState calls until the chamber is ready to receive a paste
// delivery. The substrate-class is read-only-observe: zero pane
// mutation, zero probe injection, zero send-keys before the caller's
// own Deliver call (or the caller's Ctrl+U on Stale flush).
//
// Replaces the probe-and-watch WaitForQuietPane gate per #92. The
// algorithm:
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
		hashSeen     string
		hashSeenAt   time.Time
		lastContent  string
		iterations   int
		lastState    State
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
// trimmed content past PromptSentinel on the first row that has it.
// Returns "" with nil error when no sentinel row is found (e.g.,
// chamber paused mid-spinner — the input area isn't visible).
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
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, PromptSentinel); ok {
			return strings.TrimRight(rest, " "), nil
		}
	}
	return "", nil
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

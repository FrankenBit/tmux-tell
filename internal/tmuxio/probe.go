package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QuietProbe is the U+2500 box-drawing horizontal — same shape as the
// chat-header rule so if it accumulates in the input box it visually
// extends the header rather than looking like garbage. The mailman
// injects this one character (no Enter) and watches for activity.
const QuietProbe = "─"

// ErrCapExceeded means the quiet-pane wait hit its total-time cap
// without finding a quiet window. The mailman's policy on this is
// "deliver anyway with a WARN" — `journalctl` shows why the message
// dwelled, the operator gets fragmented input on the rare bad case
// instead of indefinite starvation.
var ErrCapExceeded = errors.New("tmuxio: quiet-pane cap exceeded")

// QuietOpts configures the probe-and-watch pre-delivery gate. Zero
// values pick sensible defaults so callers can pass `QuietOpts{}` to
// get the production behaviour.
type QuietOpts struct {
	// ObserveWindow is how long to wait after injecting the probe
	// before re-capturing the pane. Default 5s.
	ObserveWindow time.Duration
	// BackoffInterval is the dwell between an activity-detected probe
	// and the next probe attempt. Default 60s.
	BackoffInterval time.Duration
	// MaxWait caps the total wait across all probe iterations. After
	// this, WaitForQuietPane returns ErrCapExceeded and the mailman
	// delivers anyway with a WARN log. Default 5min.
	//
	// Sized to the operator-latency expectation, not the absolute
	// worst case: a human who sees the probe appear typically needs
	// 2-10 minutes to close a sentence or cut their in-progress
	// message out of the input box. Beyond that they've usually
	// walked away, so delaying further just buys nothing. The
	// fragmented-delivery WARN past the cap is the honest signal
	// when this assumption fails.
	MaxWait time.Duration
	// CaptureLines is how many bottom rows of the pane to capture for
	// the diff. Smaller is sharper (less noise from agent-streaming
	// output above the input box). Default 5.
	//
	// LOAD-BEARING ASSUMPTION: the chosen N must isolate the input row
	// from any pane region that changes for non-operator reasons. The
	// gate's "agent vs operator" distinction depends on this. Things
	// that can break the assumption — bumping CaptureLines down to 1-2
	// is the usual lever — include:
	//
	//   - Claude Code's status-line tick during streaming (token
	//     counter, spinner, "Press ESC to interrupt" hint flips). Any
	//     of these in the captured tail will be misread as activity
	//     and cause unnecessary backoff.
	//   - Tool-use frames in the response area extending into the
	//     bottom rows during long tool-heavy turns. Same effect.
	//   - Alternate frontends with different bottom-row content.
	//
	// MaxWait acts as the backstop: in the worst case (long busy turn
	// where the gate never sees clean quiet), the cap fires and the
	// mailman delivers anyway with a WARN log.
	CaptureLines int

	// Ping, when non-nil, is invoked periodically during long internal
	// sleeps (the ObserveWindow and the activity-detected backoff). It
	// exists so the mailman can keep `sd_notify(WATCHDOG=1)` ticking
	// without coupling this package to internal/sdnotify — pass a
	// closure that calls Watchdog().
	//
	// Without Ping, the BackoffInterval (default 60s) can outrun
	// systemd's WatchdogSec=30s and trip SIGABRT on the mailman
	// (incident 2026-05-30: surveyor mailman crashed during a probe
	// backoff that didn't ping in time).
	Ping func()
	// PingInterval is the upper bound between Ping() calls during
	// internal sleeps. Default 10s — well under the typical
	// WatchdogSec=30s used by the mailman unit so two consecutive
	// pings still fit inside the deadline.
	PingInterval time.Duration
}

func (o QuietOpts) withDefaults() QuietOpts {
	if o.ObserveWindow <= 0 {
		o.ObserveWindow = 5 * time.Second
	}
	if o.BackoffInterval <= 0 {
		o.BackoffInterval = 60 * time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Minute
	}
	if o.CaptureLines <= 0 {
		o.CaptureLines = 5
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 10 * time.Second
	}
	return o
}

// WaitForQuietPane blocks until the recipient pane appears idle to the
// operator's eye, then returns nil. "Appears idle" means: we inject a
// single probe character and 5 seconds later see exactly that probe
// added — nothing else changed.
//
// On activity (the post-probe capture differs by more than just our
// probe), we back off for opts.BackoffInterval and retry, up to
// opts.MaxWait total. Crossing the cap returns ErrCapExceeded; the
// caller's policy is to deliver anyway and log.
//
// Per operator instruction: we do NOT backspace the probe on the
// backoff path. The operator either notices the stray dash and removes
// it, or it ends up in whatever they're typing. Eating a real
// keystroke with a guess-backspace would be worse.
//
// On the quiet path, we DO backspace the single probe before returning
// so the recipient's input is genuinely empty when the mailman starts
// pasting the rendered message or typing the slash-command.
func WaitForQuietPane(ctx context.Context, pane string, opts QuietOpts) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.MaxWait)

	for {
		if time.Now().After(deadline) {
			return ErrCapExceeded
		}
		before, err := captureTail(ctx, pane, opts.CaptureLines)
		if err != nil {
			return fmt.Errorf("tmuxio: capture before-probe: %w", err)
		}
		if out, err := tmuxRun(ctx, nil,
			"send-keys", "-t", pane, "-l", QuietProbe); err != nil {
			return fmt.Errorf("tmuxio: probe send-keys: %w: %s",
				err, strings.TrimSpace(string(out)))
		}
		if err := sleepWithPing(ctx, opts.ObserveWindow, opts.Ping, opts.PingInterval); err != nil {
			return err
		}
		after, err := captureTail(ctx, pane, opts.CaptureLines)
		if err != nil {
			return fmt.Errorf("tmuxio: capture after-probe: %w", err)
		}
		if isQuiet(before, after, QuietProbe) {
			if out, err := tmuxRun(ctx, nil,
				"send-keys", "-t", pane, "BSpace"); err != nil {
				return fmt.Errorf("tmuxio: probe backspace: %w: %s",
					err, strings.TrimSpace(string(out)))
			}
			return nil
		}
		// Activity. Do NOT backspace — the operator will deal with
		// the dash themselves or let it slide into their text. Wait,
		// retry. The cap check at the top of the next iteration handles
		// the runaway case.
		if err := sleepWithPing(ctx, opts.BackoffInterval, opts.Ping, opts.PingInterval); err != nil {
			return err
		}
	}
}

// sleepWithPing blocks for d, returning early when ctx cancels. When
// ping is non-nil, it's called at most every pingEvery seconds during
// the sleep — the systemd watchdog stays happy through long backoffs.
//
// Bug history: before #(this commit), WaitForQuietPane used plain
// `time.After(60s)` for the activity-detected backoff. The mailman
// unit's `WatchdogSec=30s` tripped at 30s and SIGABRT'd the process
// mid-backoff (2026-05-30 surveyor mailman crash). The mailman now
// passes a ping closure that calls sd_notify; sleepWithPing fires it
// often enough that a backoff longer than WatchdogSec is safe.
func sleepWithPing(ctx context.Context, d time.Duration, ping func(), pingEvery time.Duration) error {
	if d <= 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	if ping == nil || pingEvery >= d {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	deadline := time.Now().Add(d)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil
		}
		chunk := pingEvery
		if chunk > remaining {
			chunk = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(chunk):
			ping()
		}
	}
}

// captureTail returns the last `lines` visible rows of the pane.
// tmux's negative line numbers count backward from the bottom of the
// visible region; -lines through -1 is the bottom strip.
func captureTail(ctx context.Context, pane string, lines int) (string, error) {
	out, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane,
		"-S", fmt.Sprintf("-%d", lines-1), "-E", "-1")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isQuiet returns true when `after` equals `before` with exactly one
// instance of `probe` inserted. The insertion can land anywhere in
// `after` — Claude Code's input box might wrap or sit on a row we
// can't address directly, but the global "one more probe, nothing
// else changed" invariant is reliable.
//
// We strip the LAST occurrence of `probe` from `after` because:
//   - `before` may already contain `probe` characters (the chat header
//     uses the same shape).
//   - Our probe is the most recently injected, so it's the rightmost
//     newly added one.
func isQuiet(before, after, probe string) bool {
	idx := strings.LastIndex(after, probe)
	if idx == -1 {
		// Our probe didn't land in the visible region. Treat as
		// activity — something is wrong, better to back off than to
		// assume quiet.
		return false
	}
	stripped := after[:idx] + after[idx+len(probe):]
	return stripped == before
}

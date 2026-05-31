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
// injects this character via paste-buffer (no Enter) and watches for
// operator interference.
const QuietProbe = "─"

// ErrCapExceeded means the quiet-pane wait hit its total-time cap
// without finding an operator-quiet window. The mailman's policy on
// this is "deliver anyway with a WARN" — `journalctl` shows why the
// message dwelled, the operator gets fragmented input on the rare bad
// case instead of indefinite starvation.
var ErrCapExceeded = errors.New("tmuxio: quiet-pane cap exceeded")

// QuietOpts configures the pre-delivery operator-quiet gate. Zero
// values pick sensible defaults so callers can pass `QuietOpts{}` to
// get the production behaviour.
//
// The gate's contract (per #52): protect against operator-typing on
// the receiving pane. It does NOT protect against recipient-busy or
// TUI activity outside the input row — those were never the bus's
// concern, and the v0.2.1 four-way verdict's TUI-noise branch added
// 5-minute cap-hits during heavy Claude Code work for no benefit.
type QuietOpts struct {
	// ObserveWindow is how long to wait after pasting each of the two
	// probe characters before pasting the next / capturing the after-
	// state. Used both between dash#1 → dash#2 and between dash#2 →
	// final capture. Gives the operator time to react to each dash
	// individually. Default 3s (so total per-iteration is ~6s of
	// observation).
	ObserveWindow time.Duration

	// InputActivityBackoff is the dwell after a probe iteration
	// detected operator interference (probe row content didn't match
	// "before-state + N trailing probes"). Default 60s, giving the
	// operator time to finish their thought before we retry.
	InputActivityBackoff time.Duration

	// MaxWait caps the total wait across all probe iterations. After
	// this, WaitForQuietPane returns ErrCapExceeded and the mailman
	// delivers anyway with a WARN log. Default 5min.
	//
	// Sized to the operator-latency expectation: a human who sees the
	// probes appear typically needs 2-10 minutes to close a sentence
	// or cut their in-progress message out of the input box. Beyond
	// that they've usually walked away, so delaying further just buys
	// nothing. The fragmented-delivery WARN past the cap is the honest
	// signal when this assumption fails.
	MaxWait time.Duration

	// Ping, when non-nil, is invoked periodically during long internal
	// sleeps. The mailman wires this to sd_notify so the systemd
	// watchdog stays happy through long backoffs without coupling this
	// package to internal/sdnotify.
	Ping func()
	// PingInterval is the upper bound between Ping() calls during
	// internal sleeps. Default 10s — well under the typical
	// WatchdogSec=30s used by the mailman unit.
	PingInterval time.Duration
}

func (o QuietOpts) withDefaults() QuietOpts {
	if o.ObserveWindow <= 0 {
		o.ObserveWindow = 3 * time.Second
	}
	if o.InputActivityBackoff <= 0 {
		o.InputActivityBackoff = 60 * time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Minute
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 10 * time.Second
	}
	return o
}

// DeltaKind classifies whether the operator interfered with our
// probe sequence during a single gate iteration.
type DeltaKind int

const (
	// DeltaQuiet: the input row gained exactly the two probes we
	// pasted, with nothing else added or removed. Safe to deliver.
	DeltaQuiet DeltaKind = iota
	// DeltaInputActivity: the input row's content differs from
	// "before-state + N trailing probes" — operator typed, removed a
	// probe, or interfered in some other way. Back off.
	DeltaInputActivity
)

func (d DeltaKind) String() string {
	switch d {
	case DeltaQuiet:
		return "quiet"
	case DeltaInputActivity:
		return "input_activity"
	default:
		return "unknown"
	}
}

// WaitForQuietPane blocks until the recipient pane's input row appears
// operator-quiet, then returns nil. "Operator-quiet" means: we pasted
// two `─` characters into the input box and verified that the input
// row's only change is those two characters appended at the end.
//
// The two-dash design (per #52):
//  1. Paste dash #1 — dismisses any ghost-text suggested prompt the
//     CLI may be showing (Claude Code's auto-complete proposals).
//  2. Wait ObserveWindow — give the operator time to react.
//  3. Paste dash #2 — the actual quiet-state probe.
//  4. Wait ObserveWindow — give the operator time to react.
//  5. Capture pane. Look for a row that ends with exactly N trailing
//     probes (N = previously-accumulated + 2) and whose content with
//     those probes stripped matches the before-capture's corresponding
//     row. That match is DeltaQuiet → backspace all accumulated probes
//     → deliver.
//  6. Otherwise → DeltaInputActivity. Sleep InputActivityBackoff. On
//     retry, restart from step 1 (the operator may have completed and
//     submitted, leaving a fresh ghost-text prompt to dismiss).
//
// Probes are NOT backspaced between iterations — they accumulate in
// the input box as a visible "I see you" stack. Only the final
// pre-delivery cleanup backspaces all accumulated probes.
//
// What the gate explicitly DOES NOT protect against (these were
// expensive non-features in the v0.2.1 four-way verdict design):
//   - Recipient mid-conversation (streaming output above the input
//     row). The bus doesn't gate on recipient-busy.
//   - TUI animations / status-line ticks. Ignored.
//   - Cursor blinks in the response area. Ignored.
//
// The gate cares only whether the operator is typing on the input row
// of the receiving pane.
//
// Crossing MaxWait returns ErrCapExceeded. Before returning, all
// accumulated probes are backspaced so the recipient's input is clean
// even when we're delivering with the cap-exceeded WARN.
func WaitForQuietPane(ctx context.Context, pane string, opts QuietOpts) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	opts = opts.withDefaults()
	deadline := time.Now().Add(opts.MaxWait)
	probesAccumulated := 0

	cleanupProbes := func() {
		for i := 0; i < probesAccumulated; i++ {
			_, _ = tmuxRun(ctx, nil, "send-keys", "-t", pane, "BSpace")
		}
		probesAccumulated = 0
	}

	for {
		if time.Now().After(deadline) {
			cleanupProbes()
			return ErrCapExceeded
		}
		before, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
		if err != nil {
			return fmt.Errorf("tmuxio: capture before-probe: %w", err)
		}

		// Paste dash #1 — dismisses any suggested-prompt ghost text.
		if out, err := tmuxRun(ctx, nil,
			"send-keys", "-t", pane, "-l", QuietProbe); err != nil {
			return fmt.Errorf("tmuxio: probe send-keys #1: %w: %s",
				err, strings.TrimSpace(string(out)))
		}
		probesAccumulated++
		if err := sleepWithPing(ctx, opts.ObserveWindow, opts.Ping, opts.PingInterval); err != nil {
			return err
		}

		// Paste dash #2 — the actual quiet-state probe.
		if out, err := tmuxRun(ctx, nil,
			"send-keys", "-t", pane, "-l", QuietProbe); err != nil {
			return fmt.Errorf("tmuxio: probe send-keys #2: %w: %s",
				err, strings.TrimSpace(string(out)))
		}
		probesAccumulated++
		if err := sleepWithPing(ctx, opts.ObserveWindow, opts.Ping, opts.PingInterval); err != nil {
			return err
		}

		after, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
		if err != nil {
			return fmt.Errorf("tmuxio: capture after-probe: %w", err)
		}

		switch analyzeDelta(string(before), string(after), QuietProbe, 2) {
		case DeltaQuiet:
			cleanupProbes()
			return nil
		case DeltaInputActivity:
			// Don't backspace. Probes accumulate as the "I see you"
			// visual handshake. Operator can clear them manually if
			// they want to type next.
			if err := sleepWithPing(ctx, opts.InputActivityBackoff, opts.Ping, opts.PingInterval); err != nil {
				return err
			}
		}
	}
}

// analyzeDelta classifies whether the operator interfered with the
// probe sequence between before-capture and after-capture.
//
// addedProbes is the number of probe characters we just pasted in
// this iteration (always 2 in the current design — dash#1 + dash#2).
// Any previously-accumulated probes from prior backoff iterations
// are part of `before` and don't enter into addedProbes.
//
// Decision:
//   - Find a row in `after` that ends with exactly `addedProbes`
//     trailing probe characters. Strip them. If the result matches
//     any row in `before` (typically the input row at the same
//     position), the only change to the input row was our probes
//     landing — DeltaQuiet.
//   - Otherwise — operator typed (extra chars on input row), removed
//     a probe (fewer trailing probes than expected), or the input
//     row content otherwise changed in a way the strip-N trick can't
//     recover — DeltaInputActivity.
//
// Non-input rows changing is IGNORED. Conversation-area streaming,
// status-line ticks, spinner animations — all invisible to this gate.
// The bus protects against operator-typing on the input row, nothing
// else.
//
// Edge cases:
//   - The chat-header `─── header ───` and other dash-containing
//     content don't END with the probe (they end with non-probe), so
//     they're skipped by the "ends-with-probe" filter.
//   - A separator row of pure dashes (e.g., `────────────`) ends with
//     the probe; strip N gives a shorter all-dash string that won't
//     match the corresponding before-state separator (also all dashes
//     of the same length). Filtered out by the strip-then-compare.
//   - Operator types `─` themselves: the trailing-probe check can't
//     distinguish operator's dash from ours. Known limitation,
//     acceptable risk per #52.
func analyzeDelta(before, after, probe string, addedProbes int) DeltaKind {
	if addedProbes <= 0 {
		return DeltaInputActivity
	}
	afterLines := strings.Split(after, "\n")
	beforeLines := strings.Split(before, "\n")

	for _, afterRow := range afterLines {
		// Require exactly `addedProbes` trailing probe characters AND
		// either start-of-row or a non-probe character before them.
		// This filters out all-dash separator rows.
		stripped, ok := stripTrailingProbes(afterRow, probe, addedProbes)
		if !ok {
			continue
		}
		// `stripped` is what the input row looked like just before our
		// `addedProbes` probes landed. Look for it in `before`.
		for _, beforeRow := range beforeLines {
			if beforeRow == stripped {
				return DeltaQuiet
			}
		}
	}
	return DeltaInputActivity
}

// stripTrailingProbes removes exactly n trailing probe characters
// from s. Returns the stripped string and true if successful (s had
// at least n trailing probes); returns the original string and false
// otherwise.
//
// Note: stripping doesn't reject rows that originally had > n
// trailing probes. That case is handled at the analyzeDelta level by
// the inner before/after content comparison — a separator row of
// pure dashes would strip down to a shorter all-dash string that
// won't match the (unchanged) separator in `before`. Accumulated
// probes on the input row from prior backoff iterations strip down
// to "prompt + remaining prior probes" which DOES match the input
// row in `before` (which had those same prior probes).
func stripTrailingProbes(s, probe string, n int) (string, bool) {
	for i := 0; i < n; i++ {
		if !strings.HasSuffix(s, probe) {
			return s, false
		}
		s = s[:len(s)-len(probe)]
	}
	return s, true
}

// sleepWithPing blocks for d, returning early when ctx cancels. When
// ping is non-nil, it's called at the end of every chunk AND at the
// end of every short sleep, so the systemd watchdog stays happy even
// when many short sleeps run back-to-back.
//
// Bug history:
//   - 2026-05-30 (surveyor): plain `time.After(60s)` for the activity-
//     detected backoff outran the mailman's WatchdogSec=30s and
//     SIGABRT'd the process mid-backoff. Fixed by adding the Ping
//     callback + this chunked sleep.
//   - 2026-05-31 (bosun, 4 crashes): the short-sleep no-chunk path
//     (used for ObserveWindow=5s when pingEvery=15s) didn't ping
//     either, so consecutive short sleeps from outer-loop iterations
//     could accumulate >30s of silent time under load. Fixed by
//     always pinging at the end of a short sleep too.
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
			if ping != nil {
				ping()
			}
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

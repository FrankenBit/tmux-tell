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

	// InputActivityBackoff is the dwell after a probe iteration that
	// detected operator activity in the input row — typing, deleting
	// our probe (the "I see you, hold on" handshake), or any edit
	// that touches the input. Default 60s, giving the operator
	// enough time to finish their thought before we retry.
	InputActivityBackoff time.Duration

	// TUINoiseBackoff is the dwell after a probe iteration where the
	// input row was clean but other rows changed — Claude Code's
	// status-line tick, spinner cycling, streaming tool-use frames.
	// Not operator-driven, so we retry sooner. Default 5s.
	//
	// This is the #32 "empirical win" — separating real operator
	// activity from TUI re-render noise so the cap doesn't fire on
	// every delivery during active agent work.
	TUINoiseBackoff time.Duration

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

	// Ping, when non-nil, is invoked periodically during long internal
	// sleeps. The mailman wires this to sd_notify so the systemd
	// watchdog stays happy through long backoffs without coupling
	// this package to internal/sdnotify.
	Ping func()
	// PingInterval is the upper bound between Ping() calls during
	// internal sleeps. Default 10s — well under the typical
	// WatchdogSec=30s used by the mailman unit.
	PingInterval time.Duration
}

func (o QuietOpts) withDefaults() QuietOpts {
	if o.ObserveWindow <= 0 {
		o.ObserveWindow = 5 * time.Second
	}
	if o.InputActivityBackoff <= 0 {
		o.InputActivityBackoff = 60 * time.Second
	}
	if o.TUINoiseBackoff <= 0 {
		o.TUINoiseBackoff = 5 * time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 5 * time.Minute
	}
	if o.PingInterval <= 0 {
		o.PingInterval = 10 * time.Second
	}
	return o
}

// DeltaKind classifies what changed in the pane between the before-
// and after-probe captures.
type DeltaKind int

const (
	// DeltaQuiet: after == before + probe (only the probe was added,
	// and it landed on the input row). Deliver path.
	DeltaQuiet DeltaKind = iota
	// DeltaInputActivity: the input row changed beyond the probe —
	// operator typed, deleted the probe (handshake), or otherwise
	// edited. Long backoff to give them time to finish.
	DeltaInputActivity
	// DeltaTUINoise: the input row is clean (after == before + probe
	// on that row) but other rows differ — Claude Code's status-line
	// tick, spinner cycling, streaming output. Not operator-driven;
	// short backoff and retry sooner.
	DeltaTUINoise
	// DeltaProbeMissing: the probe didn't land on the captured input
	// row. Tmux ate the keystroke, capture-pane lagged, or the input
	// row index is wrong. Treated as operator activity to be safe.
	DeltaProbeMissing
)

func (d DeltaKind) String() string {
	switch d {
	case DeltaQuiet:
		return "quiet"
	case DeltaInputActivity:
		return "input_activity"
	case DeltaTUINoise:
		return "tui_noise"
	case DeltaProbeMissing:
		return "probe_missing"
	default:
		return "unknown"
	}
}

// WaitForQuietPane blocks until the recipient pane appears idle to the
// operator's eye, then returns nil. "Appears idle" means: we inject a
// probe character into the input row and observe whether the input
// row changes beyond our probe over the next ObserveWindow.
//
// The verdict is one of DeltaKind:
//   - DeltaQuiet → backspace accumulated probes, deliver.
//   - DeltaInputActivity → back off InputActivityBackoff and retry.
//     Probes accumulate (no backspace) so the operator sees the
//     dashes piling up; deleting them is also a valid handshake.
//   - DeltaTUINoise → back off the much shorter TUINoiseBackoff.
//     Doesn't burn budget on TUI re-renders that aren't operator
//     activity. This is the #32 empirical win.
//   - DeltaProbeMissing → back off InputActivityBackoff (safe
//     default) and retry.
//
// Crossing MaxWait returns ErrCapExceeded. Before returning, all
// accumulated probes are backspaced so the recipient's input is
// clean even when we're delivering with the cap-exceeded WARN —
// the visual-mess fix Alex reported on 2026-05-30.
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
		if out, err := tmuxRun(ctx, nil,
			"send-keys", "-t", pane, "-l", QuietProbe); err != nil {
			return fmt.Errorf("tmuxio: probe send-keys: %w: %s",
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

		switch analyzeDelta(string(before), string(after), QuietProbe) {
		case DeltaQuiet:
			cleanupProbes()
			return nil
		case DeltaInputActivity:
			if err := sleepWithPing(ctx, opts.InputActivityBackoff, opts.Ping, opts.PingInterval); err != nil {
				return err
			}
		case DeltaTUINoise:
			if err := sleepWithPing(ctx, opts.TUINoiseBackoff, opts.Ping, opts.PingInterval); err != nil {
				return err
			}
		case DeltaProbeMissing:
			if err := sleepWithPing(ctx, opts.InputActivityBackoff, opts.Ping, opts.PingInterval); err != nil {
				return err
			}
		}
	}
}

// analyzeDelta classifies the change between before- and after-probe
// captures. probe is the literal injected character.
//
// The input row is identified by where the probe actually landed, not
// by tmux's cursor_y — the rendering cursor moves around as Claude
// Code redraws output (tool calls, streaming), so cursor_y often
// points at the response area, not the input box. Typed input always
// lands in the input box regardless. Searching for "the row that
// gained exactly one probe and is otherwise unchanged" finds the input
// box reliably (2026-05-31 Bosun bug fix).
//
// Decision tree:
//  1. Find a row in `after` whose probe count is exactly one greater
//     than `before` AND whose content (with the rightmost probe
//     stripped) matches the corresponding row in before. That's the
//     input row, the probe landed cleanly.
//      - If other rows also changed → DeltaTUINoise.
//      - Otherwise → DeltaQuiet.
//  2. If no such row exists, but SOME row gained probe characters →
//     the operator typed on the input row (probe + their text), so the
//     strip-rightmost trick can't recover before. → DeltaInputActivity.
//  3. If no row contains a new probe at all → DeltaProbeMissing.
func analyzeDelta(before, after, probe string) DeltaKind {
	beforeLines := strings.Split(before, "\n")
	afterLines := strings.Split(after, "\n")

	// Pass 1: find the input row by the +1-probe-and-otherwise-clean
	// signature.
	for i := 0; i < len(afterLines); i++ {
		afterRow := afterLines[i]
		idx := strings.LastIndex(afterRow, probe)
		if idx == -1 {
			continue
		}
		stripped := afterRow[:idx] + afterRow[idx+len(probe):]
		var beforeRow string
		if i < len(beforeLines) {
			beforeRow = beforeLines[i]
		}
		if stripped != beforeRow {
			continue
		}
		// Found a row that gained exactly the probe with no other
		// changes — this is the input row. Now check the rest of the
		// pane for TUI noise.
		if len(beforeLines) != len(afterLines) {
			return DeltaTUINoise
		}
		for j := range beforeLines {
			if j == i {
				continue
			}
			if beforeLines[j] != afterLines[j] {
				return DeltaTUINoise
			}
		}
		return DeltaQuiet
	}

	// Pass 2: no clean +1-probe row. Either the operator typed on top
	// of the probe (so input row has probe + other chars and won't
	// match the "strip rightmost == before" pattern), or the probe
	// didn't land at all. Distinguish by checking if any row gained
	// probe characters.
	for i := 0; i < len(afterLines); i++ {
		afterCount := strings.Count(afterLines[i], probe)
		var beforeCount int
		if i < len(beforeLines) {
			beforeCount = strings.Count(beforeLines[i], probe)
		}
		if afterCount > beforeCount {
			return DeltaInputActivity
		}
	}
	return DeltaProbeMissing
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


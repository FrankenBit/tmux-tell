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

// PromptSentinel is the Claude Code TUI's input-row prefix — U+276F
// (Heavy Right-Pointing Angle Quotation Mark Ornament) followed by a
// space. Empirically verified as the marker for the current input row,
// bounded by horizontal-rule separators above and below the input area.
//
// FORWARD-WATCH: this constant is Claude-Code-version-dependent. If
// the Claude Code TUI's prompt character changes (theme update, version
// bump, customization), InputRowHasContent silently degrades to "no
// sentinel found" on every pane → over-gate. The prompt-sentinel tests
// would surface a paint-format change, but re-verify the constant
// during any major Claude Code version update.
const PromptSentinel = "❯ "

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

// QuickPresenceOpts configures QuickPresenceProbe — the asymmetric
// pre-check that gates whether to run the full WaitForQuietPane gate.
// Zero values pick aggressive defaults sized for the "buffer is
// empty" common case where the probe should finish in well under a
// second.
//
// The gate's contract (#63): a one-shot variant of the probe pattern
// — inject two dashes, wait briefly, capture, analyze, clean up. NO
// retry loop. The caller decides what to do with the verdict:
// DeltaQuiet → deliver immediately; DeltaInputActivity → fall back
// to the full WaitForQuietPane with its longer observe windows.
type QuickPresenceOpts struct {
	// PaintWait is how long to wait after pasting the two probes
	// before capturing the after-state. Sized to "long enough for
	// tmux to flush the paste into the visible buffer" — not enough
	// time for the operator to react to a probe they see. Default
	// 50ms.
	PaintWait time.Duration
}

func (o QuickPresenceOpts) withDefaults() QuickPresenceOpts {
	if o.PaintWait <= 0 {
		o.PaintWait = 50 * time.Millisecond
	}
	return o
}

// QuickPresenceProbe runs a single-cycle probe to classify the input
// row's current state. Returns DeltaQuiet when the input row had only
// previously-accumulated content (typical empty-prompt case);
// DeltaInputActivity when the operator has uncommitted content there.
//
// Compared to WaitForQuietPane: same probe-character + analyzeDelta
// machinery, but no observe windows past PaintWait and no retry loop.
// Adds two dashes that ARE backspaced before return (the caller never
// sees them) — unlike WaitForQuietPane's accumulating-probes design,
// QuickPresenceProbe leaves no visible artifact for the operator.
//
// Use case: gating whether to run the full WaitForQuietPane. When the
// quick probe says quiet, deliver immediately (~50ms overhead vs.
// ~6s+ for the full gate). When it says active, fall back to the
// full gate so the operator's draft is protected (#63 mitigation d).
//
// Errors from tmux propagate up. Callers should treat a probe error
// as conservative "fall back to full gate" rather than "deliver
// immediately" — the gate-when-uncertain default protects the same
// failure mode the quick probe was added to catch.
func QuickPresenceProbe(ctx context.Context, pane string, opts QuickPresenceOpts) (DeltaKind, error) {
	if pane == "" {
		return DeltaInputActivity, errors.New("tmuxio: pane required")
	}
	opts = opts.withDefaults()

	before, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return DeltaInputActivity, fmt.Errorf("tmuxio: quick probe capture before: %w: %s",
			err, strings.TrimSpace(string(before)))
	}

	// Paste two dashes via -l to keep the probe-char shape consistent
	// with WaitForQuietPane (the analyzeDelta strip-N logic expects
	// exactly N trailing probes).
	for i := 0; i < 2; i++ {
		if out, perr := tmuxRun(ctx, nil,
			"send-keys", "-t", pane, "-l", QuietProbe); perr != nil {
			// Cleanup any partial probes before returning.
			for j := 0; j < i; j++ {
				_, _ = tmuxRun(ctx, nil, "send-keys", "-t", pane, "BSpace")
			}
			return DeltaInputActivity, fmt.Errorf("tmuxio: quick probe send-keys #%d: %w: %s",
				i+1, perr, strings.TrimSpace(string(out)))
		}
	}

	// Short paint window so tmux has time to flush the paste into the
	// visible buffer before we capture. NOT long enough for the
	// operator to react — the whole point of the quick probe is to
	// avoid the multi-second observe windows of the full gate.
	select {
	case <-time.After(opts.PaintWait):
	case <-ctx.Done():
		for j := 0; j < 2; j++ {
			_, _ = tmuxRun(ctx, nil, "send-keys", "-t", pane, "BSpace")
		}
		return DeltaInputActivity, ctx.Err()
	}

	after, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		for j := 0; j < 2; j++ {
			_, _ = tmuxRun(ctx, nil, "send-keys", "-t", pane, "BSpace")
		}
		return DeltaInputActivity, fmt.Errorf("tmuxio: quick probe capture after: %w: %s",
			err, strings.TrimSpace(string(after)))
	}

	verdict := analyzeDelta(string(before), string(after), QuietProbe, 2)

	// Always clean up the two dashes we added. The quick probe leaves
	// no visible artifact, unlike WaitForQuietPane's accumulating
	// "I see you" stack — this is a silent pre-check.
	for j := 0; j < 2; j++ {
		_, _ = tmuxRun(ctx, nil, "send-keys", "-t", pane, "BSpace")
	}

	return verdict, nil
}

// InputRowHasContent classifies the receiving pane's CURRENT absolute
// state by inspecting the painted input row directly — no probe
// injection, no paint-wait, no pane disturbance. Returns DeltaQuiet
// only when the input area is found AND empty; DeltaInputActivity
// otherwise (content present, sentinel not found, or capture error).
//
// Substrate-class: read-only-observe (single capture-pane call).
// Distinct from WaitForQuietPane / QuickPresenceProbe which are
// write+observe (inject probes, observe paint, backspace). This makes
// InputRowHasContent the cheapest variant in the asymmetric-gate
// family: ~5ms, no pane mutation, safe to call before every delivery
// without any operational footprint on the receiver.
//
// Heuristic: scan capture-pane output for any row beginning with
// PromptSentinel ("❯ "). If found AND non-whitespace content follows
// the sentinel, return DeltaInputActivity — the input buffer is
// non-empty (operator's draft, an agent's chosen-text narration, a
// selection-menu echo, etc.) and a bus delivery's trailing Enter would
// be consumed as submit on that content. If found AND only whitespace
// follows, return DeltaQuiet — bare prompt, safe to deliver. If NO
// row begins with PromptSentinel, return DeltaInputActivity — Claude
// Code is in some non-prompt state (mid-stream output, menu overlay,
// search dialog, …) and the conservative default per the gate's
// contract is to fall back to the full gate.
//
// Known limitation — multi-line drafts with continuation rows: if
// Claude Code paints a multi-line draft as one PromptSentinel row
// plus continuation rows without the sentinel, the heuristic
// false-negatives on content past row 1 if row 1 is empty. The cli-
// semaphore#63 reproduction was a single-line draft; the strengthened
// region-based scan (scan all rows between the input area's bounding
// separators) is forward-watch to upgrade when an empirical multi-
// line paint sample is captured.
//
// Atomicity assumption: tmux capture-pane returns a single coherent
// pane snapshot (tmux serializes pane operations). No torn-read
// concern between rows of the captured output.
//
// Errors propagate up; the function returns DeltaInputActivity
// alongside any non-nil error so the safer-default-on-uncertainty is
// natural at the call site (treat any error as "fall back to full
// gate").
func InputRowHasContent(ctx context.Context, pane string) (DeltaKind, error) {
	if pane == "" {
		return DeltaInputActivity, errors.New("tmuxio: pane required")
	}
	out, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", pane)
	if err != nil {
		return DeltaInputActivity, fmt.Errorf("tmuxio: input-row capture-pane: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	sawSentinel := false
	for _, row := range strings.Split(string(out), "\n") {
		rest, found := strings.CutPrefix(row, PromptSentinel)
		if !found {
			continue
		}
		sawSentinel = true
		if strings.TrimSpace(rest) != "" {
			return DeltaInputActivity, nil
		}
	}
	if !sawSentinel {
		return DeltaInputActivity, nil
	}
	return DeltaQuiet, nil
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

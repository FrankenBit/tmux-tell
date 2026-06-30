package tmuxio

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tmuxRun is the indirection point for shelling out to `tmux` with stdin
// piped in. Tests overwrite it via SetRunner.
var tmuxRun = func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	return cmd.CombinedOutput()
}

// SetTmuxRunner swaps the tmux executor. Tests use this to provide a
// scripted fake. Returns the previous runner so callers can restore.
func SetTmuxRunner(r func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)) func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	prev := tmuxRun
	tmuxRun = r
	return prev
}

// DeliverParams is the input to Deliver.
type DeliverParams struct {
	// Pane is the target tmux pane id (e.g. "%3").
	Pane string
	// Body is the full rendered message text, pasted as a SINGLE buffer
	// (#446 demoted #336's header-first 3-part framing). A large Body that
	// collapses in the recipient TUI (codex `[Pasted Content]`) expands on
	// submit and is handled by the resubmit loop (#401) + cursor-anchor
	// verify below.
	Body string
	// VerifyToken is a short string the caller knows must appear in the
	// pane's visible content after Enter is pressed (typically the
	// message public_id). Empty disables verification.
	VerifyToken string
	// OnVerify, when set, is invoked exactly once after the post-Enter
	// verification loop with the wall-clock spent in that loop and whether
	// the token was observed within the retry budget. It lets the caller
	// record verify-attempt metrics (#146
	// tmux_tell_delivery_verify_attempt_seconds, shared with #153's budget
	// calibration) WITHOUT tmuxio importing a metrics package — tmuxio just
	// reports the timing; the caller decides what to do with it. Not called
	// when VerifyToken is empty (no verification is performed), nor on a
	// hard capture-pane/context error mid-loop (those are not a
	// verify-budget outcome — they abort the delivery).
	OnVerify func(elapsed time.Duration, verified bool)
	// PrePasteRaceCheckDisabled turns off the #616 tightest-window pre-paste
	// operator-draft re-check (the cursor-anchored input-row check immediately
	// before the paste that returns ErrInputRaced when the operator has typed
	// into the input in the probe→paste TOCTOU window). Default false: the
	// check is ON for every real delivery. Set true in unit tests that feed a
	// static post-paste capture to exercise the verify loop in isolation — that
	// idiom would otherwise read the pre-paste capture as operator content.
	// Mirrors serve.go's PrePasteSafetyDisabled (safety on by default).
	PrePasteRaceCheckDisabled bool
}

// SetRetrySchedule replaces the package-level verify-retry schedule and
// returns the previous value (for cleanup restoration). Two callers:
//   - Mailman startup: applies the per-agent verify-retry-budget config
//     knob (#153) by deriving a scaled schedule via DeriveRetrySchedule.
//   - Tests: want near-instant retries instead of the production budget.
func SetRetrySchedule(schedule []time.Duration) []time.Duration {
	prev := retryDelays
	retryDelays = schedule
	return prev
}

// SetRetryDelaysForTest is the legacy name for SetRetrySchedule, kept
// as a backward-compatible alias for existing tests.
//
// Deprecated: use SetRetrySchedule.
func SetRetryDelaysForTest(delays []time.Duration) []time.Duration {
	return SetRetrySchedule(delays)
}

// settleDelay is the pause Deliver inserts between paste-buffer and
// send-keys Enter. Without this, Enter can arrive while Claude Code's
// TUI is still ingesting the pasted characters — the Enter gets
// queued/eaten alongside the paste rather than processed as a
// distinct "submit" event. 500ms is generous enough for multi-KB
// bodies and below operator-perceptible delivery latency.
//
// Empirical: pre-#(this commit), every Surveyor/Pilot delivery with
// 800-2000 byte bodies left the text in the input box without
// submitting. The operator had to press Enter manually. Adding the
// delay lets the TUI settle before the submit keystroke lands.
//
// #360: 500ms is calibrated for Claude. Codex collapses a >~1KB paste into
// chunked `[Pasted Content N chars]` placeholders that need MORE ingest time
// before Enter submits them — at 500ms the codex submit-Enter is eaten and the
// chunks sit unsubmitted (delivered_in_input_box). The serve `-settle-delay`
// flag makes this tunable per-agent so a codex mailman can run a longer settle.
const DefaultSettleDelay = 500 * time.Millisecond

var settleDelay = DefaultSettleDelay

// SetSettleDelay overrides the paste→Enter settle pause for this process. Wired
// to the serve `-settle-delay` flag (#360) so an adapter/agent whose TUI needs
// longer to ingest a collapsed paste (codex) can be configured without a rebuild.
func SetSettleDelay(d time.Duration) { settleDelay = d }

// SetSettleDelayForTest swaps the settle delay. Tests using a fake
// tmux runner want near-zero values so they don't sleep 500ms per
// call. Returns the previous value for cleanup restoration.
func SetSettleDelayForTest(d time.Duration) time.Duration {
	prev := settleDelay
	settleDelay = d
	return prev
}

// DefaultRetryBudget is the total verify-token retry window at the
// default configuration. The full schedule sums to this duration; the
// per-agent verify-retry-budget config knob (#153) scales the schedule
// proportionally from this baseline.
const DefaultRetryBudget = 5 * time.Second

// defaultRetryDelays is the original (5s budget) backoff window — the
// baseline that DeriveRetrySchedule scales from. Frozen so the scaling
// math always references a stable reference shape regardless of any
// SetRetrySchedule override.
//
// Each delay is the wait BEFORE re-attempting capture (the first capture
// happens immediately after Enter). The shape is early-aggressive /
// later-patient so a fast response lands quickly while still giving
// Claude Code time to redraw and submit when it was mid-turn at paste
// time.
var defaultRetryDelays = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	1500 * time.Millisecond,
	1650 * time.Millisecond,
}

// retryDelays is the active post-paste verification backoff window.
// Starts equal to defaultRetryDelays; overwritten at mailman startup
// (per the verify-retry-budget config, via DeriveRetrySchedule +
// SetRetrySchedule) or in tests (via SetRetryDelaysForTest).
var retryDelays = append([]time.Duration(nil), defaultRetryDelays...)

// DeriveRetrySchedule scales the default retry schedule to fit the given
// budget. Preserves the early-aggressive / later-patient shape — the
// same relative attempt spacing, scaled proportionally so the schedule
// sum equals the budget. A budget <= 0 falls back to DefaultRetryBudget
// (defensive; the resolver should never produce a non-positive value).
func DeriveRetrySchedule(budget time.Duration) []time.Duration {
	if budget <= 0 {
		budget = DefaultRetryBudget
	}
	scale := float64(budget) / float64(DefaultRetryBudget)
	out := make([]time.Duration, len(defaultRetryDelays))
	for i, d := range defaultRetryDelays {
		out[i] = time.Duration(float64(d) * scale)
	}
	return out
}

// ErrUnverifiedDelivery is returned by Deliver when the paste + Enter
// sequence completed without tmux errors, but the verify token never
// became visible in the pane within the retry budget. The caller's
// policy is to treat this as a soft success: the text reached the
// pane, Enter was sent, but Claude Code didn't surface the message
// in time — usually because it was mid-turn and Enter was queued for
// later submission. Marking the message failed would be wrong; the
// operator will see the text and submit it manually.
var ErrUnverifiedDelivery = errors.New("tmuxio: delivery unverified")

// ErrInputRaced is returned by Deliver when, in the final cursor-anchored
// check immediately before pasting, the recipient's input row holds operator-
// typed content (the cursor sits past the prompt sentinel). The mailman's pre-
// paste AgentState probe already aborts when content is present AT PROBE TIME
// (cursor-past-sentinel → StateAwaitingOperator → paste-unsafe); this sentinel
// covers the residual TOCTOU window where a keystroke lands AFTER that probe
// passes but BEFORE this paste fires. Pasting then would prepend the message to
// the operator's draft (paste-buffer inserts at the cursor), and the post-Enter
// input-cleared verify cannot tell the corrupted submit from a clean one (both
// leave the input empty). Deliver does NOT paste in this case; the caller
// reverts the message to queued and retries on a later cycle, once the input is
// clear — the operator's draft is left untouched (#616).
var ErrInputRaced = errors.New("tmuxio: operator input raced the paste")

// Deliver pastes Body into the given tmux pane and presses Enter. It uses
// a unique named buffer per call so concurrent invocations from multiple
// mailmen can never race the default buffer.
//
// If VerifyToken is set, Deliver captures the pane after Enter and confirms
// the token landed. On miss it backs off and retries across retryDelays
// (~5s total). If the token is still not visible after the full budget,
// returns ErrUnverifiedDelivery (a soft-fail sentinel) — paste/Enter
// succeeded mechanically, but we couldn't confirm Claude Code surfaced the
// message in time. Caller policy distinguishes that from hard errors
// (tmux returning a real error from load-buffer/paste-buffer/send-keys).
func Deliver(ctx context.Context, p DeliverParams) error {
	if p.Pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if p.Body == "" {
		return errors.New("tmuxio: body required")
	}

	// 1+2. Paste the message into the pane as a SINGLE buffer (#446 demoted
	// #336's header-first 3-part framing — the separate Header/Footer paste
	// events added moving parts without proportional value and introduced the
	// #389 standalone-Header-submit window). A large Body that collapses in
	// the recipient TUI (codex `[Pasted Content]`) expands on submit; the
	// resubmit loop (#401) + cursor-anchor verify (steps 3-4) handle it.
	//
	// #533: on a collapse-capable TUI (codex) only, normalize a trailing
	// single-line paragraph (typically a `— Sender` sign-off) so codex does not
	// leave it as literal text outside the `[Pasted Content]` placeholder and
	// then submit it as a separate prompt. This is an OBSERVABLE codex-adapter
	// rendering accommodation, not a silent content mutation: the message content
	// is delivered intact (the sign-off arrives), the adapter just normalizes its
	// presentation (the trailing blank line) to fit codex's paste-collapse. Gated
	// on the collapse marker, so the Claude paste is byte-identical.
	body := p.Body
	if activeProfile.PasteCollapseMarker != "" {
		body = normalizeCollapsePaste(body)
	}
	// #616: tightest-window pre-paste operator-draft re-check. paste-buffer
	// inserts at the cursor, so any operator content in the input row now would
	// be prepended to the message; and the post-Enter verify keys on the input
	// CLEARING, which can't distinguish a prepended submit from a clean one.
	// The mailman's pre-paste probe already caught content present at probe
	// time; this closes the residual TOCTOU window between that probe and this
	// paste by re-checking as late as possible, cursor-anchored (ghost-text-
	// safe: codex paints dim placeholder text the cursor ignores — a plain-text
	// scan would false-positive on it, so we MUST anchor on the cursor, the same
	// #336 inputRowCleared primitive the verify uses). Gated on VerifyToken,
	// which every real delivery sets (control commands take the SendKeys path);
	// a token-less call skips the check, matching the verify-work gate below.
	// Degrades open when the cursor can't anchor the input row (query failed /
	// no sentinel) — paste anyway, the same best-effort posture as the verify's
	// cursor-less fallback. A stuck codex `[Pasted Content]` collapse marker
	// parks the cursor on an empty sub-line so it reads cleared here and is left
	// to the #401 resubmit machinery — out of scope for #616 (operator drafts).
	if p.VerifyToken != "" && !p.PrePasteRaceCheckDisabled {
		if pre, perr := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", p.Pane); perr == nil {
			cx, cy, cerr := agentCursor(ctx, p.Pane)
			if cleared, anchored := inputRowCleared(string(pre), cx, cy, cerr == nil); anchored && !cleared {
				return ErrInputRaced
			}
		}
	}
	if err := pasteChunk(ctx, p.Pane, body); err != nil {
		return err
	}
	// 2.5. Settle. Let Claude Code's TUI finish ingesting the pasted
	// characters before we ask it to submit. Without this, the Enter
	// in step 3 frequently arrives before the input is fully populated
	// and gets queued/eaten alongside the paste rather than processed
	// as a submission event.
	if settleDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(settleDelay):
		}
	}
	// 3. send-keys Enter
	if out, err := tmuxRun(ctx, nil,
		"send-keys", "-t", p.Pane, "Enter"); err != nil {
		return fmt.Errorf("tmuxio: send-keys Enter: %w: %s", err, strings.TrimSpace(string(out)))
	}

	if p.VerifyToken == "" {
		return nil
	}

	// 4. Verification + retry backoff. The load-bearing signal is the
	// INPUT-EMPTIED transition (#336): a paste that actually submitted
	// leaves the recipient's live input row empty — Claude clears it in
	// place, codex opens a fresh empty input block below. Emptiness is read
	// from the CURSOR position (cursor-anchor): the cursor sits at the
	// sentinel column when the input is empty and moves past it once content
	// is present. This is robust to paste-collapse, which masks the verify
	// token (codex renders a large paste as `[Pasted Content]` even after
	// submit, so token-match structurally cannot confirm it) but cannot move
	// the cursor off the sentinel; and to placeholder ghost-text (codex
	// paints a dim example prompt into an empty composer that a plain-text
	// scan misreads as populated — the cursor stays put). It is also honest
	// about the dominant mid-turn failure: a queued Enter leaves the paste
	// buffered in the input row with the cursor PAST the sentinel, so we
	// correctly report not-submitted (where token-match both false-negatives
	// on collapse and false-positives on a pasted-but-unsubmitted short
	// message whose token is literally visible in the input box).
	//
	// When the input row can't be anchored (cursor query failed, no sentinel
	// configured, or the cursor isn't on a sentinel row), Deliver GRACEFULLY
	// DEGRADES to the legacy token-match signal — same shape as AgentState's
	// cursor-less fallback. Pre-#336 behavior is preserved for captures the
	// cursor can't anchor.
	//
	// Note: a successful paste-buffer (step 2) implies the input row was
	// populated, so an empty input row after Enter means submission, not a
	// never-filled row. A pre-Enter baseline snapshot would tighten the
	// rare paste-silently-no-op'd edge into an explicit non-empty→empty
	// transition guard; deferred as hardening (#336 follow-up).
	//
	// verifyStart bounds the whole retry loop; OnVerify (when set) reports
	// its wall-clock on either terminal outcome (submitted / budget
	// exhausted) so the caller can histogram verify-attempt latency
	// (#146/#153).
	verifyStart := time.Now()
	var lastCapture string
	for attempt := 0; attempt <= len(retryDelays); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelays[attempt-1]):
			}
		}
		out, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-t", p.Pane)
		if err != nil {
			return fmt.Errorf("tmuxio: capture-pane: %w", err)
		}
		lastCapture = string(out)
		// Cursor position anchors the input-emptied signal (#336 cursor-
		// anchor): cursorOK=false (query failed) degrades to token-match
		// inside deliverySubmitted via inputRowCleared's anchored=false path.
		cursorX, cursorY, cursorErr := agentCursor(ctx, p.Pane)
		if deliverySubmitted(lastCapture, cursorX, cursorY, cursorErr == nil, p.VerifyToken) {
			// #622 cheap secondary: don't accept a cleared input that is
			// concurrent with a visible /compact frame. deliverySubmitted keys on
			// the input row clearing, but the compaction UI KEEPS the prompt
			// sentinel (the input row can read "empty" mid-compaction), so a
			// cleared signal while the compaction marker is visible is the
			// compaction redraw — not our submit. Skip acceptance; the marker
			// clears when compaction finishes, and a later poll accepts (or the
			// budget exhausts → unverified → re-queue, correct for a /compact that
			// outlasts the verify window). Codex-noop (empty CompactionMarker).
			// The stability-gate prevents pasting INTO compaction; this guards the
			// TOCTOU residual where a /compact becomes visible mid-verify.
			if m := activeProfile.CompactionMarker; m == "" || !capturedLiveCompaction(lastCapture, m) {
				if p.OnVerify != nil {
					p.OnVerify(time.Since(verifyStart), true)
				}
				return nil
			}
		}
		// Resubmit (#401): when a collapsed paste is still sitting in the input,
		// codex's first Enter was absorbed while it was still ingesting the
		// bracketed paste — re-send Enter. When codex has gone idle it submits;
		// Enter-on-empty is a safe no-op (operator + Lookout confirmed), so a
		// resubmit that races an already-submitted paste is harmless. The next
		// loop iteration waits a retryDelays backoff before re-checking, giving
		// codex time to process this Enter. No-op for adapters without a
		// collapse marker (Claude) — pasteStillInInput is false, so they submit
		// on the first Enter exactly as before.
		if pasteStillInInput(lastCapture) {
			if out, err := tmuxRun(ctx, nil, "send-keys", "-t", p.Pane, "Enter"); err != nil {
				return fmt.Errorf("tmuxio: send-keys Enter (resubmit): %w: %s",
					err, strings.TrimSpace(string(out)))
			}
		}
	}
	if p.OnVerify != nil {
		p.OnVerify(time.Since(verifyStart), false)
	}
	return fmt.Errorf("%w: input not cleared and token %q not surfaced after %d attempts; last capture (trunc):\n%s",
		ErrUnverifiedDelivery, p.VerifyToken, len(retryDelays)+1, trim(lastCapture, 400))
}

// deliverySubmitted reports whether a post-Enter pane capture confirms the
// paste submitted. Primary signal: the live input row emptied (input-
// emptied, #336), anchored on the cursor position (cursorX/cursorY, cursorOK
// reports whether the cursor query succeeded) — authoritative when the input
// row anchors, and deliberately NOT corroborated by the token there (an
// unsubmitted paste whose token is visible in a still-populated input row
// must read as not-submitted). Cursor-anchoring is what makes the signal
// robust to placeholder ghost-text (codex paints a dim example prompt into
// an empty composer that a plain-text scan misreads as populated). Fallback
// when the input row can't be anchored: the legacy token-match (the verify
// token became visible anywhere in the pane).
func deliverySubmitted(capture string, cursorX, cursorY int, cursorOK bool, verifyToken string) bool {
	// Collapse-marker override (#401): if the adapter collapses pastes to a
	// marker (codex `[Pasted Content`) and that marker is still in the INPUT
	// area, the paste is definitively NOT submitted. This OVERRIDES the
	// cursor-anchor, which false-positives on a stuck collapsed paste: codex
	// parks the cursor on an empty sub-line of the multi-line input while the
	// `[Pasted Content]` sits a line above, so inputRowCleared reads "empty".
	// The marker is the authoritative not-submitted signal for that case.
	if pasteStillInInput(capture) {
		return false
	}
	if cleared, anchored := inputRowCleared(capture, cursorX, cursorY, cursorOK); anchored {
		return cleared
	}
	return strings.Contains(capture, verifyToken)
}

// SendKeys types text directly into the recipient pane and presses Enter,
// bypassing the paste-buffer machinery used by Deliver. It is intended for
// short control strings (e.g. "/compact") that must hit Claude Code's
// slash-command parser exactly as typed, without the rendered chat header
// Deliver wraps around regular messages.
//
// No verification: control commands don't echo a predictable token.
func SendKeys(ctx context.Context, pane, text string) error {
	if pane == "" {
		return errors.New("tmuxio: pane required")
	}
	if text == "" {
		return errors.New("tmuxio: text required")
	}
	if out, err := tmuxRun(ctx, nil,
		"send-keys", "-t", pane, "-l", text); err != nil {
		return fmt.Errorf("tmuxio: send-keys literal: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := tmuxRun(ctx, nil,
		"send-keys", "-t", pane, "Enter"); err != nil {
		return fmt.Errorf("tmuxio: send-keys Enter: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pasteChunk loads content into a unique named buffer and pastes it into the
// pane, deleting the buffer on success via paste-buffer -d (and explicitly
// on paste failure, where -d never ran). A unique buffer per chunk lets
// concurrent mailmen never race the default buffer, and lets the #336 framed
// delivery paste Header / Body / Footer as separate accumulating pastes into
// the same input before a single submit. load-buffer failure leaves no
// buffer to clean up.
func pasteChunk(ctx context.Context, pane, content string) error {
	bufName := uniqueBufferName()
	if out, err := tmuxRun(ctx, strings.NewReader(content),
		"load-buffer", "-b", bufName, "-"); err != nil {
		return fmt.Errorf("tmuxio: load-buffer: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := tmuxRun(ctx, nil,
		"paste-buffer", "-b", bufName, "-t", pane, "-d"); err != nil {
		_, _ = tmuxRun(ctx, nil, "delete-buffer", "-b", bufName)
		return fmt.Errorf("tmuxio: paste-buffer: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// normalizeCollapsePaste reshapes content so a collapse-capable TUI (codex)
// does not leave a short trailing paragraph as literal composer text outside its
// `[Pasted Content]` placeholder (#533). Mechanism: codex collapses a
// over-threshold bracketed paste into a placeholder but segments at `\n\n`
// paragraph boundaries; a single-line final paragraph after the last `\n\n`
// (typically a `— Sender` sign-off) stays literal, survives the submit Enter,
// and is later submitted as a separate prompt — because it is literal text, not
// the collapse marker, the resubmit loop (pasteStillInInput) never re-fires.
//
// Two reshapes, both removing the trailing-paragraph isolation so the final line
// collapses with the bulk:
//  1. strip trailing newlines — our own appended `\n` amplifies by giving codex
//     a clean prompt boundary after the tail; and
//  2. collapse the LAST `\n\n` to a single `\n` WHEN its tail is a single line,
//     attaching that line to the preceding bulk (a single `\n` is a line break,
//     not a paragraph boundary, so codex no longer segments it out).
//
// Only the trailing single-line paragraph is touched — interior paragraph breaks
// are preserved. No-op for content that does not end in a single-line paragraph.
// Deliver applies this ONLY when a collapse marker is configured (codex), so the
// Claude paste path is unaffected.
//
// Empirical grounding (#533):
//   - Escalation MEASURED-GREEN: a `\n` (single-newline) tail submits atomically
//     on codex (Pilot factor-isolation), confirming Lookout's read that the
//     `\n\n` paragraph boundary — not the newline itself — triggers the split.
//     So collapsing `\n\n`→`\n` is sufficient; no escalation to a no-newline join.
//   - No length bound, by dominance: collapsing ANY trailing single-line
//     paragraph is acceptable whether or not long tails fragment. If they do,
//     this covers them; if they don't, it is a LOW-HARM (not zero-harm) cosmetic
//     over-reach on a rare shape — a long single-line final paragraph visibly
//     merges with the prior paragraph (more noticeable than a short sign-off
//     losing one blank line), codex-only. A length bound is the escape hatch if
//     that shape ever surfaces; unbounded is fine for v1.
//   - Multi-line trailing paragraphs are intentionally left alone — the observed
//     bug is a single-line sign-off; multi-line-tail fragmentation is unconfirmed
//     (file a follow-up if it ever surfaces).
//   - Intermittent-timing efficacy is a POST-DEPLOY confirmation: fragmentation
//     fires only in codex's intermediate-settle window (~production 500ms), which
//     a point-in-time test environment couldn't cleanly reproduce. The fix is
//     mechanism-determined + consistent with the production control; an f21d-shape
//     replay under real traffic is the standing confirmation.
func normalizeCollapsePaste(content string) string {
	content = strings.TrimRight(content, "\n")
	if i := strings.LastIndex(content, "\n\n"); i >= 0 {
		tail := content[i+len("\n\n"):]
		if tail != "" && !strings.Contains(tail, "\n") {
			content = content[:i] + "\n" + tail
		}
	}
	return content
}

func uniqueBufferName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("inject-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
}

// CheckTokenVisible reports whether token currently appears anywhere in the
// given pane's recent scrollback (up to 500 lines). A single capture-pane
// call — no retry. Returns false on any tmux error (can't substantiate
// visibility → treat as not visible, letting the caller deliver the replay).
//
// Used by the dedupe path (#157 PR2) to re-verify a prior
// delivered_in_input_box message without touching the recipient's pane.
func CheckTokenVisible(ctx context.Context, pane, token string) (bool, error) {
	out, err := tmuxRun(ctx, nil, "capture-pane", "-p", "-S", "-500", "-t", pane)
	if err != nil {
		return false, fmt.Errorf("tmuxio: capture-pane: %w", err)
	}
	return strings.Contains(string(out), token), nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Compile-time ensure bytes is used (the import is genuinely used in
// production via the runner's output type).
var _ = bytes.MinRead

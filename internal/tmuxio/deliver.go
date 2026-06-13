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
	// Header and Footer are the optional short frame parts of the #336
	// header-first 3-part framed paste. When non-empty they are pasted as
	// their OWN buffers — Header before Body, Footer after — so the short
	// frame stays literal even when a large Body collapses in the recipient
	// TUI, keeping the message bounds operator-visible. A single submit
	// (Enter) follows the last part. Both empty ⇒ a plain single-paste of
	// Body (unchanged pre-#336 behavior). render.MessageParts populates
	// these; only large messages are framed.
	Header string
	// Body is the rendered text to paste. When Header/Footer are empty this
	// is the full rendered message (header + body); when framed, just the
	// message body.
	Body string
	// Footer — see Header. Pasted after Body.
	Footer string
	// VerifyToken is a short string the caller knows must appear in the
	// pane's visible content after Enter is pressed (typically the
	// message public_id). Empty disables verification.
	VerifyToken string
	// OnVerify, when set, is invoked exactly once after the post-Enter
	// verification loop with the wall-clock spent in that loop and whether
	// the token was observed within the retry budget. It lets the caller
	// record verify-attempt metrics (#146
	// tmux_msg_delivery_verify_attempt_seconds, shared with #153's budget
	// calibration) WITHOUT tmuxio importing a metrics package — tmuxio just
	// reports the timing; the caller decides what to do with it. Not called
	// when VerifyToken is empty (no verification is performed), nor on a
	// hard capture-pane/context error mid-loop (those are not a
	// verify-budget outcome — they abort the delivery).
	OnVerify func(elapsed time.Duration, verified bool)
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

	// 1+2. Paste the message into the pane. When framed (#336 header-first
	// 3-part), Header and Footer are pasted as their OWN buffers around Body
	// — separate short paste-events that stay literal even if a large Body
	// collapses in the recipient TUI — and they accumulate in the input
	// before the single submit (step 3). Unframed (Header/Footer empty),
	// this is a plain single paste of Body, byte-for-byte the pre-#336 path.
	if p.Header != "" {
		if err := pasteChunk(ctx, p.Pane, p.Header); err != nil {
			return err
		}
	}
	if err := pasteChunk(ctx, p.Pane, p.Body); err != nil {
		return err
	}
	if p.Footer != "" {
		if err := pasteChunk(ctx, p.Pane, p.Footer); err != nil {
			return err
		}
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
			if p.OnVerify != nil {
				p.OnVerify(time.Since(verifyStart), true)
			}
			return nil
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

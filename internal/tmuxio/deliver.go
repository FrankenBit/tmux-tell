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
	// Body is the rendered text to paste, including header and rule.
	Body string
	// VerifyToken is a short string the caller knows must appear in the
	// pane's visible content after Enter is pressed (typically the
	// message public_id). Empty disables verification.
	VerifyToken string
}

// SetRetryDelaysForTest swaps the package-level retryDelays and returns
// the previous value so test cleanups can restore it. Tests that drive
// the verify-retry path want near-instant retries instead of the
// production ~5s budget.
func SetRetryDelaysForTest(delays []time.Duration) []time.Duration {
	prev := retryDelays
	retryDelays = delays
	return prev
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
var settleDelay = 500 * time.Millisecond

// SetSettleDelayForTest swaps the settle delay. Tests using a fake
// tmux runner want near-zero values so they don't sleep 500ms per
// call. Returns the previous value for cleanup restoration.
func SetSettleDelayForTest(d time.Duration) time.Duration {
	prev := settleDelay
	settleDelay = d
	return prev
}

// retryDelays are the post-paste verification backoff window. Total
// budget ~5s (100ms + 250ms + 500ms + 1s + 1.5s + 1.65s = 5s) so we
// give Claude Code time to redraw and submit when it was mid-turn at
// paste time. Each delay is the wait BEFORE re-attempting capture, so
// the first capture happens immediately after Enter.
var retryDelays = []time.Duration{
	100 * time.Millisecond,
	250 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	1500 * time.Millisecond,
	1650 * time.Millisecond,
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

	bufName := uniqueBufferName()

	// 1. load-buffer from stdin
	if out, err := tmuxRun(ctx, strings.NewReader(p.Body),
		"load-buffer", "-b", bufName, "-"); err != nil {
		return fmt.Errorf("tmuxio: load-buffer: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// Ensure we delete the named buffer if anything below fails. -d on
	// paste-buffer covers the happy path; this handles failure cleanup.
	defer func() { _, _ = tmuxRun(ctx, nil, "delete-buffer", "-b", bufName) }()

	// 2. paste-buffer (with -d so the buffer is deleted on success)
	if out, err := tmuxRun(ctx, nil,
		"paste-buffer", "-b", bufName, "-t", p.Pane, "-d"); err != nil {
		return fmt.Errorf("tmuxio: paste-buffer: %w: %s", err, strings.TrimSpace(string(out)))
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

	// 4. Verification + retry backoff.
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
		if strings.Contains(lastCapture, p.VerifyToken) {
			return nil
		}
	}
	return fmt.Errorf("%w: token %q not visible after %d attempts; last capture (trunc):\n%s",
		ErrUnverifiedDelivery, p.VerifyToken, len(retryDelays)+1, trim(lastCapture, 400))
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

func uniqueBufferName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("inject-%d-%s", os.Getpid(), hex.EncodeToString(b[:]))
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

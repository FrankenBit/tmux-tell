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

// retryDelays are the post-paste backoff window. Two attempts after the
// initial Enter — 50 ms and 200 ms — match the issue.
var retryDelays = []time.Duration{50 * time.Millisecond, 200 * time.Millisecond}

// Deliver pastes Body into the given tmux pane and presses Enter. It uses
// a unique named buffer per call so concurrent invocations from multiple
// mailmen can never race the default buffer.
//
// If VerifyToken is set, Deliver captures the pane after Enter and confirms
// the token landed. On miss it backs off and retries up to len(retryDelays)
// times before returning an error containing the last captured pane state.
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
	return fmt.Errorf("tmuxio: verify token %q not visible after %d attempts; last capture (trunc):\n%s",
		p.VerifyToken, len(retryDelays)+1, trim(lastCapture, 400))
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

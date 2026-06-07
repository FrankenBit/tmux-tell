package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// sendParams is the resolved input to runSendWithStore, post-flag-parsing.
type sendParams struct {
	From            string
	To              string
	ReplyTo         string
	Body            string
	NoReplyExpected bool
	MaxRecipient    int
	MaxSender       int
	MaxBody         int

	// Recipient-status options (#152).
	Strict           bool          // reject (ok:false) if recipient unreachable
	WaitForDelivered bool          // block until terminal delivery state or timeout
	Timeout          time.Duration // bound for WaitForDelivered (default applied if 0)
	Format           string        // "json" (default) | "text"

	// Thread-freshness option (#155). Only meaningful with ReplyTo set.
	BlockOnStale bool // reject (ok:false) if the thread moved since sender last spoke
}

// runSendCLI parses send-subcommand flags, opens the store, and dispatches
// to runSendWithStore. It returns the process exit code.
func runSendCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("send", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender agent name (env: CLAUDE_AGENT_NAME)")
	to := fs.String("to", "", "recipient agent name (required)")
	replyTo := fs.String("reply-to", "", "public_id of the message being replied to")
	noReplyExpected := fs.Bool("no-reply-expected", false, "signal recipient that no acknowledgment is needed (#145)")
	body := fs.String("body", "", "message body (else read from positional args)")
	maxRecipient := fs.Int("max-recipient-queue", capRecipientQueue,
		"reject when the recipient's queue depth would exceed this")
	maxSender := fs.Int("max-sender-backlog", capSenderBacklog,
		"reject when the sender's queued backlog would exceed this")
	maxBody := fs.Int("max-body-bytes", capBodyBytes,
		"reject bodies larger than this many bytes")
	strict := fs.Bool("strict", false,
		"fail (ok:false) if the recipient is not registered or not reachable (#152)")
	waitForDelivered := fs.Bool("wait-for-delivered", false,
		"block until the message reaches a terminal delivery state (or --timeout)")
	timeout := fs.Duration("timeout", defaultDeliveredWaitTimeout,
		"bound for --wait-for-delivered")
	blockOnStale := fs.Bool("block-on-stale", false,
		"with --reply-to: fail (ok:false) if the thread moved since you last spoke (#155)")
	format := fs.String("format", "json", "json|text")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	fromName, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	p := sendParams{
		From:             fromName,
		To:               *to,
		ReplyTo:          *replyTo,
		Body:             *body,
		NoReplyExpected:  *noReplyExpected,
		MaxRecipient:     *maxRecipient,
		MaxSender:        *maxSender,
		MaxBody:          *maxBody,
		Strict:           *strict,
		WaitForDelivered: *waitForDelivered,
		Timeout:          *timeout,
		Format:           *format,
		BlockOnStale:     *blockOnStale,
	}
	if p.Body == "" {
		p.Body = strings.Join(fs.Args(), " ")
	}

	return runSendWithStore(ctx, s, p, stdout, stderr)
}

// runSendWithStore is the pure-logic core: validates the parameters,
// enforces the caps against `s`, inserts on success, and writes the JSON
// response to stdout. Designed to be table-tested.
func runSendWithStore(ctx context.Context, s *store.Store, p sendParams, stdout, stderr io.Writer) int {
	// Identity
	if p.From == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve sender: pass --from, set $CLAUDE_AGENT_NAME, or register this pane",
			exitUsage)
	}
	if p.To == "" {
		return writeJSONError(stdout, stderr,
			"--to required", exitUsage)
	}

	// Body shape
	if p.Body == "" {
		return writeJSONError(stdout, stderr, "body required", exitDataErr)
	}
	if p.MaxBody > 0 && len(p.Body) > p.MaxBody {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("body too large (%d > %d bytes)", len(p.Body), p.MaxBody),
			exitDataErr)
	}

	// Recipient status (#152). This also serves as the registry-existence
	// check: an unknown recipient is fail-loud, unchanged since #3/#4/#15 —
	// preserving the day-one safety default. `--strict` additionally rejects
	// a registered-but-unreachable recipient (pane gone); the dead-pane
	// dimension. Without --strict, a registered-but-dead recipient still
	// queues — the message waits for the pane to come back — and the
	// recipient block reports the disposition so the sender knows.
	rs, err := resolveRecipientStatus(ctx, s, p.To)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if !rs.Registered {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown recipient: %s", p.To), exitUnavailable)
	}
	if p.Strict && !rs.Alive {
		renderSendResult(stdout, SendResponse{
			OK:        false,
			Recipient: rs,
			Error:     fmt.Sprintf("recipient %q registered but not reachable (pane %s)", p.To, rs.PaneStatus),
		}, p.To, p.Format)
		return exitUnavailable
	}

	// Sender must exist too. We trust the operator to keep the agents
	// table aligned with the actual panes — `discover` (#12) does that.
	if _, err := s.GetAgent(ctx, p.From); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown sender: %s", p.From), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	// Thread-freshness (#155). When the send threads under an earlier
	// message, check whether the thread moved since the sender last spoke.
	// This runs BEFORE the insert so --block-on-stale can refuse without
	// queueing a row. A reply_to that doesn't resolve maps to the same
	// "unknown reply-to id" error the insert path would raise.
	var freshness *ThreadFreshness
	if p.ReplyTo != "" {
		tf, err := resolveThreadFreshness(ctx, s, p.ReplyTo, p.From)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return writeJSONError(stdout, stderr,
					fmt.Sprintf("unknown reply-to id: %s", p.ReplyTo), exitDataErr)
			}
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		freshness = tf
		if p.BlockOnStale && tf.Stale {
			renderSendResult(stdout, SendResponse{
				OK:        false,
				Recipient: rs,
				Freshness: tf,
				Error:     fmt.Sprintf("thread has %d newer message(s) addressed to you since you last spoke", len(tf.NewerInThread)),
			}, p.To, p.Format)
			return exitUnavailable
		}
	}

	// Caps. After #29, cap enforcement lives inside InsertMessage's
	// transaction so we don't need to pre-check here — the store
	// returns ErrRecipientQueueFull / ErrSenderBacklogFull with a
	// precise depth/cap snapshot, which we surface to the caller.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         p.From,
		ToAgent:           p.To,
		ReplyTo:           p.ReplyTo,
		Body:              p.Body,
		NoReplyExpected:   p.NoReplyExpected,
		MaxRecipientQueue: p.MaxRecipient,
		MaxSenderBacklog:  p.MaxSender,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRecipientQueueFull),
			errors.Is(err, store.ErrSenderBacklogFull):
			return writeJSONError(stdout, stderr, err.Error(), exitTempFail)
		case errors.Is(err, store.ErrNotFound):
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown reply-to id: %s", p.ReplyTo), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	resp := SendResponse{
		OK:        true,
		ID:        res.PublicID,
		Queued:    res.Queued,
		Recipient: rs,
		Freshness: freshness,
	}
	// Opt-in synchronous delivery confirmation (#152). Bounded by --timeout
	// (defaulted at flag-parse); the row is already inserted, so a timeout
	// is informational — the message stays queued.
	if p.WaitForDelivered {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultDeliveredWaitTimeout
		}
		resp.Delivery = waitForDelivery(ctx, s, res.PublicID, timeout, pingPollInterval)
	}
	renderSendResult(stdout, resp, p.To, p.Format)
	return exitOK
}

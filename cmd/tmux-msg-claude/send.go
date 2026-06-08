package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/config"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// sendParams is the resolved input to runSendWithStore, post-flag-parsing.
type sendParams struct {
	From            string
	To              string
	ToRecipients    []string // non-empty when --to carries commas; len > 1 → multi-send path (#158)
	ReplyTo         string
	Body            string
	NoReplyExpected bool
	Quick           bool // compact single-line chrome on delivery (#154)
	MaxRecipient    int
	MaxSender       int
	MaxBody         int

	// Multi-recipient spam guard (#158). 0 = no cap.
	MaxRecipientsPerSend int

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
	from := fs.String("from", "", "sender agent name (env: TMUX_AGENT_NAME)")
	to := fs.String("to", "", "recipient agent name (required)")
	replyTo := fs.String("reply-to", "", "public_id of the message being replied to")
	noReplyExpected := fs.Bool("no-reply-expected", false, "signal recipient that no acknowledgment is needed (#145)")
	quick := fs.Bool("quick", false, "render compact single-line chrome in the recipient's pane (#154)")
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

	cfg, _ := config.Load()
	maxRPS := config.ResolveInt(cfg, fromName, "max-recipients-per-send", capMaxRecipientsPerSend)

	toList := splitRecipients(*to)
	p := sendParams{
		From:                 fromName,
		ReplyTo:              *replyTo,
		Body:                 *body,
		NoReplyExpected:      *noReplyExpected,
		Quick:                *quick,
		MaxRecipient:         *maxRecipient,
		MaxSender:            *maxSender,
		MaxBody:              *maxBody,
		MaxRecipientsPerSend: maxRPS,
		Strict:               *strict,
		WaitForDelivered:     *waitForDelivered,
		Timeout:              *timeout,
		Format:               *format,
		BlockOnStale:         *blockOnStale,
	}
	if len(toList) > 1 {
		p.ToRecipients = toList
	} else {
		p.To = *to
	}
	if p.Body == "" {
		p.Body = strings.Join(fs.Args(), " ")
	}

	return runSendWithStore(ctx, s, p, stdout, stderr)
}

// runSendWithStore is the pure-logic core: validates the parameters,
// enforces the caps against `s`, inserts on success, and writes the JSON
// response to stdout. Designed to be table-tested.
//
// When p.ToRecipients carries more than one name, runSendWithStore dispatches
// to runMultiSendWithStore (#158) and returns its result verbatim.
func runSendWithStore(ctx context.Context, s *store.Store, p sendParams, stdout, stderr io.Writer) int {
	// #228: resolve special recipient "operator" before any cap-check or
	// registry lookup. Substitutes either p.To or p.ToRecipients entries.
	// A failed resolution fails the send fail-loud (#152 semantic) rather
	// than silently dropping.
	if err := resolveOperatorInSendParams(ctx, s, &p); err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
	}
	if len(p.ToRecipients) > 1 {
		return runMultiSendWithStore(ctx, s, p, stdout, stderr)
	}
	if len(p.ToRecipients) == 1 {
		p.To = p.ToRecipients[0]
	}

	// Identity
	if p.From == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve sender: pass --from, set $TMUX_AGENT_NAME, or register this pane",
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
		Quick:             p.Quick,
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

// runMultiSendWithStore handles a send to more than one recipient (#158).
// Cross-call validation (from, body, size, sender, thread-freshness) runs
// once and fails-loud. Per-recipient outcomes are collected independently:
// an unknown or cap-full recipient produces a failed row but does not abort
// delivery to the remaining recipients. The outer response is
// {ok: true/false, messages: [...]}, where ok reflects whether ALL rows
// succeeded.
func runMultiSendWithStore(ctx context.Context, s *store.Store, p sendParams, stdout, stderr io.Writer) int {
	if p.From == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve sender: pass --from, set $TMUX_AGENT_NAME, or register this pane",
			exitUsage)
	}
	if p.Body == "" {
		return writeJSONError(stdout, stderr, "body required", exitDataErr)
	}
	if p.MaxBody > 0 && len(p.Body) > p.MaxBody {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("body too large (%d > %d bytes)", len(p.Body), p.MaxBody),
			exitDataErr)
	}
	if p.MaxRecipientsPerSend > 0 && len(p.ToRecipients) > p.MaxRecipientsPerSend {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("too many recipients: %d (max %d per send)", len(p.ToRecipients), p.MaxRecipientsPerSend),
			exitDataErr)
	}
	if _, err := s.GetAgent(ctx, p.From); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown sender: %s", p.From), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	// Thread-freshness is computed once; all recipients share the same reply_to.
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
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("thread has %d newer message(s) addressed to you since you last spoke",
					len(tf.NewerInThread)),
				exitUnavailable)
		}
	}

	results := make([]MultiSendResult, 0, len(p.ToRecipients))
	anyFailed := false
	for _, to := range p.ToRecipients {
		sp := p
		sp.To = to
		resp, err := sendOneRecipient(ctx, s, sp)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		mr := MultiSendResult{
			To:        to,
			OK:        resp.OK,
			ID:        resp.ID,
			Queued:    resp.Queued,
			Recipient: resp.Recipient,
			Delivery:  resp.Delivery,
			Freshness: freshness,
			Error:     resp.Error,
		}
		if !resp.OK {
			anyFailed = true
		}
		results = append(results, mr)
	}

	out := MultiSendResponse{OK: !anyFailed, Messages: results}
	_ = writeJSONResult(stdout, out)
	if anyFailed {
		return exitTempFail
	}
	return exitOK
}

// sendOneRecipient performs a single-recipient insert for use by the
// multi-send loop. Unknown recipient, cap-full, and strict rejection are
// returned as SendResponse{OK: false} (soft failure) so the caller can
// continue to the next recipient. Store errors are hard (non-nil error).
func sendOneRecipient(ctx context.Context, s *store.Store, p sendParams) (SendResponse, error) {
	rs, err := resolveRecipientStatus(ctx, s, p.To)
	if err != nil {
		return SendResponse{}, err
	}
	if !rs.Registered {
		return SendResponse{
			OK:        false,
			Recipient: rs,
			Error:     fmt.Sprintf("unknown recipient: %s", p.To),
		}, nil
	}
	if p.Strict && !rs.Alive {
		return SendResponse{
			OK:        false,
			Recipient: rs,
			Error:     fmt.Sprintf("recipient %q registered but not reachable (pane %s)", p.To, rs.PaneStatus),
		}, nil
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         p.From,
		ToAgent:           p.To,
		ReplyTo:           p.ReplyTo,
		Body:              p.Body,
		NoReplyExpected:   p.NoReplyExpected,
		Quick:             p.Quick,
		MaxRecipientQueue: p.MaxRecipient,
		MaxSenderBacklog:  p.MaxSender,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRecipientQueueFull),
			errors.Is(err, store.ErrSenderBacklogFull):
			return SendResponse{OK: false, Recipient: rs, Error: err.Error()}, nil
		case errors.Is(err, store.ErrNotFound):
			// reply_to was validated before the loop; shouldn't happen, but
			// surface as a hard error rather than silently attributing it to
			// this recipient.
			return SendResponse{}, fmt.Errorf("unknown reply-to id: %s", p.ReplyTo)
		}
		return SendResponse{}, err
	}
	resp := SendResponse{
		OK:        true,
		ID:        res.PublicID,
		Queued:    res.Queued,
		Recipient: rs,
	}
	if p.WaitForDelivered {
		timeout := p.Timeout
		if timeout <= 0 {
			timeout = defaultDeliveredWaitTimeout
		}
		resp.Delivery = waitForDelivery(ctx, s, res.PublicID, timeout, pingPollInterval)
	}
	return resp, nil
}

// splitRecipients splits a comma-separated recipient string, trimming spaces
// and dropping empty entries. A single entry (no comma) returns a one-element
// slice; an empty string returns nil.
func splitRecipients(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

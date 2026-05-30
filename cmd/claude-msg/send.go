package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/identity"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// sendParams is the resolved input to runSendWithStore, post-flag-parsing.
type sendParams struct {
	From         string
	To           string
	ReplyTo      string
	Body         string
	MaxRecipient int
	MaxSender    int
	MaxBody      int
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
	body := fs.String("body", "", "message body (else read from positional args)")
	maxRecipient := fs.Int("max-recipient-queue", capRecipientQueue,
		"reject when the recipient's queue depth would exceed this")
	maxSender := fs.Int("max-sender-backlog", capSenderBacklog,
		"reject when the sender's queued backlog would exceed this")
	maxBody := fs.Int("max-body-bytes", capBodyBytes,
		"reject bodies larger than this many bytes")
	if err := fs.Parse(args); err != nil {
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
		From:         fromName,
		To:           *to,
		ReplyTo:      *replyTo,
		Body:         *body,
		MaxRecipient: *maxRecipient,
		MaxSender:    *maxSender,
		MaxBody:      *maxBody,
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

	// Recipient must exist in the registry.
	if _, err := s.GetAgent(ctx, p.To); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown recipient: %s", p.To), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
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

	// Caps. Check both before either is exceeded so the error message
	// names the right one.
	if p.MaxRecipient > 0 {
		depth, err := s.RecipientQueueDepth(ctx, p.To)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		if depth >= p.MaxRecipient {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("queue full for %s (%d/%d)", p.To, depth, p.MaxRecipient),
				exitTempFail)
		}
	}
	if p.MaxSender > 0 {
		backlog, err := s.SenderBacklog(ctx, p.From)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		if backlog >= p.MaxSender {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("sender backlog full for %s (%d/%d)", p.From, backlog, p.MaxSender),
				exitTempFail)
		}
	}

	// Insert.
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: p.From,
		ToAgent:   p.To,
		ReplyTo:   p.ReplyTo,
		Body:      p.Body,
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown reply-to id: %s", p.ReplyTo), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	_ = writeJSONResult(stdout, map[string]any{
		"ok":     true,
		"id":     res.PublicID,
		"queued": res.Queued,
	})
	return exitOK
}

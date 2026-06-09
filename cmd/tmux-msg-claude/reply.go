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

// Request-reply surface (#250): ask / wait_for_reply / check_replies. `ask` is
// a send that marks intent to wait (expects_reply); wait_for_reply blocks on
// the substrate-side notify seam (store.WaitForReply) until a reply or timeout;
// check_replies is the non-blocking poll. Shared by the CLI subcommands and the
// MCP tools so the semantics can't drift.

// replyView is the agent-facing shape of a reply row.
type replyView struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Body string `json:"body"`
	// State is the display-state ("delivered" / "delivered_in_input_box" /
	// "queued" / …); Unverified is the #250-Q4 flag — true when the reply
	// landed but its delivery to the asker wasn't verify-confirmed (#169). The
	// reply is returned regardless; the flag lets the asker decide how much to
	// trust it.
	State      string `json:"state"`
	Unverified bool   `json:"unverified"`
	CreatedAt  string `json:"created_at"`
}

func viewReply(m store.Message) replyView {
	ds := displayState(m)
	return replyView{
		ID:         m.PublicID,
		From:       m.FromAgent,
		Body:       m.Body,
		State:      ds,
		Unverified: ds == displayStateDeliveredInInputBox,
		CreatedAt:  m.CreatedAt,
	}
}

// waitForReplyResult is the structured outcome of wait_for_reply.
type waitForReplyResult struct {
	OK       bool       `json:"ok"`
	AskID    string     `json:"ask_id"`
	Reply    *replyView `json:"reply,omitempty"`
	TimedOut bool       `json:"timed_out"`
	Error    string     `json:"error,omitempty"`
}

// checkRepliesResult is the structured outcome of check_replies.
type checkRepliesResult struct {
	OK      bool        `json:"ok"`
	AskID   string      `json:"ask_id"`
	Replies []replyView `json:"replies"`
}

// doWaitForReply blocks until a reply to askID addressed to caller arrives or
// the timeout elapses (#250 Q2). No auto-ack of the consumed reply (#250 Q3) —
// ack stays an explicit, separate action. Shared by the CLI + MCP surfaces.
func doWaitForReply(ctx context.Context, s *store.Store, caller, askID string, timeout time.Duration) waitForReplyResult {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m, err := s.WaitForReply(wctx, caller, askID, 0, 0)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return waitForReplyResult{OK: true, AskID: askID, TimedOut: true}
		}
		return waitForReplyResult{OK: false, AskID: askID, Error: err.Error()}
	}
	rv := viewReply(*m)
	return waitForReplyResult{OK: true, AskID: askID, Reply: &rv}
}

// doCheckReplies returns every reply to askID addressed to caller (non-blocking)
// with id > since (since=0 → all). Shared by the CLI + MCP surfaces.
func doCheckReplies(ctx context.Context, s *store.Store, caller, askID string, since int64) (checkRepliesResult, error) {
	msgs, err := s.ListReplies(ctx, caller, askID, since)
	if err != nil {
		return checkRepliesResult{}, err
	}
	out := checkRepliesResult{OK: true, AskID: askID, Replies: make([]replyView, 0, len(msgs))}
	for _, m := range msgs {
		out.Replies = append(out.Replies, viewReply(m))
	}
	return out, nil
}

// runAskCLI parses ask-subcommand flags and sends a reply-expecting message.
//
// Usage: tmux-msg-claude ask --to <agent> [--reply-to <id>] "question"
//
// `ask` is a single-recipient `send` that sets the expects_reply marker (#250
// Q1) and returns the message id as the `ask_id` to pass to wait-for-reply /
// check-replies. Single-recipient only (v1; multi-recipient ask is out of
// scope).
func runAskCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender agent name (env: TMUX_AGENT_NAME)")
	to := fs.String("to", "", "recipient agent name (required); single recipient only")
	replyTo := fs.String("reply-to", "", "public_id of the message being replied to (threads the ask)")
	body := fs.String("body", "", "question body (else read from positional args)")
	format := fs.String("format", "json", "json|text")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if strings.Contains(*to, ",") {
		return writeJSONError(stdout, stderr,
			"ask is single-recipient only (v1, #250): --to takes one agent", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
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
		ExpectsReply: true,
		MaxRecipient: capRecipientQueue,
		MaxSender:    capSenderBacklog,
		MaxBody:      capBodyBytes,
		Format:       *format,
	}
	if p.Body == "" {
		p.Body = strings.Join(fs.Args(), " ")
	}
	return runSendWithStore(ctx, s, p, stdout, stderr)
}

// runWaitForReplyCLI parses wait-for-reply-subcommand flags and blocks.
//
// Usage: tmux-msg-claude wait-for-reply <ask_id> [--timeout <duration>]
func runWaitForReplyCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("wait-for-reply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "the asking agent (env: TMUX_AGENT_NAME; default: this pane)")
	timeout := fs.Duration("timeout", 30*time.Second, "how long to block for a reply before timing out")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	askID := fs.Arg(0)
	if askID == "" {
		return writeJSONError(stdout, stderr,
			"wait-for-reply requires an ask_id: tmux-msg-claude wait-for-reply <ask_id>", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	caller, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if caller == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --from, set $TMUX_AGENT_NAME, or register this pane", exitUsage)
	}

	res := doWaitForReply(ctx, s, caller, askID, *timeout)
	_ = writeJSONResult(stdout, res)
	if !res.OK {
		return exitInternal
	}
	return exitOK
}

// runCheckRepliesCLI parses check-replies-subcommand flags (non-blocking poll).
//
// Usage: tmux-msg-claude check-replies <ask_id> [--since <id>]
func runCheckRepliesCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check-replies", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "the asking agent (env: TMUX_AGENT_NAME; default: this pane)")
	since := fs.Int64("since", 0, "only return replies with numeric id greater than this (accumulation; 0 = all)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	askID := fs.Arg(0)
	if askID == "" {
		return writeJSONError(stdout, stderr,
			"check-replies requires an ask_id: tmux-msg-claude check-replies <ask_id>", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	caller, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if caller == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --from, set $TMUX_AGENT_NAME, or register this pane", exitUsage)
	}

	res, err := doCheckReplies(ctx, s, caller, askID, *since)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	_ = writeJSONResult(stdout, res)
	return exitOK
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// resendParams is the resolved input to runResendWithStore.
type resendParams struct {
	OriginalID string // public_id of the message to replay
	Force      bool   // override the delivered / in-flight guard
	Format     string // "json" (default) | "text"
}

// runResendCLI parses the resend-subcommand flags, opens the store, and
// dispatches to runResendWithStore. The message id is positional:
//
//	claude-msg resend <id> [--force] [--format json|text]
//
// resend replays an existing message to its ORIGINAL recipient with a
// "Replayed: original sent at <ts>" chrome marker (#157 PR1) — the explicit
// recovery path for a `delivered_unverified`/`failed` message. It refuses to
// replay an already-`delivered` (or still in-flight) message without --force,
// to keep an accidental re-run from spamming a duplicate.
func runResendCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	force := fs.Bool("force", false,
		"replay even an already-delivered or in-flight message (may duplicate)")
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

	p := resendParams{
		OriginalID: fs.Arg(0),
		Force:      *force,
		Format:     *format,
	}
	return runResendWithStore(context.Background(), s, p, stdout, stderr)
}

// runResendWithStore is the pure-logic core of resend: fetch the original,
// apply the duplicate guard, then insert a replay row (body byte-identical to
// the original, replay linkage carried as metadata) for the mailman to deliver.
// Designed to be table-tested.
func runResendWithStore(ctx context.Context, s *store.Store, p resendParams, stdout, stderr io.Writer) int {
	if p.OriginalID == "" {
		return writeJSONError(stdout, stderr,
			"resend requires a message id: claude-msg resend <id>", exitUsage)
	}

	orig, err := s.GetMessage(ctx, p.OriginalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown message id: %s", p.OriginalID), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	if reason, ok := resendGuard(orig, p.Force); !ok {
		renderSendResult(stdout, replayRefusal(orig, reason), orig.ToAgent, p.Format)
		return exitUnavailable
	}

	// The recipient must still be registered — replaying to an unknown agent
	// would queue an unclaimable row. Mirrors send's day-one fail-loud.
	rs, err := resolveRecipientStatus(ctx, s, orig.ToAgent)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if !rs.Registered {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("original recipient %s is no longer registered", orig.ToAgent), exitUnavailable)
	}

	var replyTo string
	if orig.ReplyTo.Valid {
		replyTo = orig.ReplyTo.String
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent:         orig.FromAgent,
		ToAgent:           orig.ToAgent,
		ReplyTo:           replyTo,
		Body:              orig.Body, // byte-identical → PR2 body-hash dedupe can match
		NoReplyExpected:   orig.NoReplyExpected,
		ReplayOf:          orig.PublicID,
		ReplayOfAt:        orig.CreatedAt,
		MaxRecipientQueue: capRecipientQueue,
		MaxSenderBacklog:  capSenderBacklog,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrRecipientQueueFull),
			errors.Is(err, store.ErrSenderBacklogFull):
			return writeJSONError(stdout, stderr, err.Error(), exitTempFail)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	renderSendResult(stdout, SendResponse{
		OK:        true,
		ID:        res.PublicID,
		Queued:    res.Queued,
		Recipient: rs,
		Replay: &ReplayStatus{
			OriginalID:     orig.PublicID,
			OriginalSentAt: orig.CreatedAt,
			OriginalState:  string(orig.State),
			Forced:         p.Force,
		},
	}, orig.ToAgent, p.Format)
	return exitOK
}

// resendGuard decides whether orig may be replayed. With force, anything goes.
// Without it: `failed` replays directly (it never arrived); `delivered` (which
// silently includes journal-only-unverified per #169) and still-in-flight
// `queued`/`delivering` are refused, because a replay would duplicate a message
// that did or might still arrive. The journal-aware path that would let a
// confirmed-unverified message replay without --force is gated on #169 (the
// substrate can't tell verified from unverified at the DB layer today). Shared
// by the CLI and MCP paths so the policy can't drift between them.
func resendGuard(orig *store.Message, force bool) (reason string, ok bool) {
	if force {
		return "", true
	}
	switch orig.State {
	case store.StateFailed:
		return "", true
	case store.StateDelivered:
		return fmt.Sprintf("message %s is already delivered; pass --force to resend anyway (may duplicate). "+
			"A delivered-but-unverified message is indistinguishable from a verified one at the DB layer (#169) — use --force to recover it.",
			orig.PublicID), false
	case store.StateQueued, store.StateDelivering:
		return fmt.Sprintf("message %s is still in flight (%s); wait for a terminal state or pass --force to resend anyway (may duplicate)",
			orig.PublicID, orig.State), false
	default:
		return fmt.Sprintf("message %s is in state %q; pass --force to resend anyway", orig.PublicID, orig.State), false
	}
}

// replayRefusal builds the ok:false guard-rejection, carrying the Replay block
// (so the caller sees which original + its state) and the reason. Shared by the
// CLI and MCP paths.
func replayRefusal(orig *store.Message, reason string) SendResponse {
	return SendResponse{
		OK: false,
		Replay: &ReplayStatus{
			OriginalID:     orig.PublicID,
			OriginalSentAt: orig.CreatedAt,
			OriginalState:  string(orig.State),
			Forced:         false,
		},
		Error: reason,
	}
}

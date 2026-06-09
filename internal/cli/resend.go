package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sync/atomic"

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
//	tmux-msg-claude resend <id> [--force] [--format json|text]
//
// resend replays an existing message to its ORIGINAL recipient with a
// "Replayed: original sent at <ts>" chrome marker (#157 PR1) — the explicit
// recovery path for a `delivered_in_input_box`/`failed` message. It refuses to
// replay an already-`delivered` (or still in-flight) message without --force,
// to keep an accidental re-run from spamming a duplicate.
func runResendCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resend", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	force := fs.Bool("force", false,
		"replay even an already-delivered or in-flight message (may duplicate). Not needed for a delivered_in_input_box message — the verified column (#169) recognizes the soft-fail and replays it directly; passing --force there is deprecated (#230)")
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
			"resend requires a message id: tmux-msg-claude resend <id>", exitUsage)
	}

	orig, err := s.GetMessage(ctx, p.OriginalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown message id: %s", p.OriginalID), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	// #230 (C): warn if --force was passed against a delivered_in_input_box
	// message — it's no longer needed there (the guard now lets it through
	// without force). Fires before the guard so the WARN lands whether or not
	// the guard would have admitted it.
	maybeWarnResendForceUnverified(stderr, orig, p.Force)

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
		Quick:             orig.Quick,
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
			OriginalState:  displayState(*orig),
			Forced:         p.Force,
		},
	}, orig.ToAgent, p.Format)
	return exitOK
}

// resendGuard decides whether orig may be replayed. With force, anything goes.
// Without it:
//
//   - `failed` replays directly (it never arrived).
//   - `delivered` with verified=0 (`delivered_in_input_box`) replays directly
//     too — the #169 column confirms the soft-fail, so this IS the sanctioned
//     recovery path and no --force is required (#230 decision (C)).
//   - `delivered` with verified=1 (confirmed) or verified=NULL (pre-#169 row,
//     where the substrate can't claim the message wasn't seen) is refused
//     without --force — a replay would duplicate a message that did arrive.
//   - still-in-flight `queued`/`delivering` is refused without --force.
//
// Shared by the CLI and MCP paths so the policy can't drift between them.
func resendGuard(orig *store.Message, force bool) (reason string, ok bool) {
	if force {
		return "", true
	}
	switch orig.State {
	case store.StateFailed:
		return "", true
	case store.StateDelivered:
		if isDeliveredUnverified(orig) {
			// delivered_in_input_box: the column says it wasn't verified, so
			// the explicit recovery is sanctioned without --force.
			return "", true
		}
		return fmt.Sprintf("message %s is already delivered; pass --force to resend anyway (may duplicate)",
			orig.PublicID), false
	case store.StateQueued, store.StateDelivering:
		return fmt.Sprintf("message %s is still in flight (%s); wait for a terminal state or pass --force to resend anyway (may duplicate)",
			orig.PublicID, orig.State), false
	default:
		return fmt.Sprintf("message %s is in state %q; pass --force to resend anyway", orig.PublicID, orig.State), false
	}
}

// isDeliveredUnverified reports whether m is the `delivered_in_input_box`
// soft-fail: state=delivered with the #169 verified bit explicitly 0. A
// verified=1 or verified=NULL delivered row is NOT this case.
func isDeliveredUnverified(m *store.Message) bool {
	return m.State == store.StateDelivered && m.Verified.Valid && m.Verified.Int64 == 0
}

// resendForceUnverifiedRemoval is the earliest-removal version pinned in the
// deprecation WARN and the CHANGELOG `### Deprecated` entry. The `--force`-on-
// unverified surface is deprecated in v0.13.0; earliest removal v0.15.0 (two
// minor cycles per ADR-0008 §two-minor floor). #230 is ADR-0008's third real
// deprecation cycle (after #177's alias arc and #140's notify-on-* family).
const resendForceUnverifiedRemoval = "v0.15.0"

// resendForceUnverifiedWarned guards the once-per-process firing of the
// deprecation WARN. The MCP daemon resends many times in one process, so the
// signal must not repeat on every call (feedback-deprecation-warn-once).
// Resettable in tests via resetResendForceWarnForTest.
var resendForceUnverifiedWarned atomic.Bool

// maybeWarnResendForceUnverified emits the ADR-0008 deprecation WARN to stderr
// when --force was passed against a delivered_in_input_box message — the case
// where --force is no longer needed (#230 decision (C)). At most once per
// process; a no-op when force is false or the message isn't the soft-fail case.
func maybeWarnResendForceUnverified(stderr io.Writer, orig *store.Message, force bool) {
	if !force || !isDeliveredUnverified(orig) {
		return
	}
	if resendForceUnverifiedWarned.CompareAndSwap(false, true) {
		fmt.Fprintf(stderr,
			"WARN deprecated_surface_used name=resend_force_unverified removal=%s — not needed; a delivered_in_input_box message replays without --force (which is for verified-state replays only) (ADR-0008)\n",
			resendForceUnverifiedRemoval)
	}
}

// resetResendForceWarnForTest resets the once-guard so a test can assert the
// WARN fires on the first deprecated use within that test.
func resetResendForceWarnForTest() { resendForceUnverifiedWarned.Store(false) }

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

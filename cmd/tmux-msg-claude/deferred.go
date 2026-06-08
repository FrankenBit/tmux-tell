package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// Deferred-delivery triggers (#227). A `send --deliver-after=<trigger>` stores
// the message in StateDeferred until the named trigger fires; `flush_deferred
// --trigger=<trigger>` promotes the caller's matching deferred rows to queued.
//
// v1 scope: the **post-compaction self-handoff** case only — `resume`, an
// explicit "I'm back, flush my queue" signal a chamber emits as part of its
// post-/compact resume routine. The other trigger forms sketched in #227
// (`register` auto-promotion, RFC3339 / relative-duration timestamps,
// `OR`-composition) are a deferred follow-up: the deferred→queued machinery is
// general, but wiring a trigger whose promotion path doesn't exist yet would
// ship a send that silently never delivers. So v1 accepts only the triggers it
// actually fires, and rejects the rest with a pointer to the follow-up.
const deferTriggerResume = "resume"

// validDeferTriggers is the set of trigger names v1 accepts on send /
// flush_deferred. Kept as a set so adding `register` / timestamps later is a
// one-line change plus the promotion wiring.
var validDeferTriggers = map[string]bool{
	deferTriggerResume: true,
}

// validateDeferTrigger reports an error when trigger is not a v1-supported
// deferred-delivery trigger. The empty string is the caller's "not deferred"
// signal and is the caller's responsibility to special-case before calling
// this — validateDeferTrigger treats "" as invalid so a stray empty value
// can't slip through a path that expects a real trigger.
func validateDeferTrigger(trigger string) error {
	if validDeferTriggers[trigger] {
		return nil
	}
	accepted := make([]string, 0, len(validDeferTriggers))
	for t := range validDeferTriggers {
		accepted = append(accepted, t)
	}
	sort.Strings(accepted)
	return fmt.Errorf("unsupported deliver-after trigger %q (v1 accepts: %s). "+
		"register-promotion, timestamp scheduling, and OR-composition are a #258 follow-up",
		trigger, strings.Join(accepted, ", "))
}

// flushResult is the structured outcome of a flush_deferred call.
type flushResult struct {
	OK       bool   `json:"ok"`
	Trigger  string `json:"trigger"`
	Promoted int    `json:"promoted"` // how many deferred rows were promoted to queued
}

// doFlushDeferred promotes the agent's deferred rows whose trigger matches to
// `queued` (#227), returning how many. Shared by the CLI `flush` subcommand
// and the tmux-msg.flush_deferred MCP tool so the authorization + trigger
// policy can't drift between them.
//
// Authorization: the agent flushes ONLY messages addressed to itself —
// PromoteDeferred is scoped to to_agent = agent, so there is no cross-agent
// flush. Idempotent: a flush with no matching deferred rows returns
// Promoted=0 (a no-op, not an error), so a chamber can call it unconditionally
// in its resume routine.
func doFlushDeferred(ctx context.Context, s *store.Store, agent, trigger string) (flushResult, error) {
	if err := validateDeferTrigger(trigger); err != nil {
		return flushResult{}, err
	}
	n, err := s.PromoteDeferred(ctx, agent, trigger)
	if err != nil {
		return flushResult{}, err
	}
	return flushResult{OK: true, Trigger: trigger, Promoted: int(n)}, nil
}

// runFlushCLI parses the flush-subcommand flags and promotes the calling
// agent's deferred messages for the given trigger.
//
// Usage: tmux-msg-claude flush --trigger=resume
//
// The convenience wrapper for the post-compaction case: a chamber calls it as
// part of its resume routine to deliver the orientation it staged before
// /compact.
func runFlushCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "agent whose deferred messages to flush (env: TMUX_AGENT_NAME; default: this pane)")
	trigger := fs.String("trigger", deferTriggerResume,
		"the deferred-delivery trigger to fire (#227). v1: `resume` (post-compaction self-handoff).")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	agent, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if agent == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --from, set $TMUX_AGENT_NAME, or register this pane", exitUsage)
	}

	res, err := doFlushDeferred(ctx, s, agent, *trigger)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	_ = writeJSONResult(stdout, res)
	return exitOK
}

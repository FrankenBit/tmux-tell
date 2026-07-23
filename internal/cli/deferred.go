package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// Deferred-delivery triggers (#227). A `send --deliver-after=<trigger>` stores
// the message in StateDeferred until the named trigger fires; `flush_deferred
// --trigger=<trigger>` promotes the caller's matching deferred rows to queued.
//
// Scope: each trigger is accepted only once its promotion path exists, because
// staging a send for a trigger nothing fires would silently never deliver.
//   - `resume` (#227) — post-compaction self-handoff. Auto-promoted (#843) when
//     the mailman delivers a bus session reset (`/compact` or `/clear`) to this
//     agent: the settled reset is the "chamber went away and came back" edge,
//     the resume analog of the register auto-fire below. The explicit
//     `flush_deferred --trigger=resume` remains valid and is still the path for
//     a reset the mailman did NOT deliver — e.g. a `/compact` typed straight
//     into the pane, which leaves no control row for the serve loop to see.
//   - `register` (#258a) — spawn-die session bridge: "remember this for my next
//     dispatch." The register handler auto-promotes these rows when the agent
//     (re)registers, so no explicit flush is needed (the register IS the fire).
//
// The remaining sketched forms — RFC3339 / relative-duration timestamps and
// `OR`-composition — are the #295 follow-up (each needs its own promotion
// wiring, e.g. a timestamp sweeper). So this set accepts only the triggers it
// actually fires, and rejects the rest with a pointer to that follow-up.
const (
	deferTriggerResume   = "resume"
	deferTriggerRegister = "register"
)

// validDeferTriggers is the set of trigger names accepted on send /
// flush_deferred. Kept as a set so adding a trigger is a one-line change plus
// its promotion wiring.
var validDeferTriggers = map[string]bool{
	deferTriggerResume:   true,
	deferTriggerRegister: true,
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
	return fmt.Errorf("unsupported deliver-after trigger %q (accepts: %s). "+
		"timestamp scheduling and OR-composition are a #295 follow-up",
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
// and the tmux-tell.flush_deferred MCP tool so the authorization + trigger
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
// Usage: tmux-tell-claude flush --trigger=resume
//
// The convenience wrapper for the post-compaction case: a chamber calls it as
// part of its resume routine to deliver the orientation it staged before
// /compact.
func runFlushCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("flush", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "agent whose deferred messages to flush (env: TMUX_AGENT_NAME; default: this pane)")
	trigger := fs.String("trigger", deferTriggerResume,
		"the deferred-delivery trigger to fire (#227). `resume` (post-compaction self-handoff); `register` auto-fires on (re)register (#258a) so rarely needs an explicit flush.")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

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

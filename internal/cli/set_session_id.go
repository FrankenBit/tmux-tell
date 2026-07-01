package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// setSessionIDResult is the shared wire shape for the `set-session-id` CLI
// subcommand and the `set_session_id` MCP tool (#644). JSON tags = single
// source of truth so both surfaces stay byte-identical (same discipline as
// setRespawnAfterShrinksResult / setMetabolismResult).
type setSessionIDResult struct {
	OK        bool   `json:"ok"`
	Agent     string `json:"agent"`      // the target chamber backfilled
	SessionID string `json:"session_id"` // the session id written
	// Discovered is true when the session id was self-discovered from the
	// target pane's process tree rather than passed explicitly (#644 use case 3).
	Discovered bool `json:"discovered"`
}

// setSessionID is the shared core behind #644's side-effect-free session-id
// backfill. It writes ONLY the session_id column (via store.SetSessionID) for
// the target agent — it deliberately does NOT clear attention_state (#224) or
// stuck_reason (#298) the way register does.
//
// register's auto-clear is correct only when the chamber ITSELF registers
// (it's back + ready by definition). When an orchestrator backfills a stale
// chamber's session id on-behalf, clearing those signals would erase the
// chamber's real state — a pane sitting at awaiting_operator, a parked mailman
// with a non-empty stuck_reason. This is the "field-specific backfill; does
// NOT register" path of AC#5: safe to run against another chamber.
//
// An empty session id is rejected rather than written: a backfill's purpose is
// to POPULATE the column, and writing "" would silently CLEAR a prior value —
// itself a side effect, and the opposite of the intent. Fail loud instead.
func setSessionID(ctx context.Context, s *store.Store, target, sessionID string, discovered bool) (setSessionIDResult, error) {
	if target == "" {
		return setSessionIDResult{}, errors.New(
			"no target chamber — pass --name <chamber> (or run inside a registered pane to self-target)")
	}
	if sessionID == "" {
		return setSessionIDResult{}, errors.New(
			"no session id — pass --session-id <UUID>, or run where the target pane's " +
				"process tree carries the wrapper-injected TMUX_TELL_SESSION_ID")
	}
	if err := s.SetSessionID(ctx, target, sessionID); err != nil {
		return setSessionIDResult{}, err
	}
	return setSessionIDResult{OK: true, Agent: target, SessionID: sessionID, Discovered: discovered}, nil
}

// runSetSessionIDCLI parses the `set-session-id` subcommand (#644): a
// side-effect-free session-id backfill that populates ONLY the session_id
// column for a target chamber, WITHOUT the attention/stuck clears `register`
// performs. Lets an orchestrator (Bosun) migrate a stale chamber's session id
// on-behalf without disrupting its real signals — the "does NOT register"
// backfill path.
//
// An explicit --session-id wins; otherwise the value is self-discovered by
// walking the target chamber's registered pane's process tree for the
// wrapper-injected TMUX_TELL_SESSION_ID (#643), the same mechanism register
// uses. The CLI runs on the host with access to every pane, so discovery-from-
// pane (#644 use case 3) lives here; the MCP surface requires an explicit id.
//
// Usage:
//
//	tmux-tell-claude set-session-id --name pilot --session-id <UUID>
//	tmux-tell-claude set-session-id --name pilot                # discover from pilot's pane
//	tmux-tell-claude set-session-id --session-id <UUID>         # self-target
func runSetSessionIDCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("set-session-id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	format := fs.String("format", "text", "text|json")
	name := fs.String("name", "", "target chamber to backfill (default: self, resolved from the current pane)")
	sessionIDFlag := fs.String("session-id", "",
		"explicit session id (UUID) to write; if omitted, self-discovered from the target pane's process tree")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	ctx := context.Background()
	target := strings.TrimSpace(*name)
	if target == "" {
		// No explicit target: self-target from the current pane (a chamber
		// backfilling its own session id). Best-effort — if it can't resolve,
		// the empty-target error in setSessionID tells the caller to pass --name.
		if self, rerr := resolveMCPIdentity(ctx, s); rerr == nil {
			target = self
		}
	}

	sessionID := strings.TrimSpace(*sessionIDFlag)
	discovered := false
	if sessionID == "" && target != "" {
		// Self-discover from the target's registered pane process tree — the same
		// mechanism register uses (#643 wrapper-injected TMUX_TELL_SESSION_ID).
		// Best-effort: a raw non-wrapper pane carries no var, so this stays empty
		// and setSessionID fails loud with the "pass --session-id" guidance.
		if a, gerr := s.GetAgent(ctx, target); gerr == nil && a.PaneID != "" {
			if sid, ok := discover.New().SessionIDForPane(ctx, a.PaneID); ok {
				sessionID, discovered = sid, true
			}
		}
	}

	res, err := setSessionID(ctx, s, target, sessionID, discovered)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
	case "text", "":
		if res.Discovered {
			fmt.Fprintf(stdout, "session_id for %s set to %s (self-discovered)\n", res.Agent, res.SessionID)
		} else {
			fmt.Fprintf(stdout, "session_id for %s set to %s\n", res.Agent, res.SessionID)
		}
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
	return exitOK
}

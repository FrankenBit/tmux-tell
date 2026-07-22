package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runReapCLI parses reap flags and dispatches. `reap` dead-letters undeliverable
// queued fossils — queued rows older than --older-than whose recipient is
// UNREACHABLE (not registered, or registered without a live pane) — by
// transitioning them to `failed` with a dead-letter reason (#726).
//
// It NEVER touches a recipient that holds a live-pane registration, so an
// intentional not-yet-live placeholder's queue is safe (the liveness discriminant
// is intrinsic, not a toggle — see store.reapableUndeliverablePredicate). Because
// the recipient/sender caps count only state='queued', reaping a fossil
// immediately frees the slot it was wedging. Dead-letter (not delete) keeps the
// audit row; the existing retention sweep prunes failed rows later.
//
// Exactly one of --dry-run (preview) or --confirm (proceed) is required, so the
// operator always sees the matched set before any mutation.
//
// Usage: tmux-tell-claude reap {--dry-run | --confirm} [--older-than 7d] [--agent NAME] [--format json]
func runReapCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	dryRun := fs.Bool("dry-run", false, "list what would be reaped without mutating")
	confirm := fs.Bool("confirm", false, "dead-letter the matching fossils")
	olderThan := fs.String("older-than", "7d", "reap only fossils older than this window (e.g. 7d, 24h)")
	agent := fs.String("agent", "", "scope to one recipient (default: all)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *dryRun == *confirm { // neither given, or both given
		return writeJSONError(stdout, stderr,
			"reap dead-letters rows: pass exactly one of --dry-run (preview) or --confirm (proceed)", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	return runReapWithStore(context.Background(), s, *agent, *olderThan, *dryRun, *format, time.Now(), stdout, stderr)
}

func runReapWithStore(ctx context.Context, s *store.Store,
	agent, olderThan string, dryRun bool, format string, now time.Time,
	stdout, stderr io.Writer,
) int {
	w, err := parseWindow(olderThan, now)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}
	if w.All {
		return writeJSONError(stdout, stderr,
			"--older-than 'all' is not meaningful; give a duration (e.g. 7d, 24h)", exitUsage)
	}
	cutoff := w.Since.UTC().Format(strandedTimeFormat)

	scope := "all agents"
	if agent != "" {
		scope = "agent " + agent
	}

	if dryRun {
		cands, err := s.ListReapableUndeliverable(ctx, agent, cutoff)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		fmt.Fprintf(stderr, "reap --dry-run: %d undeliverable fossil(s) would be reaped (%s, older than %s)\n",
			len(cands), scope, olderThan)
		return writeOK(format, stdout, map[string]any{
			"ok":         true,
			"dry_run":    true,
			"older_than": olderThan,
			"agent":      agent,
			"count":      len(cands),
			"candidates": cands,
		}, fmt.Sprintf("%d fossil(s) would be reaped (older than %s)", len(cands), olderThan))
	}

	reason := fmt.Sprintf("dead-letter-reap: recipient unreachable, queued >%s, never claimed (#726)", olderThan)
	n, err := s.ReapUndeliverable(ctx, agent, cutoff, reason)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	fmt.Fprintf(stderr, "reap: dead-lettered %d undeliverable fossil(s) (%s, older than %s)\n", n, scope, olderThan)
	return writeOK(format, stdout, map[string]any{
		"ok":         true,
		"dry_run":    false,
		"older_than": olderThan,
		"agent":      agent,
		"reaped":     n,
	}, fmt.Sprintf("dead-lettered %d fossil(s) older than %s", n, olderThan))
}

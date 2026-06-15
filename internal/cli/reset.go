package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runResetCLI parses reset flags and dispatches.
//
// Usage: tmux-tell-claude reset --confirm [--hard] [--agent NAME] [--format json]
func runResetCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	confirm := fs.Bool("confirm", false, "mandatory acknowledgement that this is destructive")
	hard := fs.Bool("hard", false, "also wipe delivered + failed audit history")
	agent := fs.String("agent", "", "scope to one recipient (default: all)")
	olderThan := fs.String("older-than", "", "delete only messages older than this window (e.g. 7d, 24h); restricts to delivered+failed scope")
	stateFilter := fs.String("state", "", "restrict --older-than to a specific terminal state: delivered|failed (default: both)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !*confirm {
		return writeJSONError(stdout, stderr,
			"--confirm required (reset is destructive)", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runResetWithStore(context.Background(), s, *agent, *hard, *olderThan, *stateFilter, *format, time.Now(), stdout, stderr)
}

func runResetWithStore(ctx context.Context, s *store.Store,
	agent string, hard bool, olderThan, stateFilter, format string, now time.Time,
	stdout, stderr io.Writer,
) int {
	if olderThan != "" {
		if hard {
			return writeJSONError(stdout, stderr,
				"--older-than and --hard are mutually exclusive", exitUsage)
		}
		return runResetOlderThan(ctx, s, agent, olderThan, stateFilter, format, now, stdout, stderr)
	}
	states := []store.State{store.StateQueued, store.StateDelivering}
	if hard {
		states = append(states, store.StateDelivered, store.StateFailed)
	}

	n, err := s.DeleteMessages(ctx, agent, states)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	scope := "all agents"
	if agent != "" {
		scope = "agent " + agent
	}
	mode := "queued+delivering"
	if hard {
		mode = "queued+delivering+delivered+failed (--hard)"
	}
	fmt.Fprintf(stderr, "reset: deleted %d rows (%s, %s)\n", n, scope, mode)

	return writeOK(format, stdout, map[string]any{
		"ok":      true,
		"deleted": n,
		"hard":    hard,
		"agent":   agent,
	}, fmt.Sprintf("deleted %d row(s)", n))
}

func runResetOlderThan(ctx context.Context, s *store.Store,
	agent, olderThan, stateFilter, format string, now time.Time,
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

	states, err := parseResetStateFilter(stateFilter)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}

	n, err := s.DeleteMessagesBefore(ctx, agent, cutoff, states)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	stateNames := make([]string, len(states))
	for i, st := range states {
		stateNames[i] = string(st)
	}
	return writeOK(format, stdout, map[string]any{
		"ok":         true,
		"deleted":    n,
		"older_than": olderThan,
		"agent":      agent,
		"states":     stateNames,
	}, fmt.Sprintf("deleted %d row(s) older than %s", n, olderThan))
}

// parseResetStateFilter parses the --state flag for reset --older-than.
// Empty filter defaults to [delivered, failed]. In-flight states
// (queued/delivering) are never permitted — only terminal states may be
// time-pruned.
func parseResetStateFilter(filter string) ([]store.State, error) {
	if filter == "" {
		return []store.State{store.StateDelivered, store.StateFailed}, nil
	}
	var states []store.State
	for _, p := range strings.Split(filter, ",") {
		switch strings.TrimSpace(p) {
		case "delivered":
			states = append(states, store.StateDelivered)
		case "failed":
			states = append(states, store.StateFailed)
		case "acknowledged":
			states = append(states, store.StateAcknowledged)
		default:
			return nil, fmt.Errorf("--state %q: only 'delivered', 'failed', and 'acknowledged' are valid with --older-than",
				strings.TrimSpace(p))
		}
	}
	return states, nil
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// runResetCLI parses reset flags and dispatches.
//
// Usage: claude-msg reset --confirm [--hard] [--agent NAME] [--format json]
func runResetCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	confirm := fs.Bool("confirm", false, "mandatory acknowledgement that this is destructive")
	hard := fs.Bool("hard", false, "also wipe delivered + failed audit history")
	agent := fs.String("agent", "", "scope to one recipient (default: all)")
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

	return runResetWithStore(context.Background(), s, *agent, *hard, *format, stdout, stderr)
}

func runResetWithStore(ctx context.Context, s *store.Store,
	agent string, hard bool, format string,
	stdout, stderr io.Writer,
) int {
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

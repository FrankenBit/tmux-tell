package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// runWhoamiCLI parses whoami-subcommand flags and dispatches.
//
// Usage: claude-msg whoami [--as NAME] [--format text|json]
func runWhoamiCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	asName := fs.String("as", "",
		"explicit identity (overrides $CLAUDE_AGENT_NAME)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	name := resolveAgentName(*asName)
	if name == "" {
		return writeJSONError(stdout, stderr,
			"CLAUDE_AGENT_NAME not set; pass --as <name>", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	live, err := tmuxio.LivePanes(context.Background())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	return runWhoamiWithStore(context.Background(), s, live, name, *format, stdout, stderr)
}

func runWhoamiWithStore(ctx context.Context, s *store.Store,
	live map[string]bool, name, format string,
	stdout, stderr io.Writer,
) int {
	a, err := s.GetAgent(ctx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			_ = writeJSONResult(stdout, map[string]any{
				"ok":         false,
				"error":      "agent not in registry",
				"name":       name,
				"registered": false,
			})
			fmt.Fprintf(stderr, "agent %q not in registry — run 'claude-msg discover' or check the install\n", name)
			return exitUnavailable
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	paneStatus := "no-pane"
	switch {
	case a.PaneID == "":
		paneStatus = "no-pane"
	case live[a.PaneID]:
		paneStatus = "live"
	default:
		paneStatus = "stale"
	}

	depth, err := s.RecipientQueueDepth(ctx, name)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, map[string]any{
			"ok":          true,
			"name":        a.Name,
			"registered":  true,
			"pane":        a.PaneID,
			"pane_status": paneStatus,
			"paused":      a.Paused,
			"queued":      depth,
		})
		return exitOK
	case "text", "":
		pane := a.PaneID
		if pane == "" {
			pane = "-"
		}
		fmt.Fprintf(stdout, "NAME\t%s\n", a.Name)
		fmt.Fprintf(stdout, "PANE\t%s (%s)\n", pane, paneStatus)
		fmt.Fprintf(stdout, "PAUSED\t%s\n", yesNo(a.Paused))
		fmt.Fprintf(stdout, "INBOX\t%d queued\n", depth)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

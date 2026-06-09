package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// runWhoamiCLI parses whoami-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude whoami [--as NAME] [--format text|json]
func runWhoamiCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	asName := fs.String("as", "",
		"explicit identity (overrides $TMUX_AGENT_NAME)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	name, src, err := identity.Resolve(ctx, s, *asName)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	if name == "" {
		return writeJSONError(stdout, stderr,
			"cannot resolve identity: pass --as, set $TMUX_AGENT_NAME, or register this pane",
			exitUsage)
	}

	live, err := tmuxio.LivePanes(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	return runWhoamiWithStore(ctx, s, live, name, string(src), *format, stdout, stderr)
}

func runWhoamiWithStore(ctx context.Context, s *store.Store,
	live map[string]bool, name, source, format string,
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
			fmt.Fprintf(stderr, "agent %q not in registry — run 'tmux-msg-claude discover' or check the install\n", name)
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
			"source":      source,
		})
		return exitOK
	case "text", "":
		pane := a.PaneID
		if pane == "" {
			pane = "-"
		}
		fmt.Fprintf(stdout, "NAME\t%s (via %s)\n", a.Name, source)
		fmt.Fprintf(stdout, "PANE\t%s (%s)\n", pane, paneStatus)
		fmt.Fprintf(stdout, "PAUSED\t%s\n", yesNo(a.Paused))
		fmt.Fprintf(stdout, "INBOX\t%d queued\n", depth)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

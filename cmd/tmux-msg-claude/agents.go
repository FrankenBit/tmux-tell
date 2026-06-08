package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// runAgentsCLI parses agents-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude agents [--available] [--format text|json]
func runAgentsCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	available := fs.Bool("available", false,
		"only agents whose pane is live and aren't paused")
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

	live, err := tmuxio.LivePanes(context.Background())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	return runAgentsWithStore(context.Background(), s, live,
		*available, *format, stdout, stderr)
}

// agentView is the per-row payload for the agents subcommand and the
// upcoming MCP tool (#16). Pane-status values: "live" | "stale" | "no-pane".
type agentView struct {
	Name       string `json:"name"`
	Pane       string `json:"pane"`
	PaneStatus string `json:"pane_status"`
	Paused     bool   `json:"paused"`
	Queued     int    `json:"queued"`
	// AttentionState surfaces the #224 chamber → operator attention signal
	// ("idle" / "busy" / "awaiting_operator"). Empty omitted from JSON so
	// pre-#224 callers see no schema-shape change.
	AttentionState string `json:"attention_state,omitempty"`
}

func runAgentsWithStore(ctx context.Context, s *store.Store,
	live map[string]bool, availableOnly bool, format string,
	stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	rows := make([]agentView, 0, len(agents))
	for _, a := range agents {
		v := agentView{
			Name:           a.Name,
			Pane:           a.PaneID,
			Paused:         a.Paused,
			AttentionState: a.AttentionState,
		}
		switch {
		case a.PaneID == "":
			v.PaneStatus = "no-pane"
		case live[a.PaneID]:
			v.PaneStatus = "live"
		default:
			v.PaneStatus = "stale"
		}
		depth, err := s.RecipientQueueDepth(ctx, a.Name)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		v.Queued = depth

		if availableOnly && (v.PaneStatus != "live" || v.Paused) {
			continue
		}
		rows = append(rows, v)
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, rows)
		return exitOK
	case "text", "":
		header := []string{"NAME", "PANE", "STATUS", "PAUSED", "QUEUED", "ATTENTION"}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			pane := r.Pane
			if pane == "" {
				pane = "-"
			}
			attention := r.AttentionState
			if attention == "" {
				attention = "idle"
			}
			out = append(out, []string{
				r.Name, pane, r.PaneStatus, yesNo(r.Paused), itoa(r.Queued), attention,
			})
		}
		renderTextTable(stdout, header, out)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

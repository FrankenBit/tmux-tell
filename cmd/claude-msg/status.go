package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// runStatusCLI parses status-subcommand flags and dispatches.
//
// Usage: claude-msg status [--format text|json]
func runStatusCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
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

	return runStatusWithStore(context.Background(), s, *format, stdout, stderr)
}

// agentStatus is the per-agent summary status reports.
type agentStatus struct {
	Name       string `json:"name"`
	Paused     bool   `json:"paused"`
	Queued     int    `json:"queued"`
	Delivering int    `json:"delivering"`
	Delivered  int    `json:"delivered"`
	Failed     int    `json:"failed"`
}

func runStatusWithStore(ctx context.Context, s *store.Store,
	format string, stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	rows := make([]agentStatus, 0, len(agents))
	for _, a := range agents {
		st := agentStatus{Name: a.Name, Paused: a.Paused}
		for _, state := range []store.State{
			store.StateQueued, store.StateDelivering,
			store.StateDelivered, store.StateFailed,
		} {
			// Quick aggregate query per state. For ~5-20 agents this is
			// negligible; if we grow to many agents we can swap in one
			// GROUP BY query.
			msgs, err := s.ListMessages(ctx, store.ListFilter{
				ToAgent: a.Name, State: state, Limit: 1000,
			})
			if err != nil {
				return writeJSONError(stdout, stderr, err.Error(), exitInternal)
			}
			switch state {
			case store.StateQueued:
				st.Queued = len(msgs)
			case store.StateDelivering:
				st.Delivering = len(msgs)
			case store.StateDelivered:
				st.Delivered = len(msgs)
			case store.StateFailed:
				st.Failed = len(msgs)
			}
		}
		rows = append(rows, st)
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, rows)
		return exitOK
	case "text", "":
		header := []string{"NAME", "PAUSED", "QUEUED", "DELIVERING", "DELIVERED", "FAILED"}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, []string{
				r.Name,
				yesNo(r.Paused),
				itoa(r.Queued),
				itoa(r.Delivering),
				itoa(r.Delivered),
				itoa(r.Failed),
			})
		}
		renderTextTable(stdout, header, out)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

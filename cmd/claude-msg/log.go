package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/render"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// runLogCLI parses log-subcommand flags and dispatches.
//
// Usage: claude-msg log --thread <id> [--format text|json]
func runLogCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	thread := fs.String("thread", "", "public_id anywhere in the thread")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *thread == "" {
		return writeJSONError(stdout, stderr, "--thread <id> required", exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runLogWithStore(context.Background(), s, *thread, *format, stdout, stderr)
}

func runLogWithStore(ctx context.Context, s *store.Store,
	threadID, format string,
	stdout, stderr io.Writer,
) int {
	msgs, err := s.GetThread(ctx, threadID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("unknown id: %s", threadID), exitDataErr)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	switch format {
	case "json":
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, messageToMap(m))
		}
		_ = writeJSONResult(stdout, out)
		return exitOK

	case "text", "":
		for i, m := range msgs {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			// Body block from the renderer + a small footer.
			fmt.Fprint(stdout, render.Message(m))
			fmt.Fprintf(stdout, "  state=%s  created=%s",
				m.State, m.CreatedAt)
			if m.DeliveredAt.Valid {
				fmt.Fprintf(stdout, "  delivered=%s", m.DeliveredAt.String)
			}
			if m.Error.Valid {
				fmt.Fprintf(stdout, "  error=%q", m.Error.String)
			}
			fmt.Fprintln(stdout)
		}
		return exitOK

	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/render"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runLogCLI parses log-subcommand flags and dispatches.
//
// Usage: tmux-tell-claude log --thread <id> [--format text|json]
func runLogCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("log", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	thread := fs.String("thread", "", "public_id anywhere in the thread")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
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

	// Resolve the render length-marker threshold (#160) against the fleet
	// default — the `log` viewer renders an arbitrary cross-agent thread,
	// so there's no single recipient to key a per-agent override on; the
	// empty-agent resolution falls through to [defaults] then the
	// hardcoded default. A malformed value WARNs and falls back, matching
	// the mailman startup path. Config errors don't block the viewer.
	byteMarkerThreshold := render.DefaultByteMarkerThreshold
	cfg, _ := config.Load()
	if raw := config.ResolveString(cfg, "", "render-byte-marker-threshold", ""); raw != "" {
		if n, perr := config.ParseByteSize(raw); perr != nil {
			fmt.Fprintf(stderr, "WARN config: render-byte-marker-threshold %q: %v — using %d\n",
				raw, perr, byteMarkerThreshold)
		} else {
			byteMarkerThreshold = n
		}
	}

	return runLogWithStore(context.Background(), s, *thread, *format, byteMarkerThreshold, stdout, stderr)
}

func runLogWithStore(ctx context.Context, s *store.Store,
	threadID, format string, byteMarkerThreshold int,
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
			fmt.Fprint(stdout, render.Message(m, byteMarkerThreshold, time.Now()))
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

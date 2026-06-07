package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runInboxCLI parses inbox-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude inbox [AGENT] [--state STATE] [--limit N] [--format text|json]
//
// AGENT defaults to the calling pane's identity (via the same
// resolution rules as tmux-msg.whoami).
func runInboxCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	stateFlag := fs.String("state", "queued",
		"queued|delivering|delivered|failed (empty = all)")
	limit := fs.Int("limit", 50, "maximum rows to return")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: tmux-msg-claude inbox [AGENT] [flags]")
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	var agent string
	if fs.NArg() == 1 {
		agent = fs.Arg(0)
	} else {
		agent, _, err = identity.Resolve(ctx, s, "")
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		if agent == "" {
			return writeJSONError(stdout, stderr,
				"cannot resolve identity: pass AGENT, set $TMUX_AGENT_NAME, or register this pane",
				exitUsage)
		}
	}

	return runInboxWithStore(ctx, s,
		agent, store.State(*stateFlag), *limit, *format, stdout, stderr)
}

func runInboxWithStore(ctx context.Context, s *store.Store,
	agent string, state store.State, limit int, format string,
	stdout, stderr io.Writer,
) int {
	msgs, err := s.ListMessages(ctx, store.ListFilter{
		ToAgent: agent,
		State:   state,
		Limit:   limit,
	})
	if err != nil {
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
		header := []string{"ID", "FROM", "TO", "STATE", "AGE", "BODY"}
		rows := make([][]string, 0, len(msgs))
		for _, m := range msgs {
			rows = append(rows, []string{
				m.PublicID,
				m.FromAgent,
				m.ToAgent,
				string(m.State),
				ageOf(m.CreatedAt),
				shortBody(m.Body, 60),
			})
		}
		renderTextTable(stdout, header, rows)
		return exitOK

	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

// messageToMap shapes a Message for JSON output. Mirrors the wire format
// the MCP tools will use (#16), so they share one definition.
func messageToMap(m store.Message) map[string]any {
	out := map[string]any{
		"id":         m.PublicID,
		"from":       m.FromAgent,
		"to":         m.ToAgent,
		"body":       m.Body,
		"state":      string(m.State),
		"created_at": m.CreatedAt,
	}
	if m.ReplyTo.Valid {
		out["reply_to"] = m.ReplyTo.String
	}
	if m.DeliveredAt.Valid {
		out["delivered_at"] = m.DeliveredAt.String
	}
	if m.Error.Valid {
		out["error"] = m.Error.String
	}
	return out
}

// ageOf returns "32s", "4m12s", "1h", "3d" given an ISO 8601 UTC timestamp.
// Returns "-" if the input doesn't parse.
func ageOf(iso string) string {
	t, err := time.Parse("2006-01-02T15:04:05.000Z", iso)
	if err != nil {
		// fallback for older sqlite formats without subsecond
		t, err = time.Parse("2006-01-02T15:04:05Z", iso)
		if err != nil {
			return "-"
		}
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - 60*m
		return fmt.Sprintf("%dm%ds", m, s)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

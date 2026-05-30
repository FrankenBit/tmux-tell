package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// errTrackNotFound wraps store.ErrNotFound with a precise "no such
// message: <id>" message so both call-sites get the same wording, and
// the CLI can route the exit code by sentinel rather than string-match.
var errTrackNotFound = errors.New("no such message")

// trackResult is the structured response from both the
// `claude-msg track` CLI subcommand and the `semaphore.message_status`
// MCP tool. JSON tags with omitempty on the optional state-dependent
// fields (delivered_at, error, reply_to) keep the wire shape clean.
//
// Single source of truth pattern: both call-sites serialise this
// struct directly. Don't reconstruct the shape by hand in either
// caller (see Surveyor's #28 review of ea29ede).
type trackResult struct {
	OK          bool   `json:"ok"`
	ID          string `json:"id"`
	From        string `json:"from"`
	To          string `json:"to"`
	State       string `json:"state"`
	Kind        string `json:"kind"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	Error       string `json:"error,omitempty"`
	ReplyTo     string `json:"reply_to,omitempty"`
}

// doTrack is the shared lookup pipeline. Both call-sites converge here
// so the wire shape and the not-found semantics stay consistent.
func doTrack(ctx context.Context, s *store.Store, id string) (*trackResult, error) {
	if id == "" {
		return nil, errors.New("id required")
	}
	m, err := s.GetMessage(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", errTrackNotFound, id)
		}
		return nil, err
	}
	out := &trackResult{
		OK:        true,
		ID:        m.PublicID,
		From:      m.FromAgent,
		To:        m.ToAgent,
		State:     string(m.State),
		Kind:      string(m.Kind),
		CreatedAt: m.CreatedAt,
	}
	if m.DeliveredAt.Valid {
		out.DeliveredAt = m.DeliveredAt.String
	}
	if m.Error.Valid {
		out.Error = m.Error.String
	}
	if m.ReplyTo.Valid {
		out.ReplyTo = m.ReplyTo.String
	}
	return out, nil
}

// runTrackCLI parses the track-subcommand flags and dispatches.
//
// Usage: claude-msg track <id> [--format text|json]
func runTrackCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("track", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: claude-msg track <id> [flags]")
		return exitUsage
	}
	id := fs.Arg(0)

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	res, err := doTrack(ctx, s, id)
	if err != nil {
		if errors.Is(err, errTrackNotFound) {
			return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	switch *format {
	case "json":
		_ = writeJSONResult(stdout, res)
		return exitOK
	case "text", "":
		fmt.Fprintf(stdout, "ID\t%s\n", res.ID)
		fmt.Fprintf(stdout, "FROM\t%s\n", res.From)
		fmt.Fprintf(stdout, "TO\t%s\n", res.To)
		fmt.Fprintf(stdout, "STATE\t%s\n", res.State)
		fmt.Fprintf(stdout, "KIND\t%s\n", res.Kind)
		fmt.Fprintf(stdout, "CREATED\t%s (%s ago)\n", res.CreatedAt, ageOf(res.CreatedAt))
		if res.DeliveredAt != "" {
			fmt.Fprintf(stdout, "DELIVERED\t%s (%s ago)\n", res.DeliveredAt, ageOf(res.DeliveredAt))
		}
		if res.ReplyTo != "" {
			fmt.Fprintf(stdout, "REPLY_TO\t%s\n", res.ReplyTo)
		}
		if res.Error != "" {
			fmt.Fprintf(stdout, "ERROR\t%s\n", res.Error)
		}
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
}


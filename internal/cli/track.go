package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// errTrackNotFound wraps store.ErrNotFound with a precise "no such
// message: <id>" message so both call-sites get the same wording, and
// the CLI can route the exit code by sentinel rather than string-match.
var errTrackNotFound = errors.New("no such message")

// trackResult is the structured response from both the
// `tmux-tell-claude track` CLI subcommand and the `tmux-tell.message_status`
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
		State:     displayState(*m),
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
// Usage: tmux-tell-claude track <id> [--format text|json] [--watch [--watch-interval D] [--watch-timeout D]]
func runTrackCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("track", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	canonical := fs.Bool("canonical", false,
		"open the canonical XDG-default DB by name (ignores --db / $CLAUDE_MSG_DB) — the operator's ground-truth \"is id X actually in the canonical DB?\" query when an MCP view might be bound to a stale inode (#348)")
	format := fs.String("format", "text", "text|json")
	watch := fs.Bool("watch", false,
		"poll until the message reaches a terminal state (delivered/failed); exits when state stops changing or timeout fires (#49)")
	watchInterval := fs.Duration("watch-interval", 5*time.Second,
		"poll cadence when --watch is set")
	watchTimeout := fs.Duration("watch-timeout", 0,
		"bail out after this duration without reaching a terminal state when --watch is set (0 = no timeout)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(stderr, "usage: %s track <id> [flags]\n", active.BinaryName)
		return exitUsage
	}
	id := fs.Arg(0)

	openPath := resolveDBPath(*dbPath)
	if *canonical {
		if *dbPath != "" {
			return writeJSONError(stdout, stderr,
				"--canonical and --db are mutually exclusive (--canonical forces the XDG default)", exitUsage)
		}
		openPath = defaultDBLocation()
	}
	s, err := store.Open(openPath)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	if *watch {
		return runTrackWatch(s, id, *format, *watchInterval, *watchTimeout, stdout, stderr)
	}

	ctx := context.Background()
	res, err := doTrack(ctx, s, id)
	if err != nil {
		if errors.Is(err, errTrackNotFound) {
			return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
		}
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	renderTrackResult(stdout, stderr, res, *format)
	switch *format {
	case "json", "text", "":
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
}

// renderTrackResult formats a trackResult into the requested output
// shape. Extracted from runTrackCLI so runTrackWatch can reuse it on
// each state change.
func renderTrackResult(stdout, stderr io.Writer, res *trackResult, format string) {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, res)
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
	}
}

// isTerminalState reports whether a track state is one that won't
// change further (delivered or failed). Used by --watch to know when
// to exit.
func isTerminalState(state string) bool {
	return state == string(store.StateDelivered) || state == string(store.StateFailed)
}

// runTrackWatch polls the store every watchInterval and emits a render
// of the trackResult each time the state changes. Exits when the state
// becomes terminal (delivered or failed), when watchTimeout fires (if
// > 0), or when the process receives SIGINT.
//
// Per #49: useful for the "I just sent a long autonomous task; ping me
// when it's been consumed" pattern. The first render always emits
// regardless of state — so the operator gets immediate feedback.
func runTrackWatch(
	s *store.Store, id, format string,
	interval, timeout time.Duration,
	stdout, stderr io.Writer,
) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, timeout)
		defer tcancel()
	}

	var lastState string
	first := true
	for {
		res, err := doTrack(ctx, s, id)
		if err != nil {
			if errors.Is(err, errTrackNotFound) {
				return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
			}
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		if first || res.State != lastState {
			renderTrackResult(stdout, stderr, res, format)
			lastState = res.State
			first = false
		}
		if isTerminalState(res.State) {
			return exitOK
		}
		select {
		case <-ctx.Done():
			// SIGINT or timeout. Emit a final render so the operator
			// sees the last-known state on exit.
			if !first {
				return exitOK
			}
			return exitOK
		case <-time.After(interval):
			continue
		}
	}
}

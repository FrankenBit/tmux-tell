package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// pingBody is the placeholder body carried by a kind=ping row. It is
// never pasted into the recipient's pane (the mailman's ping branch
// short-circuits before delivery) — InsertMessage simply requires a
// non-empty body, and this marker keeps audit/inbox views legible.
const pingBody = "ping"

// defaultPingTimeout bounds the probe wait when the caller doesn't set
// one. A reachable agent answers in well under a second (one ClaimNext +
// one LivePanes shell-out); 5s leaves headroom for a busy daemon working
// through a queue ahead of the ping.
const defaultPingTimeout = 5 * time.Second

// pingPollInterval is how often pingProbe re-reads the row's state while
// waiting for the mailman to process it.
const pingPollInterval = 100 * time.Millisecond

// pingStateTimeout is the synthetic terminal state pingProbe reports when
// the wait elapses before the mailman transitions the row. Distinct from
// the store's "failed": a `failed` ping means the agent is registered but
// unreachable (pane gone); a `timeout` means no mailman answered in time
// (daemon down, paused, or backlogged).
const pingStateTimeout = "timeout"

// pingResult is the structured response shared by the `claude-msg ping`
// CLI subcommand and the `tmux-msg.ping` MCP tool (#144). OK is true only
// when the probe reached `delivered` (recipient reachable). State is one
// of "delivered", "failed", or "timeout".
type pingResult struct {
	OK        bool   `json:"ok"`
	Agent     string `json:"agent"`
	ID        string `json:"id"`
	State     string `json:"state"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

// pingCLIParams is the resolved input to runPingWithStore, post-flag-parse.
type pingCLIParams struct {
	From    string
	To      string
	Timeout time.Duration
	Format  string
}

// insertPing validates the recipient and inserts a kind=ping row,
// returning its public_id. The recipient MUST be registered: pinging a
// non-registered agent fails loud (per #144 out-of-scope — "should
// fail-loud with a clear error, not silently succeed") rather than
// enqueuing a row no mailman would ever claim. The ping bypasses the
// recipient-queue and sender-backlog caps: a reachability probe must not
// be rejected because the recipient's inbox is momentarily full (the row
// is transient — queued→delivered with no paste).
func insertPing(ctx context.Context, s *store.Store, from, to string) (string, error) {
	if from == "" {
		return "", errors.New("cannot resolve sender: set $CLAUDE_AGENT_NAME, pass --from, or register this pane")
	}
	if to == "" {
		return "", errors.New("recipient agent required")
	}
	if _, err := s.GetAgent(ctx, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("unknown recipient: %s (not registered — ping cannot reach an unregistered agent)", to)
		}
		return "", err
	}
	res, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: from,
		ToAgent:   to,
		Body:      pingBody,
		Kind:      store.KindPing,
		// Caps disabled (0): a probe must not be rejected on a full queue.
		MaxRecipientQueue: 0,
		MaxSenderBacklog:  0,
	})
	if err != nil {
		return "", err
	}
	return res.PublicID, nil
}

// pollPingTerminal polls the row identified by id until it reaches a
// store-terminal state (delivered/failed) or timeout elapses, returning
// the structured pingResult. agent is echoed back in the result for the
// caller's convenience. A GetMessage error aborts the poll and is
// returned. ctx cancellation is reported as a timeout-class result.
func pollPingTerminal(ctx context.Context, s *store.Store, id, agent string, timeout, pollInterval time.Duration) (pingResult, error) {
	if pollInterval <= 0 {
		pollInterval = pingPollInterval
	}
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		m, err := s.GetMessage(ctx, id)
		if err != nil {
			return pingResult{}, err
		}
		if m.State == store.StateDelivered || m.State == store.StateFailed {
			out := pingResult{
				OK:        m.State == store.StateDelivered,
				Agent:     agent,
				ID:        id,
				State:     string(m.State),
				ElapsedMs: time.Since(start).Milliseconds(),
			}
			if m.Error.Valid {
				out.Error = m.Error.String
			}
			return out, nil
		}
		if !time.Now().Before(deadline) {
			return pingResult{
				OK:        false,
				Agent:     agent,
				ID:        id,
				State:     pingStateTimeout,
				ElapsedMs: time.Since(start).Milliseconds(),
				Error:     fmt.Sprintf("no terminal state within %s (mailman down, paused, or backlogged?)", timeout),
			}, nil
		}
		select {
		case <-ctx.Done():
			return pingResult{
				OK:        false,
				Agent:     agent,
				ID:        id,
				State:     pingStateTimeout,
				ElapsedMs: time.Since(start).Milliseconds(),
				Error:     ctx.Err().Error(),
			}, nil
		case <-time.After(pollInterval):
		}
	}
}

// pingProbe is the shared core behind both the CLI and MCP surfaces
// (#144): insert a kind=ping row, then poll for its terminal state.
func pingProbe(ctx context.Context, s *store.Store, from, to string, timeout, pollInterval time.Duration) (pingResult, error) {
	id, err := insertPing(ctx, s, from, to)
	if err != nil {
		return pingResult{}, err
	}
	return pollPingTerminal(ctx, s, id, to, timeout, pollInterval)
}

// runPingCLI parses ping-subcommand flags, opens the store, resolves the
// sender identity, and dispatches to runPingWithStore.
//
// Usage: claude-msg ping <agent> [--timeout D] [--format text|json] [--from NAME]
func runPingCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("ping", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender agent name (env: CLAUDE_AGENT_NAME)")
	timeout := fs.Duration("timeout", defaultPingTimeout,
		"bound the wait for a terminal delivery state")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: claude-msg ping <agent> [--timeout D] [--format text|json]")
		return exitUsage
	}
	to := fs.Arg(0)

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	fromName, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	return runPingWithStore(ctx, s, pingCLIParams{
		From:    fromName,
		To:      to,
		Timeout: *timeout,
		Format:  *format,
	}, stdout, stderr)
}

// runPingWithStore is the pure-logic core: validates --format, runs the
// probe, renders the result, and returns the exit code. Designed to be
// table-tested.
func runPingWithStore(ctx context.Context, s *store.Store, p pingCLIParams, stdout, stderr io.Writer) int {
	switch p.Format {
	case "", "text", "json":
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", p.Format), exitUsage)
	}
	res, err := pingProbe(ctx, s, p.From, p.To, p.Timeout, pingPollInterval)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
	}
	renderPingResult(stdout, res, p.Format)
	return pingExitCode(res)
}

// renderPingResult writes the probe outcome in the requested shape.
func renderPingResult(stdout io.Writer, res pingResult, format string) {
	switch format {
	case "json":
		_ = writeJSONResult(stdout, res)
	default: // text / ""
		status := "reachable"
		if !res.OK {
			status = "UNREACHABLE"
		}
		fmt.Fprintf(stdout, "AGENT\t%s\n", res.Agent)
		fmt.Fprintf(stdout, "PING\t%s (%s)\n", res.State, status)
		fmt.Fprintf(stdout, "ELAPSED\t%dms\n", res.ElapsedMs)
		fmt.Fprintf(stdout, "ID\t%s\n", res.ID)
		if res.Error != "" {
			fmt.Fprintf(stdout, "ERROR\t%s\n", res.Error)
		}
	}
}

// pingExitCode maps a probe outcome to a sysexits-style code so tooling
// can branch on reachability:
//   - delivered → 0 (reachable)
//   - failed    → exitUnavailable (registered but unreachable: pane gone)
//   - timeout   → exitTempFail (no answer in time: daemon down/paused/busy)
func pingExitCode(res pingResult) int {
	switch res.State {
	case string(store.StateDelivered):
		return exitOK
	case pingStateTimeout:
		return exitTempFail
	default: // failed
		return exitUnavailable
	}
}

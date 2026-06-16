package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/identity"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// refreshResult is the structured return shape for the
// refresh-all-mcps subcommand. Per-agent rows in `agents` + a
// summary at the top so the operator can scan the count line without
// reading the table.
//
// The JSON tags drive the wire shape; the text formatter renders the
// same struct as an aligned table. Don't reconstruct this shape by
// hand in either path or the two outputs will drift.
type refreshResult struct {
	OK     bool                `json:"ok"`
	Sender string              `json:"sender"`
	Total  int                 `json:"total"`
	Queued int                 `json:"queued"`
	Failed int                 `json:"failed"`
	Agents []refreshAgentEntry `json:"agents"`
}

// refreshAgentEntry is one row of refreshResult.agents — a single
// agent's fan-out outcome. `DisableID` and `EnableID` are the public
// ids of the two control rows the mcp-restart-tmux-msg macro queues
// (see #28); they're present on success, absent on failure. `Error`
// is the inverse — present on failure, absent on success.
type refreshAgentEntry struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	DisableID string `json:"disable_id,omitempty"`
	EnableID  string `json:"enable_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runRefreshAllMcpsCLI parses the refresh-all-mcps flags and dispatches.
//
// Usage: tmux-tell-claude refresh-all-mcps [--format text|json]
//
// Convenience surface for the bulk version of `tmux-tell-claude control
// --to <agent> --command mcp-restart-tmux-msg`. Iterates the
// registered agents table and fires the macro per agent, then
// reports per-agent success/failure + a summary line.
//
// Cap-protected via the existing 5/2 ceilings on the per-recipient
// queue and per-sender backlog — a runaway storm can't overwhelm any
// agent. Operator-explicit only (no MCP tool variant; that would
// be a DoS-amplification class).
//
// What this v1 does NOT do (size/M follow-up triggers per #62):
//   - Does NOT gate on agent-state via #69 — if mid-tool-call
//     disruption becomes recurring felt-pain, file the size/M
//     `state in [idle, awaiting-operator]` gating follow-up.
//   - Does NOT support `--except` / `--only` filtering — file when
//     a real use case surfaces.
//   - Does NOT expose an MCP tool — operator-only by design.
func runRefreshAllMcpsCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("refresh-all-mcps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	from := fs.String("from", "", "sender agent name (env: TMUX_AGENT_NAME; auto-resolved from $TMUX_PANE if registered)")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	ctx := context.Background()
	sender, _, err := identity.Resolve(ctx, s, *from)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUnavailable)
	}
	if sender == "" {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table",
				os.Getenv("TMUX_PANE")), exitUnavailable)
	}

	return runRefreshAllMcpsWithStore(ctx, s, sender, *format, stdout, stderr)
}

// runRefreshAllMcpsWithStore is the testable core. Takes a resolved
// sender + an open store, iterates registered agents, calls
// doControl per agent with command=mcp-restart-tmux-msg, and
// renders the aggregated result.
func runRefreshAllMcpsWithStore(ctx context.Context, s *store.Store,
	sender, format string, stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("list agents: %v", err), exitInternal)
	}

	// Sort by name so the output is deterministic across runs +
	// operators reading the same output across deploys see the same
	// ordering rather than the table's storage order.
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })

	result := refreshResult{
		OK:     true,
		Sender: sender,
		Total:  len(agents),
		Agents: make([]refreshAgentEntry, 0, len(agents)),
	}

	// The mcp-restart-tmux-msg macro inserts a pair (2 control rows)
	// per agent, all from `sender`. With the default sender backlog
	// cap of 2, the second agent's fan-out would already trip the
	// cap before alice's first agent drained. Raise the sender cap
	// for THIS operation to the exact upper bound the fan-out needs:
	// 2 rows per agent, plus the normal sender cap headroom so that
	// a sender who entered the operation with `capSenderBacklog`
	// already in flight is still allowed (their normal traffic isn't
	// pre-empted by the bulk refresh).
	//
	// The per-recipient cap (capRecipientQueue) is NOT raised — each
	// agent's queue is still protected by the normal 5-slot ceiling.
	// If a agent is already over-capped (busy), its fan-out fails
	// with the correct cap-rejected error and the operator sees a
	// `failed` entry in the summary.
	//
	// This is operation-scoped cap-raising, not cap-exemption: nothing
	// here joins the `CapExemption` discipline-pin's commitment family
	// (#55 / ADR-0001). The bound is finite and exact.
	maxSenderForFanout := 2*len(agents) + capSenderBacklog

	for _, a := range agents {
		entry := refreshAgentEntry{Name: a.Name}
		ctrlRes, ctrlErr := doControl(ctx, s, controlParams{
			From:         sender,
			To:           a.Name,
			Command:      "mcp-restart-tmux-msg",
			MaxRecipient: capRecipientQueue,
			MaxSender:    maxSenderForFanout,
			MaxBody:      capBodyBytes,
		})
		if ctrlErr != nil {
			entry.OK = false
			entry.Error = ctrlErr.Error()
			result.Failed++
			// A single-agent failure (e.g., recipient queue full)
			// doesn't abort the fan-out — the operator likely wants
			// the other agents refreshed regardless. The summary
			// surfaces failed > 0 if any happened.
			result.OK = false
		} else {
			entry.OK = true
			entry.DisableID = ctrlRes.ID
			entry.EnableID = ctrlRes.EnableID
			result.Queued++
		}
		result.Agents = append(result.Agents, entry)
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, result)
	case "text", "":
		renderRefreshText(stdout, result)
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}

	if !result.OK {
		// Exit non-zero if any agent's fan-out failed so a script
		// invoking the command can detect partial success without
		// parsing the JSON.
		return exitInternal
	}
	return exitOK
}

// renderRefreshText emits an operator-readable summary + per-agent
// table to stdout. Mirrors the JSON shape's fields so the two outputs
// stay legible side-by-side during diagnostic comparisons.
func renderRefreshText(w io.Writer, r refreshResult) {
	fmt.Fprintf(w, "refresh-all-mcps from=%s total=%d queued=%d failed=%d\n",
		r.Sender, r.Total, r.Queued, r.Failed)
	if len(r.Agents) == 0 {
		fmt.Fprintln(w, "  (no agents registered)")
		return
	}
	for _, c := range r.Agents {
		if c.OK {
			fmt.Fprintf(w, "  %-16s ok disable=%s enable=%s\n",
				c.Name, c.DisableID, c.EnableID)
		} else {
			fmt.Fprintf(w, "  %-16s FAILED %s\n", c.Name, c.Error)
		}
	}
}

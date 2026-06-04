package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/identity"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// refreshResult is the structured return shape for the
// refresh-all-mcps subcommand. Per-chamber rows in `chambers` + a
// summary at the top so the operator can scan the count line without
// reading the table.
//
// The JSON tags drive the wire shape; the text formatter renders the
// same struct as an aligned table. Don't reconstruct this shape by
// hand in either path or the two outputs will drift.
type refreshResult struct {
	OK       bool                  `json:"ok"`
	Sender   string                `json:"sender"`
	Total    int                   `json:"total"`
	Queued   int                   `json:"queued"`
	Failed   int                   `json:"failed"`
	Chambers []refreshChamberEntry `json:"chambers"`
}

// refreshChamberEntry is one row of refreshResult.chambers — a single
// chamber's fan-out outcome. `DisableID` and `EnableID` are the public
// ids of the two control rows the mcp-restart-semaphore macro queues
// (see #28); they're present on success, absent on failure. `Error`
// is the inverse — present on failure, absent on success.
type refreshChamberEntry struct {
	Name      string `json:"name"`
	OK        bool   `json:"ok"`
	DisableID string `json:"disable_id,omitempty"`
	EnableID  string `json:"enable_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// runRefreshAllMcpsCLI parses the refresh-all-mcps flags and dispatches.
//
// Usage: claude-msg refresh-all-mcps [--format text|json]
//
// Convenience surface for the bulk version of `claude-msg control
// --to <chamber> --command mcp-restart-semaphore`. Iterates the
// registered agents table and fires the macro per chamber, then
// reports per-chamber success/failure + a summary line.
//
// Cap-protected via the existing 5/2 ceilings on the per-recipient
// queue and per-sender backlog — a runaway storm can't overwhelm any
// chamber. Operator-explicit only (no MCP tool variant; that would
// be a DoS-amplification class).
//
// What this v1 does NOT do (size/M follow-up triggers per #62):
//   - Does NOT gate on chamber-state via #69 — if mid-tool-call
//     disruption becomes recurring felt-pain, file the size/M
//     `state in [idle, awaiting-operator]` gating follow-up.
//   - Does NOT support `--except` / `--only` filtering — file when
//     a real use case surfaces.
//   - Does NOT expose an MCP tool — operator-only by design.
func runRefreshAllMcpsCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("refresh-all-mcps", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	from := fs.String("from", "", "sender agent name (env: CLAUDE_AGENT_NAME; auto-resolved from $TMUX_PANE if registered)")
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
			fmt.Sprintf("cannot resolve sender identity: set $CLAUDE_AGENT_NAME, or register this pane (TMUX_PANE=%s) in the agents table",
				os.Getenv("TMUX_PANE")), exitUnavailable)
	}

	return runRefreshAllMcpsWithStore(ctx, s, sender, *format, stdout, stderr)
}

// runRefreshAllMcpsWithStore is the testable core. Takes a resolved
// sender + an open store, iterates registered chambers, calls
// doControl per chamber with command=mcp-restart-semaphore, and
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
		OK:       true,
		Sender:   sender,
		Total:    len(agents),
		Chambers: make([]refreshChamberEntry, 0, len(agents)),
	}

	// The mcp-restart-semaphore macro inserts a pair (2 control rows)
	// per chamber, all from `sender`. With the default sender backlog
	// cap of 2, the second chamber's fan-out would already trip the
	// cap before alice's first chamber drained. Raise the sender cap
	// for THIS operation to the exact upper bound the fan-out needs:
	// 2 rows per chamber, plus the normal sender cap headroom so that
	// a sender who entered the operation with `capSenderBacklog`
	// already in flight is still allowed (their normal traffic isn't
	// pre-empted by the bulk refresh).
	//
	// The per-recipient cap (capRecipientQueue) is NOT raised — each
	// chamber's queue is still protected by the normal 5-slot ceiling.
	// If a chamber is already over-capped (busy), its fan-out fails
	// with the correct cap-rejected error and the operator sees a
	// `failed` entry in the summary.
	//
	// This is operation-scoped cap-raising, not cap-exemption: nothing
	// here joins the `CapExemption` discipline-pin's commitment family
	// (#55 / ADR-0001). The bound is finite and exact.
	maxSenderForFanout := 2*len(agents) + capSenderBacklog

	for _, a := range agents {
		entry := refreshChamberEntry{Name: a.Name}
		ctrlRes, ctrlErr := doControl(ctx, s, controlParams{
			From:         sender,
			To:           a.Name,
			Command:      "mcp-restart-semaphore",
			MaxRecipient: capRecipientQueue,
			MaxSender:    maxSenderForFanout,
			MaxBody:      capBodyBytes,
		})
		if ctrlErr != nil {
			entry.OK = false
			entry.Error = ctrlErr.Error()
			result.Failed++
			// A single-chamber failure (e.g., recipient queue full)
			// doesn't abort the fan-out — the operator likely wants
			// the other chambers refreshed regardless. The summary
			// surfaces failed > 0 if any happened.
			result.OK = false
		} else {
			entry.OK = true
			entry.DisableID = ctrlRes.ID
			entry.EnableID = ctrlRes.EnableID
			result.Queued++
		}
		result.Chambers = append(result.Chambers, entry)
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
		// Exit non-zero if any chamber's fan-out failed so a script
		// invoking the command can detect partial success without
		// parsing the JSON.
		return exitInternal
	}
	return exitOK
}

// renderRefreshText emits an operator-readable summary + per-chamber
// table to stdout. Mirrors the JSON shape's fields so the two outputs
// stay legible side-by-side during diagnostic comparisons.
func renderRefreshText(w io.Writer, r refreshResult) {
	fmt.Fprintf(w, "refresh-all-mcps from=%s total=%d queued=%d failed=%d\n",
		r.Sender, r.Total, r.Queued, r.Failed)
	if len(r.Chambers) == 0 {
		fmt.Fprintln(w, "  (no chambers registered)")
		return
	}
	for _, c := range r.Chambers {
		if c.OK {
			fmt.Fprintf(w, "  %-16s ok disable=%s enable=%s\n",
				c.Name, c.DisableID, c.EnableID)
		} else {
			fmt.Fprintf(w, "  %-16s FAILED %s\n", c.Name, c.Error)
		}
	}
}


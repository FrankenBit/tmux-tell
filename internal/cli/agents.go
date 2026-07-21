package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

// runAgentsCLI parses agents-subcommand flags and dispatches.
//
// Usage: tmux-tell-claude agents [--available] [--format text|json]
func runAgentsCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	available := fs.Bool("available", false,
		"only agents whose pane is live and aren't paused")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}

	// #722: agents is pure read (list-agents + live-pane probe);
	// OpenReadOnly keeps sandboxed callers unblocked. Fresh-install
	// callers get a clean "does not exist" error naming the writer-verb
	// remedy — the same shape a sandbox with no bus DB would hit anyway.
	s, err := store.OpenReadOnly(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

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
	// Metabolism surfaces the #621 chamber self-reported metabolism ("warming"
	// / "saturating" / "compact-pending"). A sibling chamber→operator signal to
	// AttentionState, on a distinct axis (intentional context-throughput, not
	// operator-action-pending). Empty (no self-report) omitted from JSON so
	// pre-#621 callers see no schema-shape change; the text formatter shows it
	// in the METABOLISM column ("-" when unset).
	Metabolism string `json:"metabolism,omitempty"`
	// MetabolismSetAt stamps when Metabolism was set (#621), so a listing
	// consumer can discount a stale self-report. Empty when there is no
	// self-report; JSON-wire only (not shown in the text table).
	MetabolismSetAt string `json:"metabolism_set_at,omitempty"`
	// Stuck surfaces the #291 mailman park reason ("pane-not-found" when the
	// mailman has stopped probing tmux for this agent after N consecutive
	// pane-probe failures). Empty (healthy) omitted from JSON so existing
	// callers see no schema-shape change.
	Stuck string `json:"stuck,omitempty"`
	// MailmanLastDelivered is the RFC3339 timestamp of the most recent delivery
	// to this agent (#348), derived from messages.delivered_at — NOT a stored
	// per-agent column (source-of-truth-derived, no delivery-hot-path write).
	// Empty when the mailman has never delivered within retained history; a
	// non-zero Queued + empty/old MailmanLastDelivered is the "queued but
	// mailman silent" divergence smell the operator can spot in one glance.
	// Also the field the #363/#366 ping-evidence block consumes.
	MailmanLastDelivered string `json:"mailman_last_delivered_at,omitempty"`
	// DeliveryMode mirrors agents.delivery_mode so callers can filter
	// hook-context agents out of mailman-related iteration without a
	// second lookup (#349 Fix 2: install.sh's bootstrap path skips
	// `systemctl --user enable` for hook-context agents). Emitted on the
	// JSON wire only; the text formatter is unchanged.
	DeliveryMode string `json:"delivery_mode,omitempty"`
	// DisplayName is the chamber-asserted display name (#556) — the
	// case-/space-preserved label set via set_pane_name. Empty omitted from
	// JSON so pre-#556 callers see no schema-shape change; the text formatter
	// shows it in the trailing DISPLAY column ("-" when unset).
	DisplayName string `json:"display_name,omitempty"`
	// PaneConflict flags that this row's pane_id is held by more than one agent
	// (#565) — the #549 duplicate-pane-row drift detect signal. #549 Fix-2a
	// prevents it at the register source, and since #595 the UNIQUE(pane_id)
	// index makes a duplicate a write-time impossibility — so this now fires only
	// on a LEGACY / index-absent duplicate (a pre-#595 DB before its heal
	// migration runs, or a future index drop by surgery/bug), not a live write
	// concern. Retained as the read-side backstop for exactly that unhappy path.
	// Omitted from JSON in the common no-conflict case (false) so the schema
	// shape is unchanged.
	PaneConflict bool `json:"pane_conflict,omitempty"`
}

// paneConflicts maps each pane_id held by more than one agent to the sorted
// names sharing it. Non-empty panes only; empty/NULL panes never conflict (a
// dormant pane-less row is the expected post-Fix-2a rebind state, #549). Pure
// over the agent set — the #565 duplicate-pane-row detect signal. Computed over
// the FULL agent list (before any --available-only filter) so a conflict whose
// stale participant is filtered out of the view is still detected and named.
func paneConflicts(agents []store.Agent) map[string][]string {
	byPane := map[string][]string{}
	for _, a := range agents {
		if a.PaneID == "" {
			continue
		}
		byPane[a.PaneID] = append(byPane[a.PaneID], a.Name)
	}
	out := map[string][]string{}
	for pane, names := range byPane {
		if len(names) > 1 {
			sort.Strings(names)
			out[pane] = names
		}
	}
	return out
}

// renderPaneConflictWarnings emits one operator-facing warning line per
// conflicted pane (#565), naming the sharers + the shared pane + a recovery
// hint. Deterministic order (panes sorted) so the output is stable.
func renderPaneConflictWarnings(w io.Writer, conflicts map[string][]string) {
	if len(conflicts) == 0 {
		return
	}
	panes := make([]string, 0, len(conflicts))
	for p := range conflicts {
		panes = append(panes, p)
	}
	sort.Strings(panes)
	for _, p := range panes {
		names := conflicts[p]
		fmt.Fprintf(w, "⚠ pane %s shared by %d agents: %s — likely #549 duplicate-pane-row drift; `unregister --name <stale>` the dormant one (or re-register to supersede)\n",
			p, len(names), strings.Join(names, ", "))
	}
}

// mailmanIdleHuman renders the agents-listing MAILMAN column (#348): a compact
// "how long since this mailman last delivered" so the operator can eyeball the
// "queued but silent" divergence smell. "never" when there's no delivery in
// retained history; a best-effort raw echo if the stamp doesn't parse.
func mailmanIdleHuman(last string, now time.Time) string {
	if last == "" {
		return "never"
	}
	t, err := time.Parse(time.RFC3339Nano, last) // store stamps fractional seconds
	if err != nil {
		return last
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// metabolismCell renders the agents-table METABOLISM column: the chamber's
// self-reported state prefixed with its #621 legend emoji (from
// store.MetabolismEmoji, the single-source legend), or "-" when there is no
// active self-report. The table is tab-joined (renderTextTable), so the
// double-width emoji does not disturb column alignment.
func metabolismCell(m string) string {
	if m == "" {
		return "-"
	}
	if e, ok := store.MetabolismEmoji[m]; ok {
		return e + " " + m
	}
	return m
}

func runAgentsWithStore(ctx context.Context, s *store.Store,
	live map[string]bool, availableOnly bool, format string,
	stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	conflicts := paneConflicts(agents)
	rows := make([]agentView, 0, len(agents))
	for _, a := range agents {
		v := agentView{
			Name:            a.Name,
			Pane:            a.PaneID,
			Paused:          a.Paused,
			AttentionState:  a.AttentionState,
			Metabolism:      a.Metabolism,
			MetabolismSetAt: a.MetabolismSetAt,
			Stuck:           a.StuckReason,
			DeliveryMode:    a.DeliveryMode,
			DisplayName:     a.DisplayName,
			PaneConflict:    len(conflicts[a.PaneID]) > 0,
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

		if last, ok, err := s.RecipientLastDelivered(ctx, a.Name); err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		} else if ok {
			v.MailmanLastDelivered = last
		}

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
		now := time.Now()
		header := []string{"NAME", "PANE", "STATUS", "PAUSED", "QUEUED", "ATTENTION", "METABOLISM", "STUCK", "MAILMAN", "DISPLAY"}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			pane := r.Pane
			if pane == "" {
				pane = "-"
			}
			if r.PaneConflict {
				pane += " ⚠" // #565: this pane_id is held by >1 agent
			}
			attention := r.AttentionState
			if attention == "" {
				attention = "idle"
			}
			metabolism := metabolismCell(r.Metabolism)
			stuck := r.Stuck
			if stuck == "" {
				stuck = "-"
			}
			display := r.DisplayName
			if display == "" {
				display = "-"
			}
			out = append(out, []string{
				r.Name, pane, r.PaneStatus, yesNo(r.Paused), itoa(r.Queued), attention, metabolism, stuck,
				mailmanIdleHuman(r.MailmanLastDelivered, now), display,
			})
		}
		renderTextTable(stdout, header, out)
		renderPaneConflictWarnings(stdout, conflicts)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

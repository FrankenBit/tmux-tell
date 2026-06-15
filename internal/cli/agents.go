package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	available := fs.Bool("available", false,
		"only agents whose pane is live and aren't paused")
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

func runAgentsWithStore(ctx context.Context, s *store.Store,
	live map[string]bool, availableOnly bool, format string,
	stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	rows := make([]agentView, 0, len(agents))
	for _, a := range agents {
		v := agentView{
			Name:           a.Name,
			Pane:           a.PaneID,
			Paused:         a.Paused,
			AttentionState: a.AttentionState,
			Stuck:          a.StuckReason,
			DeliveryMode:   a.DeliveryMode,
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
		header := []string{"NAME", "PANE", "STATUS", "PAUSED", "QUEUED", "ATTENTION", "STUCK", "MAILMAN"}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			pane := r.Pane
			if pane == "" {
				pane = "-"
			}
			attention := r.AttentionState
			if attention == "" {
				attention = "idle"
			}
			stuck := r.Stuck
			if stuck == "" {
				stuck = "-"
			}
			out = append(out, []string{
				r.Name, pane, r.PaneStatus, yesNo(r.Paused), itoa(r.Queued), attention, stuck,
				mailmanIdleHuman(r.MailmanLastDelivered, now),
			})
		}
		renderTextTable(stdout, header, out)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

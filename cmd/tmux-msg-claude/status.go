package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/healthscan"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runStatusCLI parses status-subcommand flags and dispatches.
//
// Usage: tmux-msg-claude status [--format text|json] [--today]
func runStatusCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "text", "text|json")
	today := fs.Bool("today", false,
		"include a per-agent today-block (deliveries / unverified / failed / crashes / cap-exceeded counts since 00:00 local) sourced from journalctl + systemd (#45)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runStatusWithStore(context.Background(), s, *format, *today, stdout, stderr)
}

// agentStatus is the per-agent summary status reports.
type agentStatus struct {
	Name            string `json:"name"`
	Paused          bool   `json:"paused"`
	Queued          int    `json:"queued"`
	Delivering      int    `json:"delivering"`
	Delivered       int    `json:"delivered"`
	Failed          int    `json:"failed"`
	OldestQueuedAge string `json:"oldest_queued_age,omitempty"` // "-" if no queued

	// Today is populated when --today is passed. Sourced from
	// journalctl + systemd via the healthscan package. Pointer so
	// JSON output cleanly omits it when not requested.
	Today *healthscan.AgentHealth `json:"today,omitempty"`
}

func runStatusWithStore(ctx context.Context, s *store.Store,
	format string, includeToday bool, stdout, stderr io.Writer,
) int {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	rows := make([]agentStatus, 0, len(agents))
	for _, a := range agents {
		st := agentStatus{Name: a.Name, Paused: a.Paused}
		for _, state := range []store.State{
			store.StateQueued, store.StateDelivering,
			store.StateDelivered, store.StateFailed,
		} {
			// Quick aggregate query per state. For ~5-20 agents this is
			// negligible; if we grow to many agents we can swap in one
			// GROUP BY query.
			msgs, err := s.ListMessages(ctx, store.ListFilter{
				ToAgent: a.Name, State: state, Limit: 1000,
			})
			if err != nil {
				return writeJSONError(stdout, stderr, err.Error(), exitInternal)
			}
			switch state {
			case store.StateQueued:
				st.Queued = len(msgs)
				if len(msgs) > 0 {
					// messages come back ordered by id ASC, so msgs[0] is
					// the oldest queued for this agent.
					st.OldestQueuedAge = ageOf(msgs[0].CreatedAt)
				} else {
					st.OldestQueuedAge = "-"
				}
			case store.StateDelivering:
				st.Delivering = len(msgs)
			case store.StateDelivered:
				st.Delivered = len(msgs)
			case store.StateFailed:
				st.Failed = len(msgs)
			}
		}
		rows = append(rows, st)
	}

	// Today block (#45): augment each row with healthscan data sourced
	// from journalctl + systemd over the since-midnight window.
	if includeToday {
		names := make([]string, len(rows))
		for i, r := range rows {
			names[i] = r.Name
		}
		scanner := healthscan.New()
		todayBlock, err := scanner.Scan(ctx, names, healthscan.SinceMidnight(time.Now()))
		if err != nil {
			// External-source failure shouldn't kill the core status
			// table. Surface a non-fatal warning + proceed without
			// the today block.
			fmt.Fprintf(stderr, "warn: --today scan failed: %v\n", err)
		} else if len(todayBlock) == len(rows) {
			for i := range rows {
				th := todayBlock[i]
				rows[i].Today = &th
			}
		}
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, rows)
		return exitOK
	case "text", "":
		header := []string{"NAME", "PAUSED", "QUEUED", "DELIVERING", "DELIVERED", "FAILED", "OLDEST"}
		out := make([][]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, []string{
				r.Name,
				yesNo(r.Paused),
				itoa(r.Queued),
				itoa(r.Delivering),
				itoa(r.Delivered),
				itoa(r.Failed),
				r.OldestQueuedAge,
			})
		}
		renderTextTable(stdout, header, out)
		if includeToday {
			renderTodayBlock(stdout, rows)
		}
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

// renderTodayBlock prints the journalctl-sourced per-agent today
// counts (#45). Skipped silently when no row has a Today field set
// (scan failed; warning already emitted to stderr).
func renderTodayBlock(stdout io.Writer, rows []agentStatus) {
	hasAny := false
	for _, r := range rows {
		if r.Today != nil {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return
	}
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "TODAY (since 00:00 local):")
	header := []string{"NAME", "DELIVERED", "UNVERIFIED", "FAILED", "CAPHIT", "CRASHES", "P50ms", "P99ms"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		if r.Today == nil {
			out = append(out, []string{r.Name, "-", "-", "-", "-", "-", "-", "-"})
			continue
		}
		out = append(out, []string{
			r.Name,
			itoa(r.Today.Delivered),
			itoa(r.Today.DeliveredInInputBox),
			itoa(r.Today.Failed),
			itoa(r.Today.QuietCapExceeded),
			itoa(r.Today.CrashCount),
			itoa(r.Today.DeliverP50Ms),
			itoa(r.Today.DeliverP99Ms),
		})
	}
	renderTextTable(stdout, header, out)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

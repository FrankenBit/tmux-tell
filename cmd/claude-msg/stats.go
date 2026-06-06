package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// runStatsCLI parses stats-subcommand flags and dispatches.
//
// Usage: claude-msg stats [--window all|<N>d|1h|24h] [--agent NAME]
//
//	[--pair [--top N]] [--format text|json]
//
// On-demand bus-traffic aggregates from the local DB (#147). The continuous
// observability stack (#146) covers dashboard trends; this is the in-terminal
// "show me right now" surface. Aggregation lives in internal/store (the
// reusable seam #161 `digest` consumes).
func runStatsCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "text", "text|json")
	window := fs.String("window", "24h", "time window: all | <N>d | a duration like 1h/24h")
	agent := fs.String("agent", "", "scope the per-agent + pairs view to one agent (sender or recipient)")
	pair := fs.Bool("pair", false, "show the top sender→recipient pairs")
	top := fs.Int("top", 10, "with --pair: number of pairs to show (0 = all)")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	w, err := parseWindow(*window, time.Now())
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitUsage)
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr, fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close()

	return runStatsWithStore(context.Background(), s, w, *window, *format, *agent, *pair, *top, stdout, stderr)
}

type statsResult struct {
	Window   string            `json:"window"`
	Agents   []store.AgentStat `json:"agents"`
	TopPairs []store.PairStat  `json:"top_pairs,omitempty"`
	Totals   store.Totals      `json:"totals"`
}

func runStatsWithStore(ctx context.Context, s *store.Store, w store.StatsWindow,
	windowSpec, format, agent string, pair bool, top int, stdout, stderr io.Writer,
) int {
	agents, err := s.StatsPerAgent(ctx, w)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	totals, err := s.StatsTotals(ctx, w)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	if agent != "" {
		filtered := agents[:0]
		for _, a := range agents {
			if a.Agent == agent {
				filtered = append(filtered, a)
			}
		}
		agents = filtered
	}

	var pairs []store.PairStat
	if pair {
		// Fetch all pairs (limit applied after the optional agent filter so
		// --agent --top N means "top N pairs touching this agent").
		all, err := s.StatsTopPairs(ctx, w, 0)
		if err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		for _, p := range all {
			if agent == "" || p.From == agent || p.To == agent {
				pairs = append(pairs, p)
			}
		}
		if top > 0 && len(pairs) > top {
			pairs = pairs[:top]
		}
	}

	res := statsResult{Window: windowSpec, Agents: agents, TopPairs: pairs, Totals: totals}

	if format == "json" {
		if err := writeJSONResult(stdout, res); err != nil {
			return writeJSONError(stdout, stderr, err.Error(), exitInternal)
		}
		return exitOK
	}
	renderStatsText(stdout, res, pair)
	return exitOK
}

func renderStatsText(w io.Writer, res statsResult, showPairs bool) {
	fmt.Fprintf(w, "Bus traffic — window %s\n\n", res.Window)

	header := []string{"AGENT", "SENT", "RECEIVED", "DELIVERED", "FAILED", "QUEUED", "P50"}
	rows := make([][]string, 0, len(res.Agents))
	for _, a := range res.Agents {
		rows = append(rows, []string{
			a.Agent, itoa(a.Sent), itoa(a.Received), itoa(a.Delivered),
			itoa(a.Failed), itoa(a.Queued), fmtLatency(a.P50LatencyMs),
		})
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, "  (no traffic in window)")
	} else {
		renderTextTable(w, header, rows)
	}

	if showPairs {
		fmt.Fprintln(w, "\nTop pairs:")
		if len(res.TopPairs) == 0 {
			fmt.Fprintln(w, "  (none)")
		} else {
			prows := make([][]string, 0, len(res.TopPairs))
			for _, p := range res.TopPairs {
				prows = append(prows, []string{p.From + " → " + p.To, itoa(p.Count), fmtLatency(p.P50LatencyMs)})
			}
			renderTextTable(w, []string{"PAIR", "COUNT", "P50"}, prows)
		}
	}

	t := res.Totals
	fmt.Fprintf(w, "\nTotals: %d messages — delivered %d, failed %d, queued %d",
		t.Total, t.Delivered, t.Failed, t.Queued)
	if t.Delivering > 0 {
		fmt.Fprintf(w, ", delivering %d", t.Delivering)
	}
	fmt.Fprintln(w)
	// The verified/unverified split is not DB-derivable (both are state=delivered;
	// see #169). Operators wanting it use `status --today` / `health` (journal-sourced).
	fmt.Fprintln(w, "(verified/unverified split: see `status --today` — not DB-queryable yet, #169)")
}

// fmtLatency renders a millisecond latency for the text table: "-" for none,
// "<N>ms" under a second, "<N.N>s" at or above.
func fmtLatency(ms int) string {
	if ms <= 0 {
		return "-"
	}
	if ms >= 1000 {
		return fmt.Sprintf("%.1fs", float64(ms)/1000.0)
	}
	return fmt.Sprintf("%dms", ms)
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/healthscan"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

// runHealthCLI parses health-subcommand flags and dispatches (#42).
//
// Usage: claude-msg health [--since DURATION] [--format text|json] [AGENT...]
//
// Scans journalctl + systemd for the configured window and reports
// per-agent operational health: delivery counts, soft-failure WARN
// rates, drift signals, crash counters, deliver-time percentiles.
//
// Without AGENT arguments, scans every registered agent. With AGENT
// args, scans only those.
func runHealthCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	format := fs.String("format", "text", "text|json")
	since := fs.Duration("since", 24*time.Hour,
		"scan the journal over this duration (e.g., 1h, 6h, 24h). Default 24h.")
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
	var names []string
	if fs.NArg() > 0 {
		names = fs.Args()
	} else {
		// Default: every registered agent.
		agents, err := s.ListAgents(ctx)
		if err != nil {
			return writeJSONError(stdout, stderr,
				fmt.Sprintf("list agents: %v", err), exitInternal)
		}
		for _, a := range agents {
			names = append(names, a.Name)
		}
	}
	if len(names) == 0 {
		return writeJSONError(stdout, stderr,
			"no agents to scan; pass AGENT args or register at least one",
			exitUsage)
	}

	scanner := healthscan.New()
	out, err := scanner.Scan(ctx, names,
		healthscan.SinceDuration(time.Now(), *since))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("health scan: %v", err), exitInternal)
	}

	switch *format {
	case "json":
		_ = writeJSONResult(stdout, out)
		return exitOK
	case "text", "":
		renderHealthText(stdout, out, *since)
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", *format), exitUsage)
	}
}

// renderHealthText prints the human-readable health report (#42).
// Highlights non-zero soft-failure counters so operator eye gets
// drawn to actionable rows.
func renderHealthText(stdout io.Writer, rows []healthscan.AgentHealth, window time.Duration) {
	fmt.Fprintf(stdout, "HEALTH SCAN (last %s)\n\n", window)
	header := []string{
		"NAME", "DELIVERED", "UNVERIFIED", "FAILED", "CAPHIT",
		"DRIFT_AMB", "DRIFT_UNREC", "CRASHES", "P50ms", "P95ms", "P99ms",
	}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.Name,
			itoa(r.Delivered),
			itoa(r.DeliveredUnverified),
			itoa(r.Failed),
			itoa(r.QuietCapExceeded),
			itoa(r.DriftAmbiguous),
			itoa(r.DriftUnrecoverable),
			itoa(r.CrashCount),
			itoa(r.DeliverP50Ms),
			itoa(r.DeliverP95Ms),
			itoa(r.DeliverP99Ms),
		})
	}
	renderTextTable(stdout, header, out)

	// Surface any actionable signals.
	var notes []string
	for _, r := range rows {
		if r.DeliveredUnverified > 0 {
			notes = append(notes, fmt.Sprintf(
				"  %s — %d delivered_unverified (Claude was likely mid-turn; messages sit in input box pending submit)",
				r.Name, r.DeliveredUnverified))
		}
		if r.Failed > 0 {
			notes = append(notes,
				fmt.Sprintf("  %s — %d delivery failures (grep journalctl for details)",
					r.Name, r.Failed))
		}
		if r.QuietCapExceeded > 0 {
			notes = append(notes, fmt.Sprintf(
				"  %s — %d quiet_cap_exceeded (probe-and-watch hit MaxWait; rare post-#52 unless operator was typing the full window)",
				r.Name, r.QuietCapExceeded))
		}
		if r.DriftAmbiguous > 0 || r.DriftUnrecoverable > 0 {
			notes = append(notes, fmt.Sprintf(
				"  %s — drift events (%d ambiguous, %d unrecoverable); use the WARN-line recipe to disambiguate",
				r.Name, r.DriftAmbiguous, r.DriftUnrecoverable))
		}
		if r.CrashCount > 5 {
			notes = append(notes,
				fmt.Sprintf("  %s — %d crashes (review systemd state + watchdog timing)",
					r.Name, r.CrashCount))
		}
	}
	if len(notes) > 0 {
		fmt.Fprintln(stdout, "\nNOTES:")
		for _, n := range notes {
			fmt.Fprintln(stdout, n)
		}
	}
}

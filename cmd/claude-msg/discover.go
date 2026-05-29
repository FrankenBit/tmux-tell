package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/tmuxio"
)

// cmdlineReader is the swappable hatch for tests. Production reads from
// /proc/<pid>/cmdline; tests inject a fake.
var cmdlineReader = func(pid int) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runDiscoverCLI parses discover flags and dispatches.
//
// Usage: claude-msg discover [--dry-run] [--format text|json]
func runDiscoverCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: CLAUDE_MSG_DB)")
	dryRun := fs.Bool("dry-run", false, "print proposed updates without writing")
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

	return runDiscoverWithStore(context.Background(), s, *dryRun, *format, stdout, stderr)
}

// discoverResult is the per-agent outcome of a discovery pass.
type discoverResult struct {
	Name      string `json:"name"`
	NewPaneID string `json:"new_pane_id,omitempty"`
	OldPaneID string `json:"old_pane_id,omitempty"`
	Status    string `json:"status"` // updated | unchanged | new | missing
}

func runDiscoverWithStore(ctx context.Context, s *store.Store,
	dryRun bool, format string,
	stdout, stderr io.Writer,
) int {
	panes, err := tmuxio.ListPanesWithPID(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}

	// 1. Walk panes, extract --resume name → pane_id.
	found := map[string]string{} // agent → pane_id
	for _, p := range panes {
		cmdline, err := cmdlineReader(p.PID)
		if err != nil {
			continue // process died between list and read; ignore.
		}
		argv := parseCmdline(cmdline)
		name := extractResumeName(argv)
		if name == "" {
			continue
		}
		// First pane wins on duplicate names — the operator's panes are
		// expected to be unique, but defensive coding doesn't hurt.
		if _, dup := found[name]; !dup {
			found[name] = p.ID
		}
	}

	// 2. Diff against the registry: every existing agent → status; new
	// names found in tmux → also reported.
	existing, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	seenAgent := map[string]bool{}
	var results []discoverResult
	for _, a := range existing {
		seenAgent[a.Name] = true
		newPane, here := found[a.Name]
		switch {
		case !here:
			results = append(results, discoverResult{
				Name: a.Name, OldPaneID: a.PaneID, Status: "missing",
			})
			fmt.Fprintf(stderr, "warn: agent %q has pane_id=%q but no current pane matches\n",
				a.Name, a.PaneID)
		case newPane == a.PaneID:
			results = append(results, discoverResult{
				Name: a.Name, NewPaneID: newPane, OldPaneID: a.PaneID, Status: "unchanged",
			})
		default:
			results = append(results, discoverResult{
				Name: a.Name, NewPaneID: newPane, OldPaneID: a.PaneID, Status: "updated",
			})
			if !dryRun {
				if err := s.UpsertAgent(ctx, a.Name, newPane); err != nil {
					return writeJSONError(stdout, stderr, err.Error(), exitInternal)
				}
			}
		}
	}
	for name, pane := range found {
		if seenAgent[name] {
			continue
		}
		results = append(results, discoverResult{
			Name: name, NewPaneID: pane, Status: "new",
		})
		if !dryRun {
			if err := s.UpsertAgent(ctx, name, pane); err != nil {
				return writeJSONError(stdout, stderr, err.Error(), exitInternal)
			}
		}
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, results)
		return exitOK
	case "text", "":
		header := []string{"NAME", "STATUS", "OLD_PANE", "NEW_PANE"}
		rows := make([][]string, 0, len(results))
		for _, r := range results {
			rows = append(rows, []string{
				r.Name, r.Status, dashIfEmpty(r.OldPaneID), dashIfEmpty(r.NewPaneID),
			})
		}
		renderTextTable(stdout, header, rows)
		if dryRun {
			fmt.Fprintln(stderr, "(--dry-run: no changes written)")
		}
		return exitOK
	default:
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("unknown --format: %s", format), exitUsage)
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// parseCmdline splits a /proc/<pid>/cmdline string into argv. The kernel
// uses NUL separators with a trailing NUL.
func parseCmdline(raw string) []string {
	raw = strings.TrimRight(raw, "\x00")
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\x00")
}

// extractResumeName recovers the value passed to `--resume <name>` in argv,
// supporting both `--resume name` and `--resume=name`. When the name was
// passed unquoted with embedded spaces (e.g. `--resume Master Bosun of
// Nimbus`), the shell already split it into separate argv tokens; we
// collect those tokens until the next `--<flag>` and rejoin with spaces.
//
// Returns "" if the argv doesn't contain a --resume flag.
func extractResumeName(argv []string) string {
	for i, a := range argv {
		if strings.HasPrefix(a, "--resume=") {
			return strings.TrimPrefix(a, "--resume=")
		}
		if a == "--resume" {
			var parts []string
			for j := i + 1; j < len(argv); j++ {
				if strings.HasPrefix(argv[j], "--") || strings.HasPrefix(argv[j], "-") {
					break
				}
				parts = append(parts, argv[j])
			}
			return strings.Join(parts, " ")
		}
	}
	return ""
}

package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"git.frankenbit.de/frankenbit/cli-semaphore/internal/discover"
	"git.frankenbit.de/frankenbit/cli-semaphore/internal/store"
)

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

	return runDiscoverWithStore(context.Background(), s,
		discover.New(), *dryRun, *format, stdout, stderr)
}

// discoverResult is the per-agent outcome of a discovery pass.
type discoverResult struct {
	Name      string `json:"name"`
	NewPaneID string `json:"new_pane_id,omitempty"`
	OldPaneID string `json:"old_pane_id,omitempty"`
	Source    string `json:"source,omitempty"` // cmdline | pane_title | window_name
	Status    string `json:"status"`           // updated | unchanged | new | missing
}

func runDiscoverWithStore(ctx context.Context, s *store.Store,
	walker *discover.Walker, dryRun bool, format string,
	stdout, stderr io.Writer,
) int {
	resolved, err := walker.WalkAll(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	// agent name → resolved
	found := map[string]discover.Resolved{}
	for _, r := range resolved {
		if _, dup := found[r.AgentName]; !dup {
			found[r.AgentName] = r
		}
	}

	existing, err := s.ListAgents(ctx)
	if err != nil {
		return writeJSONError(stdout, stderr, err.Error(), exitInternal)
	}
	seenAgent := map[string]bool{}
	var results []discoverResult

	for _, a := range existing {
		seenAgent[a.Name] = true
		r, here := found[a.Name]
		switch {
		case !here:
			results = append(results, discoverResult{
				Name: a.Name, OldPaneID: a.PaneID, Status: "missing",
			})
			fmt.Fprintf(stderr, "warn: agent %q has pane_id=%q but no current pane matches\n",
				a.Name, a.PaneID)
		case r.PaneID == a.PaneID:
			results = append(results, discoverResult{
				Name:      a.Name,
				NewPaneID: r.PaneID,
				OldPaneID: a.PaneID,
				Source:    string(r.Source),
				Status:    "unchanged",
			})
		default:
			results = append(results, discoverResult{
				Name:      a.Name,
				NewPaneID: r.PaneID,
				OldPaneID: a.PaneID,
				Source:    string(r.Source),
				Status:    "updated",
			})
			if !dryRun {
				if err := s.UpsertAgent(ctx, a.Name, r.PaneID); err != nil {
					return writeJSONError(stdout, stderr, err.Error(), exitInternal)
				}
			}
		}
	}
	for name, r := range found {
		if seenAgent[name] {
			continue
		}
		results = append(results, discoverResult{
			Name:      name,
			NewPaneID: r.PaneID,
			Source:    string(r.Source),
			Status:    "new",
		})
		if !dryRun {
			if err := s.UpsertAgent(ctx, name, r.PaneID); err != nil {
				return writeJSONError(stdout, stderr, err.Error(), exitInternal)
			}
		}
	}

	switch format {
	case "json":
		_ = writeJSONResult(stdout, results)
		return exitOK
	case "text", "":
		header := []string{"NAME", "STATUS", "OLD_PANE", "NEW_PANE", "SOURCE"}
		rows := make([][]string, 0, len(results))
		for _, r := range results {
			rows = append(rows, []string{
				r.Name, r.Status,
				dashIfEmpty(r.OldPaneID), dashIfEmpty(r.NewPaneID),
				dashIfEmpty(r.Source),
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

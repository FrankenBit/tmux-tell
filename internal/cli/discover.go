package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/discover"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// runDiscoverCLI parses discover flags and dispatches.
//
// Usage: tmux-tell-claude discover [--dry-run] [--apply-aliases] [--format text|json]
func runDiscoverCLI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", "", "path to messages.db (env: TMUX_TELL_DB)")
	dryRun := fs.Bool("dry-run", false, "print proposed updates without writing")
	applyAliases := fs.Bool("apply-aliases", false,
		"detect long --resume values that overlap with existing canonicals and ADD them as aliases instead of creating new rows (#46). Without this flag, propose-only — output a 'PROPOSED ALIAS' note but make no changes.")
	format := fs.String("format", "text", "text|json")
	if err := fs.Parse(reorderFlagsFirst(fs, args)); err != nil {
		return exitUsage
	}

	s, err := store.Open(resolveDBPath(*dbPath))
	if err != nil {
		return writeJSONError(stdout, stderr,
			fmt.Sprintf("open store: %v", err), exitInternal)
	}
	defer s.Close() //nolint:errcheck // best-effort close

	return runDiscoverWithStore(context.Background(), s,
		discover.New(), *dryRun, *applyAliases, *format, stdout, stderr)
}

// discoverResult is the per-agent outcome of a discovery pass.
type discoverResult struct {
	Name      string `json:"name"`
	NewPaneID string `json:"new_pane_id,omitempty"`
	OldPaneID string `json:"old_pane_id,omitempty"`
	Source    string `json:"source,omitempty"` // cmdline | pane_title | window_name
	Status    string `json:"status"`           // updated | unchanged | new | missing | alias_proposed | alias_added
	// AliasOf, when non-empty, names the existing canonical agent that
	// this discovered (long) name should be aliased onto rather than
	// landing as a new row. Populated when status is alias_proposed or
	// alias_added. Closes #46 — the discover flow no longer duplicates
	// when long --resume names overlap with canonical short names.
	AliasOf string `json:"alias_of,omitempty"`
}

// findCanonicalForAlias returns the canonical agent name whose short
// name appears as a case-insensitive whitespace-bounded substring of
// the discovered long name. Returns "" if no canonical matches or if
// the match would be ambiguous (multiple canonicals).
//
// The canonical's name must be a "whole word" inside the discovered
// name — embedded substring matches (e.g., canonical "ai" inside
// "Pair") are rejected to avoid false positives.
//
// canonicals is the list of agents already registered in the store
// that have a non-empty short canonical name. discoveredName is the
// raw --resume value the walker found.
func findCanonicalForAlias(canonicals []store.Agent, discoveredName string) string {
	if discoveredName == "" {
		return ""
	}
	tokens := tokenizeForAliasMatch(discoveredName)
	var matches []string
	for _, c := range canonicals {
		if c.Name == discoveredName {
			continue // exact match isn't an alias situation
		}
		// Match a canonical whose name is one of the discovered name's
		// whitespace-bounded tokens (case-insensitive).
		for _, tok := range tokens {
			if strings.EqualFold(tok, c.Name) {
				matches = append(matches, c.Name)
				break
			}
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// tokenizeForAliasMatch splits a discovered name into matchable
// whitespace-bounded tokens, lower-cased.
func tokenizeForAliasMatch(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(f))
	}
	return out
}

func runDiscoverWithStore(ctx context.Context, s *store.Store,
	walker *discover.Walker, dryRun, applyAliases bool, format string,
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
		// #46: before creating a new row for this long-name agent,
		// check whether the long name is "X-something-Y" where X is
		// already a registered canonical short name. If so, the
		// discovered name should be ADDED as an alias rather than
		// landing as a brand new row.
		if canonical := findCanonicalForAlias(existing, name); canonical != "" {
			status := "alias_proposed"
			if applyAliases && !dryRun {
				if err := s.AddAlias(ctx, canonical, name); err != nil {
					return writeJSONError(stdout, stderr,
						fmt.Sprintf("add alias %q on %q: %v",
							name, canonical, err), exitInternal)
				}
				// Also bind the canonical's pane_id to the discovered
				// pane so future deliveries land on the right pane.
				if err := s.UpsertAgent(ctx, canonical, r.PaneID); err != nil {
					return writeJSONError(stdout, stderr, err.Error(), exitInternal)
				}
				status = "alias_added"
			}
			results = append(results, discoverResult{
				Name:      name,
				NewPaneID: r.PaneID,
				Source:    string(r.Source),
				Status:    status,
				AliasOf:   canonical,
			})
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
		header := []string{"NAME", "STATUS", "OLD_PANE", "NEW_PANE", "SOURCE", "ALIAS_OF"}
		rows := make([][]string, 0, len(results))
		hasAliasProposal := false
		for _, r := range results {
			rows = append(rows, []string{
				r.Name, r.Status,
				dashIfEmpty(r.OldPaneID), dashIfEmpty(r.NewPaneID),
				dashIfEmpty(r.Source), dashIfEmpty(r.AliasOf),
			})
			if r.Status == "alias_proposed" {
				hasAliasProposal = true
			}
		}
		renderTextTable(stdout, header, rows)
		if hasAliasProposal && !applyAliases {
			fmt.Fprintln(stderr, "(re-run with --apply-aliases to ADD long names as aliases on the listed canonicals; #46)")
		}
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

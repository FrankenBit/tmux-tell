// changelog-assemble implements the fragment-per-PR CHANGELOG pattern (#494).
//
// The single `## [Unreleased]` section in CHANGELOG.md is a structural
// serialization point: every change-carrying PR edits the same lines, so
// parallel PRs in a milestone collide (v0.18.1 #480/#486; v0.17.2 #464). The
// fix is to stop touching CHANGELOG.md during the PR cycle — each PR drops a
// fragment file in changelog.d/ — and assemble the fragments into [Unreleased]
// at release-prep time, just before release.yml's existing CHANGELOG transition.
//
// Substrate-fit: this mirrors tools/check-pin-slugs/ — a small Go program run
// via `go run ./tools/changelog-assemble` in CI/release, no external binary
// dependency. It fills only the `### Type` bullet blocks in [Unreleased] and
// leaves the hand-curated per-release prelude + Headlines (the surface
// release-draft.yml extracts, #427) untouched.
//
// Modes:
//
//	(default)   assemble fragments into [Unreleased] (mutating)
//	-prune      after a successful assemble, delete the consumed fragment files
//	-check      read-only: validate every fragment's name + type + non-emptiness;
//	            no mutation. Run in CI (test.yml).
//
// Flags:
//
//	-changelog  path to CHANGELOG.md            (default "CHANGELOG.md")
//	-dir        path to the fragments directory (default "changelog.d")
//
// Exit codes:
//
//	0 — success (assembled, or check passed, or nothing to do).
//	1 — check found a malformed fragment, or assemble failed on bad input.
//	2 — internal error (couldn't read/write a file).
//
// Run via `go run ./tools/changelog-assemble` (assemble) or
// `go run ./tools/changelog-assemble -check` (CI lint).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	exitOK  = 0
	exitBad = 1
	exitErr = 2
)

// typeOrder is the canonical emit order — Keep a Changelog's six categories plus
// the project's `Documentation` (matching the historical `### ` headings). A
// fragment whose type is not in this set is rejected by -check.
var typeOrder = []string{
	"added", "changed", "deprecated", "removed", "fixed", "security", "documentation",
}

// typeHeading maps a canonical lowercase type to its `### ` heading word.
var typeHeading = map[string]string{
	"added":         "Added",
	"changed":       "Changed",
	"deprecated":    "Deprecated",
	"removed":       "Removed",
	"fixed":         "Fixed",
	"security":      "Security",
	"documentation": "Documentation",
}

// fragmentNameRE matches `<issue>[.<seq>].<type>` (the base name without the
// `.md` extension). <issue> and the optional <seq> discriminator are digits;
// <type> is validated against typeHeading separately so an unknown type yields
// a type-specific diagnostic rather than a generic name-parse failure.
var fragmentNameRE = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?\.([a-z]+)$`)

// Fragment is one parsed changelog.d/ entry.
type Fragment struct {
	Issue int    // leading issue/PR number
	Seq   int    // optional same-(issue,type) discriminator; 0 when absent
	Type  string // canonical lowercase type
	Body  string // verbatim entry markdown (trailing whitespace trimmed)
	File  string // source path (for -prune + diagnostics)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("changelog-assemble", flag.ContinueOnError)
	fs.SetOutput(stderr)
	check := fs.Bool("check", false, "validate fragments only; no mutation (CI lint)")
	prune := fs.Bool("prune", false, "delete consumed fragment files after assembling")
	changelogPath := fs.String("changelog", "CHANGELOG.md", "path to CHANGELOG.md")
	dir := fs.String("dir", "changelog.d", "path to the fragments directory")
	if err := fs.Parse(args); err != nil {
		return exitErr
	}

	frags, badNames, err := loadFragments(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "changelog-assemble: read %s: %v\n", *dir, err)
		return exitErr
	}

	if *check {
		if len(badNames) == 0 {
			fmt.Fprintf(stdout, "changelog-assemble: %d fragment(s) in %s, all well-formed\n", len(frags), *dir)
			return exitOK
		}
		fmt.Fprintf(stderr, "changelog-assemble: %d malformed fragment(s) in %s:\n", len(badNames), *dir)
		for _, b := range badNames {
			fmt.Fprintf(stderr, "  - %s\n", b)
		}
		fmt.Fprintf(stderr, "expected `<issue>[.<seq>].<type>.md` with <type> ∈ %v\n", typeOrder)
		return exitBad
	}

	// assemble mode: a malformed fragment is a hard error (don't silently skip).
	if len(badNames) > 0 {
		fmt.Fprintf(stderr, "changelog-assemble: refusing to assemble with %d malformed fragment(s) (run -check):\n", len(badNames))
		for _, b := range badNames {
			fmt.Fprintf(stderr, "  - %s\n", b)
		}
		return exitBad
	}

	if len(frags) == 0 {
		fmt.Fprintf(stdout, "changelog-assemble: no fragments in %s — [Unreleased] left as-is\n", *dir)
		return exitOK
	}

	original, err := os.ReadFile(*changelogPath)
	if err != nil {
		fmt.Fprintf(stderr, "changelog-assemble: read %s: %v\n", *changelogPath, err)
		return exitErr
	}
	assembled, err := assembleInto(string(original), frags)
	if err != nil {
		fmt.Fprintf(stderr, "changelog-assemble: %v\n", err)
		return exitBad
	}
	if err := os.WriteFile(*changelogPath, []byte(assembled), 0o644); err != nil {
		fmt.Fprintf(stderr, "changelog-assemble: write %s: %v\n", *changelogPath, err)
		return exitErr
	}
	fmt.Fprintf(stdout, "changelog-assemble: assembled %d fragment(s) into [Unreleased] of %s\n", len(frags), *changelogPath)

	if *prune {
		for _, f := range frags {
			if err := os.Remove(f.File); err != nil {
				fmt.Fprintf(stderr, "changelog-assemble: prune %s: %v\n", f.File, err)
				return exitErr
			}
		}
		fmt.Fprintf(stdout, "changelog-assemble: pruned %d fragment file(s)\n", len(frags))
	}
	return exitOK
}

// loadFragments reads every `*.md` file in dir (non-recursive), parsing each
// into a Fragment. A file whose name doesn't match `<issue>[.<seq>].<type>.md`
// (unknown type, non-numeric issue, empty body) is collected into badNames with
// a reason rather than parsed. A missing dir is not an error (no fragments).
func loadFragments(dir string) (frags []Fragment, badNames []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue // ignore .keep, READMEs, subdirs
		}
		base := strings.TrimSuffix(name, ".md")
		m := fragmentNameRE.FindStringSubmatch(base)
		if m == nil {
			badNames = append(badNames, fmt.Sprintf("%s (name must be <issue>[.<seq>].<type>.md)", name))
			continue
		}
		typ := m[3]
		if _, ok := typeHeading[typ]; !ok {
			badNames = append(badNames, fmt.Sprintf("%s (unknown type %q)", name, typ))
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, nil, fmt.Errorf("read %s: %w", name, rerr)
		}
		body := strings.TrimRight(string(raw), " \t\r\n")
		if strings.TrimSpace(body) == "" {
			badNames = append(badNames, fmt.Sprintf("%s (empty body)", name))
			continue
		}
		issue, _ := strconv.Atoi(m[1]) // regex guarantees digits
		seq := 0
		if m[2] != "" {
			seq, _ = strconv.Atoi(m[2])
		}
		frags = append(frags, Fragment{
			Issue: issue, Seq: seq, Type: typ, Body: body,
			File: filepath.Join(dir, name),
		})
	}
	return frags, badNames, nil
}

// assembleInto inserts the fragments' bullets into the `## [Unreleased]` section
// of changelogText and returns the rewritten document. It is merge-aware: any
// `### Type` block already present in [Unreleased] (a legacy hand-edit during
// the migration window) is preserved, with the fragment bullets appended into
// the matching type. Everything outside [Unreleased] — the prelude before the
// first `### `, every released section — is byte-identical.
func assembleInto(changelogText string, frags []Fragment) (string, error) {
	lines := strings.Split(changelogText, "\n")

	// Locate the [Unreleased] header and the next `## [` section header.
	start := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "## [Unreleased]") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("CHANGELOG has no `## [Unreleased]` section")
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## [") {
			end = i
			break
		}
	}

	// Split the [Unreleased] body (between the header and `end`) into the
	// prelude (everything before the first `### `) and existing per-type blocks.
	bodyLines := lines[start+1 : end]
	preludeLines, existing := splitUnreleasedBody(bodyLines)

	// Merge fragments into the existing per-type entries.
	byType := map[string][]string{}
	for t, entries := range existing {
		byType[t] = append(byType[t], entries...)
	}
	sortedFrags := append([]Fragment(nil), frags...)
	sort.SliceStable(sortedFrags, func(i, j int) bool {
		if sortedFrags[i].Issue != sortedFrags[j].Issue {
			return sortedFrags[i].Issue < sortedFrags[j].Issue
		}
		return sortedFrags[i].Seq < sortedFrags[j].Seq
	})
	for _, f := range sortedFrags {
		byType[f.Type] = append(byType[f.Type], f.Body)
	}

	// Rebuild the [Unreleased] body: preserved prelude, then `### Type` blocks
	// in canonical order. An unknown existing type (shouldn't happen post-check)
	// is preserved after the canonical ones so nothing is silently dropped.
	var out []string
	out = append(out, lines[:start+1]...) // up to and including `## [Unreleased]`

	prelude := strings.TrimRight(strings.Join(preludeLines, "\n"), "\n")
	if strings.TrimSpace(prelude) != "" {
		out = append(out, "", prelude)
	}

	emit := func(t string) {
		entries, ok := byType[t]
		if !ok || len(entries) == 0 {
			return
		}
		out = append(out, "", "### "+typeHeading[t], "")
		out = append(out, strings.Join(entries, "\n"))
		delete(byType, t)
	}
	for _, t := range typeOrder {
		emit(t)
	}
	// Any leftover (non-canonical) types, deterministically ordered.
	var leftover []string
	for t := range byType {
		if len(byType[t]) > 0 {
			leftover = append(leftover, t)
		}
	}
	sort.Strings(leftover)
	for _, t := range leftover {
		heading := typeHeading[t]
		if heading == "" {
			heading = t
		}
		out = append(out, "", "### "+heading, "")
		out = append(out, strings.Join(byType[t], "\n"))
	}

	out = append(out, "") // blank line separating [Unreleased] from the next section
	out = append(out, lines[end:]...)

	return strings.Join(out, "\n"), nil
}

// splitUnreleasedBody divides the [Unreleased] body lines into the prelude
// (everything before the first `### ` heading) and a map of type → entry-bodies
// for each existing `### Type` block. Each existing block's bullets are captured
// as a single entry string (verbatim, trailing blanks trimmed) so a legacy
// hand-edit survives the merge intact.
func splitUnreleasedBody(bodyLines []string) (prelude []string, existing map[string][]string) {
	existing = map[string][]string{}
	firstHeading := len(bodyLines)
	for i, ln := range bodyLines {
		if strings.HasPrefix(ln, "### ") {
			firstHeading = i
			break
		}
	}
	prelude = bodyLines[:firstHeading]

	var curType string
	var curEntry []string
	flush := func() {
		if curType == "" {
			return
		}
		entry := strings.TrimRight(strings.Join(curEntry, "\n"), "\n")
		if strings.TrimSpace(entry) != "" {
			existing[curType] = append(existing[curType], entry)
		}
		curType, curEntry = "", nil
	}
	for _, ln := range bodyLines[firstHeading:] {
		if strings.HasPrefix(ln, "### ") {
			flush()
			curType = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ln, "### ")))
			continue
		}
		curEntry = append(curEntry, ln)
	}
	flush()
	return prelude, existing
}

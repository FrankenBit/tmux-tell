// check-changelog-placement guards against the rebase-past-cut misplacement edge
// (#471): when a PR is rebased over a release cut, the CHANGELOG.md diff anchor
// shifts from ## [Unreleased] to the newly-sealed ## [X.Y.Z] section, silently
// injecting PR content into a published release record.
//
// The tool diffs CHANGELOG.md against a base ref (default: origin/main) and
// fails if any added line falls under a versioned ## [X.Y.Z] heading rather
// than ## [Unreleased]. Lines under [Unreleased] are clean; lines under a
// version header are not.
//
// Note: since #494, PRs should add changelog.d/ fragments rather than editing
// CHANGELOG.md directly. This guard is a second line of defence for the cases
// where CHANGELOG.md is touched directly (release-cut PRs legitimately do so —
// the CI step skips those branches; see test.yml).
//
// Exit codes:
//
//	0 — CHANGELOG.md not modified, or all additions are under [Unreleased].
//	1 — at least one added line falls under a sealed ## [X.Y.Z] section.
//	2 — internal error (git unavailable, diff failed, etc.).
//
// Usage:
//
//	go run ./tools/check-changelog-placement [-base=origin/main]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	exitOK  = 0
	exitBad = 1
	exitErr = 2
)

// versionHeadingRE matches a sealed ## [X.Y.Z] or ## [X.Y.Z] — date heading.
// Does NOT match ## [Unreleased].
var versionHeadingRE = regexp.MustCompile(`^## \[(\d+\.\d+\.\d+[^\]]*)\]`)

// unreleasedHeadingRE matches ## [Unreleased] exactly.
var unreleasedHeadingRE = regexp.MustCompile(`^## \[Unreleased\]`)

func main() {
	os.Exit(run())
}

func run() int {
	base := flag.String("base", "origin/main", "git ref to diff against")
	flag.Parse()

	changed, err := changedFiles(*base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-changelog-placement: git diff --name-only: %v\n", err)
		return exitErr
	}
	if !changed {
		fmt.Println("check-changelog-placement: CHANGELOG.md not modified — OK.")
		return exitOK
	}

	diff, err := changelogDiff(*base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-changelog-placement: git diff CHANGELOG.md: %v\n", err)
		return exitErr
	}

	violations := checkPlacement(diff)
	if len(violations) == 0 {
		fmt.Println("check-changelog-placement: additions confined to [Unreleased] — OK.")
		return exitOK
	}

	fmt.Fprintln(os.Stderr, "check-changelog-placement: FAIL")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "The following lines were added to a sealed ## [X.Y.Z] section of CHANGELOG.md")
	fmt.Fprintln(os.Stderr, "rather than ## [Unreleased]. This usually means the PR was rebased past a")
	fmt.Fprintln(os.Stderr, "release cut and the CHANGELOG entry landed in the wrong section (#471).")
	fmt.Fprintln(os.Stderr)
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "  %s\n", v)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To fix: move the entry to the ## [Unreleased] section, or (preferred) use a")
	fmt.Fprintln(os.Stderr, "changelog.d/<issue>.<type>.md fragment instead (#494).")
	return exitBad
}

// changedFiles reports whether CHANGELOG.md appears in the PR diff.
func changedFiles(base string) (bool, error) {
	out, err := git("diff", "--name-only", base+"...HEAD")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "CHANGELOG.md" {
			return true, nil
		}
	}
	return false, nil
}

// changelogDiff returns the unified diff of CHANGELOG.md against base.
// -U999999 ensures section headings (## [X.Y.Z]) always appear in the diff
// context regardless of how many lines away from an added line they are —
// the checkPlacement state machine relies on seeing those headings to know
// which section it's in (#471).
func changelogDiff(base string) (string, error) {
	return git("diff", "-U999999", base+"...HEAD", "--", "CHANGELOG.md")
}

// checkPlacement parses a unified diff of CHANGELOG.md and returns a
// description string for each added line that falls under a ## [X.Y.Z] section.
// Lines under ## [Unreleased] are clean and not returned.
func checkPlacement(diff string) []string {
	var violations []string
	currentSection := ""
	sealed := false

	scanner := bufio.NewScanner(strings.NewReader(diff))
	for scanner.Scan() {
		line := scanner.Text()

		// Diff metadata lines — not CHANGELOG content.
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") ||
			strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "diff ") ||
			strings.HasPrefix(line, "index ") {
			continue
		}

		// Strip the diff prefix to get the raw CHANGELOG line.
		prefix := ""
		content := line
		if len(line) > 0 {
			prefix = string(line[0])
			content = line[1:]
		}

		// Track the current section (applies to both context and added lines).
		if strings.HasPrefix(content, "## ") {
			if unreleasedHeadingRE.MatchString(content) {
				currentSection = "Unreleased"
				sealed = false
			} else if versionHeadingRE.MatchString(content) {
				m := versionHeadingRE.FindStringSubmatch(content)
				currentSection = m[1]
				sealed = true
			}
		}

		// Flag added lines (not deletions, not context) in sealed sections.
		if prefix == "+" && sealed {
			violations = append(violations, fmt.Sprintf("[%s]: %s", currentSection, content))
		}
	}
	return violations
}

// git runs a git command and returns its stdout.
func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, ee.Stderr)
		}
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

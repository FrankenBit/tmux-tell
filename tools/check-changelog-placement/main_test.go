package main

import (
	"strings"
	"testing"
)

// diffWith constructs a minimal unified diff fragment that adds lines under a
// given section heading. Used to keep smoke-test cases readable.
func diffWith(section string, lines ...string) string {
	var b strings.Builder
	b.WriteString("--- a/CHANGELOG.md\n")
	b.WriteString("+++ b/CHANGELOG.md\n")
	b.WriteString("@@ -1,3 +1,5 @@\n")
	b.WriteString(" " + section + "\n") // context line — section heading
	for _, l := range lines {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func TestCheckPlacement_SealedSectionFails(t *testing.T) {
	diff := diffWith("## [0.21.0] — 2026-05-20", "### Added", "- my feature (#123)")
	violations := checkPlacement(diff)
	if len(violations) == 0 {
		t.Fatal("expected violations for additions under sealed [0.21.0], got none")
	}
	for _, v := range violations {
		if !strings.Contains(v, "0.21.0") {
			t.Errorf("violation should name the sealed section: %q", v)
		}
	}
}

func TestCheckPlacement_UnreleasedPasses(t *testing.T) {
	diff := diffWith("## [Unreleased]", "### Added", "- my feature (#123)")
	violations := checkPlacement(diff)
	if len(violations) != 0 {
		t.Fatalf("expected no violations for additions under [Unreleased], got %d: %v", len(violations), violations)
	}
}

func TestCheckPlacement_EmptyDiffPasses(t *testing.T) {
	violations := checkPlacement("")
	if len(violations) != 0 {
		t.Fatalf("expected no violations for empty diff, got %v", violations)
	}
}

func TestCheckPlacement_NoAdditionsPasses(t *testing.T) {
	// Deletion-only change under a sealed section should not trigger violations.
	diff := "--- a/CHANGELOG.md\n+++ b/CHANGELOG.md\n@@ -1,2 +1,1 @@\n ## [0.21.0]\n-removed line\n"
	violations := checkPlacement(diff)
	if len(violations) != 0 {
		t.Fatalf("expected no violations for deletion-only diff, got %v", violations)
	}
}

func TestCheckPlacement_MixedSections(t *testing.T) {
	// Additions in both [Unreleased] (clean) and [0.21.0] (violation).
	diff := "--- a/CHANGELOG.md\n" +
		"+++ b/CHANGELOG.md\n" +
		"@@ -1,6 +1,8 @@\n" +
		" ## [Unreleased]\n" +
		"+- clean addition (#200)\n" +
		" \n" +
		" ## [0.21.0] — 2026-05-20\n" +
		"+- bad addition (#100)\n"
	violations := checkPlacement(diff)
	if len(violations) != 1 {
		t.Fatalf("expected exactly 1 violation, got %d: %v", len(violations), violations)
	}
	if !strings.Contains(violations[0], "0.21.0") {
		t.Errorf("violation should name 0.21.0, got: %q", violations[0])
	}
}

func TestCheckPlacement_MultipleSealedVersions(t *testing.T) {
	diff := "--- a/CHANGELOG.md\n" +
		"+++ b/CHANGELOG.md\n" +
		"@@ -1,10 +1,12 @@\n" +
		" ## [0.22.0] — 2026-06-20\n" +
		"+- bad v0.22.0 entry\n" +
		" \n" +
		" ## [0.21.0] — 2026-05-20\n" +
		"+- bad v0.21.0 entry\n"
	violations := checkPlacement(diff)
	if len(violations) != 2 {
		t.Fatalf("expected 2 violations, got %d: %v", len(violations), violations)
	}
}

func TestCheckPlacement_SectionHeadingChangePasses(t *testing.T) {
	// Adding a new ## [Unreleased] heading line itself is fine (idempotent).
	diff := "--- a/CHANGELOG.md\n" +
		"+++ b/CHANGELOG.md\n" +
		"@@ -1,1 +1,2 @@\n" +
		"+## [Unreleased]\n" +
		" ## [0.21.0]\n"
	violations := checkPlacement(diff)
	// The "+" line IS the [Unreleased] heading, so we update currentSection to
	// Unreleased and sealed=false — the heading line itself should not trigger
	// a violation even if it appears after scanning a versioned context line.
	// (The heading is checked before the violation flag check.)
	if len(violations) != 0 {
		t.Fatalf("expected no violations for adding an [Unreleased] heading, got %v", violations)
	}
}

func TestCheckPlacement_VersionWithDatePasses(t *testing.T) {
	// Regression: version headings with em-dash date separator should be detected.
	diff := diffWith("## [0.22.0] — 2026-06-20", "- sneaky entry")
	violations := checkPlacement(diff)
	if len(violations) == 0 {
		t.Fatal("expected violation for addition under sealed [0.22.0] — date heading")
	}
}

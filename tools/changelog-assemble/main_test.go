package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFrag drops a fragment file in dir and returns nothing; helper for the
// loadFragments + run() integration tests.
func writeFrag(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write fragment %s: %v", name, err)
	}
}

// TestAssembleInto_TypeOrderingAndSort pins the canonical emit order (Added →
// Changed → Deprecated → Removed → Fixed → Security → Documentation) and the
// (issue,seq) sort within a type, and proves everything outside [Unreleased] is
// byte-identical.
func TestAssembleInto_TypeOrderingAndSort(t *testing.T) {
	changelog := "# Changelog\n\n## [Unreleased]\n\n## [0.18.1] — 2026-06-16\n\nSealed.\n\n### Fixed\n\n- old entry\n"
	frags := []Fragment{
		{Issue: 486, Type: "fixed", Body: "- **Fixed B** (#486)"},
		{Issue: 480, Type: "changed", Body: "- **Changed A** (#480)"},
		{Issue: 448, Seq: 2, Type: "added", Body: "- **Added two** (#448)"},
		{Issue: 448, Seq: 1, Type: "added", Body: "- **Added one** (#448)"},
		{Issue: 450, Type: "documentation", Body: "- **Doc** (#450)"},
	}
	got, err := assembleInto(changelog, frags)
	if err != nil {
		t.Fatalf("assembleInto: %v", err)
	}

	// Order of headings within [Unreleased].
	wantOrder := []string{"### Added", "### Changed", "### Fixed", "### Documentation"}
	idx := -1
	for _, h := range wantOrder {
		at := strings.Index(got, h)
		if at < 0 {
			t.Fatalf("missing heading %q in:\n%s", h, got)
		}
		if at < idx {
			t.Errorf("heading %q out of canonical order:\n%s", h, got)
		}
		idx = at
	}
	// (issue,seq) sort: Added one before Added two.
	if strings.Index(got, "Added one") > strings.Index(got, "Added two") {
		t.Errorf("seq sort wrong (one should precede two):\n%s", got)
	}
	// The sealed released section is byte-identical (untouched).
	if !strings.Contains(got, "## [0.18.1] — 2026-06-16\n\nSealed.\n\n### Fixed\n\n- old entry") {
		t.Errorf("released section was altered:\n%s", got)
	}
	// No Deprecated/Removed/Security headings emitted (no fragments for them).
	for _, absent := range []string{"### Deprecated", "### Removed", "### Security"} {
		if strings.Contains(got, absent) {
			t.Errorf("emitted empty %q:\n%s", absent, got)
		}
	}
}

// TestAssembleInto_MergeAware pins the migration-window behavior: a legacy
// hand-written `### Changed` block already in [Unreleased] is preserved, and a
// fragment of the same type appends into it rather than duplicating the heading.
func TestAssembleInto_MergeAware(t *testing.T) {
	changelog := "## [Unreleased]\n\n### Changed\n\n- legacy hand-edit (#400)\n\n## [0.18.1] — 2026-06-16\n"
	frags := []Fragment{{Issue: 480, Type: "changed", Body: "- fragment entry (#480)"}}
	got, err := assembleInto(changelog, frags)
	if err != nil {
		t.Fatalf("assembleInto: %v", err)
	}
	if n := strings.Count(got, "### Changed"); n != 1 {
		t.Errorf("### Changed appears %d times, want 1 (merge, not duplicate):\n%s", n, got)
	}
	if !strings.Contains(got, "legacy hand-edit (#400)") || !strings.Contains(got, "fragment entry (#480)") {
		t.Errorf("merge lost an entry:\n%s", got)
	}
	if strings.Index(got, "legacy hand-edit") > strings.Index(got, "fragment entry") {
		t.Errorf("legacy entry should precede fragment entry:\n%s", got)
	}
}

// TestAssembleInto_PreludePreserved pins #427: a hand-curated prelude paragraph
// already in [Unreleased] survives, and `### Type` blocks are inserted AFTER it
// (so the toolkit's extract-before-first-### release-body boundary still works).
func TestAssembleInto_PreludePreserved(t *testing.T) {
	changelog := "## [Unreleased]\n\nThe foundation release — fragment pattern lands.\n\n## [0.18.1] — 2026-06-16\n"
	frags := []Fragment{{Issue: 494, Type: "changed", Body: "- **Fragment pattern** (#494)"}}
	got, err := assembleInto(changelog, frags)
	if err != nil {
		t.Fatalf("assembleInto: %v", err)
	}
	preludeAt := strings.Index(got, "The foundation release")
	headingAt := strings.Index(got, "### Changed")
	if preludeAt < 0 || headingAt < 0 {
		t.Fatalf("prelude or heading missing:\n%s", got)
	}
	if preludeAt > headingAt {
		t.Errorf("prelude must precede the first ### heading (#427 boundary):\n%s", got)
	}
}

// TestAssembleInto_NoUnreleased errors rather than silently corrupting.
func TestAssembleInto_NoUnreleased(t *testing.T) {
	if _, err := assembleInto("# Changelog\n\n## [0.18.1]\n", []Fragment{{Issue: 1, Type: "fixed", Body: "- x"}}); err == nil {
		t.Errorf("expected error when no [Unreleased] section present")
	}
}

// TestAssembleInto_MultilineBullet preserves a fragment's continuation lines.
func TestAssembleInto_MultilineBullet(t *testing.T) {
	changelog := "## [Unreleased]\n\n## [0.18.1]\n"
	frags := []Fragment{{Issue: 7, Type: "added", Body: "- **Multi** (#7) headline\n  continuation line with detail"}}
	got, err := assembleInto(changelog, frags)
	if err != nil {
		t.Fatalf("assembleInto: %v", err)
	}
	if !strings.Contains(got, "  continuation line with detail") {
		t.Errorf("continuation line not preserved:\n%s", got)
	}
}

// TestLoadFragments_ParsingAndValidation covers good names, the optional seq
// discriminator, and every rejection path (-check surface).
func TestLoadFragments_ParsingAndValidation(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "480.changed.md", "- **A** (#480)")
	writeFrag(t, dir, "448.1.added.md", "- one")
	writeFrag(t, dir, ".keep", "")                  // ignored (not .md)
	writeFrag(t, dir, "README.md", "not a frag")    // bad: name doesn't parse
	writeFrag(t, dir, "500.bogus.md", "- x")        // bad: unknown type
	writeFrag(t, dir, "501.fixed.md", "   \n")      // bad: empty body
	writeFrag(t, dir, "notanumber.fixed.md", "- y") // bad: non-numeric issue

	frags, bad, err := loadFragments(dir)
	if err != nil {
		t.Fatalf("loadFragments: %v", err)
	}
	if len(frags) != 2 {
		t.Errorf("parsed %d good fragments, want 2: %+v", len(frags), frags)
	}
	if len(bad) != 4 {
		t.Errorf("got %d bad fragments, want 4: %v", len(bad), bad)
	}
	// Spot-check the seq discriminator parsed.
	var found bool
	for _, f := range frags {
		if f.Issue == 448 && f.Seq == 1 && f.Type == "added" {
			found = true
		}
	}
	if !found {
		t.Errorf("448.1.added.md not parsed with seq=1: %+v", frags)
	}
}

// TestLoadFragments_MissingDir is a clean no-op (not an error).
func TestLoadFragments_MissingDir(t *testing.T) {
	frags, bad, err := loadFragments(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || len(frags) != 0 || len(bad) != 0 {
		t.Errorf("missing dir should be clean no-op; got frags=%v bad=%v err=%v", frags, bad, err)
	}
}

// TestRun_CheckMode exercises the CI lint surface end-to-end.
func TestRun_CheckMode(t *testing.T) {
	dir := t.TempDir()
	writeFrag(t, dir, "480.changed.md", "- **A** (#480)")

	var out, errb bytes.Buffer
	if code := run([]string{"-check", "-dir", dir}, &out, &errb); code != exitOK {
		t.Fatalf("check on clean dir: exit %d, stderr=%s", code, errb.String())
	}

	writeFrag(t, dir, "bad.frag.md", "- x") // unknown type "frag"
	out.Reset()
	errb.Reset()
	if code := run([]string{"-check", "-dir", dir}, &out, &errb); code != exitBad {
		t.Fatalf("check with malformed fragment: exit %d, want %d", code, exitBad)
	}
	if !strings.Contains(errb.String(), "bad.frag.md") {
		t.Errorf("check diagnostic should name the bad file; got: %s", errb.String())
	}
}

// TestRun_AssembleAndPrune is the full mutating path: assemble into a temp
// CHANGELOG + delete the consumed fragments.
func TestRun_AssembleAndPrune(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "changelog.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cl := filepath.Join(tmp, "CHANGELOG.md")
	if err := os.WriteFile(cl, []byte("## [Unreleased]\n\n## [0.18.1] — 2026-06-16\n\nSealed.\n"), 0o600); err != nil {
		t.Fatalf("write changelog: %v", err)
	}
	writeFrag(t, dir, "480.changed.md", "- **Changed** (#480)")
	writeFrag(t, dir, "486.fixed.md", "- **Fixed** (#486)")

	var out, errb bytes.Buffer
	if code := run([]string{"-prune", "-changelog", cl, "-dir", dir}, &out, &errb); code != exitOK {
		t.Fatalf("assemble: exit %d, stderr=%s", code, errb.String())
	}
	body, _ := os.ReadFile(cl)
	got := string(body)
	for _, want := range []string{"### Changed", "- **Changed** (#480)", "### Fixed", "- **Fixed** (#486)"} {
		if !strings.Contains(got, want) {
			t.Errorf("assembled CHANGELOG missing %q:\n%s", want, got)
		}
	}
	// Sealed section untouched.
	if !strings.Contains(got, "## [0.18.1] — 2026-06-16\n\nSealed.") {
		t.Errorf("released section altered:\n%s", got)
	}
	// Fragments pruned.
	if remaining, _ := os.ReadDir(dir); len(remaining) != 0 {
		t.Errorf("prune left %d file(s) in changelog.d", len(remaining))
	}
}

// TestRun_AssembleEmptyDirNoOp is the migration passthrough: no fragments → the
// CHANGELOG is left byte-identical (legacy hand-edit path still works).
func TestRun_AssembleEmptyDirNoOp(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "changelog.d")
	_ = os.Mkdir(dir, 0o755)
	cl := filepath.Join(tmp, "CHANGELOG.md")
	original := "## [Unreleased]\n\n### Changed\n\n- legacy hand-edit (#400)\n\n## [0.18.1]\n"
	if err := os.WriteFile(cl, []byte(original), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-changelog", cl, "-dir", dir}, &out, &errb); code != exitOK {
		t.Fatalf("exit %d, stderr=%s", code, errb.String())
	}
	body, _ := os.ReadFile(cl)
	if string(body) != original {
		t.Errorf("empty-dir assemble mutated CHANGELOG:\n got:%q\nwant:%q", string(body), original)
	}
}

// TestRun_AssembleRefusesMalformed: a malformed fragment hard-fails assemble (no
// silent skip) so a typo can't drop an entry on the floor at release time.
func TestRun_AssembleRefusesMalformed(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "changelog.d")
	_ = os.Mkdir(dir, 0o755)
	cl := filepath.Join(tmp, "CHANGELOG.md")
	_ = os.WriteFile(cl, []byte("## [Unreleased]\n\n## [0.18.1]\n"), 0o600)
	writeFrag(t, dir, "480.changed.md", "- ok")
	writeFrag(t, dir, "490.bogus.md", "- bad type")

	var out, errb bytes.Buffer
	if code := run([]string{"-changelog", cl, "-dir", dir}, &out, &errb); code != exitBad {
		t.Fatalf("assemble with malformed fragment: exit %d, want %d", code, exitBad)
	}
	// CHANGELOG must be untouched on refusal.
	body, _ := os.ReadFile(cl)
	if strings.Contains(string(body), "### Changed") {
		t.Errorf("CHANGELOG was mutated despite malformed-fragment refusal:\n%s", string(body))
	}
}

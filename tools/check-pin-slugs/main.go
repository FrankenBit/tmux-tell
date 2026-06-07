// check-pin-slugs enforces ADR-0001's discipline-pin slug register
// against the slugs actually in use across the codebase. Per #51.
//
// The "deliberate act" framing in ADR-0001 — that adding a fifth
// commitment slug is intentional, not an accidental side-effect of
// writing another test — is convention-only until this check runs on
// every CI pass. The check parses two sources:
//
//  1. The marker-block-delimited slug register in
//     docs/adr/0001-discipline-pins-as-test-category.md. The block is
//     anchored by `<!-- pin-slug-register-start -->` and
//     `<!-- pin-slug-register-end -->` so we don't have to parse the
//     ADR's full markdown — just the slug list between the markers.
//
//  2. The slugs in use, extracted from `testpin.Triage(t, "<slug>",`
//     calls across the codebase. The helper's slug argument is the
//     canonical source-of-truth — the `// PIN:` docstring and the
//     `TestPin_<Slug>_` function name are human-readable adjacent
//     views that may drift; the Triage call is what tooling sees.
//
// Exit codes:
//
//	0 — every in-use slug is registered. CI green.
//	1 — at least one slug is in use but not in the register. CI red
//	    with a clear pointer to the ADR (so the failing Pilot knows
//	    to either amend the ADR or correct the slug spelling).
//	2 — internal error (couldn't read the ADR, regex bug, etc.).
//
// Run via `make check-pin-slugs` or directly:
//
//	go run ./tools/check-pin-slugs/
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	exitOK      = 0
	exitDrift   = 1
	exitErr     = 2
	adrPath     = "docs/adr/0001-discipline-pins-as-test-category.md"
	markerStart = "<!-- pin-slug-register-start -->"
	markerEnd   = "<!-- pin-slug-register-end -->"
)

// slugInRegisterRE matches a register entry's slug. The format is
// `- **`<Slug>`** — description` so we anchor on the `**` markdown
// bold + backtick pair around the slug.
var slugInRegisterRE = regexp.MustCompile("- \\*\\*`(\\w+)`\\*\\*")

// slugInTriageCallRE matches calls to `testpin.Triage(t, "<slug>",`.
// We tolerate whitespace + line breaks between `Triage(` and the
// first quoted argument so multi-line invocations work.
var slugInTriageCallRE = regexp.MustCompile(`testpin\.Triage\s*\(\s*[^,]+,\s*"(\w+)"`)

func main() {
	os.Exit(run())
}

func run() int {
	registered, err := readRegister(adrPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-pin-slugs: read register: %v\n", err)
		return exitErr
	}
	if len(registered) == 0 {
		fmt.Fprintf(os.Stderr,
			"check-pin-slugs: no slugs found in register between %q and %q in %s\n",
			markerStart, markerEnd, adrPath)
		return exitErr
	}

	inUse, sources, err := scanInUseSlugs(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-pin-slugs: scan in-use slugs: %v\n", err)
		return exitErr
	}
	if len(inUse) == 0 {
		fmt.Fprintf(os.Stderr,
			"check-pin-slugs: no testpin.Triage(t, ...) calls found anywhere — codebase may have moved them\n")
		return exitErr
	}

	var drift []string
	for slug := range inUse {
		if _, ok := registered[slug]; !ok {
			drift = append(drift, slug)
		}
	}
	sort.Strings(drift)

	if len(drift) == 0 {
		fmt.Printf("check-pin-slugs: %d slug(s) registered, %d in use, all aligned\n",
			len(registered), len(inUse))
		return exitOK
	}

	fmt.Fprintln(os.Stderr, "check-pin-slugs: FAIL")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "The following slugs are in use in testpin.Triage(...) calls but NOT")
	fmt.Fprintln(os.Stderr, "registered in ADR-0001's commitment slugs marker block:")
	fmt.Fprintln(os.Stderr)
	for _, slug := range drift {
		fmt.Fprintf(os.Stderr, "  %s\n", slug)
		for _, src := range sources[slug] {
			fmt.Fprintf(os.Stderr, "    %s\n", src)
		}
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "To resolve:")
	fmt.Fprintf(os.Stderr, "  1. If the slug is correct: amend %s to add the slug + rationale\n", adrPath)
	fmt.Fprintln(os.Stderr, "     between the <!-- pin-slug-register-start --> / -end markers,")
	fmt.Fprintln(os.Stderr, "     per ADR-0001's by-commitment growth rules.")
	fmt.Fprintln(os.Stderr, "  2. If the slug is a typo: correct the testpin.Triage(...) call.")
	return exitDrift
}

// readRegister extracts the slug set from the marker block in
// docs/adr/0001-discipline-pins-as-test-category.md.
func readRegister(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(raw)
	start := strings.Index(content, markerStart)
	end := strings.Index(content, markerEnd)
	if start == -1 || end == -1 {
		return nil, fmt.Errorf("marker block not found (%q / %q)", markerStart, markerEnd)
	}
	if start >= end {
		return nil, fmt.Errorf("marker block malformed: start %d >= end %d", start, end)
	}
	block := content[start+len(markerStart) : end]

	registered := make(map[string]bool)
	for _, match := range slugInRegisterRE.FindAllStringSubmatch(block, -1) {
		registered[match[1]] = true
	}
	return registered, nil
}

// scanInUseSlugs walks the codebase rooted at root and finds every
// testpin.Triage(t, "<slug>", ...) call's slug argument. Returns the
// set of in-use slugs and a per-slug list of source-file locations
// (for the failure-mode error message).
func scanInUseSlugs(root string) (map[string]bool, map[string][]string, error) {
	inUse := make(map[string]bool)
	sources := make(map[string][]string)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip non-Go and non-test files (Triage only meaningful in tests).
		// Also skip the tools directory to avoid self-matching this tool's
		// own source if it ever quotes a slug for documentation.
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "bin" || d.Name() == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the testpin package's own test file — it uses fixture
		// slug strings (TestSlug, ExampleSlug) that aren't meant to be
		// in the register. The helper is the discipline; its own tests
		// don't exercise the registered surface.
		if strings.HasSuffix(path, filepath.Join("internal", "testpin", "testpin_test.go")) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		for _, match := range slugInTriageCallRE.FindAllStringSubmatch(string(raw), -1) {
			slug := match[1]
			inUse[slug] = true
			sources[slug] = append(sources[slug], path)
		}
		return nil
	})
	return inUse, sources, err
}

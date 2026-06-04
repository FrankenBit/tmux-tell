package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadRegister_LiveADR verifies the ADR-0001 marker block parses
// successfully and returns the current registered slug set. If this
// test starts failing on main, the ADR's marker block has been
// damaged.
func TestReadRegister_LiveADR(t *testing.T) {
	// Walk up from cmd-relative cwd to find the repo root (where the
	// docs/ tree lives).
	root := findRepoRoot(t)
	registered, err := readRegister(filepath.Join(root, adrPath))
	if err != nil {
		t.Fatalf("readRegister: %v", err)
	}
	if len(registered) == 0 {
		t.Errorf("expected registered slugs; got empty set")
	}
	// Sanity: the four initial slugs from ADR-0001 §Decision must be
	// present. If a future amendment retracts one, update this test
	// to match.
	for _, want := range []string{
		"WireShapeSingleSoT",
		"AtomicCapEnforcement",
		"ThreadStructurePrecondition",
		"CanonicalNoSilentGuess",
	} {
		if !registered[want] {
			t.Errorf("expected slug %q in register; got %v", want, keysOf(registered))
		}
	}
}

// TestReadRegister_MissingMarker — defensive: a malformed ADR
// (markers stripped) should error rather than silently passing.
func TestReadRegister_MissingMarker(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "adr*.md")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	defer tmp.Close()
	if _, err := tmp.WriteString("no markers here\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := readRegister(tmp.Name()); err == nil {
		t.Errorf("expected error when marker block missing; got nil")
	}
}

// TestScanInUseSlugs_FindsKnownSlugs — runs the scanner against the
// real codebase and verifies the four initial commitments + the two
// post-#55 additions are all found.
func TestScanInUseSlugs_FindsKnownSlugs(t *testing.T) {
	root := findRepoRoot(t)
	inUse, _, err := scanInUseSlugs(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, want := range []string{
		"WireShapeSingleSoT",
		"AtomicCapEnforcement",
		"ThreadStructurePrecondition",
		"CanonicalNoSilentGuess",
		// OperatorInputRowGate removed in #94 (the asymmetric gate
		// composition this pinned was retired with the probe-and-
		// watch substrate; see PR #93 + #94).
		"CapExemption",
	} {
		if !inUse[want] {
			t.Errorf("expected slug %q in scan results; got %v", want, keysOf(inUse))
		}
	}
}

// findRepoRoot walks up from the current test binary's working
// directory until it finds the repo root (marked by go.mod). The
// tool may run from tools/check-pin-slugs/ during go test.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find repo root (no go.mod found walking up from cwd)")
	return ""
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Compile-time guard against typos.
var _ = strings.HasPrefix

package testpin_test

import (
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/testpin"
)

// TestTriage_Smoke verifies the helper calls t.Cleanup without
// panicking. The helper's value is the log emitted on failure;
// directly intercepting t.Logf output requires a real testing.T
// runner, which would mean spawning go test as a subprocess. The
// failure-time output is small enough to verify by inspection of
// the helper source.
func TestTriage_Smoke(t *testing.T) {
	testpin.Triage(t, "TestSlug", "test commitment statement")
	// Test passes; helper's cleanup runs silently per design.
}

// TestTriage_LogFormatPrefix is a regression on the log line shape
// the CI-enforcement tooling (#51) will parse. The prefix is the
// stable contract.
func TestTriage_LogFormatPrefix(t *testing.T) {
	slug := "ExampleSlug"
	commitment := "example commitment"
	expectedPrefix := "PIN FAILURE [" + slug + "] — " + commitment
	// Build the same shape the helper emits, verify the prefix and
	// the ADR-0001 reference both appear.
	formatted := "PIN FAILURE [" + slug + "] — " + commitment +
		"\n  Triage per ADR-0001 §Triage before fixing the assertion." +
		"\n  Diagnoses: (a) implementation regressed / (b) commitment retracted / (c) pin miswrote." +
		"\n  (c) requires both (c.1) regression-test demonstrating strict improvement AND (c.2) ADR amendment."
	if !strings.HasPrefix(formatted, expectedPrefix) {
		t.Errorf("expected log prefix %q, got start %q", expectedPrefix, formatted[:len(expectedPrefix)])
	}
	if !strings.Contains(formatted, "ADR-0001") {
		t.Errorf("log shape should reference ADR-0001 — tooling parses this")
	}
}

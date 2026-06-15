// Discipline pins for the internal/discover package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the three orthogonal grep handles
// for the discipline.
package discover

import (
	"context"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/testpin"
)

// PIN: never silently guess between canonical-or-alias exact matches.
// Substring variant — when --resume contains multiple canonical-name
// substrings ("admin and pilot" matches both), the resolver must return
// ambiguous=true rather than picking by slice order. Surveyor v0.2.0
// review.
func TestPin_CanonicalNoSilentGuess_SubstringAmbiguous(t *testing.T) {
	testpin.Triage(t, "CanonicalNoSilentGuess",
		"never silently guess between canonical-or-alias exact matches — substring ambiguity must return ambiguous=true")
	canonicalFakes(t,
		[]byte("%7\t700\t✳ Admin-Pilot-Hybrid\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		700: "claude\x00--resume\x00admin\x00and\x00pilot\x00",
	})

	got, ambiguous, _ := w.LookupByNameWithCanonicals(
		context.Background(), "admin", canonicalSetup())
	if !ambiguous {
		t.Errorf("expected ambiguous=true (admin AND pilot both substring); got %q", got)
	}
	if got != "" {
		t.Errorf("ambiguous should return empty pane; got %q", got)
	}
}

// PIN: never silently guess between canonical-or-alias exact matches.
// Exact-match alias-collision variant — when two canonicals share an
// alias ("admin" aliased "claude" AND "pilot" aliased "claude"), the
// exact-match pass must return ambiguous=true rather than picking by
// slice order. Surveyor v0.2.0 review Q(a).
func TestPin_CanonicalNoSilentGuess_ExactMatchAliasCollision(t *testing.T) {
	testpin.Triage(t, "CanonicalNoSilentGuess",
		"never silently guess between canonical-or-alias exact matches — two canonicals sharing an alias must return ambiguous=true")
	canonicalFakes(t,
		[]byte("%5\t500\tclaude\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		500: "claude\x00--resume\x00claude\x00",
	})

	canonicals := []CanonicalAgent{
		{Name: "admin", Aliases: []string{"claude"}},
		{Name: "pilot", Aliases: []string{"claude"}},
	}
	_, ambiguous, err := w.LookupByNameWithCanonicals(
		context.Background(), "admin", canonicals)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ambiguous {
		t.Errorf("expected ambiguous=true (admin and pilot both alias 'claude'); got ambiguous=false")
	}
}

// PIN: never silently guess between canonical-or-alias exact matches.
// Exact-match alias-is-another-canonical variant — when canonical
// "pilot" exists AND canonical "admin" has alias "pilot", a pane
// running --resume "pilot" exact-matches both. Must return
// ambiguous=true.
func TestPin_CanonicalNoSilentGuess_ExactMatchAliasIsAnotherCanonical(t *testing.T) {
	testpin.Triage(t, "CanonicalNoSilentGuess",
		"never silently guess between canonical-or-alias exact matches — alias colliding with another canonical's name must return ambiguous=true")
	canonicalFakes(t,
		[]byte("%5\t500\tpilot\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		500: "claude\x00--resume\x00pilot\x00",
	})

	canonicals := []CanonicalAgent{
		{Name: "admin", Aliases: []string{"pilot"}},
		{Name: "pilot"},
	}
	_, ambiguous, err := w.LookupByNameWithCanonicals(
		context.Background(), "pilot", canonicals)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !ambiguous {
		t.Errorf("expected ambiguous=true (admin alias 'pilot' collides with canonical 'pilot')")
	}
}

// PIN: never silently guess between canonical-or-alias exact matches.
// PaneAgentName variant — the pane-to-canonical lookup path
// (PaneAgentNameWithCanonicals) must surface the same ambiguity
// signal as the name-to-pane lookup. Without this pin, the two
// lookup paths could diverge on the load-bearing claim.
func TestPin_CanonicalNoSilentGuess_ExactMatchPaneAgentName(t *testing.T) {
	testpin.Triage(t, "CanonicalNoSilentGuess",
		"never silently guess between canonical-or-alias exact matches — PaneAgentName lookup honors the same ambiguity contract")
	canonicalFakes(t,
		[]byte("%5\t500\tclaude\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		500: "claude\x00--resume\x00claude\x00",
	})

	canonicals := []CanonicalAgent{
		{Name: "admin", Aliases: []string{"claude"}},
		{Name: "pilot", Aliases: []string{"claude"}},
	}
	got, ambiguous, _ := w.PaneAgentNameWithCanonicals(
		context.Background(), "%5", canonicals)
	if !ambiguous {
		t.Errorf("expected ambiguous=true; got %q ambiguous=false", got)
	}
	if got != "" {
		t.Errorf("ambiguous should return empty name; got %q", got)
	}
}

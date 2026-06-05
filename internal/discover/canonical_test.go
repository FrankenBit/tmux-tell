package discover

import (
	"context"
	"errors"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// canonicalSetup gives every test the same canonical agent registry
// the production system has: short canonical names ("bosun", "pilot"
// etc) plus aliases for the long --resume values claude is launched
// with on alcatraz.
func canonicalSetup() []CanonicalAgent {
	return []CanonicalAgent{
		{Name: "bosun", Aliases: []string{"Master Bosun of Nimbus"}},
		{Name: "surveyor"},
		{Name: "pilot"},
		{Name: "admin", Aliases: []string{"Alcatraz Infra Admin"}},
	}
}

func canonicalFakes(t *testing.T, panes []byte, cmdlines map[int]string) {
	t.Helper()
	prevList := tmuxio.SetListPanesWithPIDRunner(func(_ context.Context) ([]byte, error) {
		return panes, nil
	})
	t.Cleanup(func() { tmuxio.SetListPanesWithPIDRunner(prevList) })
}

func canonicalWalker(cmdlines map[int]string) *Walker {
	return &Walker{
		CmdlineReader: func(pid int) (string, error) {
			if c, ok := cmdlines[pid]; ok {
				return c, nil
			}
			return "", errors.New("no fake")
		},
		ChildrenReader: func(int) []int { return nil },
		MaxDepth:       1,
	}
}

// LookupByNameWithCanonicals: exact match wins over substring.
func TestLookupByNameWithCanonicals_ExactMatch(t *testing.T) {
	canonicalFakes(t,
		[]byte("%3\t300\t✳ Surveyor\tclaude\n"+
			"%5\t500\t✳ Admin\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		300: "claude\x00--resume\x00surveyor\x00",
		500: "claude\x00--resume\x00admin\x00",
	})

	got, ambiguous, err := w.LookupByNameWithCanonicals(
		context.Background(), "surveyor", canonicalSetup())
	if err != nil || ambiguous {
		t.Fatalf("err=%v ambiguous=%v", err, ambiguous)
	}
	if got != "%3" {
		t.Errorf("got %q, want %%3", got)
	}
}

// LookupByNameWithCanonicals: alias match.
func TestLookupByNameWithCanonicals_AliasMatch(t *testing.T) {
	canonicalFakes(t,
		[]byte("%2\t200\t✳ Master Bosun of Nimbus\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		200: "claude\x00--resume\x00Master\x00Bosun\x00of\x00Nimbus\x00",
	})

	got, _, err := w.LookupByNameWithCanonicals(
		context.Background(), "bosun", canonicalSetup())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "%2" {
		t.Errorf("got %q, want %%2 (alias 'Master Bosun of Nimbus' should resolve to bosun)", got)
	}
}

// LookupByNameWithCanonicals: substring fallback when neither exact
// nor alias matches. Realistic only when an agent was launched with a
// "natural" suffix the operator never registered as an alias.
func TestLookupByNameWithCanonicals_SubstringFallback(t *testing.T) {
	canonicalFakes(t,
		[]byte("%9\t900\t✳ The Bosun Returns\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		900: "claude\x00--resume\x00The\x00Bosun\x00Returns\x00",
	})

	got, ambiguous, err := w.LookupByNameWithCanonicals(
		context.Background(), "bosun", canonicalSetup())
	if err != nil || ambiguous {
		t.Fatalf("err=%v ambiguous=%v", err, ambiguous)
	}
	if got != "%9" {
		t.Errorf("got %q, want %%9 (substring match should resolve)", got)
	}
}

// The substring-ambiguity discipline pin
// (TestPin_CanonicalNoSilentGuess_SubstringAmbiguous) lives in
// pin_test.go per ADR-0001.

// Sanity: unambiguous exact matches still resolve correctly, even
// when other canonicals have non-overlapping aliases. Regression-
// shaped — the discipline-pin partner is in pin_test.go.
func TestLookupByNameWithCanonicals_DistinctAliases_NoFalseAmbiguity(t *testing.T) {
	canonicalFakes(t,
		[]byte("%5\t500\tsurveyor\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		500: "claude\x00--resume\x00surveyor\x00",
	})

	canonicals := []CanonicalAgent{
		{Name: "admin", Aliases: []string{"Alcatraz Infra Admin"}},
		{Name: "surveyor"},
		{Name: "pilot", Aliases: []string{"Pilot"}},
	}
	got, ambiguous, err := w.LookupByNameWithCanonicals(
		context.Background(), "surveyor", canonicals)
	if err != nil || ambiguous {
		t.Fatalf("err=%v ambiguous=%v", err, ambiguous)
	}
	if got != "%5" {
		t.Errorf("got %q, want %%5", got)
	}
}

// PaneAgentNameWithCanonicals: exact running name resolves to canonical.
func TestPaneAgentNameWithCanonicals_ExactToCanonical(t *testing.T) {
	canonicalFakes(t,
		[]byte("%3\t300\t✳ Surveyor\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		300: "claude\x00--resume\x00surveyor\x00",
	})

	got, _, err := w.PaneAgentNameWithCanonicals(
		context.Background(), "%3", canonicalSetup())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "surveyor" {
		t.Errorf("got %q, want surveyor", got)
	}
}

// PaneAgentNameWithCanonicals: alias resolves back to canonical name.
// This is the 2026-05-31 incident scenario: a pane runs `--resume
// "Master Bosun of Nimbus"`, the canonical name is `bosun`, the alias
// list closes the gap.
func TestPaneAgentNameWithCanonicals_AliasToCanonical(t *testing.T) {
	canonicalFakes(t,
		[]byte("%2\t200\t✳ Master Bosun of Nimbus\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		200: "claude\x00--resume\x00Master\x00Bosun\x00of\x00Nimbus\x00",
	})

	got, ambiguous, err := w.PaneAgentNameWithCanonicals(
		context.Background(), "%2", canonicalSetup())
	if err != nil || ambiguous {
		t.Fatalf("err=%v ambiguous=%v", err, ambiguous)
	}
	if got != "bosun" {
		t.Errorf("got %q, want bosun (alias resolution)", got)
	}
}

// PaneAgentNameWithCanonicals: no canonical matches → raw --resume
// value passed through. Keeps the helper useful for tests / contexts
// without a canonical registry.
func TestPaneAgentNameWithCanonicals_PassthroughWhenNoMatch(t *testing.T) {
	canonicalFakes(t,
		[]byte("%8\t800\t✳ Stranger\tclaude\n"),
		nil)
	w := canonicalWalker(map[int]string{
		800: "claude\x00--resume\x00Stranger\x00",
	})

	got, _, err := w.PaneAgentNameWithCanonicals(
		context.Background(), "%8", canonicalSetup())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "Stranger" {
		t.Errorf("got %q, want Stranger (raw --resume value, no canonical)", got)
	}
}

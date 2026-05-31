package discover

import (
	"context"
	"testing"
)

// Surveyor v0.2.0 review Q(a): the exact-match pass walked canonicals
// in slice order and returned the first hit. If two canonicals both
// have an alias that matches the running --resume value (e.g. admin
// has alias "claude" AND pilot has alias "claude"), the resolver
// silently picks by slice order rather than flagging ambiguous.
// Discipline-pin: we never silently guess between two exact matches.

func TestLookupByNameWithCanonicals_ExactMatchAmbiguous_AliasCollision(t *testing.T) {
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

func TestLookupByNameWithCanonicals_ExactMatchAmbiguous_AliasIsAnotherCanonical(t *testing.T) {
	// canonical "pilot" exists; canonical "admin" has alias "pilot".
	// Pane runs --resume "pilot". Both admin (via alias) AND pilot
	// (via canonical name) exact-match.
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

func TestPaneAgentNameWithCanonicals_ExactMatchAmbiguous(t *testing.T) {
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

// Sanity: unambiguous exact matches still resolve correctly, even
// when other canonicals have non-overlapping aliases.
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

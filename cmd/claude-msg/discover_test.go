package main

import (
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

func TestFindCanonicalForAlias_WhitespaceBoundedMatch(t *testing.T) {
	canonicals := []store.Agent{
		{Name: "bosun"},
		{Name: "pilot"},
		{Name: "surveyor"},
	}
	// "Master Bosun of Nimbus" — bosun is a whole word.
	if got := findCanonicalForAlias(canonicals, "Master Bosun of Nimbus"); got != "bosun" {
		t.Errorf("got %q, want bosun", got)
	}
	// "Pilot Console" — pilot is a whole word (case-insensitive).
	if got := findCanonicalForAlias(canonicals, "Pilot Console"); got != "pilot" {
		t.Errorf("got %q, want pilot", got)
	}
}

func TestFindCanonicalForAlias_RejectsAmbiguous(t *testing.T) {
	canonicals := []store.Agent{
		{Name: "bosun"},
		{Name: "pilot"},
	}
	// Both canonicals appear as whole words → ambiguous → empty.
	if got := findCanonicalForAlias(canonicals,
		"Bosun and Pilot together"); got != "" {
		t.Errorf("ambiguous match should return empty; got %q", got)
	}
}

func TestFindCanonicalForAlias_RejectsExactMatch(t *testing.T) {
	canonicals := []store.Agent{{Name: "bosun"}}
	// Exact match isn't an alias situation — the discover happy path
	// handles it as "unchanged".
	if got := findCanonicalForAlias(canonicals, "bosun"); got != "" {
		t.Errorf("exact match should return empty; got %q", got)
	}
}

func TestFindCanonicalForAlias_RejectsEmbeddedSubstring(t *testing.T) {
	canonicals := []store.Agent{{Name: "ai"}}
	// "Pair Programming" contains "ai" embedded but not as a whole
	// word. Reject to avoid false positives.
	if got := findCanonicalForAlias(canonicals, "Pair Programming"); got != "" {
		t.Errorf("embedded-substring match should be rejected; got %q", got)
	}
}

func TestFindCanonicalForAlias_NoMatchReturnsEmpty(t *testing.T) {
	canonicals := []store.Agent{
		{Name: "bosun"},
		{Name: "pilot"},
	}
	if got := findCanonicalForAlias(canonicals, "Surveyor"); got != "" {
		// Surveyor isn't in canonicals — so this is a brand-new agent.
		t.Errorf("no-match should return empty; got %q", got)
	}
}

func TestTokenizeForAliasMatch_WhitespaceAndCase(t *testing.T) {
	tokens := tokenizeForAliasMatch("Master Bosun of Nimbus")
	want := []string{"master", "bosun", "of", "nimbus"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %v, want %v", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Errorf("tokens[%d] = %q, want %q", i, tokens[i], want[i])
		}
	}
}

package provider

import "testing"

// The constant values ARE the wire contract — they must equal the exact strings
// the adapters write via store.SetProvider (#448). A change here is a
// provider-rename and must move in lockstep with the adapters + the two
// provider-keyed maps; this pin makes an accidental edit loud.
func TestConstantValues(t *testing.T) {
	if Anthropic != "anthropic" {
		t.Errorf("Anthropic = %q, want %q", Anthropic, "anthropic")
	}
	if OpenAI != "openai" {
		t.Errorf("OpenAI = %q, want %q", OpenAI, "openai")
	}
}

// All() enumerates every known provider exactly once, no empties. The
// provider-keyed maps' guards iterate this, so a duplicate or empty entry would
// corrupt their drift checks.
func TestAll(t *testing.T) {
	all := All()
	if len(all) != 2 {
		t.Fatalf("All() = %v, want 2 providers", all)
	}
	seen := map[string]bool{}
	for _, p := range all {
		if p == "" {
			t.Error("All() contains an empty provider id")
		}
		if seen[p] {
			t.Errorf("All() contains duplicate %q", p)
		}
		seen[p] = true
	}
	if !seen[Anthropic] || !seen[OpenAI] {
		t.Errorf("All() = %v, want it to contain both Anthropic and OpenAI", all)
	}
}

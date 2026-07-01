package store

import (
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/provider"
)

// TestRecipientQueueCapFloor pins the per-provider floors (#412) and guards the
// #580-style dead-key trap: codex's real provider value is provider.OpenAI, NOT
// "openai-codex". Keying on the latter would silently never apply the floor.
func TestRecipientQueueCapFloor(t *testing.T) {
	if got := recipientQueueCapFloor(provider.OpenAI); got != 20 {
		t.Errorf("recipientQueueCapFloor(OpenAI) = %d, want 20 (codex)", got)
	}
	if got := recipientQueueCapFloor(provider.Anthropic); got != 0 {
		t.Errorf("recipientQueueCapFloor(Anthropic) = %d, want 0 (no floor; claude keeps the passed cap)", got)
	}
	if got := recipientQueueCapFloor(""); got != 0 {
		t.Errorf("recipientQueueCapFloor(\"\") = %d, want 0 (conservative — keep the caller's cap)", got)
	}
	// Dead-key guard: codex registers provider "openai" (cmd/tmux-tell-codex),
	// so "openai-codex" must NOT be the key — a non-zero here means the dead-key
	// trap (#580's fanoutthrottle pool key) reappeared in this map. This stays a
	// raw literal on purpose: it is the WRONG value, so it has no constant.
	if got := recipientQueueCapFloor("openai-codex"); got != 0 {
		t.Errorf("recipientQueueCapFloor(\"openai-codex\") = %d, want 0 — codex's real provider is provider.OpenAI; key on the value agents.provider carries", got)
	}

	// Enumerator drift-guard: every key in the cap map MUST be a canonical
	// provider. A future entry keyed on a non-provider string (the drift class)
	// fails here — the #600 anti-drift the shared constants make structural.
	known := map[string]bool{}
	for _, p := range provider.All() {
		known[p] = true
	}
	for key := range recipientQueueCapByProvider {
		if !known[key] {
			t.Errorf("recipientQueueCapByProvider has key %q not in provider.All() — dead key (#412/#580 drift class)", key)
		}
	}
}

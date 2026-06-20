package store

import "testing"

// TestRecipientQueueCapFloor pins the per-provider floors (#412) and guards the
// #580-style dead-key trap: codex's real provider value is "openai", NOT
// "openai-codex". Keying on the latter would silently never apply the floor.
func TestRecipientQueueCapFloor(t *testing.T) {
	if got := recipientQueueCapFloor("openai"); got != 20 {
		t.Errorf("recipientQueueCapFloor(\"openai\") = %d, want 20 (codex)", got)
	}
	if got := recipientQueueCapFloor("anthropic"); got != 0 {
		t.Errorf("recipientQueueCapFloor(\"anthropic\") = %d, want 0 (no floor; claude keeps the passed cap)", got)
	}
	if got := recipientQueueCapFloor(""); got != 0 {
		t.Errorf("recipientQueueCapFloor(\"\") = %d, want 0 (conservative — keep the caller's cap)", got)
	}
	// Dead-key guard: codex registers provider "openai" (cmd/tmux-tell-codex),
	// so "openai-codex" must NOT be the key — a non-zero here means the dead-key
	// trap (#580's fanoutthrottle pool key) reappeared in this map.
	if got := recipientQueueCapFloor("openai-codex"); got != 0 {
		t.Errorf("recipientQueueCapFloor(\"openai-codex\") = %d, want 0 — codex's real provider is \"openai\"; key on the value agents.provider carries", got)
	}
}

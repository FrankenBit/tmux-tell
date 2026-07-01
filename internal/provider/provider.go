// Package provider holds the canonical adapter provider identifiers — the
// `provider` string each adapter writes via store.SetProvider (#448) and that
// the provider-keyed maps key on (the fanout-throttle pool #580, the
// recipient-cap floor #412).
//
// Before this package the literals ("anthropic", "openai") were hand-synced
// across the adapters (cmd/tmux-tell-*), the maps, and the maps' drift-guards.
// When a key drifted from the literal an adapter writes, the map entry became
// dead code that silently fell through to a default — the n=2 failure class:
// #597 (a fanout pool keyed "openai-codex" matched nothing) and #412 (the
// recipient-cap floor). Centralising the literals here makes such drift a
// compile error, and lets both guards enumerate the known set via All()
// instead of re-hard-coding it.
package provider

const (
	// Anthropic is the provider the claude adapter writes
	// (cmd/tmux-tell-claude); the model runs on Anthropic's API.
	Anthropic = "anthropic"
	// OpenAI is the provider the codex adapter writes
	// (cmd/tmux-tell-codex); the model runs on OpenAI's API.
	OpenAI = "openai"
)

// All returns the canonical provider identifiers that have a live adapter, in a
// stable order. Both provider-keyed maps' drift-guards enumerate this set, but
// enforce it asymmetrically:
//   - fanout-throttle (forward): every All() member MUST resolve to its own
//     throttle, so adding a constant here without a poolThrottles entry fails
//     the guard.
//   - recipient-cap (reverse-only): every cap-map key must be in All(), but a
//     provider with no cap entry is fine — a queue floor is opt-in (anthropic
//     has none). Adding a constant here does NOT require a cap entry.
//
// Either way a key can't drift off the canonical set into silent dead code. A
// forward provider (e.g. an ollama-backed adapter) is added here together with
// its throttle entry (and a cap entry only if it needs a floor), never as a
// bare map key.
func All() []string {
	return []string{Anthropic, OpenAI}
}

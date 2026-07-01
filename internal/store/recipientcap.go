package store

import "git.frankenbit.de/frankenbit/tmux-tell/internal/provider"

// recipientQueueCapByProvider holds per-provider floors for the recipient-queue
// cap (#412). A sender passes a cap (the default capRecipientQueue = 5, or a
// `--max-recipient-queue` override); checkCapsInTx floors that value UP to the
// recipient provider's entry here, so a slow-draining adapter gets a deeper queue
// without every sender having to know to ask for one.
//
// v1-sketched (Bosun-ratified 2026-06-20); tuning is empirical — if a provider's
// queue behaviour misbehaves, file a follow-up with evidence rather than guessing
// here. The key is the agent's `provider` (#448), structurally symmetric with the
// CLI per-pool fan-out throttle (internal/cli/fanoutthrottle.go's poolThrottles).
//
// NOTE on the key: codex's real provider value is "openai"
// (cmd/tmux-tell-codex/main.go), NOT "openai-codex" — this map keys on the value
// agents.provider actually carries, verified against the live agents table.
//
// Why codex (openai) earns a deeper queue: codex paste-and-enter delivery drains
// at ~6s/message vs ~0.7s for claude (#412 store-timestamp measurement), because
// the 5s post-deliver cooldown (#449) is load-bearing collision protection that
// cannot be safely shrunk under the current classifier (#590, codex false-idles
// mid-turn). A deeper queue trades the `recipient queue full` rejection (message
// lost, sender must re-send) for honest delay (the message drains slowly) — the
// right trade for a message bus. The cadence fix that would let the cooldown
// shrink is tracked separately as #592 (substrate busy-lease).
var recipientQueueCapByProvider = map[string]int{
	provider.OpenAI: 20, // codex
}

// recipientQueueCapFloor returns the minimum recipient-queue cap for a provider,
// or 0 when the provider has no floor (the caller's passed cap stands). An empty
// or unrecognised provider returns 0 — conservative: keep the caller's cap rather
// than widening a queue for an adapter we can't characterise.
func recipientQueueCapFloor(provider string) int {
	return recipientQueueCapByProvider[provider]
}

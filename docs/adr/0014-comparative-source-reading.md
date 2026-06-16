# ADR-0014 Background: Comparative Source-Reading — IS / IS NOT Evidence

> **Background doc for** [ADR-0014](0014-tmux-tell-scope-and-cross-host-reach.md)  
> **Date**: 2026-06-16  
> **Author**: Pilot  
> **Feeds**: tmux-tell #442 AC 7 ("scope-ADR updated with comparative findings")

Full source citations and per-framework topology analysis live in the Binnacle companion doc
(Binnacle PR #413, `docs/framework-comparison.md`, 2026-06-15, Pilot). This doc maps those
findings onto ADR-0014's IS / IS NOT lists and states the amendment verdict.

**SHAs:** tmux-tell `bca8d6f3` · AutoGen `027ecf0a` · CrewAI `a5cc6f6d` · Claude Agent SDK `634c2f61`

---

## Framework summaries (topology)

**AutoGen-core** (`microsoft/autogen @ 027ecf0a`): broker-flat. Any agent sends to any other
via `SingleThreadedAgentRuntime._message_queue`; no coordinator enforced in the substrate.
Single-process asyncio only — no cross-process or host-level isolation. The `agentchat` layer
ships `GroupChatManager` as a hierarchy primitive above the flat substrate.

**CrewAI** (`crewAIInc/crewAI @ a5cc6f6d`): orchestrator-mandatory. All task routing passes
through `Crew.kickoff()` with a `Process` enum selecting sequential (no inter-agent messaging)
or hierarchical (all through `manager_agent`). No peer-to-peer path exists.

**Claude Agent SDK** (`anthropics/claude-agent-sdk-python @ 634c2f61`): coordinator→subagents,
depth-1 hard limit. `SessionKey` types encode parent vs. subagent at the type level. No
peer-to-peer. SDK drives Claude Code as a subprocess.

---

## IS list — evidence per item

**IS #1 — "peer-style coordination bus"**
AutoGen-core is also broker-flat (same structural move: symmetric send_message, no enforced
coordinator). CrewAI and the Claude Agent SDK embed hierarchy in the substrate, confirming that
peer-style is not the design default. **CONFIRMED, but needs qualification** (see amendment
below): "peer-style" alone doesn't distinguish tmux-tell from AutoGen-core.

**IS #2 — "TUI-paste delivery substrate with operator-presence-aware deferral"**
None of the three frameworks have paste-and-enter delivery, an observe-gate, or
operator-presence deferral. AutoGen uses in-process asyncio calls; CrewAI uses synchronous
Python calls; Claude Agent SDK uses HTTP to a Claude Code subprocess.
**CONFIRMED UNIQUE** — no peer analogue.

**IS #3 — "SQLite-backed persistence"**
AutoGen: ephemeral `asyncio.Queue`, no cross-session persistence. CrewAI: SQLite for
task-output snapshots (`latest_kickoff_task_outputs.db`), not message-level persistence.
Claude Agent SDK: `SessionStore` for transcript mirroring; base `query()` is stateless.
tmux-tell's message-level SQLite queue (cross-session delivery, StateDeferred) has no peer
equivalent. **CONFIRMED.**

**IS #4 — "Substrate-vs-adapter boundary (Profile seam, ADR-0009)"**
AutoGen seam: `ChatCompletionClient` ABC (model routing). CrewAI seam: per-agent
`llm: str | BaseLLM` + LiteLLM fallback (model routing). Claude Agent SDK seam: `model: str`
string field, Anthropic-only. tmux-tell `Profile` seam: gates delivery mechanism
(`PasteCapable`, `SupportsMCPSlashCommand`, `Pane` classifier) — not model routing.
**CONFIRMED DISTINCT in kind:** the concern is delivery-mechanism, not LLM-backend selection.

**IS #5 — "Hook-context delivery mode"**
No peer equivalent. AutoGen/CrewAI are in-process; Claude Agent SDK is HTTP subprocess. The
hook-context path (adapter-side hook injects messages as `additionalContext` rather than paste)
is tmux-tell-specific. **CONFIRMED UNIQUE.**

**IS #6 — "MCP server surface"**
AutoGen and CrewAI are Python-SDK-native with no MCP bus surface. Claude Agent SDK can consume
MCP tools but does not expose the bus as an MCP server. tmux-tell's MCP server (agent
self-registration, send, inbox) is the substrate's primary non-CLI access path. **CONFIRMED.**

**IS #7 — "Host-local trust boundary (per-user UID, per-host SQLite)"**
AutoGen: single-process, no host concept. CrewAI: single-machine default; `agent.a2a` allows
per-agent remote delegation, but the Crew itself stays single-machine. Claude Agent SDK:
Anthropic-cloud-hosted; shared API key across coordinator + subagents. tmux-tell's per-user
UID + per-host SQLite = OS-level isolation. **CONFIRMED** (also the load-bearing justification
for IS NOT #3).

**IS #8 — "Forward extensible at the adapter axis (new binary = new Profile)"**
All three frameworks are Python/TypeScript packages where adding an adapter means adding a
class, not a binary. tmux-tell's binary-per-adapter pattern has no peer equivalent.
**CONFIRMED.**

---

## IS NOT list — evidence per item

**IS NOT #1 — "Generic message broker (NATS / RabbitMQ style)"**
None of the three frameworks are generic brokers. AutoGen-core's broker-flat shape specifically
does NOT make it a general-purpose message broker — it is tied to asyncio and Python objects.
The TUI-paste constraint is what makes tmux-tell narrower. **CONFIRMED.**

**IS NOT #2–6**: Confirmed by absence across all three frameworks. Real-time streaming,
multi-tenant network-addressable agents, browser/web UI, E2E encryption, and persistent service
for non-LLM consumers are out-of-scope for all peers as well.

---

## Verdict on the substrate-flat + conventions-hierarchical tenet

**Defensible, with one nuance to name.**

The tenet holds squarely against CrewAI and the Claude Agent SDK: both embed hierarchy in the
substrate (Crew orchestrator, coordinator→subagent depth-1 type-level enforcement). tmux-tell's
choice to keep the substrate flat with conventions carrying hierarchy is a genuine design
distinction from these peers.

The nuance is AutoGen-core: it is also broker-flat at the substrate level (same structural
move). The differentiator is mechanism, not topology: tmux-tell is **cross-process** flat
(OS-level pane delivery, host-local SQLite, per-user UID isolation), while AutoGen-core's flat
substrate is single-process asyncio only. Additionally, AutoGen ships a hierarchy layer
(`agentchat`/`GroupChatManager`) as a first-class framework component; tmux-tell does not ship
any hierarchy layer and leaves it entirely to operator convention.

The full substrate-honest claim:

> tmux-tell is flat at the substrate level (any chamber → any chamber, no coordinator in the
> dispatch path) AND cross-process (OS-level pane delivery, host-local SQLite). AutoGen-core
> shares the flat-substrate structural move but is single-process only and ships a hierarchy
> layer (`agentchat`) on top. CrewAI and the Claude Agent SDK embed hierarchy in the substrate
> itself.

---

## ADR-0014 amendment recommendation

One IS item warrants a qualification clause. All others are confirmed as-written.

### IS #1 — proposed qualification

Current:
> "A peer-style coordination bus for operator-launched LLM-CLI sessions (Claude, Codex, …)
> attached to tmux panes on a single host."

Proposed:
> "A **cross-process** peer-style coordination bus for operator-launched LLM-CLI sessions
> (Claude, Codex, …) attached to tmux panes on a single host. The cross-process + TUI-paste
> delivery combination distinguishes tmux-tell from other broker-flat substrates (e.g.
> AutoGen-core, which is also peer-flat but single-process asyncio only)."

**Rationale:** "peer-style" alone doesn't distinguish from AutoGen-core. "Cross-process" is the
load-bearing qualifier that explains the different trust model (per-user UID isolation),
persistence need (SQLite survives process restarts), and TUI-paste delivery mechanism — none of
which arise in a single-process flat substrate.

### IS NOT list — no amendments

All IS NOT items confirmed as-written. No additions or removals proposed.

### Operator note

The IS #1 qualification is additive (clarification only; no IS/IS NOT semantics change). The
amendment can be applied directly to ADR-0014 at operator's preferred timing.

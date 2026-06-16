---
arc42-section: 8
revisit-triggers:
  - a new cross-cutting concept emerges (one that touches multiple components)
  - an ADR lands that should anchor a subsection here
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §8 Crosscutting Concepts

Concepts that pervade multiple [components](05-building-block-view.md) and so
belong to none of them. Each subsection frames the concept inline and links its
ADR / `docs/` / code home for depth (link-first).

## §8.1 Identity resolution

A sender resolves to exactly one agent via a fixed precedence chain:
`TMUX_AGENT_NAME` → `TMUX_PANE` → the `agents` registry (`pane_id`). This makes
attribution provable — no "the mailman did it" black hole. The codex MCP wrapper
carries an env-block discipline so the chain resolves correctly under MCP (#320,
#384). Depth: [docs/security.md §3.2](../security.md), `internal/identity`.

## §8.2 Substrate-state-claim integrity (the load-bearing invariant)

The system **never claims a state it hasn't verified.** Delivery is `delivered`
only after a verify-token round-trip; otherwise it's honestly `delivered_in_input_box`.
This is the project's signature discipline and the [§10](10-quality-requirements.md)
NFR; it recurs at the doc layer too (the Arc42 freshness convention). Depth:
[§6](06-runtime-view.md), [docs/observe-gate.md](../observe-gate.md).

## §8.3 Error classification

Failures carry structured, closed-set reasons rather than one opaque error:
`ping` reachability is a three-way `class` (`reachable`/`pending`/`unreachable`)
over a finer `reason` (`mailman_down`/`stuck`/`pane_dead`/`blocked_delivery`/
`backlog_draining`) (#358, #366). The operator's recovery differs per class.
Depth: `internal/healthscan`, the `ping` reference.

## §8.4 Caps + admission control

`capRecipientQueue` (per recipient) + `capSenderBacklog` (per sender→recipient pair,
#296) bound backlog so a misbehaving sender can't storm a mailbox; enforcement is
DB-level via `_txlock=immediate` + `busy_timeout` (#29). `refresh-all-mcps` is
additionally cap-protected + operator-explicit-only. Depth:
[docs/security.md §3.3](../security.md), [§4 Caps](../reference.md).

## §8.5 The `Profile` abstraction (substrate-vs-adapter boundary)

Per-LLM-CLI TUI traits (paste-collapse marker, MCP-slash-command support,
settle-delay) live in a `Profile` behind the substrate-vs-adapter boundary, so the
substrate stays vendor-agnostic and a new adapter is a new binary + flags. Depth:
[ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md), `cmd/`.

## §8.6 WAL + single-writer concurrency

One mailman per recipient (single-writer-per-mailbox) + WAL journaling + immediate
transactions give consistency under concurrent senders without a server process.
Depth: [docs/security.md §3.3 / §3.5](../security.md), `internal/store`.

## §8.7 Control-command boundary

Peer + operator control commands are gated by a whitelist; source-code edits are the
only control-command boundary (#24/#25/#28). Depth:
[docs/security.md §3.1](../security.md), `internal/control`.

## §8.8 Logging + observability

Structured `WARN`/log lines (fail-loud, never fail-silent) + a Prometheus surface
(`tmux_tell_*`, `internal/metrics`) make the substrate legible. Depth:
[docs/diagnostic-playbook.md](../diagnostic-playbook.md), the README Observability section.

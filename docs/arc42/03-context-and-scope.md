---
arc42-section: 3
revisit-triggers:
  - ADR-0014 scope statement is amended (IS / IS-NOT / planned-but-distinct lists)
  - a new adapter axis is added (new LLM-CLI)
  - the host-locality model changes (#312) or cross-host reach lands
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A initial cut — #386) -->

# §3 Context and Scope

The canonical, operator-ratified scope statement — *what tmux-tell IS, IS NOT,
and what is planned-but-distinct* — is **[ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md)**.
That ADR is the source of truth; this section frames the context an architect
needs and links the lists rather than restating them (link-first, per the
[Arc42 spine ADR](../adr/0015-adopt-arc42-architecture-spine.md)).

## Business / system context

```
        operator
           │  launches + attaches panes, may be typing
           ▼
   ┌─────────────────────────────────────────────┐
   │  one host, one UID                           │
   │                                              │
   │   agent A ──send──▶ [   tmux-tell   ]──paste─▶ agent B
   │   (tmux pane)        SQLite + mailmen        (tmux pane)
   │                          │                   │
   │                      observe-gate (defers while operator types)
   └─────────────────────────────────────────────┘
```

- **Actors**: operator-launched LLM-CLI sessions (Claude, Codex, …)
  in tmux panes, plus the operator (who can send too).
- **What gets delivered**: addressed text messages (name → name), each a durable
  row with a lifecycle (`queued → delivering → delivered/failed`). *Not* file
  transfer, RPC, or a job queue.
- **Trust boundary**: a single host, a single UID, OS-level isolation. tmux-tell has
  no network listener — the property [#312](https://git.frankenbit.de/frankenbit/tmux-tell/issues/312)
  coins as **"bus-host-locality"**, and which this document calls **host-locality**:
  it is local to the host; cross-host reach is *planned via SSH-back-tunnels
  that compose with host-locality rather than replicating the bus* (ADR-0014).

## Scope — see ADR-0014 for the binding lists

| | Summary (binding text in [ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md)) |
|---|---|
| **IS** | peer-style directed messaging · TUI-paste delivery w/ observe-gate · SQLite persistence · substrate-vs-adapter boundary · hook-context mode · MCP server surface · host-local trust boundary · forward-extensible at the adapter axis |
| **IS NOT** | a networked message queue · a multi-host bus · a chat app · a job scheduler (docs/why.md §What-it-is-and-isn't) |
| **Planned but distinct** | cross-host reach via SSH-back-tunnel (composes with, does not replace, host-locality) |

## External interfaces

- **tmux** — the delivery substrate (paste into a pane) and the runtime the agents
  live in.
- **systemd-user** — runs the per-agent mailman daemons (see [§7](07-deployment-view.md)).
- **MCP** — the agent-facing self-registration + send surface.
- **Forgejo** — issue tracking + CI; not part of the running system.

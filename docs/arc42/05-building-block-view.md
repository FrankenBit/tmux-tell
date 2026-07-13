---
arc42-section: 5
revisit-triggers:
  - a top-level component is added or removed
  - the PaneProfile shape changes
  - the substrate-vs-adapter boundary moves
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §5 Building Block View

The static component view — the boxes and what each owns. The component shape is
stable; per-component internal evolution (e.g. `Profile` content) doesn't
invalidate it, so this is a thin map that links the code + [reference](../reference.md)
for depth.

## Level 1 — the system

```
  cmd/tmux-tell-claude ─┐                  ┌─ tmux pane (claude)
  cmd/tmux-tell-codex  ─┤   adapters       ├─ tmux pane (codex)
                        │  (thin entry      │
                        ▼   points)         │
   ┌──────────────────────────────────────────────────────┐
   │                  substrate (internal/)                │
   │  store ── identity ── cli(mailman) ── tmuxio          │
   │    │         │            │            │              │
   │  mcp ── control ── render ── metrics ── discover      │
   └──────────────────────────────────────────────────────┘
            │ paste-and-enter / hook-context
            ▼
        tmux panes (the agents)
```

## Components (substrate, `internal/`)

| Package | Responsibility |
|---|---|
| `store` | the SQLite message store — messages, agents, lifecycle; WAL + `BEGIN IMMEDIATE` cap enforcement |
| `cli` | command surface + the **mailman** delivery loop (per-recipient, single-writer) |
| `tmuxio` | tmux interaction — `capture-pane`, cursor query, `paste-buffer`, send-keys |
| `identity` | the sender-resolution chain (`TMUX_AGENT_NAME` → `TMUX_PANE` → registry) |
| `mcp` | the agent-facing MCP server surface (register / send / inbox / …) |
| `control` | peer + operator control commands (the whitelisted control-command boundary) |
| `render` | message chrome (the `[Sender · time · id]` framing, compact/quick forms) |
| `metrics` | Prometheus surface (`tmux_tell_*`) |
| `discover` | walks tmux state to (re)populate the agents table |
| `healthscan` | reachability / state probing (feeds `ping`, `doctor`) |
| `config` | per-agent TOML config (`/etc/tmux-tell/config.toml`) |
| `sdnotify` | systemd readiness integration for the mailman units |
| `version` | build/version stamp |

## Components (adapters, `cmd/`)

- **`tmux-tell-claude`**, **`tmux-tell-codex`** — thin per-LLM-CLI entry points around
  the shared substrate, each carrying its `Profile` (TUI traits: paste-collapse
  marker, MCP-slash-command support, settle-delay). The substrate stays
  vendor-agnostic; the adapter is where the CLI-specific knowledge lives
  ([ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)).

> Depth: [docs/reference.md](../reference.md) (the command set + storage schema) +
> the package source under `internal/` and `cmd/`.

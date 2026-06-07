# tmux-msg-claude — the Claude Code adapter

`tmux-msg-claude` is the **adapter binary** for the **tmux-msg substrate**: the
CLI tool that a Claude Code session inside a tmux pane uses to send, receive, and
diagnose messages on the bus. The binary name encodes both halves —
`tmux-msg` (the substrate) + `claude` (the adapter) — per the #174 Option 2
naming decision (see ADR-0003 for the substrate-vs-flavor framing this realizes
structurally).

This note draws the **substrate ↔ adapter boundary** so a future
`tmux-msg-codex` / `tmux-msg-copilot` adapter knows what to reuse and what to
re-implement.

## What is substrate (lives in `internal/`, reused by every adapter)

These packages are CLI-tool-agnostic. A codex or copilot adapter composes the
exact same code:

| package | substrate responsibility |
|---|---|
| `internal/store` | SQLite mailbox: messages + agents tables, the message/state vocabulary, caps, the delivery lifecycle (`queued → delivering → delivered/failed`), the `verified` marker (#169), replay linkage (#157) |
| `internal/tmuxio` | tmux mechanics: pane liveness, the observe-gate, paste-and-Enter delivery, the verify-token probe |
| `internal/render` | the bracket-header message chrome (`[From · clock · id]`), reply/replay/length markers |
| `internal/discover` | pane-id ↔ agent-name resolution from live tmux state |
| `internal/identity` | resolve "who am I" from env + the agents registry |
| `internal/healthscan` | journald-derived per-agent health audit |
| `internal/config` | host-level TOML config (caps, delivery modes, thresholds) |
| `internal/mcp` | the MCP server framing (stdio transport, tool registration) |
| `internal/control` | whitelisted slash-command dispatch + scope gating |
| `internal/sdnotify`, `internal/version` | systemd watchdog notify; version stamping |

The mailbox schema and the `discover` / `store` / `tmuxio` Go API are an
**external contract** (ADR-0007) governed by the deprecation policy (ADR-0008).

## What is adapter (lives here, in `cmd/tmux-msg-claude/`)

This directory is the Claude-Code-specific composition of the substrate:

- **Subcommand dispatch** (`main.go` + the per-subcommand `run*CLI` files) — the
  `tmux-msg-claude send|serve|ping|…` surface.
- **The mailman daemon** (`serve.go`) — composes the substrate's delivery loop,
  observe-gate, and store into the per-agent daemon. *This is substrate-shaped
  code that currently lives adapter-side* — see "Deferred" below.
- **MCP handler wiring** (`mcp.go`) — registers the `tmux-msg.*` tools onto the
  substrate's MCP server and binds them to the subcommand cores.
- **Claude-specific identity** — reads `$CLAUDE_AGENT_NAME` (the env-var rename
  to `$TMUX_AGENT_NAME` is #177 PR2).
- **systemd integration** (`systemctl.go`) — the `tmux-msg-claude-mailman@`
  template instance names.
- **Send-response schema** (`sendstatus.go`) — `SendResponse` + its named blocks
  (`recipient`/`delivery`/`thread_freshness`/`replay`).

A future adapter would mirror this directory: its own `cmd/tmux-msg-codex/` with
codex-specific identity (`$CODEX_AGENT_NAME`), its own subcommand surface, and
its own daemon composition — reusing every `internal/` package above unchanged.

## Deferred: physical extraction of the daemon loop

The mailman/serve loop in `serve.go` is *genuinely substrate-shaped* — a second
adapter would want to reuse it rather than re-implement it. The clean move is to
extract it to `internal/serve` (or a `pkg/` if external consumers need it). That
extraction is **deliberately deferred** until a second adapter actually
materializes (#177 ships the multi-binary shape, not the codex/copilot adapters):

- Extracting before there is a second consumer is premature abstraction — the
  "right" seam is the one the *second* adapter's needs reveal, not a guessed one.
- The multi-binary layout + the shared `internal/` packages already deliver the
  extensibility this rename is about; a codex adapter can drop in today and reuse
  ~90% of the code. The serve-loop extraction is the last ~10%, best shaped by
  the real second consumer.

When that second adapter lands, its issue extracts `serve.go`'s substrate core to
`internal/serve` and leaves only the adapter-specific composition here.

# tmux-tell-codex — OpenAI Codex adapter

`tmux-tell-codex` is the [Codex CLI](https://github.com/openai/codex) adapter for the
[tmux-tell](../../README.md) inter-agent message bus. It is a thin wrapper over the same
adapter-agnostic core as `tmux-tell-claude`; the two adapters coexist and share the bus DB.

## Install

```bash
sudo -A ./install.sh --adapter=codex   # installs tmux-tell-codex + mailman unit template
```

Coexists with the Claude adapter; each adapter gets its own
`tmux-tell-<adapter>-mailman@.service` unit template and both share the one message DB.

## Hook wiring (hook-context delivery)

Codex delivers messages to agents via `hook-context` — the hook injects pending messages as
`additionalContext` on the recipient's next turn. Wire it in `~/.codex/config.toml`:

```toml
[features]
hooks = true        # or pass --enable hooks on the codex command line

[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "tmux-tell-codex hook-context --from <agent> --event-name UserPromptSubmit"
```

`--event-name` is required: Codex validates that the hook output's `hookEventName` matches
the firing event, and its hook stdin schema is not documented. Pinning the event name
explicitly makes the command deterministic regardless of Codex's stdin shape. Wire one block
per event you enable (`SessionStart`, `UserPromptSubmit`, `PostToolUse`), each with its own
`--event-name`.

## MCP server (sender-resolution requirement)

When wiring `tmux-tell-codex mcp` as an MCP server, **Codex's MCP host does not propagate
`$TMUX_PANE`** to the spawned server process. The substrate's sender-resolution logic
(`$TMUX_AGENT_NAME` or `$TMUX_PANE → registry`) falls back to `$TMUX_AGENT_NAME` — but
that variable is also absent from the MCP server's environment unless explicitly injected.
Without it, `tmux-tell.send` calls fail to resolve the sender.

Supply the agent name via the `env` table in `~/.codex/config.toml`:

```toml
[mcp_servers.tmux-tell]
command = "tmux-tell-codex"
args = ["mcp"]
env = { TMUX_AGENT_NAME = "lookout" }
```

Replace `"lookout"` with the agent's registered name (the value passed to
`tmux-tell-codex register --name`). The name is hardcoded per deployment; a chamber rename
via `tmux-tell.register` requires a matching update here.

> **CLI path is unaffected.** `tmux-tell-codex send …` and `hook-context` both run in the
> operator's shell where the full environment is propagated; only the MCP server spawn path
> is isolated. See `docs/diagnostic-playbook.md` §MCP-path sender-unknown if calls via the
> MCP server fail to resolve sender identity.

## Reference

Full adapter integration docs, delivery-mode overview, and verification notes:
[`docs/reference.md` §Adapter integration](../../docs/reference.md#adapter-integration).

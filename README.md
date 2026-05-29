# cli-semaphore

Inter-agent message bus for Claude CLI sessions on alcatraz. Each Claude pane has a mailbox; senders post messages, a per-recipient mailman daemon serializes delivery into the target tmux pane.

The name is a pun: ship-to-ship semaphore signalling, and the OS synchronization primitive — both descriptions of the same thing here.

## Status

Design phase / scaffolding. See **the epic** for the roadmap and milestones.

## Architecture

```
            ┌────────────────────────────────────┐
   Bosun ──►│  SQLite mailbox (messages, agents) │──►  mailman@surveyor
            └────────────────────────────────────┘     (single writer to %3)
   Pilot ──►   (same DB; rows per recipient)

   Surveyor's reply ──► claude-msg send --reply-to <id> --to bosun "…"
```

**Senders** never touch tmux. They call `claude-msg send` which validates the message, checks caps, and inserts a row. **Mailmen** are per-agent systemd services that loop on their inbox, paste-buffer the formatted message into the target tmux pane, and mark it delivered.

Because each recipient has exactly one mailman, the obvious tmux concurrency bugs (paste-buffer race, idle-check TOCTOU, turn concatenation) collapse to a single-writer invariant.

## Storage

SQLite, WAL mode, two tables:

```sql
CREATE TABLE messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id     TEXT NOT NULL UNIQUE,           -- 7f3a — short, copy-pastable
  from_agent    TEXT NOT NULL,
  to_agent      TEXT NOT NULL,
  reply_to      TEXT REFERENCES messages(public_id),
  body          TEXT NOT NULL,
  state         TEXT NOT NULL DEFAULT 'queued', -- queued|delivering|delivered|failed
  created_at    TEXT NOT NULL DEFAULT (datetime('now','subsec')),
  delivered_at  TEXT,
  error         TEXT
);
CREATE INDEX ix_msg_queue ON messages(to_agent, state, id);

CREATE TABLE agents (
  name        TEXT PRIMARY KEY,
  pane_id     TEXT,                        -- "%3", refreshed by boot-time discovery
  paused      INTEGER NOT NULL DEFAULT 0,  -- the kill switch
  updated_at  TEXT NOT NULL DEFAULT (datetime('now','subsec'))
);
```

DB lives at `/var/lib/cli-semaphore/messages.db`.

## CLI shape (target)

```
claude-msg send  --from X --to Y [--reply-to ID] "body"   # one-shot, JSON response
claude-msg inbox AGENT                                    # list queued for AGENT
claude-msg serve --agent AGENT                            # mailman daemon (systemd-templated)
claude-msg status                                         # paused state + queue depths per agent
claude-msg pause  AGENT | --all
claude-msg resume AGENT | --all
claude-msg reset  --confirm [--hard]                      # purge queued; --hard also wipes audit log
claude-msg log    --thread ID                             # follow a reply chain
```

`send` returns `{"ok":true,"id":"7f3a","queued":3}` on success, `{"ok":false,"error":"…"}` on failure, with sysexits-style exit codes.

## Caps (MVP defaults)

| Cap | Default | Reason |
|---|---|---|
| **Per-recipient queue depth** | 5 | A pane that's not draining is wedged — fail fast, don't accumulate. |
| **Per-sender backlog** | 2 | One runaway loop in a single agent can't starve others. |
| **Body size** | 16 KB | Anything bigger should be a file reference, not a tmux paste. |

`send` rejects with `{"ok":false}` when any cap is exceeded.

## Kill switch & retention

- **Pause** sets `agents.paused = 1`. The mailman checks at the top of every loop iteration; queued messages keep accumulating (up to the cap) but nothing is injected. `resume` flips it back.
- **Retention** is free — SQLite on disk; on mailman start, any row left in `delivering` from a crashed run is reset to `queued`.
- **Reset** purges queued + delivering. `--hard` also wipes the delivered audit log. `--confirm` is mandatory either way.

## Message rendering

What the recipient sees in their pane:

```
─── Message from Bosun ── 11:04:12 ── id 7f3a ──
please check CI on PR 1234
────────────────────────────────────────────────
```

Replies carry the original id in the header:

```
─── Reply from Surveyor → Bosun ── re: 7f3a ── id 9c1d ──
looking now, ETA 3 min
────────────────────────────────────────────────
```

## Install

On a Linux host that has tmux, sqlite3, and Go (≥ 1.24):

```bash
git clone https://git.frankenbit.de/frankenbit/cli-semaphore.git
cd cli-semaphore
make build
sudo -A ./install.sh                  # uses sudo -A so a tmux-popup
                                      # askpass surfaces nicely on alcatraz
```

This:
- builds `bin/claude-msg`,
- installs the binary to `/usr/local/bin/claude-msg`,
- creates `/var/lib/cli-semaphore/` (operator-owned, holds `messages.db`),
- drops the systemd user template into the operator's `~/.config/systemd/user/`.

Then, **as the operator (not root)**:

```bash
# Make sure the user systemd manager keeps running across reboots:
sudo loginctl enable-linger $USER

# Reload the user manager so it sees the new template:
systemctl --user daemon-reload

# Populate the agents table from the current tmux state:
claude-msg discover

# Enable a mailman per agent you want to receive messages:
systemctl --user enable --now claude-mailman@surveyor.service

# Tail the mailman log:
journalctl --user -u claude-mailman@surveyor.service -f
```

Each Claude pane that should be able to *send* must export its identity
in its shell profile (matches the pane's `--resume <name>`):

```bash
export CLAUDE_AGENT_NAME=bosun
```

After that, `claude-msg send --to surveyor "…"`, `claude-msg whoami`, and
the rest of the read-side subcommands work without flags.

## Use from Claude Code (MCP)

The same binary speaks MCP over stdio under `claude-msg mcp` so each pane
gets `semaphore.send / agents / whoami / inbox / status` as native Claude
tools. Wire it up per pane:

```json
{
  "mcpServers": {
    "semaphore": {
      "command": "/usr/local/bin/claude-msg",
      "args": ["mcp"],
      "env": { "CLAUDE_AGENT_NAME": "bosun" }
    }
  }
}
```

## Roadmap

Tracked in the epic + milestone sub-issues — see the [Issues tab](../../issues).

## License

MIT.

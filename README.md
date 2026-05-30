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

The same binary speaks MCP over stdio under `claude-msg mcp`, exposing
`semaphore.send / control / agents / whoami / inbox / status / register
/ unregister` as native Claude tools. **One user-level config; identity
is auto-resolved per pane.**

Add the server once in `~/.claude.json` (or your equivalent Claude Code
config) — no per-pane env or config files needed:

```json
{
  "mcpServers": {
    "semaphore": {
      "command": "/usr/local/bin/claude-msg",
      "args": ["mcp"]
    }
  }
}
```

### How identity works

When Claude in a tmux pane spawns the semaphore MCP server, the child
process inherits `$TMUX_PANE` from the shell (tmux sets it automatically
for every pane it owns — `%1`, `%3`, etc.). The MCP server looks that
pane id up in the `agents` table and uses the matching agent name as the
session's identity.

So the workflow for a **new pane** is just one tool call from that pane:

> *Claude, please call `semaphore.register name=myname`*

The pane is auto-detected from `$TMUX_PANE`, the row is inserted, and
`systemctl --user enable --now claude-mailman@myname.service` runs in
the same step. Equivalent CLI fallback:

```bash
# from inside the new pane
CLAUDE_AGENT_NAME=myname claude-msg ...   # (CLI doesn't yet expose register;
                                          # fall back to SQL until then)
sqlite3 /var/lib/cli-semaphore/messages.db \
  "INSERT INTO agents (name, pane_id) VALUES ('myname', '$TMUX_PANE');"
systemctl --user enable --now claude-mailman@myname.service
```

### Whitelisted control commands

`semaphore.control` types a vetted Claude Code slash-command into a
pane — either the caller's own pane (most common: an agent quietly
asking itself to `/compact` at a quiescent point) or another agent's
pane (for benign peer nudges like retitling). The string is typed
directly (no chat header, no buffer) so Claude Code parses it exactly
as if the operator had typed it.

The whitelist is two-axis: every command opts in to *self* and/or
*peer* independently. Self-only commands are rejected at the MCP
boundary when targeted at another agent, so a peer can never wipe your
working context.

| command | self | peer | note |
|---|---|---|---|
| `compact` | ✓ | ✗ | Self-only — peers can't truncate your context |
| `rename`  | ✓ | ✓ | Useful for `<Project> #<Issue>` automation |
| `cost`    | ✓ | ✗ | Self-only — output goes to recipient |
| `help`    | ✓ | ✓ | Harmless either way |
| `mcp-enable-semaphore`  | ✓ | ✓ | Refresh tool surface after deploying a new `semaphore.*` tool — no context loss |
| `mcp-disable-semaphore` | ✓ | ✗ | Self-only: raw peer-disable is a DoS surface; use the restart macro instead |
| `mcp-restart-semaphore` | ✓ | ✓ | Macro: the handler synthesises `disable` + `enable` as two control rows for a peer-safe reconnect cycle |

```text
# Self: an agent asks itself to compact
semaphore.control to=bosun command=compact   # invoked from the bosun pane

# Peer: Bosun retitles Pilot's tab
semaphore.control to=pilot command=rename
```

Adding a command or flipping a scope flag requires a code change
(`internal/control/control.go`) — the audit surface is intentionally
small.

#### From a shell

The same surface is available as a CLI subcommand, useful for
scripts, cron jobs, and sessions whose MCP isn't loaded yet:

```bash
claude-msg control --to surveyor --command rename
claude-msg control --to bosun --command compact \
  --resume-with "carry on with the v0.15.0 cut"
```

Identity is auto-resolved the same way as the MCP tool — `$TMUX_PANE`
→ `agents` registry. Pass `--from` to override.

#### Self-compact with a follow-up

`/compact` leaves the session sitting at an empty prompt — no work
resumes until input lands. To bridge the gap, `semaphore.control`
accepts an optional `resume_with` string when `command=compact` on
self-invocation:

```text
semaphore.control to=bosun command=compact \
  resume_with="finish #25 follow-ups, then triage tomorrow's queue"
```

The MCP handler queues two rows back-to-back (the `/compact` control
plus the resume message, threaded via `reply_to` for audit). The
mailman holds the queue for `--post-compact-pause` (default 120s)
after delivering `/compact`, so the follow-up lands once Claude Code
has settled — not into the slash-command parser mid-compaction.

`resume_with` is only valid with `command=compact` on self; the call
is rejected at the MCP boundary otherwise.

### Removing a pane

> *Claude, please call `semaphore.unregister name=oldname`*

Stops the mailman, drops the agent row, and optionally purges the
agent's message history (`purge_messages: true`).

### New tools require a session restart

MCP tool lists are sent once during the `initialize` handshake and not
refreshed. Updating `/usr/local/bin/claude-msg` and restarting the
mailmen makes new tools available to *future* Claude sessions, but
sessions that started before the upgrade stay pinned to the tool
surface they were initialized with. To propagate a new `semaphore.*`
tool into a running pane, restart its Claude session.

### Tracking delivery

With the probe-and-watch gate the bus is no longer instantaneous —
a message can dwell minutes waiting for the recipient pane to go
quiet. To check whether a sent message has actually landed:

```bash
# From any shell:
claude-msg track 9c1d           # human-readable text
claude-msg track 9c1d --format json   # piping into scripts

# From a Claude session (MCP):
# call semaphore.message_status with {"id": "9c1d"}
```

Both paths return the same shape (`state`, `created_at`,
`delivered_at`, `error`, `reply_to`). `state` walks through
`queued → delivering → delivered` (or `failed` with the reason in
`error`). Queue-full rejections at *send* time are still synchronous —
`claude-msg send` returns `{ok: false, error: "queue full ..."}` at
attempt time — so `track` is purely for confirming positive
delivery after queuing.

### Delivery semantics: probe-and-watch quiet-pane gate

Before each delivery the mailman checks whether the recipient pane is
quiet: it injects a single `─` character (no Enter), waits 5 seconds,
and re-captures the bottom of the pane. If the only change is that
probe character, nobody was typing — the mailman backspaces the probe
and delivers normally. If anything else changed (operator typed,
deleted, pasted), the mailman backs off 60 seconds and retries.

The probe is **not** backspaced on the backoff path. The operator will
usually delete the stray dash themselves, or it lands harmlessly in
the text they're already typing. Eating one of their real keystrokes
with a guess-backspace would be worse.

Agent-streaming output doesn't trigger backoff because the input box
sits below the response area; only changes to the input row itself
count as activity. So this gate distinguishes operator vs Claude
correctly without ever blocking on an agent's own work.

A total-time cap (default 5 min) sets the expectation honestly: a
human who sees the probe appear typically needs 2-10 minutes to close
a sentence or cut their in-progress message out of the input box.
Beyond that they've usually walked away, so delaying further just buys
nothing. Crossing the cap delivers anyway with a `WARN
quiet_cap_exceeded` line in journalctl so the operator can see why
fragmented input happened.

Flags on `claude-msg serve`:

- `--quiet-observe-window` (default 5s)
- `--quiet-backoff` (default 60s)
- `--quiet-max-wait` (default 5m)
- `--quiet-disabled` (escape hatch)

### Identity precedence

Both the MCP server and the CLI subcommands (`send`, `inbox`, `whoami`,
`control`) resolve identity through one shared helper
(`internal/identity`). Precedence:

1. Explicit override — `--from` on `send`, `--as` on `whoami`, or
   `$CLAUDE_AGENT_NAME` for any path. Highest precedence.
2. `$TMUX_PANE` → `agents.pane_id` → name. The default for a
   registered pane; no env var needed.
3. Neither → the tool errors with an actionable message pointing the
   operator at registration.

`whoami` surfaces a `source` field (`explicit` / `env` / `pane`) so
the operator can see how identity was resolved.

**Spoofing note:** `$TMUX_PANE` is settable by anything with shell
access, and the registry has no per-pane authentication. This widens
*convenience*, it does not authenticate identity — the trust model is
"whoever has shell access is trusted," same as the rest of the bus.

## Development

Local pre-commit recipe:

```bash
go vet ./...
go build ./...
go test -race -count=1 ./...
```

CI runs `go vet`, `go build`, and `go test -count=1` (without `-race`)
— the Forgejo runner image doesn't ship cgo / a C compiler, which the
race detector requires. Local runs catch data races; CI catches the
rest. Push with `-race` clean.

## Roadmap

Tracked in the epic + milestone sub-issues — see the [Issues tab](../../issues).

## License

MIT.

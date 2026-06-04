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

## Versioning

cli-semaphore follows [Semantic Versioning](https://semver.org/) at
the `0.x.y` cadence. Minor bumps (`0.1.0` → `0.2.0`) may break
compatibility while the post-MVP shape settles; patch bumps are
backward-compatible within a minor. See `CHANGELOG.md` for what's
shipped per release.

```bash
$ claude-msg --version
claude-msg v0.1.0
```

A binary built via `make build` (not bare `go build`) stamps the
version from `git describe --tags --always --dirty`. Bare-`go build`
binaries report `dev`.

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

## Removal

To uninstall:

```bash
sudo -A ./uninstall.sh                # stops mailmen, removes binary,
                                      # leaves the SQLite DB alone
sudo -A ./uninstall.sh --purge        # also wipes /var/lib/cli-semaphore/
                                      # (interactive confirmation when run
                                      # from a TTY)
```

`uninstall.sh` is idempotent — re-running it on a partial state is safe.
The script:

- Stops + disables every running `claude-mailman@*.service` user unit
  under the operator's session.
- Removes the systemd user template from
  `~/.config/systemd/user/claude-mailman@.service` and reloads the user
  manager.
- Removes the `claude-msg` binary from `${PREFIX}/bin/`.

What `uninstall.sh` does **NOT** touch (remove by hand if you want them
gone):

- `/etc/cli-semaphore/` — host-level config (#54); the operator may
  have hand-edited it.
- The semaphore MCP entry in `~/.claude.json` — remove with
  `claude mcp remove semaphore -s user`.
- `loginctl enable-linger` — other services on the host may rely on
  the user manager continuing to run at boot.
- `/var/lib/cli-semaphore/` — message history. Default-preserved so
  an accidental re-install can pick up where the previous one left off.
  Pass `--purge` to wipe it after an interactive confirmation.

For safety, the script refuses to run from inside
`/var/lib/cli-semaphore/` (so a sloppy `cd` + `./uninstall.sh` can't
delete the very directory it's running from with `--purge`).

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

The whitelist is three-axis: every command opts in to *self*, *peer*,
and — for destructive commands that need narrow exceptions to a
blanket peer-deny — a per-edge allowlist of specific (sender,
recipient) pairs. Self-only commands are rejected at the MCP boundary
when targeted at another agent; peer-denied commands are rejected
across the board unless a `PeerEdges` entry matches the caller's
identity exactly.

| command | self | peer | note |
|---|---|---|---|
| `compact` | ✓ | ✗ | Self-only — peers can't truncate your context |
| `rename`  | ✓ | ✓ | Useful for `<Project> #<Issue>` automation |
| `cost`    | ✓ | ✗ | Self-only — output goes to recipient |
| `help`    | ✓ | ✓ | Harmless either way |
| `clear`   | ✗ | ✗ | **Edge-only**: Bosun→Pilot rescue path when `/compact` can't recover from token exhaustion (#60). Loses in-flight work. |
| `mcp-enable-semaphore`  | ✓ | ✓ | Refresh tool surface after deploying a new `semaphore.*` tool — no context loss |
| `mcp-disable-semaphore` | ✓ | ✗ | Self-only: raw peer-disable is a DoS surface; use the restart macro instead |
| `mcp-restart-semaphore` | ✓ | ✓ | Macro: the handler synthesises `disable` + `enable` as two control rows for a peer-safe reconnect cycle |

```text
# Self: an agent asks itself to compact
semaphore.control to=bosun command=compact   # invoked from the bosun pane

# Peer: Bosun retitles Pilot's tab
semaphore.control to=pilot command=rename

# Edge-allowed: Bosun rescues a token-exhausted Pilot
semaphore.control to=pilot command=clear     # only works when sender == bosun
```

Adding a command, flipping a scope flag, OR adding a new edge entry
requires a code change (`internal/control/control.go`) — the audit
surface is intentionally small, and edge exceptions are reviewable as
explicit code rather than runtime configuration.

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

The `mcp-restart-semaphore` macro (#28) re-initializes one chamber's
MCP stdio without losing in-session context. For deploys that need
EVERY chamber refreshed, the bulk shortcut collapses the per-chamber
typing tax:

```bash
claude-msg refresh-all-mcps                 # text summary
claude-msg refresh-all-mcps --format json   # per-chamber outcome rows
```

Iterates the registered `agents` table and fires
`mcp-restart-semaphore` per chamber, then reports each chamber's
success or cap-rejected failure. Cap-protected by the existing 5-slot
per-recipient queue ceiling — a busy chamber gets a `failed` entry
rather than queue-bypass. Operator-only (no MCP tool variant; that
would be a DoS amplification class).

> **Forward-watch**: v1 fires the macro unconditionally per chamber.
> If a chamber is mid-tool-call at restart time, that single tool-call
> is disrupted (the chamber session itself is unaffected). If this
> becomes recurring felt-pain, the post-#69 chamber-state primitive
> enables a `state in [idle, awaiting-operator]` gate as a size/M
> follow-up — file an issue citing the friction.

### Tracking delivery

When the probe-and-watch gate is enabled (opt-in since 2026-06-01)
the bus is no longer instantaneous — a message can dwell minutes
waiting for the recipient pane to go quiet. With the gate off
(default) delivery happens immediately and the `delivered_unverified`
notice is the load-bearing transparency signal. To check whether a
sent message has actually landed:

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

**Watch mode** (post-#49): `claude-msg track <id> --watch` polls
every 5s (configurable via `--watch-interval`) and re-renders on each
state change, exiting when state becomes terminal (`delivered` or
`failed`). Useful for "I just sent a long autonomous task; ping me
when it's been consumed". `--watch-timeout DURATION` bails out if no
terminal state lands within the window; SIGINT exits cleanly.

### Diagnosing a failed or unverified message

When `claude-msg send` returns a message ID but the recipient never
sees it, walk this flow:

1. **Check the delivery state.** `claude-msg track <id>` (or
   `--watch` if you want to wait for resolution) shows where the
   message is in its lifecycle.
2. **If state is `failed`** — grep the recipient's mailman journal
   for the message ID:
   ```bash
   journalctl --user -u claude-mailman@<recipient>.service | grep <id>
   ```
   The hard-failure WARN line carries the reason.
3. **If state is `delivered_unverified`** — the paste + Enter
   sequence ran but the verify token didn't surface in the retry
   budget. Typically means the recipient was mid-turn and Enter was
   queued. The message text IS sitting in the input box; the
   operator may need to submit it manually.
4. **As of #53**, both `failed` and `delivered_unverified` also push
   a `delivery_failure_notice` back to the original sender's
   pane — so the diagnostic flow above is mainly for cases where
   notifications are disabled or you want to dig deeper.

Common cause patterns:

- **`drift_check_ambiguous`** — multiple canonicals exact-or-substring
  match the running `--resume` value. Fix: `semaphore.register name=X
  alias=Y force=true` (the WARN line carries the exact recipe since
  #47).
- **`drift_detected_unrecoverable`** — `discover` couldn't find the
  agent on any pane. Fix: respawn the agent in tmux + run
  `claude-msg discover`.
- **`quiet_cap_exceeded`** — the probe-and-watch gate's `MaxWait`
  (default 5min) elapsed without finding an operator-quiet window.
  Post-#52 this is rare — the two-dash gate doesn't trigger on
  conversation-area streaming the way the v0.2.1 four-way verdict
  did.
- **Mailman not running** — check `systemctl --user status
  claude-mailman@<recipient>.service`. Orphan-recovery on next
  startup will re-queue any in-flight messages.

> Different shape: if the chamber claims a message went missing but
> `claude-msg track` says **no such id** (or you don't even have the
> id), the failure may be **sender-side** rather than bus-side. Walk
> the [sender-outbox-first diagnostic playbook](docs/diagnostic-playbook.md)
> instead — it starts from the SQLite store rather than the receiver's
> mailman journal.

### Delivery semantics: probe-and-watch quiet-pane gate (opt-in)

**Default state since 2026-06-01: OFF.** Empirical use during M2.11
exchange showed the gate added up to 5 min worst-case latency without
preventing mid-turn collisions in practice — the verify-token retry
+ `delivered_unverified` notice path (always on by default) is the
load-bearing safety net. Re-enable per agent via TOML
`quiet-disabled = false` or `--quiet-disabled=false` if a polite-wait
shape is wanted for a specific recipient.

When enabled, the gate checks whether the recipient pane's input row
is operator-quiet before each delivery. Per #52's redesign
(2026-05-31), the gate is a **two-dash check** rather than the older
four-way verdict:

1. Paste `─` (probe #1 — dismisses any ghost-text suggested prompt the
   CLI may be showing).
2. Wait `ObserveWindow` (default 3s) — gives the operator time to
   react.
3. Paste `─` (probe #2 — the actual quiet-state probe).
4. Wait `ObserveWindow` again.
5. Capture the pane. The verdict is binary:
   - **Quiet** — the input row gained exactly the two probes we
     pasted, with nothing else added or removed. Safe to deliver;
     backspace accumulated probes and paste the message body.
   - **Input activity** — the input row's content differs from
     "before-state + N trailing probes" — operator typed, removed a
     probe, or interfered. Back off `InputActivityBackoff` (default
     60s) so they get time to finish, then restart from step 1 (a
     fresh ghost-text prompt may have appeared between iterations).

What the gate explicitly does **not** protect against:

- Recipient mid-conversation. The bus doesn't gate on recipient-busy;
  the recipient processes one message at a time anyway.
- TUI animations, status-line ticks, streaming output above the input
  row. All ignored — the gate cares only whether the operator is
  typing on the input row.

Probes are **never** backspaced between iterations — they accumulate
in the input box as a visible "I see you" stack of dashes until the
operator clears them or the gate exits (quiet OR cap-exceeded). Only
the final pre-delivery cleanup or the cap-exit backspaces all
accumulated probes so the recipient's input starts clean.

A total-time cap (default 5 min) sets the expectation honestly: a
human who sees the probes appear typically needs 2-10 minutes to close
a sentence or cut their in-progress message out of the input box.
Beyond that they've usually walked away, so delaying further just buys
nothing. Crossing the cap delivers anyway with a `WARN
quiet_cap_exceeded` line in journalctl so the operator can see why
fragmented input happened.

Flags on `claude-msg serve`:

- `--quiet-observe-window` (default 3s; applied twice per iteration —
  once after each probe)
- `--quiet-input-backoff` (default 60s)
- `--quiet-max-wait` (default 5m)
- `--quiet-disabled` (default `true` since 2026-06-01; set
  `--quiet-disabled=false` to re-enable the gate for an agent)
- `--notify-on-failed` / `--notify-on-delivered-unverified` (default
  on; see [Delivery-failure notifications](#delivery-failure-notifications) below)

### Delivery-failure notifications

Post-#53 (`[Unreleased]`): when a recipient's outbound message
transitions to a terminal-failure state (`failed` or
`delivered_unverified`), the mailman auto-inserts a
`delivery_failure_notice` back to the original sender. The notice
carries the original message id, the recipient, the failure class, the
reason, and a 200-char body preview.

Cap-exempt: notifications bypass `MaxRecipientQueue` and
`MaxSenderBacklog` so they're never silently dropped. Loop-prevention
by-kind: a notice that itself fails to deliver does NOT generate
another notice (the wedged-pane cascade is avoided).

The two toggles on `claude-msg serve` are independent and both
default-on:

- `--notify-on-failed` — hard failures
- `--notify-on-delivered-unverified` — soft failures

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

### Canonical names and aliases (post-restart resilience)

The bus addresses agents by **canonical** short name: `bosun`, `surveyor`,
`pilot`, `admin`. The discover walker, however, extracts the agent
name from `claude --resume <name>` in the process tree — so a session
launched with `claude --resume "Master Bosun of Nimbus"` produces a
running name that doesn't match canonical `bosun`.

Without help, this fails twice:

1. After a tmux restore (or any restart that renumbers pane ids),
   `claude-msg discover` would create new agent rows for the long
   names instead of remapping the canonical short names.
2. The mailman's auto-heal can't find `bosun` because the only
   matching pane runs `Master Bosun of Nimbus`.

**Solution: register an alias.**

```text
# from the bosun pane (MCP):
semaphore.register name=bosun alias='Master Bosun of Nimbus'

# from a shell (CLI; identity already resolved):
claude-msg control ...  # alias support via register is MCP-only today
```

After this, discover and the mailman's drift check both resolve
`Master Bosun of Nimbus` back to canonical `bosun`. Multiple aliases
per canonical are supported (append-only via `AddAlias` in the store).

If two canonicals both substring-match the same running value
(`admin` and `pilot` both appearing in `--resume "admin pilot test"`),
the resolver returns ambiguous and the mailman logs
`drift_check_ambiguous` rather than guess. Add an explicit alias on
the agent you actually meant.

## Roadmap

Tracked in the epic + milestone sub-issues — see the [Issues tab](../../issues).

## License

MIT.

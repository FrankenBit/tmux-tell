# tmux-msg

A tmux-based message-bus substrate. Each tmux session that hosts a CLI agent has a mailbox; senders post messages via the SQLite store, and a per-recipient mailman daemon serializes paste-and-Enter delivery into the target tmux pane through a read-only-observe-only gate that defers while the recipient is busy.

**Substrate vs CLI-tool-flavor.** The substrate is tmux: the pane registry, the paste-and-Enter delivery, the per-pane chrome detection (idle / busy / popup-open / mid-compaction / awaiting-operator). The CLI tool running inside the pane is downstream — `claude-msg` is the binary built for Claude Code today; sibling binaries (`codex-msg`, `copilot-msg`) can be built from the same substrate when there's need for them. The repo name was chosen to reflect that: tmux-msg is what the substrate IS, not which CLI tool happens to run on top.

(The project was originally named `cli-semaphore` — re-grounded on the substrate's actual primitive in v0.5.0.)

## Status

Production on alcatraz across 6 agents (Bosun, Surveyor, Engineer, Pilot, Herald, Quartermaster) since v0.3.0 (2026-06-04). See [the epic](https://git.frankenbit.de/frankenbit/tmux-msg/issues) for the roadmap and open work.

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

DB lives at `/var/lib/tmux-msg/messages.db`.

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

tmux-msg follows [Semantic Versioning](https://semver.org/) at
the `0.x.y` cadence. Minor bumps (`0.1.0` → `0.2.0`) may break
compatibility while the post-MVP shape settles; patch bumps are
backward-compatible within a minor. See `CHANGELOG.md` for what's
shipped per release.

```bash
$ claude-msg --version
claude-msg v0.5.0
```

A binary built via `make build` (not bare `go build`) stamps the
version from `git describe --tags --always --dirty`. Bare-`go build`
binaries report `dev`.

## Install

On a Linux host that has tmux, sqlite3, and Go (≥ 1.24):

```bash
git clone https://git.frankenbit.de/frankenbit/tmux-msg.git
cd tmux-msg
make build
sudo -A ./install.sh                  # uses sudo -A so a tmux-popup
                                      # askpass surfaces nicely on alcatraz
```

This:
- builds `bin/claude-msg`,
- installs the binary to `/usr/local/bin/claude-msg`,
- creates `/var/lib/tmux-msg/` (operator-owned, holds `messages.db`),
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
sudo -A ./uninstall.sh --purge        # also wipes /var/lib/tmux-msg/
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

- `/etc/tmux-msg/` — host-level config (#54); the operator may
  have hand-edited it.
- The tmux-msg MCP entry in `~/.claude.json` — remove with
  `claude mcp remove tmux-msg -s user`.
- `loginctl enable-linger` — other services on the host may rely on
  the user manager continuing to run at boot.
- `/var/lib/tmux-msg/` — message history. Default-preserved so
  an accidental re-install can pick up where the previous one left off.
  Pass `--purge` to wipe it after an interactive confirmation.

For safety, the script refuses to run from inside
`/var/lib/tmux-msg/` (so a sloppy `cd` + `./uninstall.sh` can't
delete the very directory it's running from with `--purge`).

## Use from Claude Code (MCP)

The same binary speaks MCP over stdio under `claude-msg mcp`, exposing
`tmux-msg.send / control / agents / whoami / inbox / status / register
/ unregister` as native Claude tools. **One user-level config; identity
is auto-resolved per pane.**

Add the server once in `~/.claude.json` (or your equivalent Claude Code
config) — no per-pane env or config files needed:

```json
{
  "mcpServers": {
    "tmux-msg": {
      "command": "/usr/local/bin/claude-msg",
      "args": ["mcp"]
    }
  }
}
```

### How identity works

When Claude in a tmux pane spawns the tmux-msg MCP server, the child
process inherits `$TMUX_PANE` from the shell (tmux sets it automatically
for every pane it owns — `%1`, `%3`, etc.). The MCP server looks that
pane id up in the `agents` table and uses the matching agent name as the
session's identity.

So the workflow for a **new pane** is just one tool call from that pane:

> *Claude, please call `tmux-msg.register name=myname`*

The pane is auto-detected from `$TMUX_PANE`, the row is inserted, and
`systemctl --user enable --now claude-mailman@myname.service` runs in
the same step. Equivalent CLI fallback:

```bash
# from inside the new pane
CLAUDE_AGENT_NAME=myname claude-msg ...   # (CLI doesn't yet expose register;
                                          # fall back to SQL until then)
sqlite3 /var/lib/tmux-msg/messages.db \
  "INSERT INTO agents (name, pane_id) VALUES ('myname', '$TMUX_PANE');"
systemctl --user enable --now claude-mailman@myname.service
```

### Whitelisted control commands

`tmux-msg.control` types a vetted Claude Code slash-command into a
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
| `mcp-enable-tmux-msg`  | ✓ | ✓ | Refresh tool surface after deploying a new `tmux-msg.*` tool — no context loss |
| `mcp-disable-tmux-msg` | ✓ | ✗ | Self-only: raw peer-disable is a DoS surface; use the restart macro instead |
| `mcp-restart-tmux-msg` | ✓ | ✓ | Macro: the handler synthesises `disable` + `enable` as two control rows for a peer-safe reconnect cycle |

```text
# Self: an agent asks itself to compact
tmux-msg.control to=bosun command=compact   # invoked from the bosun pane

# Peer: Bosun retitles Pilot's tab
tmux-msg.control to=pilot command=rename

# Edge-allowed: Bosun rescues a token-exhausted Pilot
tmux-msg.control to=pilot command=clear     # only works when sender == bosun
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
resumes until input lands. To bridge the gap, `tmux-msg.control`
accepts an optional `resume_with` string when `command=compact` on
self-invocation:

```text
tmux-msg.control to=bosun command=compact \
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

> *Claude, please call `tmux-msg.unregister name=oldname`*

Stops the mailman, drops the agent row, and optionally purges the
agent's message history (`purge_messages: true`).

### New tools require a session restart

MCP tool lists are sent once during the `initialize` handshake and not
refreshed. Updating `/usr/local/bin/claude-msg` and restarting the
mailmen makes new tools available to *future* Claude sessions, but
sessions that started before the upgrade stay pinned to the tool
surface they were initialized with. To propagate a new `tmux-msg.*`
tool into a running pane, restart its Claude session.

The `mcp-restart-tmux-msg` macro (#28) re-initializes one agent's
MCP stdio without losing in-session context. For deploys that need
EVERY agent refreshed, the bulk shortcut collapses the per-agent
typing tax:

```bash
claude-msg refresh-all-mcps                 # text summary
claude-msg refresh-all-mcps --format json   # per-agent outcome rows
```

Iterates the registered `agents` table and fires
`mcp-restart-tmux-msg` per agent, then reports each agent's
success or cap-rejected failure. Cap-protected by the existing 5-slot
per-recipient queue ceiling — a busy agent gets a `failed` entry
rather than queue-bypass. Operator-only (no MCP tool variant; that
would be a DoS amplification class).

> **Forward-watch**: v1 fires the macro unconditionally per agent.
> If a agent is mid-tool-call at restart time, that single tool-call
> is disrupted (the agent session itself is unaffected). If this
> becomes recurring felt-pain, the post-#69 agent-state primitive
> enables a `state in [idle, awaiting-operator]` gate as a size/M
> follow-up — file an issue citing the friction.

### Tracking delivery

Since v0.3.0 the **observe-gate** (on by default) holds each delivery
until the recipient pane is ready — typically ~3–5s in the idle case,
with a ~5 min `MaxWait` safety cap as the worst case. The gate is
read-only, so it never mutates the recipient's pane while it waits; the
`delivered_unverified` notice remains the load-bearing post-hoc signal
for the residual observe-then-paste race. To check whether a sent
message has actually landed:

```bash
# From any shell:
claude-msg track 9c1d           # human-readable text
claude-msg track 9c1d --format json   # piping into scripts

# From a Claude session (MCP):
# call tmux-msg.message_status with {"id": "9c1d"}
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
  match the running `--resume` value. Fix: `tmux-msg.register name=X
  alias=Y force=true` (the WARN line carries the exact recipe since
  #47).
- **`drift_detected_unrecoverable`** — `discover` couldn't find the
  agent on any pane. Fix: respawn the agent in tmux + run
  `claude-msg discover`.
- **`gate_max_wait`** — the observe-gate's `MaxWait` (default 5min)
  elapsed without the recipient reaching `idle` or the operator's
  draft going stale. The mailman delivers anyway (fail-loud, not
  fail-stop) and logs the WARN so the operator can see why a delivery
  landed onto a busy pane. Rare in practice — the gate only waits this
  long when the recipient is continuously `working` for the full
  window.
- **Mailman not running** — check `systemctl --user status
  claude-mailman@<recipient>.service`. Orphan-recovery on next
  startup will re-queue any in-flight messages.

> Different shape: if the agent claims a message went missing but
> `claude-msg track` says **no such id** (or you don't even have the
> id), the failure may be **sender-side** rather than bus-side. Walk
> the [sender-outbox-first diagnostic playbook](docs/diagnostic-playbook.md)
> instead — it starts from the SQLite store rather than the receiver's
> mailman journal.

### Delivery semantics: the observe-gate

**Default since v0.3.0: ON** (read-only, ~3–5s typical). Before each
delivery the mailman runs `ObserveGate` — a *read-only-observe-only*
check that decides when the recipient pane is ready to receive a paste.
It replaces the probe-and-watch quiet-pane gate retired in v0.3.0: no
`─` probe dashes are injected into the recipient's input row, and
typical-case latency drops from ~72s (the legacy gate's single backoff
cycle) to ~3–5s.

The gate polls the recipient's `AgentState` (two read-only
`capture-pane` snapshots plus a cursor query — zero pane mutation) and
decides across the five `AgentState` values:

| Recipient state | Gate decision |
|---|---|
| **idle** — cursor at the `❯ ` prompt sentinel (empty prompt or auto-suggestion ghost-text) | deliver immediately (fast path) |
| **awaiting-operator** — cursor past the sentinel (operator is drafting) | hash the input-row content; if it stays unchanged for `input-stale-threshold` (default 2m), treat the draft as abandoned and flush-then-deliver (see below); otherwise keep polling |
| **working** / **at-rest-in-compaction** / **unknown** | safer-default wait — re-poll with progressive backoff |

Polling backs off multiplicatively (3s → 4.5s → 6.75s → … → 15s cap)
and resets to the floor whenever the operator's input content changes
(fresh activity → fresh cadence). A total `MaxWait` cap (default 5min)
bounds the loop: on expiry the mailman delivers anyway and logs `WARN
gate_max_wait` — fail-loud, not fail-stop.

The gate does **not** try to eliminate the race between
"observe-decides-idle" and "caller-pastes" — that residual window is
covered by the same verify-token + `delivered_unverified` post-hoc
safety net as before. What the observe-gate eliminates is the *pane
mutation* the probe-and-watch gate used to inflict while observing.

#### Stale-draft flush — recovering operator content

When the gate flushes a stale draft (the operator typed something, then
left it untouched for ≥ `input-stale-threshold`), it does **not**
silently discard the typed content. The flush is a three-path decision:

1. **(c) Clear-paste-archive (primary).** The gate snapshots the
   operator's input-row content into the bus as a `kind=stranded_draft`
   row — **self-addressed to the recipient agent** (cap-bypass, so a
   congested inbox can't drop it) — then sends `Ctrl+U` to clear the
   input and pastes the message.
2. **(a) Append (fallback).** If the archive write fails, the mailman
   skips the clear and pastes onto the operator's content (a compound
   message) rather than risk losing the draft.
3. **(b) Clear-and-discard is rejected** in code and comments: the
   content sitting in the input row might be a *half-delivered bus
   message* from a previous failed delivery, so a blind `Ctrl+U` could
   destroy bus content, not just operator content. The safe paths above
   always archive before clearing.

Because the snapshot is self-addressed, the agent's own mailman
delivers it straight back into the agent's pane — so the **primary
recovery is inline**: the cleared draft reappears as a
`kind=stranded_draft` bus message carrying the content verbatim plus the
public_id of the delivery that triggered the flush. To find it again in
the store *after* that self-delivery (the row has moved past `queued`),
the SQLite-inbox fallback is:

```bash
claude-msg inbox <agent> --state delivered   # the self-delivered snapshot
claude-msg inbox <agent> --state ""          # all states, if unsure
```

#### Tuning knobs

All four are CLI flags on `claude-msg serve` and TOML knobs (per-agent
or `[defaults]`), composable through the standard precedence chain
(CLI flag > per-agent block > `[defaults]` > compiled default):

| Knob (flag / TOML) | Default | Meaning |
|---|---|---|
| `--gate-disabled` / `gate-disabled` | `false` | bypass the gate entirely; deliver immediately on every queue head |
| `--poll-interval-min` / `poll-interval-min` | `3s` | initial sleep between observe iterations |
| `--poll-interval-max` / `poll-interval-max` | `15s` | backoff ceiling per iteration |
| `--input-stale-threshold` / `input-stale-threshold` | `2m` | how long an operator draft must sit unchanged before it's flushed |

The notification toggles (`--notify-on-failed` /
`--notify-on-delivered-unverified`) are independent of the gate — see
[Delivery-failure notifications](#delivery-failure-notifications) below.

#### Migration from v0.2.x

The observe-gate is on for every agent with no per-agent config. If
you carried `[agent.<name>]` blocks that only set the old probe-and-watch
knobs — `quiet-disabled`, `prompt-sentinel-gate`, `quick-presence-probe`,
`quiet-observe-window`, `quiet-input-backoff`, `quiet-max-wait` — those
are now **no-ops**: the mailman logs a startup `WARN` naming any that
are set, and you can delete the blocks at your convenience. A block that
existed only to hold `prompt-sentinel-gate = true` can be removed
entirely.

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
tmux-msg.register name=bosun alias='Master Bosun of Nimbus'

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

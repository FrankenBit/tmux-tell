# tmux-msg

**A message bus for CLI agents running in tmux.** Each pane gets a mailbox; an agent
(or you) sends a message and it lands in the target pane as if typed there — gated so
it never pastes over what you're in the middle of typing.

### You're already running a message bus. It's you.

You've got a few agents open in tmux — one mid-refactor, one writing tests, one
reviewing a branch. You alt-tab to check whether the reviewer's done, hand-paste
*"the API changed, look at what I just pushed"* into the next pane, and squint across
the panes trying to remember which one is blocked on which. Right now the coordination
layer between your agents is **you** — the slowest, most forgettable part of your own
setup.

tmux-msg lets them tell each other instead. The reviewer finishes and notifies the
implementer; the implementer warns the tester the moment the contract moves. You set
the work up and let the agents keep each other current.

→ **[Why tmux-msg? — the longer pitch](docs/why.md)**

No cloud, no daemon phoning home: it's a SQLite file and a tmux paste. You can read
every message with `sqlite3`, and uninstall is one script.

## What it is — and what it isn't

- **It is:** local inter-agent messaging for CLI tools sharing one tmux server — one
  mailbox per pane, a single writer per mailbox (no paste-races), delivery you can
  watch happen.
- **It isn't:** a networked message queue, a multi-host bus, a chat app, or a job
  scheduler. It moves a message from one pane into another, safely, while a human
  might also be using that pane.

> **Substrate vs CLI-tool flavor.** The substrate is tmux: the pane registry, the
> paste-and-Enter delivery, the per-pane state detection (idle / busy / popup-open /
> mid-compaction / awaiting-operator). The CLI tool inside the pane is downstream —
> `claude-msg` is the binary built for Claude Code today; sibling binaries
> (`codex-msg`, `copilot-msg`) could be built from the same substrate. The repo name
> reflects what the substrate *is*, not which tool runs on top. (Originally named
> `cli-semaphore`; re-grounded on the substrate's primitive in v0.5.0.)

## How it works

```
   agent-a ──►┌─────────────────────────────────────┐──► mailman@agent-c
   agent-b ──►│  SQLite mailbox (messages, agents)  │    (single writer to its pane)
              └─────────────────────────────────────┘
   reply ──►  claude-msg send --reply-to <id> --to agent-a "…"
```

**Senders** never touch tmux — `claude-msg send` validates the message, checks the
caps, and inserts a row. **Mailmen** are per-agent daemons (systemd user services)
that loop on their inbox, paste the formatted message into the target pane through the
[observe-gate](#delivery-semantics-the-observe-gate), and mark it delivered. Because
each recipient has exactly one mailman, the usual tmux concurrency hazards (paste-buffer
races, idle-check TOCTOU, turn concatenation) collapse to a single-writer invariant.

## Install

On a Linux host with tmux, sqlite3, and Go (≥ 1.24):

```bash
# from inside a tmux session:
git clone https://github.com/FrankenBit/tmux-msg && cd tmux-msg
make build
sudo ./install.sh        # installs the binary + the systemd user template
```

`install.sh` builds `bin/claude-msg`, installs it to `/usr/local/bin/claude-msg`,
creates `/var/lib/tmux-msg/` (holds `messages.db`), and drops the systemd user
template into `~/.config/systemd/user/`. Then, **as your user (not root)**:

```bash
sudo loginctl enable-linger "$USER"   # keep the user manager running across reboots
systemctl --user daemon-reload        # so the mailman unit is visible
```

## Quickstart

From two panes in the same tmux session:

```bash
claude-msg register --name alice     # in pane A — registers + starts alice's mailman
claude-msg register --name bob       # in pane B
claude-msg send --to bob "first message across the bus"
```

`bob`'s pane shows:

```
[Alice · 14:02:09 · id 7f3a]

first message across the bus
```

That's the whole loop. `send` returns `{"ok":true,"id":"7f3a","queued":1}` on success
(or `{"ok":false,"error":"…"}` with a sysexits-style exit code on failure).

To confirm a freshly-registered agent is reachable *without* sending it a message,
`claude-msg ping bob` probes daemon-up + pane-live (no pane paste) — see
[Reachability probe](#operating-the-bus) under Operating the bus.

## Message rendering

Headers come in two shapes:

**Compact** — `[Sender · HH:MM:SS · id XXXX]` — an unthreaded message (no `reply_to`);
the common case, a new thread:

```
[Alice · 14:02:09 · id 7f3a]

please check CI on the latest push
```

**Threaded** — `[Sender → Recipient · re YYYY · id XXXX]` — when `reply_to=YYYY` is
set; surfaces the direction *and* the parent message for thread-following:

```
[Bob → Alice · re 7f3a · id 9c1d]

on it — green in ~3 min
```

**No-reply marker** — either shape can carry a trailing `· 🔕` when the sender sets
`--no-reply-expected` (CLI) or `no_reply_expected=true` (MCP); a discipline aid for
FYI / status messages that would otherwise accumulate ack-cascades:

```
[Alice · 11:04:12 · id 7f3a · 🔕]

FYI: tagged v0.8.0 — no ack needed
```

The recipient's Claude reads but doesn't acknowledge — content is still judged on
its own merits; the marker is a hint, not a hard rule.

System-generated messages (`delivery_failure_notice`, `stranded_draft`) carry their
own chrome so they're distinguishable from agent traffic. Sender names render
title-cased in the header; stored agent names are lowercase by convention.

## Caps

| Cap | Default | Why |
|---|---|---|
| Per-recipient queue depth | 5 | a pane that isn't draining is wedged — fail fast, don't accumulate |
| Per-sender backlog | 2 | one runaway agent can't starve the others |
| Body size | 16 KB | anything bigger should be a file reference, not a tmux paste |

`send` rejects with `{"ok":false}` when a cap is exceeded.

## Delivery modes

Each registered agent has a `delivery_mode`:

| mode | what the mailman does | the recipient's view |
|---|---|---|
| **`paste-and-enter`** *(default)* | pastes into the agent's pane through the observe-gate | messages **appear in the pane** — no inbox polling needed; the substrate pushes |
| **`mailbox-only`** | does not paste (no pane to push into); messages stay queued | the recipient **polls** `claude-msg inbox` / `tmux-msg.inbox` to read them |

`mailbox-only` makes a plain shell a bus *destination* without an always-on agent
session — e.g. your own shell: agents `send to=you` and you read when you choose. Set
it via MCP (`register … delivery_mode=mailbox-only`), CLI (`register --name you
--delivery-mode mailbox-only`), or a per-agent TOML block. Precedence (highest wins):
**per-agent block > `[defaults]` > the DB column > compiled default (`paste-and-enter`)**.
`claude-msg config show` prints the resolved value per agent.

## Delivery semantics: the observe-gate

Before each delivery the mailman runs the **observe-gate** — a near-read-only check
that waits for a safe moment to paste so it never lands on top of something you're
typing. It polls the recipient's state (idle / working / awaiting-operator /
mid-compaction / unknown) and:

- **idle** → delivers immediately (~3–5s typical);
- **you're typing** → holds, drops a single 📫 in your input row so you know something's
  queued, and delivers once you stop (or, if your draft sits untouched past a
  threshold, archives it safely and then delivers);
- **busy / compacting / unknown** → waits with progressive backoff, up to a 5-minute
  `MaxWait` cap, then delivers anyway and logs `WARN gate_max_wait` (fail-loud, never
  fail-silent).

It's *near*-read-only: apart from the optional 📫 nudge (opt out with
`notify-emoji-disabled`), it mutates nothing while observing — no probe characters in
your input row. The residual "decided-idle then pasted" race is caught by the
verify-token + `delivered_unverified` safety net.

→ **Full operator guide, decision matrix, knobs, and stale-draft recovery:
[`docs/observe-gate.md`](docs/observe-gate.md).**

## Operating the bus

```
claude-msg send   --to Y [--reply-to ID] [--no-reply-expected] "body"   # one-shot
claude-msg ping   AGENT [--timeout D] [--format json]   # reachability probe (no pane paste)
claude-msg inbox  AGENT [--state STATE]            # list messages for AGENT
claude-msg track  ID [--watch]                     # delivery state of one message
claude-msg get    ID                               # fetch a processed message by id
claude-msg status [--today]                        # paused state + queue depths per agent
claude-msg stats  [--window all|7d|1h] [--agent X] [--pair]  # on-demand bus-traffic aggregates
claude-msg state  --agent AGENT                    # probe an agent's current activity
claude-msg health [--since DUR]                    # per-agent operational audit
claude-msg pause  AGENT | --all                    # halt delivery (queue keeps filling)
claude-msg resume AGENT | --all
claude-msg reset  --confirm [--hard]               # purge queued; --hard wipes audit log
claude-msg log    --thread ID                      # a reply chain, flat-chronological
claude-msg thread ID [--format tree|json]          # a reply chain, as a parent→child tree
claude-msg discover                                # re-derive agents.pane_id from tmux
```

**Kill switch & retention.** `pause` sets `agents.paused = 1`; the mailman stops
injecting (messages keep queuing up to the cap) until `resume`. History is free —
SQLite on disk; on mailman start, any row left `delivering` from a crashed run is
reset to `queued`. `reset` purges `queued` + `delivering`; `--hard` also wipes the
delivered audit log; `--confirm` is mandatory.

**Reachability probe.** `claude-msg ping <agent>` answers "is the daemon up + the
agent registered + its pane reachable?" without the side effect a test `send` has —
it queues a `kind=ping` row the mailman picks up (proving the daemon is alive) and
resolves via substrate-health checks (agent registered, pane live), transitioning
straight to `delivered`/`failed` **without pasting into the recipient's pane**. The
clean "is this chamber wired up?" check for new-agent setup and post-restart sanity.
States (and exit codes): `delivered` reachable (`0`), `failed` registered-but-
unreachable (`69`), `timeout` no answer in `--timeout` — daemon down/paused/
backlogged (`75`). Pinging a non-registered agent fails loud. From MCP, call
`tmux-msg.ping {"agent": "surveyor"}`. (A `mailbox-only` agent has no mailman, so a
ping to it reports `timeout`.)

**Tracking delivery.** `claude-msg track <id>` shows where a message is
(`queued → delivering → delivered`, or `failed` with the reason in `error`);
`--watch` re-renders on each state change until terminal. From MCP, call
`tmux-msg.message_status {"id": "9c1d"}`.

**Reading a reply thread.** Two views of the same `reply_to` chain (both resolve
the whole chain from *any* id in it — walk to root, then all descendants):
`claude-msg log --thread <id>` renders it **flat-chronological** (an audit view);
`claude-msg thread <id>` renders it as a **parent→child tree** (a navigation /
diagnostic view — "who replied to what, and did it land?"):

```
○ id=6970 from=quartermaster to=bosun kind=message state=delivered  (PR #397 ready for merge)
└─ ✓ id=7501 from=bosun to=quartermaster kind=message state=delivered  (PR #397 merged)
   ├─ ✓ id=6d35 from=quartermaster to=bosun kind=delivery_failure_notice state=delivered  (…)
   └─ ✗ id=01ff from=quartermaster to=bosun kind=message state=failed  (merge acked)
      └─ … id=ac44 from=bosun to=quartermaster kind=message state=queued  (state-sync ack)
```

Glyphs: `○` root · `✓` delivered · `✗` failed · `…` queued/delivering. (There is no
distinct `delivered_unverified` glyph yet — the substrate stores that soft-failure
as `delivered`; making it DB-queryable is tracked in #169.) `--format json` emits
the nested tree for tooling. `thread` is read-only and never touches a pane.

**Bus-traffic stats.** `claude-msg stats` is the in-terminal "show me the bus
right now" surface — on-demand aggregates computed straight from the local
`messages.db`, complementing the continuous observability stack that owns
dashboard trends. The default reports a per-agent table (sent / received /
delivered / failed / queued + p50 delivery latency) plus window totals for the
last 24h; `--window` takes `all`, `<N>d` (e.g. `7d`), or any Go duration
(`1h`/`90m`); `--agent X` scopes to one agent; `--pair --top N` shows the
busiest sender→recipient pairs; `--format json` emits machine-readable output
(also carrying `p95_latency_ms`). The verified-vs-unverified delivery split is
*not* shown here — both land as `state='delivered'` in the DB (the
`delivered_unverified` signal is a mailman journal line, not a column), so use
`status --today` / `health` for that breakdown; making it DB-queryable is
tracked in #169.

**Delivery-failure notifications.** When an outbound message hits a terminal-failure
state (`failed` or `delivered_unverified`), the mailman auto-inserts a
`delivery_failure_notice` back to the sender (original id, recipient, failure class,
reason, 200-char preview). These bypass the queue caps so they're never dropped, and
a notice that itself fails does not generate another (no wedged-pane cascade). Both
`--notify-on-failed` and `--notify-on-delivered-unverified` default on.

**When a message seems to go missing,** walk the sender-first triage in
[`docs/diagnostic-playbook.md`](docs/diagnostic-playbook.md) — it starts from the
SQLite store (did the send reach the bus at all?) before the receiver's mailman
journal.

## Use from Claude Code (MCP)

The same binary speaks MCP over stdio under `claude-msg mcp`, exposing
`tmux-msg.send / control / agents / whoami / inbox / status / register / unregister /
message_status / agent_state` as native tools. **One user-level config; identity is
auto-resolved per pane.** Add the server once in `~/.claude.json`:

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

When the CLI tool in a pane spawns the MCP server, the child inherits `$TMUX_PANE`
(tmux sets it for every pane — `%1`, `%3`, …). The server looks that pane id up in the
`agents` table and uses the matching name as the session's identity. So onboarding a
**new pane** is one call from that pane:

> *call `tmux-msg.register name=myname`*

The pane is auto-detected, the row inserted, and the mailman started in the same step.
Equivalent CLI: `claude-msg register --name myname`.

### Canonical name mapping

The same tool is referred to by different sanitized names at different layers — worth
a glance when writing runbooks or invoking tools from a shell:

| Layer | Example name |
|---|---|
| Wire protocol (`tools/list` JSON-RPC) | `tmux-msg.register` |
| Source (`srv.RegisterTool(...)`) | `tmux-msg.register` |
| Claude Code tool-call slug | `mcp__tmux-msg__tmux-msg_register` |
| Documentation / prose | `tmux-msg.register` *(preferred)* |

Prefer the wire-protocol name (`tmux-msg.register`) in prose; use the slug when
invoking from Claude Code's tool surface. The Claude Code sanitization rule: dots →
underscores, dashes preserved, server-name prefix repeated as
`mcp__<server>__<server>_<tool>`. You can read the live wire names directly:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | claude-msg mcp
```

> A Claude session started before an MCP-tool rename keeps the names it was
> initialized with until it restarts — so an older session may still surface the
> pre-v0.6.0 `mcp__semaphore__semaphore_*` names (same handler). See
> [New tools require a session restart](#new-tools-require-a-session-restart).

### Whitelisted control commands

`tmux-msg.control` types a vetted slash-command into a pane — the caller's own (most
commonly an agent asking itself to `/compact` at a quiescent point) or, for benign
peer nudges, another's. The string is typed directly (no chat header) so the CLI tool
parses it as if you'd typed it.

The whitelist is three-axis: each command opts in to *self*, *peer*, and — for
destructive commands needing a narrow exception to a blanket peer-deny — a per-edge
allowlist of specific (sender, recipient) pairs.

| command | self | peer | note |
|---|---|---|---|
| `compact` | ✓ | ✗ | self-only — peers can't truncate your context |
| `rename`  | ✓ | ✓ | useful for `<Project> #<Issue>` tab automation |
| `cost`    | ✓ | ✗ | self-only — output goes to the recipient |
| `help`    | ✓ | ✓ | harmless either way |
| `clear`   | ✗ | ✗ | **edge-only** rescue path when `/compact` can't recover from token exhaustion (#60); loses in-flight work |
| `mcp-enable-tmux-msg`  | ✓ | ✓ | refresh the tool surface after deploying a new `tmux-msg.*` tool — no context loss |
| `mcp-disable-tmux-msg` | ✓ | ✗ | self-only: raw peer-disable is a DoS surface; use the restart macro |
| `mcp-restart-tmux-msg` | ✓ | ✓ | macro: `disable` + `enable` as two rows for a peer-safe reconnect |

Adding a command, flipping a scope, or adding an edge requires a code change
(`internal/control/control.go`) — the audit surface is intentionally small. The same
surface is a CLI subcommand (`claude-msg control --to … --command …`) for scripts and
sessions whose MCP isn't loaded.

**Self-compact with a follow-up.** `/compact` leaves the session at an empty prompt;
`command=compact` accepts a `resume_with` string (self-invocation only). The handler
queues the `/compact` plus the resume message (threaded via `reply_to`), and the
mailman holds the queue for `--post-compact-pause` (default 120s) so the follow-up
lands after the CLI tool has settled, not into the slash-command parser mid-compaction.

### Removing a pane

`tmux-msg.unregister name=oldname` (or `claude-msg unregister`) stops the mailman,
drops the agent row, and optionally purges its history (`purge_messages: true`).

### New tools require a session restart

MCP tool lists are sent once during the `initialize` handshake and not refreshed.
Updating the binary and restarting the mailmen makes new tools available to *future*
sessions; sessions started earlier stay pinned to the tool surface they initialized
with. `mcp-restart-tmux-msg` (#28) re-initializes one session's MCP stdio without
losing context; for a fleet, `claude-msg refresh-all-mcps` fires it per registered
agent (operator-only — a peer-invokable bulk restart would be a DoS amplification
class).

## Identity, names & aliases

**Identity precedence** (shared by the MCP server and the CLI): (1) explicit override
— `--from` on `send`, `--as` on `whoami`, or `$CLAUDE_AGENT_NAME`; (2) `$TMUX_PANE` →
`agents.pane_id` → name (the default for a registered pane, no env var needed);
(3) neither → an actionable error pointing at registration. `whoami` reports a
`source` field (`explicit` / `env` / `pane`) so you can see how identity resolved.

> **Trust model.** `$TMUX_PANE` is settable by anything with shell access, and the
> registry has no per-pane authentication. This widens *convenience*, not *security* —
> the model is "whoever has shell access is trusted," same as the rest of the bus.
> Don't run it on a box where that isn't true.

**Canonical names & aliases.** The bus addresses agents by canonical short name. The
discover walker, though, reads the name from `<cli> --resume <name>` in the process
tree — so a session launched as `--resume "My Long Session Name"` produces a running
name that won't match a short canonical. Register an alias to bridge it:

```text
tmux-msg.register name=alice alias='My Long Session Name'
```

After that, `discover` and the mailman's drift-check resolve the long name back to
`alice`. Multiple aliases per canonical are supported. If two canonicals both
substring-match one running value, the resolver logs `drift_check_ambiguous` rather
than guess — add an explicit alias on the one you meant.

## Storage

SQLite (WAL mode), two tables; the DB lives at `/var/lib/tmux-msg/messages.db`:

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
  pane_id     TEXT,                        -- "%3", refreshed by discovery
  paused      INTEGER NOT NULL DEFAULT 0,  -- the kill switch
  updated_at  TEXT NOT NULL DEFAULT (datetime('now','subsec'))
);
```

## Versioning

tmux-msg follows [Semantic Versioning](https://semver.org/) at the `0.x.y` cadence;
minor bumps may break compatibility while the shape settles, patch bumps stay
backward-compatible within a minor. See `CHANGELOG.md` for what shipped per release.

```bash
$ claude-msg --version
claude-msg v0.7.0
```

A binary built via `make build` stamps the version from `git describe`; a bare
`go build` reports `dev`.

## Development

```bash
go vet ./...
go build ./...
go test -race -count=1 ./...    # CI runs without -race (the runner lacks cgo)
```

See [`docs/`](docs/) for the operator guides (observe-gate, diagnostic playbook,
failure modes, security) and `docs/adr/` for the architecture decision records.
Contributions welcome — open an issue or a PR.

## Removal

```bash
sudo ./uninstall.sh            # stops mailmen, removes the binary, leaves the DB
sudo ./uninstall.sh --purge    # also wipes /var/lib/tmux-msg/ (interactive confirm)
```

`uninstall.sh` is idempotent. It leaves alone (remove by hand if you want them gone):
`/etc/tmux-msg/` (host config), the MCP entry in `~/.claude.json` (`claude mcp remove
tmux-msg -s user`), `loginctl enable-linger`, and `/var/lib/tmux-msg/` (history,
default-preserved; `--purge` wipes it).

## License

MIT.

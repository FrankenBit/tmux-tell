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
> `tmux-msg-claude` is the binary built for Claude Code today; sibling binaries
> (`tmux-msg-codex`, `tmux-msg-copilot`) could be built from the same substrate — the
> binary name encodes the substrate (`tmux-msg`) plus the CLI-tool adapter (`claude`).
> The repo name reflects what the substrate *is*, not which tool runs on top.
> (Originally named `cli-semaphore`; re-grounded on the substrate's primitive in
> v0.5.0. The adapter binary was `claude-msg` before v0.9.0 — see Install for the
> deprecation alias.)

## How it works

```
   agent-a ──►┌─────────────────────────────────────┐──► mailman@agent-c
   agent-b ──►│  SQLite mailbox (messages, agents)  │    (single writer to its pane)
              └─────────────────────────────────────┘
   reply ──►  tmux-msg-claude send --reply-to <id> --to agent-a "…"
```

**Senders** never touch tmux — `tmux-msg-claude send` validates the message, checks the
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

`install.sh` builds `bin/tmux-msg-claude`, installs it to `/usr/local/bin/tmux-msg-claude`,
creates `/var/lib/tmux-msg/` (holds `messages.db`), and drops the systemd user
template (`tmux-msg-claude-mailman@.service`) into `~/.config/systemd/user/`. Pick a
specific adapter with `--adapter=claude` (the default). Then, **as your user (not
root)**:

```bash
sudo loginctl enable-linger "$USER"   # keep the user manager running across reboots
systemctl --user daemon-reload        # so the mailman unit is visible
```

> **Renamed in v0.9.0 (`claude-msg` → `tmux-msg-claude`, #177).** The binary, the
> systemd template (`claude-mailman@` → `tmux-msg-claude-mailman@`), and the agent-name
> env var (`$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME`) were renamed to encode the
> substrate + adapter. For one deprecation cycle (removed **v0.11.0**, per ADR-0008's
> two-minor floor) `install.sh` keeps `claude-msg` and `claude-mailman@` working as
> aliases, and the identity layer still reads `$CLAUDE_AGENT_NAME` as a fallback —
> each emits a `WARN deprecated_surface_used … removal=v0.11.0` when used. Migrate
> scripts, units, and env to the new names at your leisure before v0.11.0.

### What runs as root, and what runs as you

`sudo ./install.sh` asks for root, but root's reach is deliberately narrow.
**As root** the script does exactly two privileged things: installs the
binary to `/usr/local/bin/tmux-msg-claude` (mode `0755`, owned `root:root`) and
creates `/var/lib/tmux-msg/` owned by *you*, the operator. **As you** —
never as root — it runs `go build`, chowns the data dir + the systemd
template to your account, and (after install) the mailman daemons run in
your linger-enabled `systemctl --user` session. No daemon ever runs as
root; root touches nothing but the binary path and the data-dir creation.

The operator account is resolved from `$SUDO_USER` (set by `sudo`), falling
back to `$USER`. There is **no hardcoded fallback** — if neither resolves
(or resolves to `root`), the installer fails loud rather than guessing an
owner. To install for a different target user without `sudo`, set it
explicitly: `OPERATOR_USER=alice ./install.sh`.

That boundary is the whole point of shipping the installer as a readable
shell script: the same "audit it in an afternoon" property the bus itself
offers applies to the install story too — you can confirm exactly which
two operations need root before you grant it.

## Quickstart

From two panes in the same tmux session:

```bash
tmux-msg-claude register --name alice     # in pane A — registers + starts alice's mailman
tmux-msg-claude register --name bob       # in pane B
tmux-msg-claude send --to bob "first message across the bus"
```

`bob`'s pane shows:

```
[Alice · 14:02:09 · id 7f3a]

first message across the bus
```

That's the whole loop. `send` returns `{"ok":true,"id":"7f3a","queued":1, "recipient":{…}}`
on success (or `{"ok":false,"error":"…"}` with a sysexits-style exit code on failure).

The `recipient` block (#152) reports the recipient's **send-time disposition** so the
sender knows where the message is headed: `registered`, `alive` (pane present),
`delivery_mode`, `mailman_running`, and `pane_status` (`live`/`paused`/`unknown`). An
**unregistered** recipient is always fail-loud (the message is *not* queued — a typo
shouldn't sit unclaimed forever). Two opt-in flags refine this:

- `--strict` — also fail (`ok:false`) when the recipient is registered but **not
  reachable** (pane gone). Without it, a registered-but-dead recipient still queues
  (the message waits for the pane to return) and the block reports `alive:false`.
- `--wait-for-delivered [--timeout 10s]` — block until the message reaches a terminal
  delivery state, then return a `delivery` block (`state` + `verify_ms`) — the
  synchronous "delivered?" confirmation without a follow-up `track`/`message_status`
  poll. (The verified-vs-unverified split isn't surfaced here — both are `delivered`
  in the DB, per #169.)
- `--block-on-stale` — with `--reply-to`, refuse the send (`ok:false`) when the thread
  has moved since you last spoke. See the `thread_freshness` block below.

When the send carries `--reply-to <id>`, the response adds a **`thread_freshness`**
block (#155) — the crossed-message guard. Async bus traffic means replies cross in
flight: you `reply_to` a thread-state that an inbound you haven't read may already have
superseded. The block reports `{stale, newer_in_thread[], you_replied_to,
latest_in_thread}`, where `newer_in_thread` lists messages in the reply chain that are
**addressed to you and arrived after your own last message in that chain** — "the thread
moved since you last spoke." That's a substrate-knowable signal (reply_to walk + arrival
order + to/from); it deliberately does *not* claim "messages you haven't *processed*",
which the substrate can't know — a `delivered` paste is in your context stream but may
not be attended-to (per #155's semantic correction; see also #169). By default `stale`
is informational and the send still succeeds; `--block-on-stale` turns it into a hard
refusal so you can re-read before replying.

The same fields are available over MCP (`tmux-msg.send` with `strict` /
`wait_for_delivered` / `timeout` / `block_on_stale`). The response schema is a named
struct contract that later disposition features (#157) extend.

To confirm a freshly-registered agent is reachable *without* sending it a message,
`tmux-msg-claude ping bob` probes daemon-up + pane-live (no pane paste) — see
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

**Length marker** — either shape gains a trailing `· <size>` when the body exceeds a
byte threshold (default 512 bytes), so a reader scrolling history can tell a two-line
ack from a 3K wall of review text, and a sender sees the size cost of what they're
about to send:

```
[Surveyor → Quartermaster · re abad · id 4825 · 2.3k]

<a long, substantive review body…>
```

The count is the body's byte length. Sizes read `<n>b` under 1000 bytes and `<n.n>k`
above (×1000, so `2.3k` is 2300 bytes — the lowercase suffix borrows the `du -h`/`ls -h`
look, but the math is decimal, not 1024-based, so a threshold maps cleanly back to a
marker). The threshold is configurable via the `render-byte-marker-threshold` TOML key
(fleet `[defaults]` + per-`[agent.<name>]` override), e.g. `render-byte-marker-threshold = "2k"`;
set it above any realistic message size to suppress the marker entirely.

**Compact chrome** — set `--quick` (CLI) or `quick=true` (MCP) to collapse the full
bracket-header block to a single line; for routine acks where typing-overhead-to-signal
ratio is high:

```
✓ Bosun · acked, ⚓
✓ Quartermaster · re bd19 · acked, ⚓
```

The compact form preserves the load-bearing fields — sender, optional thread linkage (`re
<id>` when `reply_to` is set), and content — and drops the spatial framing (no timestamp,
no message id, no blank line between envelope and body). The `✓` prefix marks the shape
at a glance so a reader scrolling history can distinguish it from a regular bracket-header
message. `no_reply_expected`, if set, is preserved as a `🔕` prefix on the body. The
length marker is not applied to quick messages (single-line chrome is already the
compactness signal). Sister to `--no-reply-expected` (#145): `--no-reply-expected`
reduces unnecessary acks; `--quick` reduces the overhead of necessary acks.

System-generated messages (`delivery_failure_notice`, `stranded_draft`) carry their
own chrome so they're distinguishable from agent traffic. Sender names render
title-cased in the header; stored agent names are lowercase by convention.

## Caps

| Cap | Default | Why |
|---|---|---|
| Per-recipient queue depth | 5 | a pane that isn't draining is wedged — fail fast, don't accumulate |
| Per-sender backlog | 2 | one runaway agent can't starve the others |
| Body size | 16 KB | anything bigger should be a file reference, not a tmux paste |
| Recipients per send | 10 | limits blast radius on multi-recipient fan-out (#158); configurable via `max-recipients-per-send` |

`send` rejects with `{"ok":false}` when a cap is exceeded.

## Delivery modes

Each registered agent has a `delivery_mode`:

| mode | what the mailman does | the recipient's view |
|---|---|---|
| **`paste-and-enter`** *(default)* | pastes into the agent's pane through the observe-gate | messages **appear in the pane** — no inbox polling needed; the substrate pushes |
| **`mailbox-only`** | does not paste (no pane to push into); messages stay queued | the recipient **polls** `tmux-msg-claude inbox` / `tmux-msg.inbox` to read them |

`mailbox-only` makes a plain shell a bus *destination* without an always-on agent
session — e.g. your own shell: agents `send to=you` and you read when you choose. Set
it via MCP (`register … delivery_mode=mailbox-only`), CLI (`register --name you
--delivery-mode mailbox-only`), or a per-agent TOML block. Precedence (highest wins):
**per-agent block > `[defaults]` > the DB column > compiled default (`paste-and-enter`)**.
`tmux-msg-claude config show` prints the resolved value per agent.

## Delivery semantics: the observe-gate

Before each delivery the mailman runs the **observe-gate** — a near-read-only check
that waits for a safe moment to paste so it never lands on top of something you're
typing. It polls the recipient's state (idle / working / awaiting-operator /
mid-compaction / unknown) and:

- **idle** → delivers immediately (~3–5s typical);
- **you're typing** → holds, drops a single 📫 in your input row so you know something's
  queued, and delivers once you stop (or, if your draft sits untouched past a
  threshold, archives it safely as a `stranded_draft` bookmark — recover it with
  `tmux-msg-claude stranded` — and then delivers);
- **busy / compacting / unknown** → waits with progressive backoff, up to a 5-minute
  `MaxWait` cap, then delivers anyway and logs `WARN gate_max_wait` (fail-loud, never
  fail-silent).

It's *near*-read-only: apart from the optional 📫 nudge (opt out with
`notify-emoji-disabled`), it mutates nothing while observing — no probe characters in
your input row. The residual "decided-idle then pasted" race is caught by the
verify-token + `delivered_unverified` safety net.

**Verified vs unverified deliveries (#169).** After a paste+Enter, the mailman looks
for a verify token to confirm the message actually surfaced. If it does, the delivery
is *verified*; if the token never appears in the retry budget (typically the recipient
was mid-turn and Enter was queued), the message still landed in the pane but the
delivery is *unverified* — a soft outcome, logged `WARN delivered_unverified`. Both
are `state = delivered`: the message IS in the recipient's pane either way, so the
state isn't a failure. The distinction is carried by a durable `verified` column on
the row (`1` = verified, `0` = unverified, `NULL` = delivered before this marker
existed — never retroactively guessed), so it's queryable from the DB rather than only
from the journal. `tmux-msg-claude stats` reports the split; `tmux-msg-claude resend <id>` is the
recovery path for one you want to re-send (an unverified delivery needs `--force`,
since the DB can't yet tell a *confirmed* unverified from a verified one per-row — the
journal does, and surfacing that per-row is the natural next consumer of this marker).

→ **Full operator guide, decision matrix, knobs, and stale-draft recovery:
[`docs/observe-gate.md`](docs/observe-gate.md).**

## Operating the bus

```
tmux-msg-claude send   --to Y[,Z,...] [--reply-to ID] [--strict] [--wait-for-delivered] [--block-on-stale] "body"  # one-shot; --to a,b,c fans to multiple recipients (#158)
tmux-msg-claude resend ID [--force]                     # replay a failed/unverified message (#157)
tmux-msg-claude ping   AGENT [--timeout D] [--format json]   # reachability probe (no pane paste)
tmux-msg-claude inbox  AGENT [--state STATE]            # list messages for AGENT
tmux-msg-claude sent   [--since DUR] [--state STATE] [--to AGENT]  # sender's outbox (#159)
tmux-msg-claude track  ID [--watch]                     # delivery state of one message
tmux-msg-claude get    ID                               # fetch a processed message by id
tmux-msg-claude status [--today]                        # paused state + queue depths per agent
tmux-msg-claude stats  [--window all|7d|1h] [--agent X] [--pair]  # on-demand bus-traffic aggregates
tmux-msg-claude digest [--since today|week|24h] [--counterparty X]  # campaign-arc narrative summary
tmux-msg-claude tail   [--from X] [--to Y] [--kind K] [--state S]   # live cross-chamber firehose
tmux-msg-claude state  --agent AGENT                    # probe an agent's current activity
tmux-msg-claude health [--since DUR]                    # per-agent operational audit
tmux-msg-claude pause  AGENT | --all                    # halt delivery (queue keeps filling)
tmux-msg-claude resume AGENT | --all
tmux-msg-claude reset  --confirm [--hard]               # purge queued; --hard wipes audit log
tmux-msg-claude log    --thread ID                      # a reply chain, flat-chronological
tmux-msg-claude thread ID [--format tree|json]          # a reply chain, as a parent→child tree
tmux-msg-claude stranded list|show|prune                # recover flushed operator paste snapshots
tmux-msg-claude discover                                # re-derive agents.pane_id from tmux
```

**Kill switch & retention.** `pause` sets `agents.paused = 1`; the mailman stops
injecting (messages keep queuing up to the cap) until `resume`. History is free —
SQLite on disk; on mailman start, any row left `delivering` from a crashed run is
reset to `queued`. `reset` purges `queued` + `delivering`; `--hard` also wipes the
delivered audit log; `--confirm` is mandatory.

**Reachability probe.** `tmux-msg-claude ping <agent>` answers "is the daemon up + the
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

**Tracking delivery.** `tmux-msg-claude track <id>` shows where a message is
(`queued → delivering → delivered`, or `failed` with the reason in `error`);
`--watch` re-renders on each state change until terminal. From MCP, call
`tmux-msg.message_status {"id": "9c1d"}`.

**Diagnosing a failed or unverified message — `resend` (#157).** When a message
lands `failed`, or lands `delivered` but you can't tell whether it actually
surfaced in the recipient (a `delivered_unverified` — the paste landed but the
verify-token never came back in budget), the recovery path is `tmux-msg-claude resend
<id>`. It replays the original to its recipient as a *new* message whose body is
byte-identical to the original, carrying a `↻ Replayed: original sent at <ts>`
chrome marker so the recipient sees it's a re-send, not fresh content. The
response adds a `replay` block (`original_id`, `original_sent_at`,
`original_state`, `forced`). From MCP: `tmux-msg.resend {"id": "9c1d"}`.

The duplicate guard keeps an accidental re-run from spamming:

- A **`failed`** message replays directly — it never arrived.
- A **`delivered`** message is refused without `--force`. **This includes a
  delivered-but-unverified message**: the substrate has no `delivered_unverified`
  column — verified and unverified both read as `delivered`, and only a mailman
  journal line distinguishes them (#169). So recovering an unverified delivery
  means `tmux-msg-claude resend <id> --force`. Once #169 makes the verified/unverified
  split DB-queryable, a confirmed-unverified message could replay without
  `--force`; until then `--force` is the deliberate "yes, I know it may already
  have arrived" signal.
- A still **in-flight** message (`queued`/`delivering`) is likewise refused
  without `--force` — wait for a terminal state, or force a duplicate knowingly.

**Reading a reply thread.** Two views of the same `reply_to` chain (both resolve
the whole chain from *any* id in it — walk to root, then all descendants):
`tmux-msg-claude log --thread <id>` renders it **flat-chronological** (an audit view);
`tmux-msg-claude thread <id>` renders it as a **parent→child tree** (a navigation /
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

When you *write* into a chain with `send --reply-to <id>`, the substrate runs the same
walk to warn you if the thread moved since you last spoke — the `thread_freshness`
block, described under [the send loop](#quickstart). `thread`/`log` *read*
the chain; `thread_freshness` *guards a write* against replying to a superseded state.

**Bus-traffic stats.** `tmux-msg-claude stats` is the in-terminal "show me the bus
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

**Campaign digest.** `tmux-msg-claude digest` is the *qualitative* sibling to `stats`:
where `stats` answers "how much / how fast," `digest` answers "what conversations
happened and what's still owed." It reports a by-counterparty table (sent /
received / threads / closed / in-flight) plus an **in-flight threads** section
listing the reply-chains whose last word still awaits an answer — the day's-end
"what do I need to follow up on?" view. `--since` takes the calendar shortcuts
`today` / `yesterday` / `week` (alongside `all`, `<N>d`, and any duration);
`--counterparty X` scopes to conversations involving one agent; `--format json`
emits the structure. A thread counts as **closed** when its latest message is
marked `🔕` no-reply-expected (or the send failed) and **in-flight** otherwise —
a heuristic, not ground truth: the substrate can't know if a conversation is
*semantically* done, so the output says "likely needs follow-up," and setting
`--no-reply-expected` on a genuine last word is what keeps a closed thread out of
the list. System chrome (`delivery_failure_notice`, `stranded_draft`, `ping`) is
excluded from thread analysis.

**Live tail.** `tmux-msg-claude tail` is the cross-chamber firehose — all bus traffic,
live, filtered to what you care about. It's the view the per-mailman journals and
single-message `track` couldn't give: when a bug spans two chambers (the #137
walk-back needed exactly this), `tail --from X --to Y` shows the correlated stream
in one terminal. New rows print as they're inserted and `queued → delivered/failed`
transitions print on the same id (a multi-line lifecycle). Filters compose (AND):
`--from` / `--to` / `--kind` / `--state` / `--since`. `--since` defaults to `now`
(start live, no backfill) but takes any `parseWindow` spec (`5m`, `today`, `all`) to
backfill first. `--format json` emits one object per line for piping. Ctrl-C exits
cleanly.

The watch mechanism is **rowid-polling**, not SQLite's `update_hook`: the mailmen
that write rows are *separate processes* from the `tail` CLI, and `update_hook` only
fires for the connection that registered it (per-connection, same-process), so it
would never see their writes. `tail` polls `MAX(id)` since-last-seen (configurable
`--interval`, default 300ms) and re-reads in-flight ids for state transitions; WAL
mode keeps these reads safe concurrent with mailman writes.

**Delivery-failure notifications.** When an outbound message hits a terminal-failure
state (`failed` or `delivered_unverified`), the mailman auto-inserts a
`delivery_failure_notice` back to the sender (original id, recipient, failure class,
reason, 200-char preview). These bypass the queue caps so they're never dropped, and
a notice that itself fails does not generate another (no wedged-pane cascade). Both
`--notify-on-failed` and `--notify-on-delivered-unverified` default on.

**Recovering a flushed paste.** When the observe-gate archives your in-flight
input before pasting over it (see below), it stores the snapshot as a
`stranded_draft` bookmark. `tmux-msg-claude stranded list` shows your bookmarks (id,
pane, timestamp, byte-size); `tmux-msg-claude stranded show <id>` prints the recovered
content (`-o file` writes it out, for long pastes); `tmux-msg-claude stranded prune
--older-than 7d` clears old ones. Note: the snapshot holds whatever the substrate
captured from the input row — for a large bracketed paste tmux may have shown only
its `[Pasted text #N +M lines]` placeholder rather than the literal text, so
recovery is best-effort on big pastes.

**When a message seems to go missing,** walk the sender-first triage in
[`docs/diagnostic-playbook.md`](docs/diagnostic-playbook.md) — it starts from the
SQLite store (did the send reach the bus at all?) before the receiver's mailman
journal.

## Use from Claude Code (MCP)

The same binary speaks MCP over stdio under `tmux-msg-claude mcp`, exposing
`tmux-msg.send / control / agents / whoami / inbox / status / register / unregister /
message_status / agent_state` as native tools. **One user-level config; identity is
auto-resolved per pane.** Add the server once in `~/.claude.json`:

```json
{
  "mcpServers": {
    "tmux-msg": {
      "command": "/usr/local/bin/tmux-msg-claude",
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
Equivalent CLI: `tmux-msg-claude register --name myname`.

The register response includes a **`queued`** count — the number of messages already
waiting for this agent at register time (#151). A fresh or post-restart session (e.g.
the spawn-per-task pattern, or a chamber that lost its pane and re-registers) learns it
has backlog without a separate `tmux-msg.inbox` poll: if `queued > 0`, run
`tmux-msg.inbox` to read it. The count is informational and never blocks registration;
on the rare event the count can't be read, the response carries `queued_error` instead
and registration still succeeds.

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
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | tmux-msg-claude mcp
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
surface is a CLI subcommand (`tmux-msg-claude control --to … --command …`) for scripts and
sessions whose MCP isn't loaded.

**Self-compact with a follow-up.** `/compact` leaves the session at an empty prompt;
`command=compact` accepts a `resume_with` string (self-invocation only). The handler
queues the `/compact` plus the resume message (threaded via `reply_to`), and the
mailman holds the queue for `--post-compact-pause` (default 120s) so the follow-up
lands after the CLI tool has settled, not into the slash-command parser mid-compaction.

### Removing a pane

`tmux-msg.unregister name=oldname` (or `tmux-msg-claude unregister`) stops the mailman,
drops the agent row, and optionally purges its history (`purge_messages: true`).

### New tools require a session restart

MCP tool lists are sent once during the `initialize` handshake and not refreshed.
Updating the binary and restarting the mailmen makes new tools available to *future*
sessions; sessions started earlier stay pinned to the tool surface they initialized
with. `mcp-restart-tmux-msg` (#28) re-initializes one session's MCP stdio without
losing context; for a fleet, `tmux-msg-claude refresh-all-mcps` fires it per registered
agent (operator-only — a peer-invokable bulk restart would be a DoS amplification
class).

## Identity, names & aliases

**Identity precedence** (shared by the MCP server and the CLI): (1) explicit override
— `--from` on `send`, `--as` on `whoami`, or `$TMUX_AGENT_NAME`; (2) `$TMUX_PANE` →
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
backward-compatible within a minor. See `CHANGELOG.md` for what shipped per release, and [ADR-0008](docs/adr/0008-deprecation-policy.md) for the post-1.0 deprecation policy (two-minor-cycle floor).

```bash
$ tmux-msg-claude --version
tmux-msg-claude v0.9.0
```

A binary built via `make build` stamps the version from `git describe`; a bare
`go build` reports `dev`.

### Release stability (the K-counter)

The road to `1.0` is gated on **K=3**: three consecutive releases with no
breaking change across any of the five public surfaces — MCP tool schemas, CLI
subcommand args/flags/exit codes, `--format json` shapes, the DB schema, and the
exported Go API (`discover` / `store` / `tmuxio`). Each clean cut increments K;
any break on a tracked surface resets it to 0.

**Current K: 3 of 3.** The `cli-semaphore → tmux-msg` substrate rename (v0.5.0)
and the MCP wire-protocol rename (v0.6.0) were the last deliberate breaks; v0.7.0,
v0.8.0, and v0.9.0 have each been non-breaking. The v0.9.0 `claude-msg →
tmux-msg-claude` rename ships with one-cycle aliases + fallback + WARN (per
ADR-0008) so no operator config breaks at the cutover — deprecation-with-functioning-alias
preserves K-counter progress. The Sea-trials milestone's K=3 gate clears at
v0.9.0. The live per-release record lives in the tracker at
[#163](https://git.frankenbit.de/frankenbit/tmux-msg/issues/163).

## Development

```bash
go vet ./...
go build ./...
go test -race -count=1 ./...    # CI runs without -race (the runner lacks cgo)
```

See [`docs/`](docs/) for the operator guides (observe-gate, diagnostic playbook,
failure modes, security) and `docs/adr/` for the architecture decision records.
Contributions welcome — see [`CONTRIBUTING.md`](CONTRIBUTING.md). **Building on tmux-msg downstream?** `CONTRIBUTING.md` records the external-contract commitments — the exported Go API + DB schema as stability surfaces — for consumers like Binnacle, which composes with tmux-msg as an external Go module ([ADR-0007](docs/adr/0007-binnacle-coexist-external-contract.md)).

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

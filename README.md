# tmux-tell

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

tmux-tell lets them tell each other instead. The reviewer finishes and notifies the
implementer; the implementer warns the tester the moment the contract moves. You set
the work up and let the agents keep each other current.

→ **[Why tmux-tell? — the longer pitch](docs/why.md)**

No cloud, no daemon phoning home: it's a SQLite file and a tmux paste. You can read
every message with `sqlite3`, and uninstall is one script.

## What it is — and what it isn't

- **It is:** local inter-agent messaging for CLI tools sharing one tmux server — one
  mailbox per pane, a single writer per mailbox (no paste-races), delivery you can
  watch happen.
- **It isn't:** a networked message queue, a multi-host bus, a chat app, or a job
  scheduler. It moves a message from one pane into another, safely, while a human
  might also be using that pane.

> **Why the name has two parts.** `tmux-tell` is the substrate — the tmux pane
> registry, the paste-and-Enter delivery, the per-pane state detection. The
> `tmux-tell-claude` binary is that substrate plus the adapter for the CLI tool in the
> pane (Claude Code today). The repo name reflects what the substrate *is*, not which
> tool runs on top.

## How it works

```
   agent-a ──►┌─────────────────────────────────────┐──► mailman@agent-c
   agent-b ──►│  SQLite mailbox (messages, agents)  │    (single writer to its pane)
              └─────────────────────────────────────┘
   reply ──►  tmux-tell-claude send --reply-to <id> --to agent-a "…"
```

**Senders** never touch tmux — `tmux-tell-claude send` validates the message, checks the
caps, and inserts a row. **Mailmen** are per-agent daemons (systemd user services)
that loop on their inbox, paste the formatted message into the target pane through the
[observe-gate](#delivery-semantics-the-observe-gate), and mark it delivered. Because
each recipient has exactly one mailman, the usual tmux concurrency hazards (paste-buffer
races, idle-check TOCTOU, turn concatenation) collapse to a single-writer invariant.

Not every recipient has a pane to push into. An agent registered `mailbox-only` (e.g.
your own shell) is a bus *destination* without an always-on session — the mailman never
pastes; its queue is drained on demand with `tmux-tell-claude inbox`, `inbox --ack`, or
the interactive `inbox --watch` TUI (live list + cursor-nav + one-key ack). A third mode,
`hook-context`, delivers via Claude Code's lifecycle hooks — the recipient's session pulls
pending messages as `additionalContext` on its next turn instead of being pasted into
(#249). See [delivery modes](docs/reference.md#delivery-modes) in the operator reference.

## Install

On a Linux host with tmux, sqlite3, and Go (≥ 1.24):

```bash
# from inside a tmux session:
git clone https://github.com/FrankenBit/tmux-tell && cd tmux-tell
make build
sudo ./install.sh        # installs the binary + the systemd user template
```

`install.sh` builds `bin/tmux-tell-claude`, installs it to `/usr/local/bin/tmux-tell-claude`,
and drops the systemd user template (`tmux-tell-claude-mailman@.service`) into
`~/.config/systemd/user/`. The DB (`messages.db`) lives under your user-home
(`$XDG_DATA_HOME/tmux-tell` or `~/.local/share/tmux-tell/`) and is created lazily on
first use — no install-time data dir to create or chown. Pick a specific adapter with
`--adapter=claude` (the default). Then, **as your user (not root)**:

```bash
sudo loginctl enable-linger "$USER"   # keep the user manager running across reboots
systemctl --user daemon-reload        # so the mailman unit is visible
```

### What runs as root, and what runs as you

`sudo ./install.sh` asks for root, but root's reach is deliberately narrow: **as root**
it does exactly one privileged thing — installs the binary to
`/usr/local/bin/tmux-tell-claude`. Everything else (`go build`, the systemd template, the
mailman daemons) runs as your user, never as root — and the DB lives under your user-home,
created lazily by the binary with no privileged step at all. That's the whole point of
shipping the installer as a readable shell script: you can confirm exactly which operation
needs root before you grant it.

→ Full breakdown — the `$SUDO_USER` resolution, the `OPERATOR_USER` override, the
no-hardcoded-fallback rule: [operator reference → Install internals](docs/reference.md#install-internals-what-runs-as-root).

## Quickstart

From two panes in the same tmux session:

```bash
tmux-tell-claude register --name alice     # in pane A — registers + starts alice's mailman
tmux-tell-claude register --name bob       # in pane B
tmux-tell-claude send --to bob "first message across the bus"
```

`bob`'s pane shows:

```
[Alice · 14:02:09 · id 7f3a]

first message across the bus
```

That's the whole loop. `send` returns `{"ok":true,"id":"7f3a","queued":1, "recipient":{…}}`
on success (or `{"ok":false,"error":"…"}` with a sysexits-style exit code on failure).

> The disposition flags (`--strict` / `--wait-for-delivered` / `--block-on-stale`),
> the `thread_freshness` crossed-message guard, and the full command set live in the
> **[operator reference](docs/reference.md)**.

To confirm an agent is reachable *without* sending it a message,
`tmux-tell-claude ping <name>` probes daemon-up + pane-live (no pane paste).

## Caps

| Cap | Default | Why |
|---|---|---|
| Per-recipient queue depth | 5 | a pane that isn't draining is wedged — fail fast, don't accumulate |
| Per-sender backlog | 2 | one runaway agent can't starve the others |
| Body size | 16 KB | anything bigger should be a file reference, not a tmux paste |
| Recipients per send | 10 | limits blast radius on multi-recipient fan-out; configurable via `max-recipients-per-send` |

`send` rejects with `{"ok":false}` when a cap is exceeded.

## Delivery semantics: the observe-gate

Before each delivery the mailman runs the **observe-gate** — a near-read-only check
that waits for a safe moment to paste so it never lands on top of something you're
typing. It polls the recipient's state and:

- **idle** → delivers immediately (~3–5s typical);
- **you're typing** → holds, drops a single 📫 in your input row, and delivers once you
  stop (archiving an untouched draft as a recoverable `stranded_draft` first if needed);
- **busy / compacting / unknown** → waits with progressive backoff up to a 5-minute cap,
  then delivers anyway and logs `WARN gate_max_wait` (fail-loud, never fail-silent).

A delivered paste the mailman couldn't confirm surfaced is marked *unverified*
(`delivered_in_input_box`) rather than failed — the message is in the pane either way.

→ **Full operator guide** (decision matrix, the five states, tuning knobs, stale-draft
recovery): [`docs/observe-gate.md`](docs/observe-gate.md). **The `verified` column and
the verified-vs-unverified split:** [operator reference](docs/reference.md#verified-vs-unverified-deliveries).

## Observability (Prometheus metrics)

Each mailman can expose a Prometheus `/metrics` endpoint for continuous monitoring
(throughput, the `delivered_unverified` rate, latency, queue depth, the talk-pair
heatmap). It's **off by default** — pass `--metrics-addr` (or set `metrics-addr` in
config) to turn it on, with no behavior change for deploys that don't scrape:

```bash
tmux-tell-claude serve --agent bob --metrics-addr :9099   # exposes http://…:9099/metrics
```

Each per-agent mailman is its own process, so give each agent a distinct port — the
clean way is a per-agent config block (`[agent.bob] metrics-addr = ":9099"`). A scraper
(Prometheus, Grafana Alloy, VictoriaMetrics' vmagent, …) then pulls each endpoint;
pull-only, no push-gateway needed.

Metrics exposed (all prefixed `tmux_tell_`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tmux_tell_messages_total` | counter | `from`, `to`, `state` | terminal delivery outcomes; `state` ∈ `delivered` / `delivered_in_input_box` / `failed` — the talk-pair heatmap source |
| `tmux_tell_delivery_latency_seconds` | histogram | `recipient` | queued→delivered wall-clock |
| `tmux_tell_delivery_verify_attempt_seconds` | histogram | `recipient` | time in the post-Enter verify-token retry loop |
| `tmux_tell_queue_depth` | gauge | `agent` | current queued (undelivered) depth, sampled each loop |
| `tmux_tell_mailman_loop_iterations_total` | counter | `agent` | serve-loop iterations (liveness + cadence) |
| `tmux_tell_paste_unsafe_aborts_total` | counter | `agent`, `reason` | deliveries aborted because the pane was paste-unsafe; `reason` ∈ `awaiting_operator` / `compaction` / `unknown` / `probe_failed` |

The endpoint is standard Prometheus text exposition, so any compatible scraper works —
point its scrape config at the per-agent `host:port/metrics`. The alcatraz Alloy scrape
job + the Grafana dashboard JSON (talk-pair heatmap, latency heatmap, unverified-rate
trend) live in the infra repo, not here.

## Deferred delivery (post-compaction self-handoff)

The mailman is **eager** — it delivers a queued message as soon as the pane is ready.
That's wrong for one specific pattern: handing your *post-`/compact`* self a note. Send it
normally and it lands *before* the compaction, gets folded into the summary, and the point
is lost. Deferred delivery fixes that — you **stage** the message and release it yourself
once you've resumed:

```bash
# before /compact — stage orientation for your next self
tmux-tell-claude send --to me --deliver-after=resume "remember: PR #256 is mid-review, ping Surveyor"

# as part of your post-/compact resume routine — release it into the fresh context
tmux-tell-claude flush --trigger=resume
```

A staged message sits in a `deferred` state: invisible to `inbox`, never picked up by the
mailman, not counted against queue caps. `flush --trigger=resume` (or the
`tmux-tell.flush_deferred` MCP tool) promotes your matching staged messages to the live
queue, where the mailman delivers them normally. `flush` is idempotent — calling it with
nothing staged is a harmless no-op, so it's safe to put in a resume routine
unconditionally. `tmux-tell-claude sent --deferred` shows what you've got staged.

You can only flush messages addressed to **you**, and a promoted message **jumps the
backlog floor** (§don't-flood) so re-registering between staging and flushing never skips
it. v1 ships the `resume` trigger (the post-compaction case); register-on-spawn,
timestamped reminders, and trigger composition are tracked in #258.

## Request-reply (`ask` / `wait_for_reply` / `check_replies`)

The reply-to chain is asynchronous — you send, they answer whenever. When you want to
**pause until answered**, request-reply bundles the wait:

```bash
ask_id=$(tmux-tell-claude ask --to bob "is CI green on main?" | jq -r .id)
tmux-tell-claude wait-for-reply "$ask_id" --timeout 60s   # blocks until bob replies / times out
```

`ask` is a `send` that returns an `ask_id`; `wait-for-reply` blocks until a reply lands
(returning it, with an `unverified` flag if its delivery wasn't verify-confirmed) or
times out; `check-replies` is the non-blocking poll for the work-while-waiting pattern.
Same three as MCP tools. Full semantics: [operator reference →
Request-reply](docs/reference.md#reading-a-reply-thread).

**Lightweight reply intent** — when you want to signal "I expect a reply" without the
blocking wait machinery, pass `--expects-reply` to `send`:

```bash
tmux-tell-claude send --to bob --expects-reply "please confirm deploy"
```

The message carries the marker; bob's queue and delivery are unchanged. Recipients can
find unanswered asks with `inbox --unanswered`; senders can track open asks with
`sent --awaiting-reply`. Both surfaces are also available as MCP tool parameters.

## Use from Claude Code (MCP)

The same binary speaks MCP over stdio under `tmux-tell-claude mcp`, exposing
`tmux-tell.send / control / agents / whoami / inbox / status / register / unregister /
message_status / agent_state` as native tools. **One user-level config; identity is
auto-resolved per pane.** Add the server once in `~/.claude.json`:

```json
{
  "mcpServers": {
    "tmux-tell": {
      "command": "/usr/local/bin/tmux-tell-claude",
      "args": ["mcp"]
    }
  }
}
```

→ Identity resolution, the canonical name mapping, the control-command whitelist, and
session-restart semantics: [operator reference → Use from Claude Code](docs/reference.md#use-from-claude-code-mcp-details).

## Versioning

tmux-tell follows [Semantic Versioning](https://semver.org/) at the `0.x.y` cadence;
minor bumps may break compatibility while the shape settles, patch bumps stay
backward-compatible within a minor. See `CHANGELOG.md` for what shipped per release, and [ADR-0008](docs/adr/0008-deprecation-policy.md) for the post-1.0 deprecation policy (two-minor-cycle floor).

```bash
$ tmux-tell-claude --version
tmux-tell-claude v0.16.1
```

A binary built via `make build` stamps the version from `git describe`; a bare
`go build` reports `dev`.

### Release stability (the K-counter)

The road to `1.0` is gated on **K=3** — three consecutive releases with no breaking
change across the five public surfaces (MCP tool schemas, CLI args/flags/exit codes,
`--format json` shapes, the DB schema, the exported Go API). Each clean cut increments
K; any break resets it to 0. **Current K: 8** (the gate cleared at v0.9.0; the counter
keeps raising and retires at v1.0).

→ The per-surface rules, the deprecation-preserves-K nuance, and the live per-release
record: [operator reference](docs/reference.md#versioning-and-the-k-counter) and
[`CHANGELOG.md`](CHANGELOG.md).

## Development

```bash
go vet ./... && go build ./... && go test -race -count=1 ./...   # CI runs without -race (runner lacks cgo)
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the contributor guide and the
external-contract commitments — the exported Go API + DB schema as stability surfaces,
for downstream consumers like Binnacle ([ADR-0007](docs/adr/0007-binnacle-coexist-external-contract.md)).
Operator guides live in [`docs/`](docs/), architecture decisions in [`docs/adr/`](docs/adr/).

## Removal

```bash
sudo ./uninstall.sh            # stops mailmen, removes the binary, leaves the DB
sudo ./uninstall.sh --purge    # also wipes ~/.local/share/tmux-tell/ (interactive confirm)
```

`uninstall.sh` is idempotent. It leaves alone (remove by hand if you want them gone):
`/etc/tmux-tell/` (host config), the MCP entry in `~/.claude.json` (`claude mcp remove
tmux-tell -s user`), `loginctl enable-linger`, and the user-home DB dir
(`~/.local/share/tmux-tell/`, history, default-preserved; `--purge` wipes it).

## Where to go next

- **[Operator reference](docs/reference.md)** — every command, flag, and edge-case
  semantic: send/reply flags, message-rendering chrome, the full command set, MCP
  details, the storage schema, and migrating from `claude-msg`.
- **[Why tmux-tell?](docs/why.md)** — the longer pitch, and why this over raw
  `tmux send-keys` or a single session with subagents.
- **[The observe-gate](docs/observe-gate.md)** — how safe-moment delivery decides.
- **[Architecture (Arc42)](docs/arc42/)** — the 12-section architecture spine: goals, constraints, context, deployment, glossary (Phase 1), with the rest phasing in.
- **[Diagnostic playbook](docs/diagnostic-playbook.md)** — when a message seems to go missing.
- **[Trust boundaries & threat model](docs/security.md)** · **[Architecture decisions](docs/adr/)** · **[Contributing](CONTRIBUTING.md)**.

## License

MIT.

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

> **Why the name has two parts.** `tmux-msg` is the substrate — the tmux pane
> registry, the paste-and-Enter delivery, the per-pane state detection. The
> `tmux-msg-claude` binary is that substrate plus the adapter for the CLI tool in the
> pane (Claude Code today). The repo name reflects what the substrate *is*, not which
> tool runs on top.

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

### What runs as root, and what runs as you

`sudo ./install.sh` asks for root, but root's reach is deliberately narrow: **as root**
it does exactly two privileged things — installs the binary to
`/usr/local/bin/tmux-msg-claude` and creates `/var/lib/tmux-msg/` owned by *you*.
Everything else (`go build`, the chowns, the mailman daemons) runs as your user, never
as root. That's the whole point of shipping the installer as a readable shell script:
you can confirm exactly which two operations need root before you grant it.

→ Full breakdown — the `$SUDO_USER` resolution, the `OPERATOR_USER` override, the
no-hardcoded-fallback rule: [operator reference → Install internals](docs/reference.md#install-internals-what-runs-as-root).

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

> The disposition flags (`--strict` / `--wait-for-delivered` / `--block-on-stale`),
> the `thread_freshness` crossed-message guard, and the full command set live in the
> **[operator reference](docs/reference.md)**.

To confirm an agent is reachable *without* sending it a message,
`tmux-msg-claude ping <name>` probes daemon-up + pane-live (no pane paste).

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
tmux-msg-claude serve --agent bob --metrics-addr :9099   # exposes http://…:9099/metrics
```

Each per-agent mailman is its own process, so give each agent a distinct port — the
clean way is a per-agent config block (`[agent.bob] metrics-addr = ":9099"`). A scraper
(Prometheus, Grafana Alloy, VictoriaMetrics' vmagent, …) then pulls each endpoint;
pull-only, no push-gateway needed.

Metrics exposed (all prefixed `tmux_msg_`):

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tmux_msg_messages_total` | counter | `from`, `to`, `state` | terminal delivery outcomes; `state` ∈ `delivered` / `delivered_in_input_box` / `failed` — the talk-pair heatmap source |
| `tmux_msg_delivery_latency_seconds` | histogram | `recipient` | queued→delivered wall-clock |
| `tmux_msg_delivery_verify_attempt_seconds` | histogram | `recipient` | time in the post-Enter verify-token retry loop |
| `tmux_msg_queue_depth` | gauge | `agent` | current queued (undelivered) depth, sampled each loop |
| `tmux_msg_mailman_loop_iterations_total` | counter | `agent` | serve-loop iterations (liveness + cadence) |
| `tmux_msg_paste_unsafe_aborts_total` | counter | `agent`, `reason` | deliveries aborted because the pane was paste-unsafe; `reason` ∈ `awaiting_operator` / `compaction` / `unknown` / `probe_failed` |

The endpoint is standard Prometheus text exposition, so any compatible scraper works —
point its scrape config at the per-agent `host:port/metrics`. The alcatraz Alloy scrape
job + the Grafana dashboard JSON (talk-pair heatmap, latency heatmap, unverified-rate
trend) live in the infra repo, not here.

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

→ Identity resolution, the canonical name mapping, the control-command whitelist, and
session-restart semantics: [operator reference → Use from Claude Code](docs/reference.md#use-from-claude-code-mcp-details).

## Versioning

tmux-msg follows [Semantic Versioning](https://semver.org/) at the `0.x.y` cadence;
minor bumps may break compatibility while the shape settles, patch bumps stay
backward-compatible within a minor. See `CHANGELOG.md` for what shipped per release, and [ADR-0008](docs/adr/0008-deprecation-policy.md) for the post-1.0 deprecation policy (two-minor-cycle floor).

```bash
$ tmux-msg-claude --version
tmux-msg-claude v0.11.0
```

A binary built via `make build` stamps the version from `git describe`; a bare
`go build` reports `dev`.

### Release stability (the K-counter)

The road to `1.0` is gated on **K=3** — three consecutive releases with no breaking
change across the five public surfaces (MCP tool schemas, CLI args/flags/exit codes,
`--format json` shapes, the DB schema, the exported Go API). Each clean cut increments
K; any break resets it to 0. **Current K: 5** (the gate cleared at v0.9.0; the counter
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
sudo ./uninstall.sh --purge    # also wipes /var/lib/tmux-msg/ (interactive confirm)
```

`uninstall.sh` is idempotent. It leaves alone (remove by hand if you want them gone):
`/etc/tmux-msg/` (host config), the MCP entry in `~/.claude.json` (`claude mcp remove
tmux-msg -s user`), `loginctl enable-linger`, and `/var/lib/tmux-msg/` (history,
default-preserved; `--purge` wipes it).

## Where to go next

- **[Operator reference](docs/reference.md)** — every command, flag, and edge-case
  semantic: send/reply flags, message-rendering chrome, the full command set, MCP
  details, the storage schema, and migrating from `claude-msg`.
- **[Why tmux-msg?](docs/why.md)** — the longer pitch, and why this over raw
  `tmux send-keys` or a single session with subagents.
- **[The observe-gate](docs/observe-gate.md)** — how safe-moment delivery decides.
- **[Diagnostic playbook](docs/diagnostic-playbook.md)** — when a message seems to go missing.
- **[Trust boundaries & threat model](docs/security.md)** · **[Architecture decisions](docs/adr/)** · **[Contributing](CONTRIBUTING.md)**.

## License

MIT.

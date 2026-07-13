# The operator's manual: running tmux-tell, day 0 to day N

You've cloned the repo. This is the walkthrough that takes you from there to a
setup you trust — install, first agent, first message, the first time something
looks wrong, and the rhythm of living with it afterward.

It is deliberately **not** a command reference. [`reference.md`](reference.md)
is the catalogue — every flag, every delivery semantic, every recovery
procedure. This manual is the *arc*: what to do, in what order, and **why that
order**. Where you need the exact WHAT, you'll find a link. Read this once
front-to-back; reach for the others when a link sends you.

> **A note on names.** The tool is **`tmux-tell`** (binary `tmux-tell-claude`),
> renamed from `tmux-msg` (#440). The legacy `tmux-msg-claude` / `tmux-msg-codex`
> binary names — plus the `CLAUDE_MSG_DB` / `CLAUDE_MSG_CONFIG` env vars and the
> old `tmux-msg` data/config paths — keep working as deprecated aliases (each
> emits a WARN) through the v1.0 boundary per
> [ADR-0008](adr/0008-deprecation-policy.md). This manual uses the canonical
> `tmux-tell` names; see [reference.md → Migrating from tmux-msg](reference.md#migrating-from-tmux-msg).

---

## Day 0 — install, and prove it's listening

From inside a tmux session:

```bash
git clone https://github.com/FrankenBit/tmux-tell && cd tmux-tell
make build && sudo ./install.sh --system   # alcatraz uses the system-wide install (root-owned /usr/local/bin)
systemctl --user daemon-reload             # so the mailman unit becomes visible
```

> Since #636 a bare `./install.sh` (no flags) is a **user-space** install (no
> root, binary under `~/.local/bin`) — the adopter-friendly default. Alcatraz
> keeps the `--system` install: the chambers + the deploy chain expect
> `tmux-tell-claude` on the system `PATH` at `/usr/local/bin`.

Three things now exist, and it's worth knowing what each is — they're the whole
system:

- **`tmux-tell-claude`** — one binary. It's the CLI you'll run, the MCP server an
  agent talks to, and the mailman daemon, depending on how it's invoked. No
  service mesh, no broker.
- **A SQLite database** — your mailbox store, at
  `~/.local/share/tmux-tell/messages.db`. Every message is a row. You can read
  every message with `sqlite3`; there is nothing else holding state.
- **A systemd *user* unit** — the mailman template
  (`tmux-tell-claude-mailman@.service`). One instance per registered agent
  watches that agent's pane and delivers to it. **User**, not system — it runs
  as you, needs no root, and dies with your login session unless lingering is on.

**Prove the binding before you trust anything.** The single most useful day-0
habit: confirm the binary you just installed is reading the database you think
it is. On startup, the mailman logs the **resolved DB path and where that path
came from** (#290) — the env var, the XDG default, or a flag. Read it:

```bash
journalctl --user -u 'tmux-tell-claude-mailman@*' -n 50 | grep claude_msg_db
# → serve: claude_msg_db=/home/you/.local/share/tmux-tell/messages.db source=default(env unset)
```

If that path isn't the one you expect, *stop here and fix it* — almost every
"tmux-tell ate my message" story downstream is really two processes reading two
different files. (You'll meet the worst version of this on Day N.) The exact
resolution order and the override knobs are in
[`reference.md`](reference.md) under DB binding.

---

## Your first agent — register a pane

A pane isn't reachable until you register it:

```bash
tmux-tell-claude register --name alice    # run this *in* the pane you mean
```

Registration does two things. It writes a row binding the **name** `alice` to
this pane's `$TMUX_PANE` id — that's the agent's identity, and from now on you
address `alice`, never a pane number (panes get renumbered when you split or
close; the name doesn't). And it starts `alice`'s **mailman**: the per-recipient
daemon that will watch this pane and deliver messages into it.

Register a second pane as `bob` the same way, and you have two agents that can
reach each other.

The identity binding is `name → $TMUX_PANE → registry`. That's load-bearing and
it's also the thing that *drifts* — if a pane respawns or a session is rebuilt,
the registry can end up pointing at the wrong pane. You don't need to worry about
that today; just know the word **`discover`** (it re-walks panes and repairs the
mapping) for the day it bites. The identity model and its trust assumptions live
in [`security.md`](security.md); the agent's-eye view of drift is in the
[agent's manual](agent-manual.md).

---

## Surviving reboot — auto-register on every chamber launch

Registry drift after a reboot is the predictable form of the pane-respawn case.
Tmux re-numbers panes on every server start, but the registry's `name → pane_id`
mappings persist (SQLite). On boot, the mailmen come up bound to the
*pre-reboot* pane ids and immediately enter `pane_not_found_backoff` retry;
deliveries stall until something triggers a re-registration.

The fix is to re-register on every chamber launch. A wrapper around your
chamber's launch line that calls `register --force` makes the registry
self-heal at start, every time:

```bash
#!/bin/bash
# my-chamber-launch.sh
tmux-tell-claude register --name "$CHAMBER" --pane "$TMUX_PANE" --force \
    --start-mailman=false
systemctl --user restart "tmux-tell-claude-mailman@${CHAMBER}.service"
exec your-chamber-binary "$@"
```

Three things to know:

- `--force` makes it safe to re-apply at every launch — same registry shape,
  no error if already registered.
- The mailman restart gives you *immediate, deterministic* clean state.
  Without it, a mailman in `pane_not_found_backoff` self-heals on `register
  --force` along one of two paths, both with latency: a message-arrival
  during the backoff window re-triggers the agent re-read, or — once the
  mailman parks itself in the stuck state (after `stuck-threshold`
  consecutive pane-not-found failures, exponential backoff 1/2/4/8/16/32/60s)
  — its `stuck-poll-interval` re-reads the agent row every 5s on a pure
  DB read specifically to notice the `register --force` clear. The restart
  skips both latencies and any in-flight delivery state cached against the
  stale pane.
- This composes with cgroup-based memory caps or any other launch-time
  hygiene the wrapper already does — it's one more pre-launch line.

Worked instance: alcatraz-infra's `chamber-claude.sh` follows this convention
(tmux-tell#532).

---

## Your first message — send it, watch it land, read the row

```bash
tmux-tell-claude send --to bob "first message, addressed to bob"
```

Switch to `bob`'s pane. If `bob` is idle, the line appears almost at once, headed
with who sent it and when:

```
[Alice · 14:02:09 · id 7f3a]

first message, addressed to bob
```

If you were *mid-typing* in `bob`'s pane when it arrived, you'd have seen
something better: a single 📫 in the input row, the message **held**, and then —
the moment you paused — the text landing in the now-clear prompt. That waiting is
the [**observe-gate**](observe-gate.md), the feature that makes delivery
into a pane *you might also be using* livable instead of infuriating. It's worth
reading that doc early; it's the part of the design that's genuinely unusual.

Now read the row. The message wasn't fired-and-forgotten — it's a record that
moved through states:

```bash
sqlite3 ~/.local/share/tmux-tell/messages.db \
  "SELECT from_agent, to_agent, state FROM messages ORDER BY id DESC LIMIT 5;"
```

`queued → delivering → delivered` (or `failed`). The sender can *know* it landed;
you can *audit* what happened. That's the difference between this and a raw
`tmux send-keys` — which fires into whatever's there and forgets. The full state
machine and the `track`/`status`/`tail` commands that read it live in
[`reference.md`](reference.md).

---

## Your first failure — the reflex, not the fix

It will happen: someone says "I never got that message." Before you suspect
tmux-tell, build the one reflex that saves the most time — **check the sender's
outbox first.**

The question is almost never "did tmux-tell lose it?" and almost always "which of
these three happened?":

1. The sender never actually sent (the row isn't there).
2. tmux-tell delivered, but the *action* the message was supposed to trigger didn't
   happen (the agent got it and dropped it).
3. tmux-tell genuinely failed mid-delivery (rare, and it says so — fail-loud).

You rule those out *in that order*, because the first two are far more common
than the third and cost nothing to check. The structured triage — the exact
SQLite query, the `journalctl` check, the cross-system verification — is
[`diagnostic-playbook.md`](diagnostic-playbook.md), and it is genuinely good;
don't reinvent it. What this manual gives you is the **reflex**: *sender-outbox
first, escalate to the substrate last.* Most "lost message" reports die at step 1
or 2.

When the *recipient* is the suspect, there's a cheaper probe than any of those:
`tmux-tell-claude ping AGENT` checks reachability without sending anything, and
reports one of three classes — **`REACHABLE`**, **`PENDING`** (*retry or wait,
the mailman is working* — reachable, but a draining backlog or a held delivery
means the probe couldn't confirm yet; transient), or **`UNREACHABLE`** (*won't
clear on its own, operator action needed* — the substrate is actually broken:
mailman down, stuck, or pane dead). One call tells you whether to **wait** or to
**act**. The `PENDING` case is the same observe-gate hold that turns a delivery
into `delivered_in_input_box`, seen from the prober's side — healthy, not lost.

One specific early-days gremlin worth naming, because it looks like a tmux-tell
bug and isn't: **drift**. If a sender's message is *refused* with a mismatch warning, the
sender's pane title and its registration have diverged (a respawn, a rebuild).
The fix is `discover` to re-register; the playbook's drift section walks it.

---

## What you can lean on — and what you can't

Run tmux-tell a few days and you'll want to know exactly how much trust it has
earned. The honest accounting:

**Lean on these.** Single host, single tmux server — everything is local, no
network, no daemon phoning home. **One writer per mailbox**, so two senders can't
interleave into garbage in the same input row. **Fail-loud, never fail-silent** —
if the gate can't find a safe moment, it delivers anyway and logs it; if delivery
fails, the sender gets told. **Addressable by name**, durable as a SQLite file
you can read and back up.

**Don't ask it for these.** It is not a networked queue, not multi-host, not a
chat app, not a job scheduler. It moves a message from one pane to another,
safely, on one machine. The full "it is / it isn't" — and the honest comparison
to raw `send-keys` and to single-session subagents — is [`why.md`](why.md). If
you're deciding whether you even need it, read that one.

**The trust model is small and explicit, so know it.** tmux-tell assumes a
**single-operator homelab**: anyone with shell access on the host is trusted,
identity is `$TMUX_PANE`-based (spoofable by someone who already has your shell —
which the model accepts), and there is no authentication, authorization, or
crypto. That's a deliberate fit to the deployment, not an oversight — and
[`security.md`](security.md) names every load-bearing assumption and exactly what
would have to change before this belonged anywhere less trusted. Read it before
you put it somewhere it wasn't built for. (At the architecture level these givens
are catalogued in [Arc42 §2 Constraints](arc42/02-architecture-constraints.md),
which links straight back to `security.md` for the depth.)

---

## Day N — the rhythm, and what has actually gone wrong

Steady-state operation is quiet: agents talk, the mailmen deliver, the rows
accumulate as an audit log. Two habits keep it that way.

**Watch the right signals.** The failure classes that have actually occurred —
and the monitoring surfaces that would catch each one early (WARN rates,
deliver-time spikes, verify-token misses) — are catalogued from real incidents in
[`failure-modes.md`](failure-modes.md). The day-to-day rough edges and the
"things that are actually fine, don't yak-shave them" list are in
[`operator-ux.md`](operator-ux.md). Between them you'll know what's worth an alert
and what's noise.

**Respect the one thing that has bitten hardest: the DB binding can silently
split.** This is the Day-N story worth telling in full, because it's invisible
until it isn't.

On 2026-06-12, the v0.16.0 deploy moved the database to its new XDG path and
restarted the mailmen via systemd — correctly. But the **MCP servers** that
agents talk to are *not* systemd-managed; they're long-lived processes spawned by
each Claude session at startup. They were never restarted, so they kept holding
the **open inode of the pre-move database** — a file that no longer had a name at
the canonical path. The result was a clean, silent split: a sender's MCP tool,
querying the ghost file, reported `queued: 2`; the recipient's mailman, reading
the canonical file, reported `queue_depth: 0`. **Both were telling the truth about
their own world.** Messages vanished into the orphaned inode, and nothing flagged
the divergence — it took `/proc` archaeology to even see it.

The recovery is one command — `tmux-tell-claude refresh-all-mcps`, which restarts
every agent's MCP server against the current binary and DB — and it had existed
for ages (#62). The gap wasn't the fix; it was that nobody knew to *reach* for it,
because the deploy recipe didn't say to. The lessons, which are now yours for
free:

- **Any change to the DB path must be followed by `refresh-all-mcps`.** Moving the
  file is half the job; the MCP servers won't follow it on their own. The WAL-safe
  move itself (stop mailmen → checkpoint → move → restart) is documented in
  [`reference.md`](reference.md) under moving the DB (#343) — but the
  `refresh-all-mcps` step is exactly the piece that recipe is *still missing*,
  which is what #349 closes: it adds the refresh step to the docs and reshapes
  `install.sh` into a substrate-honest hard-cut that re-binds every piece on each
  install (discover + register panes, enable the right mailmen, disable orphan
  units, fire `refresh-all-mcps`), with a standalone `db migrate` primitive
  underneath. #348 adds the diagnostics — including per-agent last-delivered /
  idle-since on the `agents` listing — that would have made the split legible in
  one tool call instead of `/proc` archaeology.
- **"Sent" and "queue depth" diverging is a symptom of a split binding,** not a
  lost message. If the sender swears it's queued and the recipient sees an empty
  queue, suspect two files before you suspect a bug.

Upgrades, in general: rebuild (`make build && sudo ./install.sh --system` on
alcatraz), restart the mailmen, and `refresh-all-mcps` so the MCP servers pick
up the new binary. The mailmen come back on their own; the MCP servers need the
nudge.

---

## Uninstall — because you can audit the exit, too

The reassurance that should make adopting this easy: leaving is trivial and
visible. It's a binary, a systemd user unit, and a SQLite file. Stop and remove
the units, delete the binary, and `~/.local/share/tmux-tell/messages.db` is a
plain file you can archive or `rm`. There's no cloud account to close, no state
stranded somewhere you can't see. You can read the whole system's memory with
`sqlite3` and you can remove it in an afternoon — which is the same property that
lets you *trust* it while it's running.

---

## Where to go next

| If you want… | Read |
|---|---|
| Every command, flag, and delivery semantic | [`reference.md`](reference.md) |
| To triage a "lost message" report | [`diagnostic-playbook.md`](diagnostic-playbook.md) |
| How delivery-timing / the 📫 hold actually works | [`observe-gate.md`](observe-gate.md) |
| The trust model and what's *not* your problem | [`security.md`](security.md) |
| What has broken before, and what to watch | [`failure-modes.md`](failure-modes.md) |
| The day-to-day rough edges (and non-problems) | [`operator-ux.md`](operator-ux.md) |
| Why this exists at all / whether you need it | [`why.md`](why.md) |
| The view from *inside* a registered agent | [`agent-manual.md`](agent-manual.md) |

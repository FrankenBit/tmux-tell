# Diagnostic playbook: when a agent says "I missed a message"

When a agent reports a missing bus message, the instinct is to assume
tmux-msg dropped or corrupted it. The existing bug catalog reflects
that — [#59](https://git.frankenbit.de/frankenbit/tmux-msg/issues/59)
covers Enter-injection corruption when the receiver is at a Claude prompt,
[#63](https://git.frankenbit.de/frankenbit/tmux-msg/issues/63) covers
mid-typing collisions when delivery lands during operator composition.

But the 2026-06-03 incident that surfaced this playbook showed a third
category: **sender-side gaps**, where the agent's own flow skipped the
`tmux-msg.send` call entirely. No DB row, no mailman delivery attempt,
no failure trace. The external action (a Forgejo PR filing) had happened
cleanly; the corresponding bus notification simply was never inserted.

A "bus is broken" narrative was forwarded as recovered substrate before
the actual DB was checked. The recovery path took the wrong shape.

This playbook captures the triage so the **next** time a agent reports
a missing message, the answer to "did the bus drop it?" lands in under
five minutes instead of seeding a bus-recovery investigation.

## Discipline framing

This is an **operational-coordination-layer expression** of the broader
*"filed-bug root cause is hypothesis until probed"* discipline that
already applies at the code-bug layer. The agent reporting the gap is
generating a hypothesis (*"tmux-msg dropped it"*), not a verified
diagnosis. The playbook's job is to keep the hypothesis labelled as
such until the substrate has been checked.

The same shape: don't act on the hypothesis as if it were the root cause
until the deployed system has been probed for the evidence that would
falsify it.

## The triage — sender-outbox-first

Walk three checks in order. **Stop and act** the moment the first one
identifies the gap; don't continue to the next layer.

> **Quick pre-check (#144).** Before the deep triage, confirm the
> receiver is even reachable on the bus: `tmux-msg-claude ping <receiver>`.
> It probes daemon-up + pane-live without pasting into the pane. A
> `failed`/`timeout` here means the receiver's mailman is down or its
> pane is gone — fix that first; the outbox triage below assumes a
> live bus. A `delivered` here rules reachability out and points the
> investigation at the sender-side / receiver-processing layers.

### 1. Did the sender actually send?

The SQLite store is the authoritative record of what reached the bus.
Replace the placeholders with the alleged sender and a tight time
window. Bounds are UTC (tmux-msg stores ISO UTC timestamps);
convert from local if needed.

```bash
# DB is under the operator's user-home (#308) — no sudo needed.
sqlite3 ~/.local/share/tmux-msg/messages.db -header -column "
  SELECT public_id, from_agent, to_agent, state, kind,
         length(body) AS body_len,
         created_at, delivered_at,
         COALESCE(error, '') AS err
  FROM messages
  WHERE from_agent = '<sender>'
    AND created_at >= '<T-2min UTC>'
    AND created_at <= '<T+2min UTC>'
  ORDER BY created_at;
"
```

`from_agent` is included in the projection (not just the WHERE clause)
so each row is self-describing when the operator copies it into an
incident report or bug filing.

Decision tree on the result:

| Result                                        | What it means                                                                       | Next action                                                                                                                       |
|-----------------------------------------------|-------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------|
| **No row**                                    | Sender never reached the bus. Agent-side flow gap.                                | **Stop investigating tmux-msg.** Probe the sender agent's state at the alleged send time.                                  |
| **`state = 'delivered'`**                     | Bus did its job.                                                                    | Cross-check the receiver pane's state at `delivered_at`. The receiver may not have processed it (UI race, popup collision #59).   |
| **`state = 'failed'`**                        | Bus tried and failed cleanly.                                                       | Check the `error` column. The `delivered_in_input_box` notification path should have fired — if it didn't, that's a separate gap.   |
| **`state = 'queued'` / `'delivering'` stale** | Genuine delivery stall.                                                             | File a fresh bug citing the row's `public_id` + the receiver's mailman journal excerpt from §2.                                   |

### 2. Did the receiver's mailman try to deliver?

If §1 found a row, §2 confirms whether the mailman daemon attempted
delivery. The mailman log is the authoritative record of what the bus
tried to put on the receiver's pane.

```bash
journalctl --user-unit=tmux-msg-claude-mailman@<receiver> \
  --since="<T-2min local>" \
  --until="<T+2min local>" \
  --no-pager
```

**Time-zone note**: `journalctl`'s `--since` / `--until` default to
**local time** (not UTC like §1's SQL bounds). If you mentally shifted
into UTC for §1, shift back to local here. The two layers store
timestamps in their respective conventions and the playbook follows
each layer's default rather than forcing one into the other's frame.

Look for `delivering id=<public_id>` / `delivered id=<public_id>` /
`WARN delivered_in_input_box` / `WARN drift_detected` lines for the
public_id from §1.

- **Delivering + delivered**: substrate did its job; cross-correlate
  with the receiver pane's state.
- **Delivering, no delivered, no WARN**: mailman is mid-delivery or
  stalled. Capture `systemctl --user status tmux-msg-claude-mailman@<receiver>`
  + the `delivered_at` (or lack of it) in the DB row, file as a bus
  stall.
- **No `delivering` line at all**: row exists in the DB but the mailman
  never picked it up. Daemon may not have been running. Capture
  `systemctl --user status` and the row's `created_at`.

### 3. Did the alleged external action actually happen?

If the missing message was *about* an external action (a Forgejo PR
filing, a BookStack page update, a deploy, a Surveyor review), check
the external system in the same window.

The 2026-06-03 incident's key signal: Pilot's PR #305 was filed on
Forgejo at 15:57 local, but Pilot's last bus message was at 15:23 local
— the agent had gone silent ~34 minutes *before* the external action.
The PR existed; the corresponding bus notification simply hadn't been
fired.

Common cross-checks:

| Action            | Where to look                                                                            |
|-------------------|------------------------------------------------------------------------------------------|
| Forgejo PR        | `curl -sS "https://git.frankenbit.de/api/v1/repos/<owner>/<repo>/pulls/<N>"` → `created_at` |
| Forgejo review    | `…/pulls/<N>/reviews` → match `id` + `submitted_at`                                      |
| BookStack page    | Page's `updated_at`                                                                      |
| Deploy            | Systemd-unit's `ActiveEnterTimestamp` or release-publish webhook log                     |

Outcomes:

- **External action at alleged time, no bus message**: gap in the
  agent's flow (the post-action notification step was skipped or
  unreachable from where the agent landed). Not a bus problem.
- **External action at a *different* time than the operator
  remembers**: gap in the operator's mental model. Easier to recover
  from — verify the actual time, re-align the narrative.
- **No external action at all**: agent didn't do what was expected.
  Investigate the agent's session log for where the flow stopped.

## Watching it happen live — `tmux-msg-claude tail`

The triage above is post-hoc: it reconstructs what happened from the stored
row + the journal. When the failure is *reproducible* — you can trigger the
send again — skip the reconstruction and watch it live:

```bash
# all bus traffic, live (Ctrl-C to stop)
tmux-msg-claude tail

# narrow to the pair you suspect, then re-trigger the send
tmux-msg-claude tail --from bosun --to surveyor

# only failures / notices, across everyone
tmux-msg-claude tail --kind delivery_failure_notice
tmux-msg-claude tail --state failed
```

`tail` prints each new row as it's inserted and each `queued → delivering →
delivered/failed` transition on the same id, so a message that stalls in
`delivering` or flips to `failed` is visible the instant it happens — the
exact correlation the #137 walk-back had to assemble by hand from two
mailmen's journals. Filters compose (AND); `--since 5m` backfills recent
history before going live; `--format json` pipes into `jq`. It's a read-only
poll over the SQLite store (rowid-polling, WAL-safe), so it never perturbs
delivery — run it alongside a live repro freely.

## When this playbook DOES escalate to "bus-side"

The bus IS the failure point when:

- §1 returns a row with `state` stuck in `queued` or `delivering`
  significantly past `created_at`, AND §2 shows the mailman is healthy
  (no stall, no drift)
- §1 returns a row with `state = 'delivered'` but the receiver pane
  capture at `delivered_at` shows none of the body text landed
  (delivery succeeded at the tmux paste-buffer layer but the receiver
  UI consumed it as something else — sibling failure mode to #59)
- §1 returns a row with `state = 'failed'` AND the `error` column says
  something tmux-msg-specific (e.g., `can't find pane`, drift
  unrecoverable) — these are real bus-side failure modes worth filing

In each case, file with the `public_id`, the journal excerpt, and the
operator's recollection of the alleged send time so the report is
fully grounded.

## When this playbook does NOT apply

- A message **arrived** but the receiver acted on it wrong: that's a
  receiver-agent behavioral question, not a delivery question.
- A message arrived but the receiver claims "wasn't expecting that":
  legitimate routing question, but separate from the
  did-it-actually-deliver triage this playbook is for.
- Bulk fan-out scenarios where some recipients got the message and
  some didn't: walk this playbook for *one* of the non-receiving
  recipients first; if that turns up sender-side, the others are
  almost certainly the same.

## Drift-detection refused my send

When a `tmux-msg-claude send` command reports `delivery: state=failed` and the
mailman log shows
`WARN drift_detected_unrecoverable id=… agent=… registered_pane=… runs=… — discover couldn't find <name> anywhere`,
the substrate's safety machinery is working: the registered agent name doesn't
match any reachable pane's self-declared title, so the bus refuses to paste
rather than risk delivering to the wrong pane.

This is a **real safety event**, not a bug. Drift detection treats
name-vs-title mismatch as "the pane was repurposed since registration" and
fails-loud rather than fail-silent.

**Root cause.** The `discover` walker enumerates tmux panes and matches each
pane's self-declared identity (typically `pane_title`, also `cmdline` /
`window_name`) against the registered agent name. When no match is found
within the configured retry budget, the mailman logs
`drift_detected_unrecoverable` and refuses delivery. Typical triggers:

- Chamber registered with a name that differs from what the chamber process
  eventually sets as its pane title (the 2026-06-11 Caymans-Admin observation:
  registered as `caymans-admin`, but pane self-declared as `Admin`; re-registering
  as `admin` made delivery succeed).
- Pane respawned and the new process didn't restore the expected title.
- Operator renamed the pane manually but left the registry stale.

**Symptom.**

- `tmux-msg-claude send …` returns `state: failed` with `error: drift_detected_unrecoverable`.
- Mailman journal shows
  `WARN drift_detected_unrecoverable id=<public_id> agent=<name> registered_pane=<%N> runs=<count> — discover couldn't find <name> anywhere`.
- The chamber pane itself is alive and prompt-responsive; the gap is purely on
  the registry-vs-pane-title axis.

**Fix — two paths, substrate-honest first.**

1. **Match the name (recommended).** Re-register with the pane's self-declared
   title so drift detection stays useful for genuine repurpose events:

   ```bash
   # Query each pane's self-declared title
   tmux list-panes -a -F '#{pane_id}  #{pane_title}'

   # Re-register with the matching name (lowercased per convention)
   tmux-msg-claude register --name <pane-title-lowercased> --pane <pane-id> --force
   ```

2. **Override with `--drift-soft-fail`** (deliberate experiments / atypical
   setups). Run a foreground mailman with the safety check relaxed:

   ```bash
   tmux-msg-claude serve --agent <name> --drift-soft-fail
   ```

   Use sparingly — the soft-fail mode bypasses the safety check that protects
   against repurposed-pane delivery. Substrate-honest only when the operator
   has deliberately accepted the name/title divergence and accounted for the
   risk.

**Cross-ref.** The discover walker mechanics live in
[`reference.md`](./reference.md) §"Identity, names & aliases" for operators
who want the deeper substrate model. [ADR-0009](adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
frames why drift detection fires consistently across adapters: it's a
substrate-general invariant on pane-identity, not an adapter-specific behavior.

## MCP-path sender-unknown (Codex MCP server)

A distinct failure mode surfaces when a Codex agent calls `tmux-msg.send` via the MCP
server path (`[mcp_servers.tmux-msg]` in `~/.codex/config.toml`): the call fails or reports
an unknown sender because the substrate cannot resolve the agent's identity.

**Root cause.** The substrate resolves sender from `$TMUX_AGENT_NAME` or
`$TMUX_PANE → registry`. Codex's MCP host does **not** propagate `$TMUX_PANE` to spawned
MCP server processes, and `$TMUX_AGENT_NAME` is also absent unless explicitly injected. The
CLI path (`tmux-msg-codex send …`) and the hook-context path run in the operator's shell
where both variables are available; only the MCP server spawn is isolated.

**Symptom.** `tmux-msg.send` calls via the MCP server return a sender-resolution error or
send with `from = ""` instead of the agent's registered name.

**Fix.** Add an `env` table to the `[mcp_servers.tmux-msg]` stanza in
`~/.codex/config.toml`:

```toml
[mcp_servers.tmux-msg]
command = "tmux-msg-codex"
args = ["mcp"]
env = { TMUX_AGENT_NAME = "lookout" }
```

Replace `"lookout"` with the agent's registered name. After updating the config, restart
Codex (the MCP server is spawned fresh on each Codex start) and retry the send.

This is an adapter-vs-substrate boundary issue per [ADR-0009](adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md):
the fix lives in the adapter's config, not in the substrate. See
[`cmd/tmux-msg-codex/README.md`](../cmd/tmux-msg-codex/README.md) §MCP server for the full
wiring recipe.

## Post-deploy MCP-binding divergence

A deploy that moves the DB file (e.g. the #308 path move from
`/var/lib/tmux-msg/` to `~/.local/share/tmux-msg/`) but does **not** restart the
long-lived chamber MCP server processes leaves those processes writing to the
**old inode**. The MCP servers are stdio-spawned by the host (Claude Code /
Codex) at session start, not by systemd, so a `docker compose`-style restart
doesn't touch them — they keep their pre-deploy open file handle.

**Symptom.** A sender's `tmux-msg.agents` reports `queued: N` for a recipient,
but the recipient's mailman sees `queue_depth = 0` (and the message never
arrives). Both are correct *within their own DB* — the sender's MCP is writing
to an orphaned inode no mailman reads. Forgejo/other surfaces look fine; only
the bus-side delivery is lost. Pre-#348, diagnosing this meant `/proc/PID/exe`
archeology + `pgrep -af tmux-msg-claude` + `sqlite3` on the canonical path.

**Triage primitive — `tmux-msg-claude doctor`.** One command walks every live
`tmux-msg-claude` process, reads each one's *actual* open DB handle from
`/proc/PID/fd` (inode and all), and flags any that diverge from the canonical
DB:

```bash
tmux-msg-claude doctor          # exits non-zero on any divergence
tmux-msg-claude doctor --format json
```

```
CANONICAL	/home/alex/.local/share/tmux-msg/messages.db (inode 1895479)
✓ PID 326865  binary=/usr/local/bin/tmux-msg-claude  db=…/messages.db (inode 1895479)  — canonical
✗ PID 12381   binary=/usr/local/bin/tmux-msg-claude (deleted)  db=/var/lib/tmux-msg/messages.db (inode 12345) [file removed]  — orphan DB inode — file removed; writes invisible to mailmen on the canonical path
DIVERGENCE: one or more processes hold a DB binding invisible to mailmen on the canonical path. Run `tmux-msg-claude refresh-all-mcps` to restart MCP servers (and check for stale mailmen).
```

**Ask one process directly** — `tmux-msg.whoami_db` (MCP tool) returns the
calling server's live binding `{pid, binary_path, started_at, db_path,
db_inode, db_deleted}` straight from `/proc`, so you can confirm where a
specific session is writing without enumerating the fleet.

**Confirm ground truth** — `tmux-msg-claude track <id> --canonical` opens the
canonical XDG-default DB by name (ignoring `--db` / `$CLAUDE_MSG_DB`), answering
"is id X *actually* in the canonical DB?" without trusting a possibly-stale MCP
view.

**Fix.** `tmux-msg-claude refresh-all-mcps` restarts the MCP servers so they
rebind to the canonical DB; re-run `doctor` to confirm a clean fleet. The
prevention side (a WAL-safe DB-move recipe) is tracked separately in
[#343](https://git.frankenbit.de/frankenbit/tmux-msg/issues/343); `doctor` is
the *detection* half of that defense-in-depth pair ([#348](https://git.frankenbit.de/frankenbit/tmux-msg/issues/348)).

## See also

- [#59](https://git.frankenbit.de/frankenbit/tmux-msg/issues/59)
  — Enter-injection delivery into a Claude prompt
- [#63](https://git.frankenbit.de/frankenbit/tmux-msg/issues/63)
  — mid-typing collision
- [`failure-modes.md`](./failure-modes.md) §3 — observable diagnostics
  scoped to bus-side failure modes
- [`README.md`](../README.md) §"Diagnosing a failed or unverified
  message" — sibling recipe for the case where step §1 returns a
  `failed` row

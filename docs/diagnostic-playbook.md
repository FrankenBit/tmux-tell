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
sudo sqlite3 /var/lib/tmux-msg/messages.db -header -column "
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

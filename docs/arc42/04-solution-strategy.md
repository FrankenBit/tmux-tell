---
arc42-section: 4
revisit-triggers:
  - a core substrate mechanism changes (mailman loop, verify-token loop, WAL/concurrency model)
  - a new delivery mode is added alongside paste-and-enter / hook-context
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §4 Solution Strategy

The high-level technical shape of how the architecture meets the [§1 goals](01-introduction-and-goals.md).
This is *the shape of the answer* — distinct from §1 (*what* we want) and
[§10](10-quality-requirements.md) (*how well*). Each strategy links the
[crosscutting concept](08-cross-cutting-concepts.md) or component
([§5](05-building-block-view.md)) that realizes it; the depth lives there.

| Goal (§1) | Strategy | Realizing mechanism |
|---|---|---|
| Don't clobber the human | **Observe before paste.** Each delivery runs a near-read-only gate that classifies the pane's state and defers while the operator is engaged. | the [observe-gate](../observe-gate.md) + its five agent-states ([§6](06-runtime-view.md)) |
| Never over-claim delivery | **Verify after paste.** A round-trip token confirms the paste landed; if it can't, the message is honestly marked `delivered_in_input_box`, never silently `delivered`. | verify-token loop ([§8](08-cross-cutting-concepts.md), [§10](10-quality-requirements.md)) |
| Coordinate without a courier | **A durable mailbox per agent.** Messages are rows with a lifecycle (`queued → delivering → delivered/failed`), addressed by name not pane. | SQLite store (`internal/store`) |
| One writer per pane | **A per-recipient mailman daemon** serializes delivery so two senders can't interleave into one input row. | mailman (`internal/cli`, systemd-user) |
| Stay auditable + local | **SQLite + tmux, nothing else.** No network listener, no cloud; `sqlite3`-readable, one-script uninstall. | single-host per-UID trust boundary ([ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md)) |
| Extend at the adapter axis | **A `Profile` abstraction** behind a substrate-vs-adapter boundary; a new LLM-CLI is a new binary + flags, not a substrate fork. | [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md), `cmd/tmux-tell-*` |

## Cross-cutting strategic choices

- **Atomic delivery via paste-and-enter.** The target CLIs don't expose IPC, so the
  substrate types into their input the way a human would — the lowest-common-denominator
  channel. Adapters whose TUI can't be safely pasted into use **hook-context**
  delivery instead (injection at a turn boundary), keeping the substrate uniform
  ([ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)).
- **WAL + `BEGIN IMMEDIATE` for write-concurrency.** A single writer per recipient
  plus WAL journaling and immediate-transaction cap enforcement keeps the DB
  consistent under concurrent senders without a server process
  ([docs/security.md §3.3](../security.md), #29).
- **systemd-user as the daemon manager.** Mailmen are per-agent `systemctl --user`
  instance units — no root daemon; lifecycle (`enable`/`restart`) is owned by
  `install.sh` bootstrap ([§7](07-deployment-view.md)).
- **Cap-bounded admission control.** `capRecipientQueue` + `capSenderBacklog` bound
  per-recipient and per-pair backlog so a misbehaving sender can't storm a mailbox
  ([§8](08-cross-cutting-concepts.md)).
- **Self-recovery over manual intervention.** Stuck mailmen back off and park
  (#297); the operator recovery surface (`doctor`, `resend`, `refresh-all-mcps`,
  `db migrate`) handles the residue ([§7](07-deployment-view.md)).

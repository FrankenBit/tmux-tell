---
arc42-section: 2
revisit-triggers:
  - a constraint is promoted to / retired from an ADR IS / IS-NOT list
  - the substrate-vs-adapter boundary moves (ADR-0009)
  - the host-locality / per-UID trust boundary changes (e.g. cross-host reach lands)
  - a new hard dependency is taken (beyond tmux / SQLite / Go / systemd-user)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A initial cut — #386) -->

# §2 Architecture Constraints

The load-bearing givens the architecture is built on. These are *constraints*,
not decisions made freely — most are forced by the substrate or codified as
architectural law in an ADR. They were previously scattered across ADRs,
`AGENTS.md`, and README assumptions; this section consolidates them.

## Technical constraints

| Constraint | Why it binds | Source |
|---|---|---|
| **Single host, per-UID** | The trust boundary is OS-level: a per-user SQLite DB + per-user systemd units. No network listener, no multi-host bus. | [ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md) §What-it-IS 1/7 |
| **tmux is the runtime substrate** | Agents are CLI TUIs in tmux panes; delivery *is* a tmux paste into a pane. No tmux → no delivery. | ADR-0014 §IS 1–2 |
| **paste-and-enter is the lowest-common-denominator delivery** | The target CLIs don't natively support IPC, so tmux-tell types into their input the way a human would. | ADR-0009 |
| **hook-context is the required mode for paste-incapable adapters** | An adapter whose TUI can't be safely pasted into receives via its hook channel instead — a complementary delivery mode that keeps the substrate uniform. | [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md) |
| **SQLite is the only datastore** | One file, WAL-journaled, `sqlite3`-auditable; durability via WAL + `BEGIN IMMEDIATE`. No external DB. | ADR-0014 §IS 3 |
| **Go is the implementation language** | Single static binary per adapter; shared substrate in `internal/`. | repo layout |
| **systemd-user manages the daemons** | Per-agent mailman daemons are `systemctl --user` units; no root daemon. | §7 Deployment View |
| **sudo only at install** | Installation needs sudo; *operation* does not. | docs/why.md |

## Organizational / conceptual constraints

- **Substrate-vs-adapter boundary is architectural law.** The substrate stays
  delivery-method- and vendor-agnostic; per-LLM-CLI specifics live behind the
  `Profile` abstraction in the adapter. New adapter = new binary + flags, never a
  substrate fork. ([ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md))
- **Substrate-state-claim integrity.** The system never claims a state it hasn't
  verified (the "never over-claim delivery" invariant). This pervades the design
  rather than living in one component — see [§8](08-cross-cutting-concepts.md) and
  the §10 NFR.
- **Project scope is fenced.** Per [ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md),
  "X is out-of-scope per ADR-0014" is the default answer to a scope-expansion
  proposal unless the proposer names a load-bearing reason X falls within scope.
- **ADRs are point-in-time and length-capped.** Decisions are recorded, not
  retro-edited; ADR-0006+ caps each ADR at 350 lines. ([docs/adr/README.md](../adr/README.md))

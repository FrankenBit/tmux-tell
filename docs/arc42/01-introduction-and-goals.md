---
arc42-section: 1
revisit-triggers:
  - major version bump (1.0 or a later major)
  - operator pivot on what the project is for
  - docs/why.md goals restructure
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A initial cut — #386) -->

# §1 Introduction and Goals

## What tmux-tell is

tmux-tell is **per-user TUI-paste directed messaging for operator-launched
LLM-CLI sessions attached to tmux panes on a single host** (the binding scope
statement is [ADR-0014](../adr/0014-tmux-tell-scope-and-cross-host-reach.md)).
Each pane gets a mailbox; an agent — or the operator — sends a message and it
lands in the target pane as if typed there, *waiting for a safe moment* so it
never clobbers a half-written line or fires into a running turn.

The narrative pitch (the *"Your agents already have a coordination layer. It's
you."* framing, the
`send-keys`-with-the-sharp-edges-filed-off comparison, and the honest
when-not-to-use-it trade) lives in **[docs/why.md](../why.md)** — that is the
depth for this section; §1 carries only the goals hierarchy.

## Goals (in priority order)

1. **Don't clobber the human.** Delivery defers while the operator is typing or
   a turn is running (the [observe-gate](../observe-gate.md)); if no safe moment
   is found within the budget, it delivers anyway and *says so* — fail-loud,
   never fail-silent. This is the goal that makes tmux-tell usable rather than
   infuriating.
2. **Never over-claim delivery.** A message is marked `delivered` only when a
   verify step confirms the paste landed; otherwise it is honestly marked (e.g.
   `delivered_in_input_box`). Substrate-state-claim integrity is the load-bearing
   invariant — see [§10](10-quality-requirements.md).
3. **Coordinate without a courier.** Move a message from one pane into another so
   the operator stops hand-ferrying status between agents.
4. **Stay auditable and local.** No cloud, no daemon phoning home — a SQLite file
   plus a tmux paste; readable with `sqlite3`, uninstallable with one script.
5. **Extend at the adapter axis, not the substrate.** A new LLM-CLI is a new
   `cmd/tmux-tell-<name>` binary + `Profile` flags; the substrate is unchanged
   (the substrate-vs-adapter boundary, [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)).

## Stakeholders

| Stakeholder | Concern |
|---|---|
| **Operator** | Runs the panes; must not be clobbered; wants auditability + one-script uninstall |
| **Agent (chamber)** | Sends/receives messages; needs provable sender identity + honest delivery state |
| **Adapter author** | Adds a new LLM-CLI without touching the substrate |
| **Downstream (Binnacle)** | Consumes tmux-tell as an external Go module ([ADR-0007](../adr/0007-binnacle-coexist-external-contract.md)) |

> **Quality goals** are catalogued in [§10 Quality Requirements](10-quality-requirements.md)
> (Phase-1 seed pending the collaborative working session).

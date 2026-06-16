---
arc42-section: 12
revisit-triggers:
  - a new load-bearing term is coined (typically in an ADR or a delivery-mechanism change)
  - an existing term is renamed (e.g. the chamber → agent substrate rename, ADR-0005)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A initial cut — #386) -->

# §12 Glossary

The single source for tmux-tell-specific vocabulary used across the architecture
documentation. Entries are one sentence; each cross-references where the term is
used in depth. Adding a term here is part of the discipline — when an ADR or a
substrate change coins a load-bearing term, it lands here with a gloss and a link.

## A

- **Adapter** — the per-LLM-CLI layer (`cmd/tmux-tell-<name>` binary + `Profile`)
  that knows one CLI's TUI quirks; swappable without touching the substrate. See
  [§2](02-architecture-constraints.md), [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md).
- **Agent** — a registered tmux pane on the bus (one mailbox per agent). The
  substrate term for what the chamber-level docs call a *chamber*; the two refer
  to the same per-pane LLM-CLI session. See *chamber vs agent*, [ADR-0005](../adr/0005-substrate-honest-terminology.md).

## B

- **bootstrap** — the binary subcommand `install.sh` runs (as the operator) to wire
  a fully-working bus in one invocation: daemon-reload, stale-DB detect, discover,
  enable+restart mailmen, orphan-prune, refresh MCPs. See [§7](07-deployment-view.md).

## C

- **Chamber (vs agent)** — *chamber* is the project-lexicon naval term for an
  operator-side Claude instance in a tmux pane (Bosun, Herald, …); *agent* is the
  substrate-neutral term for the same thing. The substrate-side rename (ADR-0005)
  did not propagate to chamber-level docs. See [ADR-0005](../adr/0005-substrate-honest-terminology.md).

## D

- **`delivered_in_input_box`** — the honest delivery state for a paste that reached
  the input but could not be verify-confirmed as submitted; the substrate marks it
  rather than over-claiming `delivered`. See [§10](10-quality-requirements.md).

## H

- **Hook-context** — the alternative delivery mode for adapters whose TUI can't be
  safely pasted into: the message is injected via the CLI's hook channel at a turn
  boundary instead of typed into a live pane. See [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md).

## M

- **Mailman** — the per-agent (per-recipient) daemon that serializes delivery into
  one pane (one writer per mailbox, so senders can't collide); a `systemctl --user`
  instance unit. See [§5](05-building-block-view.md), [§7](07-deployment-view.md).

## O

- **Observe-gate** — the safe-moment machinery that defers a paste while the operator
  is typing or a turn is running, dropping a 📫 marker, and delivers once it's safe
  (fail-loud if no safe moment is found). See [docs/observe-gate.md](../observe-gate.md).

## P

- **`PaneProfile` / Profile** — the per-adapter abstraction carrying a CLI's TUI
  traits (paste-collapse marker, MCP-slash-command support, settle-delay, …) so the
  substrate stays vendor-agnostic. See [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md).
- **paste-and-enter** — the lowest-common-denominator delivery: the bus types a
  message into a CLI's input the way a human would, then submits it. See [§2](02-architecture-constraints.md).

## R

- **`refresh-all-mcps`** — the cap-protected, operator-explicit cascade that rebinds
  every chamber's MCP server to the freshly-installed binary + canonical DB after a
  deploy. See [§7](07-deployment-view.md).

## S

- **Substrate** — the vendor- and delivery-method-agnostic core (the SQLite bus +
  mailmen + shared `internal/` code) below the adapter boundary. The substrate-vs-
  adapter boundary is architectural law. See [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md).

## V

- **Verify token** — the round-trip check that confirms a paste actually landed
  before the message is marked `delivered`; the mechanism behind substrate-state-
  claim integrity. See [§10](10-quality-requirements.md).

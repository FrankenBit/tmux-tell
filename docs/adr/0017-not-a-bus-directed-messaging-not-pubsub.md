# ADR-0017: tmux-tell is directed messaging, not a bus

> **Status**: Accepted
> **Date**: 2026-07-11
> **Authors**: operator, Bosun (recorded), original crew (naming discussion, 2026-05-xx)

## Context

The name tmux-tell was chosen after a crew naming discussion that explicitly considered and rejected "tmux-bus" (the operator's initial proposal, with a Weird Al Yankovic reference). The technical objection was substrate-honest: what was being built is not a bus.

The mechanism: sender addresses a specific recipient (`--to bob`), the message lands in that recipient's mailbox, that recipient's per-agent mailman pastes it into their pane. No topic-based routing, no publish/subscribe semantics, no broadcast — every message is a directed point-to-point delivery from one sender to one named recipient.

The name landed as "tell" (a directed verb — you tell *someone* something) rather than "bus" (broadcast semantics on a shared channel). The naming decision was documented at the time via the crew discussion, but not anchored as an ADR — so the framing was not on the substrate-of-record.

Empirical anchor for filing now (2026-07-11): the operator noticed that the tmux-tell repo description and the top of the README had re-adopted "bus" framing across the doc — including the tagline (*"A message bus for CLI agents running in tmux"*), a subhead (*"You're already running a message bus. It's you."*), and multiple in-text occurrences (*"on the one bus"*, *"bus destination"*, *"first message across the bus"*). The disclaimer section (*"What it is — and what it isn't"*) carefully says *"not a NETWORKED bus"* — implying it IS a local bus, which contradicts the crew's original decision.

Nothing in the architecture had changed. This was frame-drift in the docs, not substrate-drift.

## Decision

**tmux-tell is directed inter-agent messaging.** Sender addresses a specific recipient via mailbox; each recipient has one mailman; delivery is point-to-point, one-shot, and never broadcast. No topic-based routing, no publish/subscribe semantics.

Operational consequences that flow from this:

- Public copy (repo description, README, docs) uses **"tell"**, **"send"**, **"directed messaging"**, **"mailbox"**, **"recipient"** — never **"bus"**, **"publish"**, **"subscribe"**, or **"broadcast"** to describe tmux-tell's own primitive.
- The **"What it is — and what it isn't"** section (or its equivalent) names the actual category being denied, not "networked bus" (which implies local bus is fine).
- If a future doc revision or PR reintroduces "bus" as the description of tmux-tell's primitive, this ADR is the substrate-of-record to point at.

## Alternatives considered

- **"tmux-bus" with pub/sub bus semantics** — rejected in the original naming discussion; would have required different substrate (topic routing, subscription management, fan-out delivery). Kept as design-shelf: see [frankenbit/tmux-bus](https://git.frankenbit.de/frankenbit/tmux-bus).
- **Keep the doc drift** — rejected because "bus" is technically inaccurate and drift on the identity of a project is a substrate-of-record failure, not a stylistic choice.
- **Rewrite as "message queue"** — considered but rejected as jargon-heavier without accuracy gain; "directed messaging" reads cleaner and matches the verb the project name teaches.

## Consequences

**Upside**:
- Doc drift on the "is tmux-tell a bus" question has an ADR to catch it at review time, not on the operator's re-litigation cycle.
- Contributor onboarding uses accurate mental model from the start.
- The tmux-bus sibling repo (design-shelf) has a clean anchor for how the two projects differ — not "same thing at different scales" but "different primitives for different use cases."

**Cost**:
- Casual shorthand in conversation (calling tmux-tell "the bus" among ourselves) needs to not leak into public copy. Convention discipline, not enforcement.
- The existing multi-recipient send (`send --to a,b,c`, tmux-tell#158) is a comma-separated recipient list on the same directed-send primitive — the copy describes it as "multi-recipient send" (or "sending to multiple recipients"), never "publish to a topic list."

## What would change the decision

- A substrate rewrite that actually introduces pub/sub semantics (topic-based routing, subscription management, delivery to N subscribers). Currently no roadmap for this — sibling project tmux-bus captures the design-shelf shape if it becomes real.
- A crew re-litigation that overrides the original naming discussion. Would need explicit operator + crew consensus + this ADR superseded.

## References

- [ADR-0005: substrate-honest terminology](0005-substrate-honest-terminology.md) — sibling discipline; different axis (agent vs. chamber) but same substrate-honesty principle
- [frankenbit/tmux-bus](https://git.frankenbit.de/frankenbit/tmux-bus) — design-shelf sibling repo capturing the topic-subscription bus shape if we ever build one
- Original crew naming discussion (2026-05-xx) — surfaced via operator recollection 2026-07-11
- README frame-drift diagnosis: operator conversation 2026-07-11 (repo description + README top + 18 in-text "bus" occurrences)

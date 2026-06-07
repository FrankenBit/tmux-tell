# ADR-0007: Binnacle coexists with tmux-msg as an external Go module

> **Status**: Accepted
> **Date**: 2026-06-07
> **Authors**: operator (decision on #164, 2026-06-07), Herald (ADR)

## Context

tmux-msg is MIT-licensed and usable standalone (the alcatraz agent
substrate; any third-party integrator). Binnacle — a GPL-3.0-only
project — needs tmux-msg's bus substrate. Issue #164 forced the
pre-1.0 architectural disposition: does Binnacle **absorb** tmux-msg
(Option A), or **coexist** with it as an external dependency (Option B)?

Two prior commitments shape the choice:

- **ADR-0003 (substrate-vs-flavor)** established that tmux-msg is the
  *substrate* (the tmux pane registry, paste-and-Enter delivery, state
  detection) and the consumer running on top is *flavor* — downstream.
  A clean substrate boundary is what makes composing-with viable
  instead of mandating absorption.
- The **Sea-trials** milestone names the absorb-or-coexist decision as
  one of the v1.0 trigger criteria, so it must be settled and recorded.

## Decision

**Option B — coexist.** tmux-msg stays MIT and standalone; Binnacle
consumes it as an **external Go module** across a license-compatible
boundary. tmux-msg is not absorbed into Binnacle and is not relicensed.

Operational details that flow from this:

- tmux-msg's **exported Go API** and **DB schema** (columns + the
  message/agent state vocabulary) become the **external contract**
  Binnacle (and any downstream) relies on. That contract is committed
  in `CONTRIBUTING.md` and governed by the deprecation policy (#162).
- Binnacle pins tmux-msg as a versioned module and commits to a
  version-pin convention — recorded in a paired Binnacle-side ADR
  (follow-up, cross-chamber-routed; references this ADR for the
  substrate decision).
- The import path is stable; coexist requires no migration of existing
  Binnacle code that already imports tmux-msg.

## Alternatives considered

- **Option A — absorb tmux-msg into Binnacle** (vendor-in and/or
  relicense to GPL). Rejected: it destroys tmux-msg's standalone
  usefulness for non-Binnacle consumers, couples the substrate's
  release cadence to Binnacle's, and buys nothing the clean substrate
  boundary doesn't already provide. Adoption and coexistence are better
  handled by *organizational* concerns (versioning, a contract doc)
  than by absorption.
- **Relicense tmux-msg up to GPL or Apache.** Rejected: Option B
  explicitly keeps MIT. MIT maximizes downstream reach (copyleft and
  permissive consumers alike), and the MIT→GPL direction already lets
  Binnacle combine without any relicense.

## License combination

The MIT + GPL-3.0-only combination is legally clean per the FSF
license-compatibility list — MIT (Expat) is in the GPL-compatible set:

- GPL-3.0-only may link to and redistribute MIT-licensed code.
- The **combined Binnacle binary** distributes under GPL-3.0-only (the
  stronger copyleft propagates to the combined work).
- The **tmux-msg module itself retains MIT** for every non-Binnacle
  consumer.

This is the ordinary Go pattern (MIT dependencies under a copyleft
umbrella — e.g. Kubernetes shipping MIT deps under Apache-2.0). This
ADR cites the FSF matrix rather than re-deriving the legal pieces:
<https://www.gnu.org/licenses/license-list.html#Expat>.

## Consequences

**Cleaner.** tmux-msg stays a reusable substrate with its own identity,
release cadence, and MIT reach; Binnacle composes rather than swallows;
the substrate-vs-flavor line (ADR-0003) is honored at the repo
boundary.

**Cost.** tmux-msg now owes downstream a **stability contract** — the
exported Go API and the DB schema. Post-1.0, changes to those surfaces
carry the deprecation policy's two-minor-cycle floor (#162). That is a
real, ongoing maintenance constraint; `CONTRIBUTING.md` is where it
lives so contributors see it.

**Coordination.** A paired Binnacle-side ADR commits Binnacle to the
external-module consumption and version-pin convention (follow-up).

## What would change the decision

- If tmux-msg's substrate boundary stops being clean enough to compose
  across — e.g. Binnacle comes to need deep internal hooks only
  absorption can provide — the absorb path returns via a superseding
  ADR.
- If a license constraint changes (tmux-msg relicensing, or a Binnacle
  distribution requirement MIT cannot satisfy), revisit.

## References

- #164 — operator decision (Option B, 2026-06-07)
- ADR-0003 — substrate-vs-flavor naming (the distinction this builds on)
- #162 — deprecation policy (governs the external-contract stability
  commitments; its own ADR is a Sea-trials follow-up)
- `CONTRIBUTING.md` — the external-contract surfaces + stability commitments
- Binnacle ADR-0022 — named-mailbox primitive (referenced in #164)
- FSF license-compatibility list:
  <https://www.gnu.org/licenses/license-list.html#Expat>

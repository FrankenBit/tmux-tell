# Architecture Decision Records

This directory holds ADRs for tmux-tell. Each ADR records a single
architectural decision: what was decided, why, and what would have to
change for the decision to be revisited.

> **A note on the project name.** ADRs are point-in-time records and are
> *not* retro-edited. ADRs landed before the rename use the legacy project
> name `tmux-msg` (and `cli-semaphore` before that) per the timeline of
> their acceptance — see [ADR-0010](0010-tool-name.md) (the rename arc) and
> [ADR-0014](0014-tmux-tell-scope-and-cross-host-reach.md) (scope). The
> project is **`tmux-tell`** as of the v0.18.0 rename (#440).

## Convention

- **Filename**: `NNNN-kebab-case-title.md` where `NNNN` is a
  zero-padded sequence number assigned at acceptance time. First ADR
  is `0001-discipline-pins-as-test-category.md`.
- **Sequence**: monotonic; never reused. A retracted ADR keeps its
  number; the file documents the retraction.
- **Status**: one of `Proposed`, `Accepted`, `Superseded by ADR-NNNN`,
  `Retracted`. Status flips happen in the same PR that lands the
  decision (or its retraction).
- **Structure**: each ADR follows the template in `template.md` (the
  template is not itself an ADR and has no number — real ADRs start at
  `0001`).

## Index

| #    | Title                                          | Status   | Landed |
|------|------------------------------------------------|----------|--------|
| 0001 | [Discipline pins as a test category](0001-discipline-pins-as-test-category.md) | Accepted (amended 2026-05-31 per #55; amended 2026-06-01 per Surveyor #42 retrospective; amended 2026-06-09 per #254) | 2026-05-31 |
| 0002 | [Chamber-state carry-forward spec for Binnacle's M6b](0002-chamber-state-carry-forward.md) | Accepted | 2026-06-04 |
| 0003 | [Substrate-vs-flavor naming](0003-substrate-vs-flavor-naming.md) | Accepted | 2026-06-05 |
| 0004 | [MCP wire-surface naming (application of ADR-0003)](0004-mcp-wire-surface-naming.md) | Accepted | 2026-06-05 |
| 0005 | [Substrate-honest terminology (chamber → agent)](0005-substrate-honest-terminology.md) | Accepted | 2026-06-05 |
| 0006 | [ADR length cap (350 lines) + background docs](0006-adr-length-cap-and-background-docs.md) | Accepted | 2026-06-05 |
| 0007 | [Binnacle coexists with tmux-msg as an external Go module](0007-binnacle-coexist-external-contract.md) | Accepted | 2026-06-07 |
| 0008 | [Deprecation policy — two-minor-cycle floor (post-1.0)](0008-deprecation-policy.md) | Accepted | 2026-06-07 |
| 0009 | [Hook-context delivery — substrate stays delivery-method-agnostic](0009-hook-context-delivery-substrate-vs-adapter-boundary.md) | Accepted | 2026-06-09 |
| 0010 | [Tool name — `tmux-msg`, or rename?](0010-tool-name.md) | Accepted | 2026-06-10 |
| 0011 | [Mailman scope-expansion — three-fence test, applied to chamber respawn](0011-mailman-scope-expansion-chamber-respawn.md) | Accepted | 2026-06-11 |
| 0012 | [Session rename on bus-mediated clear (application of ADR-0011)](0012-session-rename-on-bus-mediated-clear.md) | Accepted | 2026-06-11 |
| 0013 | [Plan-first workflow for size/M+ work](0013-plan-first-workflow.md) | Proposed | 2026-06-14 |
| 0014 | [tmux-tell scope — IS / IS NOT / SSH-back-tunnel](0014-tmux-tell-scope-and-cross-host-reach.md) | Accepted | 2026-06-16 |
| 0015 | [Adopt Arc42 as the architecture-doc spine](0015-adopt-arc42-architecture-spine.md) | Accepted | 2026-06-16 |

> **Note on ADR length.** ADRs 0001-0005 predate the length-cap
> convention codified in ADR-0006 and run 276-440 lines each. ADR-0006+
> caps at 350 lines per ADR file; deeper exploration lives in co-located
> `NNNN-<slug>-background.md` files. The convention is forward-only —
> existing ADRs stay as written per ADR-0004 §Generality's parent-frozen
> precedent. ADR-0006 itself is the first worked example: ADR file
> under cap + co-located background doc carrying the deeper
> cross-project routing-graph analysis that didn't fit the ADR's
> §Calibration cleanly.

## When to file an ADR

File an ADR when the decision:

- Touches an **architectural commitment** (something a discipline pin
  test could guard).
- Constrains future work meaningfully (the decision narrows what
  later code is allowed to do).
- Has reasonable alternatives that were considered and rejected.

**Symmetric direction**: every commitment slug in the discipline-pin
register MUST have an ADR establishing the commitment. The ADR may be
ADR-0001 itself (for slugs in the initial register) or a dedicated ADR
for new commitments. Adding a pin without a corresponding ADR is
silently violating ADR-0001's discipline.

Decisions that DON'T need an ADR:

- Routine fixes, refactors, or feature additions that don't change
  the architecture.
- Style choices already governed by formatter / linter.
- Single-PR tactical choices with no downstream constraint.

When in doubt, write a one-paragraph rationale in the commit message
instead. Promote it to an ADR if it gets cited a second time.

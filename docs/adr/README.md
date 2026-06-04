# Architecture Decision Records

This directory holds ADRs for cli-semaphore. Each ADR records a single
architectural decision: what was decided, why, and what would have to
change for the decision to be revisited.

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
| 0001 | [Discipline pins as a test category](0001-discipline-pins-as-test-category.md) | Accepted (amended 2026-05-31 per #55; amended 2026-06-01 per Surveyor #42 retrospective) | 2026-05-31 |
| 0002 | [Chamber-state carry-forward spec for Binnacle's M6b](0002-chamber-state-carry-forward.md) | Accepted | 2026-06-04 |

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

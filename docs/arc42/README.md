# Arc42 Architecture Documentation

tmux-tell's architecture follows the [Arc42](https://arc42.org/) 12-section
template. Each section lives in its own file; this README is the index. The
decision to adopt the spine is recorded in
[ADR-0015](../adr/0015-adopt-arc42-architecture-spine.md) (mirroring Binnacle's
ADR-0007, for cross-project operator-consistency).

The spine follows a **link-first principle**: each section either links the
living doc that already covers it, or carries genuinely-missing content inline —
no section duplicates a linked source. This keeps the spine from rotting into the
gap it was meant to close.

## Index

| § | Section | File | Status |
|---|---|---|---|
| 1 | Introduction and Goals | [01-introduction-and-goals.md](01-introduction-and-goals.md) | drafted (PR-A) |
| 2 | Architecture Constraints | [02-architecture-constraints.md](02-architecture-constraints.md) | drafted (PR-A) |
| 3 | Context and Scope | [03-context-and-scope.md](03-context-and-scope.md) | drafted (PR-A) |
| 4 | Solution Strategy | [04-solution-strategy.md](04-solution-strategy.md) | spine stub → PR-B |
| 5 | Building Block View | [05-building-block-view.md](05-building-block-view.md) | spine stub → PR-B |
| 6 | Runtime View | [06-runtime-view.md](06-runtime-view.md) | spine stub → PR-B (one flow) |
| 7 | Deployment View | [07-deployment-view.md](07-deployment-view.md) | drafted (PR-A) |
| 8 | Crosscutting Concepts | [08-cross-cutting-concepts.md](08-cross-cutting-concepts.md) | spine stub → PR-B |
| 9 | Architecture Decisions | [09-architecture-decisions.md](09-architecture-decisions.md) | spine stub → PR-B (indexes [`../adr/`](../adr/)) |
| 10 | Quality Requirements | [10-quality-requirements.md](10-quality-requirements.md) | seed only → canonical PR-C (working session) |
| 11 | Risks and Technical Debt | [11-risks-and-technical-debt.md](11-risks-and-technical-debt.md) | spine stub → PR-B |
| 12 | Glossary | [12-glossary.md](12-glossary.md) | drafted (PR-A) |

This is **Phase 1** of #386. PR-A (this) lands the full 12-section spine + the
five cheap/gap-fill sections (§§1/2/3/7/12) + the freshness convention. PR-B fills
the substantive sections (§§4/5/6/8/9/11). PR-C canonicalizes §10 after the
collaborative working session. The *spine is complete now* so a reader can tell a
section is *planned*, not *missing* — that completeness guarantee is what Arc42 is
for.

## Freshness convention (two markers, single source each)

Per [ADR-0015](../adr/0015-adopt-arc42-architecture-spine.md) every section file
carries two complementary, non-duplicating freshness markers:

1. **YAML frontmatter `revisit-triggers`** — the mechanical list of changes that
   should pull this section into a review. A cut-driver consults these at Layer 1
   (below): instead of "did anything change that affects any section," the
   question becomes "did any of *these* triggers fire."
2. **HTML comment `<!-- last-reviewed: YYYY-MM-DD (context) -->`** — the date of
   the last substantive review/content pass, invisible in rendered Markdown but
   visible in source diff so drift between the marker and git history is spottable.
   The context in parentheses names *why* the date moved.

The date lives **only** in the HTML comment; the triggers live **only** in the
frontmatter — no field is duplicated, so neither can drift against the other (the
doc-state-claim-integrity NFR, [§10](10-quality-requirements.md), applied to this
spine itself).

> Note (D4): the operator-ratified scope-call was "both markers." This realizes
> "both" as *one source per mechanism* (date in the comment, triggers in the
> frontmatter) rather than duplicating the date in two places — the
> integrity-preserving reading. Flagged for review.

### Two-layer revisit discipline (#386)

- **Layer 1 — per-release-cut checklist item** (lands PR-B, in `CONTRIBUTING.md`
  §Release cuts, sibling to the #495 docs-coherence step): "Arc42 sections reviewed
  for staleness against this cut's changes." The cut-driver scans the cut's
  changes against the sections' `revisit-triggers` and pulls any needed section
  update into the cut. Salience, not machine-enforcement — the same framing as the
  #495 docs-coherence gate.
- **Layer 2 — the per-section `revisit-triggers` frontmatter** (this PR) that
  Layer 1 consults.

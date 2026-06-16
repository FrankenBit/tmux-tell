# ADR-0015: Adopt Arc42 as the architecture-doc spine

> **Status**: Accepted
> **Date**: 2026-06-16
> **Authors**: Herald (operator-ratified Arc42 adoption + the six Phase-1 scope-calls, 2026-06-16, via Bosun relay; #386)

## Context

tmux-tell has 16+ versioned releases of organically-grown substrate-history
documented across `README.md`, eleven `docs/*.md` files, fourteen ADRs,
`CHANGELOG.md`, `CONTRIBUTING.md`, and `AGENTS.md` — but **no single frame that
provides a completeness guarantee**. A reader arriving cold cannot tell whether a
topic is missing or just undiscovered. There are also genuine gaps: §10 Quality
Requirements, §11 Risks & Technical Debt, and §12 Glossary did not exist.

The operator raised (2026-06-13) adopting the **Arc42** 12-section template
already in use on Binnacle (`docs/arc42/`, governed by Binnacle ADR-0007), for a
consistent cross-project operator experience: the same section anchors in every
project. The real gap is structure, not volume — Arc42 gives a shared completeness
checklist and vocabulary without mandating new prose.

Adopting a documentation framework is itself an architecture decision, so it earns
an ADR (the same reasoning that produced Binnacle ADR-0007). Composes with
[ADR-0009](0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
(substrate-vs-adapter boundary, which §§2/3/8 crystallize) and
[ADR-0014](0014-tmux-tell-scope-and-cross-host-reach.md) (scope-fence, which §3
links as its canonical scope statement).

## Decision

Adopt Arc42 as the architecture-doc spine for tmux-tell. `docs/arc42/README.md` is
the entry point; the 12 sections live in per-section files (`01-…md` … `12-…md`)
following two binding principles:

1. **Link-first principle.** Each section either links the living doc that already
   covers it, or carries genuinely-missing content inline. No section carries prose
   that duplicates a linked source — a static section with no living source rots
   into the exact gap it was meant to close. (Coverage is the value Arc42 adds;
   depth stays in the living docs.)

2. **Two-marker freshness convention, single source per marker.** Every section
   carries (a) a YAML frontmatter `revisit-triggers` list — the mechanical triggers
   that pull the section into a review — and (b) an HTML `<!-- last-reviewed: DATE
   (context) -->` comment carrying the review date. The date lives only in the
   comment, the triggers only in the frontmatter, so neither can drift against the
   other. This is the doc-state-claim-integrity NFR (§10) applied to the spine
   itself — the documentation-layer analogue of the substrate's verify-token
   "never over-claim" invariant.

§9 points at `docs/adr/` by construction — no separate ADR-indexing mechanism is
needed. Phase-1 lands the full spine; depth is phased (see §References, #386).

## Alternatives considered

- **No spine (status quo)** — rich but unchecked docs; readers cannot detect gaps,
  owners cannot confirm completeness. Rejected: the completeness guarantee is the
  whole point.
- **Full-prose template (all 12 sections authored deep at once)** — guaranteed
  staleness (prose about component structure diverges from code the moment code
  moves) plus an operator doc-density-overwhelm cost. Rejected in favour of
  link-first + phased depth.
- **A single `architecture.md` file** — PRs would conflict on unrelated sections.
  Rejected: per-section files let PRs target one section.
- **BookStack-only spine** — not version-controlled / not PR-reviewable. Rejected:
  a structural completeness doc must live in-repo where it is diff-able. (BookStack
  remains the newcomer orientation layer.)

## Consequences

- `docs/arc42/README.md` becomes the single whole-system entry point; every
  perceived gap can be filed as a targeted issue against a named section.
- Existing docs (ADRs, `docs/*.md`) are **not** renamed or moved; the spine links
  them. Each new architecture-relevant doc should state which section it serves.
- The `revisit-triggers` frontmatter feeds a per-release-cut staleness check
  (Layer 1, landing with PR-B in `CONTRIBUTING.md`), sibling to the #495
  docs-coherence gate — both are salience mechanisms, not machine-enforced.
- Cost: the spine is one more surface to keep current; the freshness convention is
  the mitigation, but it relies on cut-driver vigilance (not CI).

## What would change the decision

- If link-first proves insufficient (sections rot despite the freshness markers),
  a CI/lint gate over the markers — or an auto-index-gen for §9 — becomes the
  follow-up (deferred, #386 out-of-scope).
- A move away from per-user single-host scope (ADR-0014) large enough to restructure
  the sections wholesale.

## References

- [#386](https://git.frankenbit.de/frankenbit/tmux-tell/issues/386) — Arc42 Phase 1 (this work; phased PR-A/B/C).
- Binnacle ADR-0007 (arc42-architecture-spine) — the precedent this mirrors.
- [ADR-0009](0009-hook-context-delivery-substrate-vs-adapter-boundary.md), [ADR-0014](0014-tmux-tell-scope-and-cross-host-reach.md) — anchored by §§2/3/8.
- [#495](https://git.frankenbit.de/frankenbit/tmux-tell/issues/495) — the docs-coherence gate the Layer-1 staleness check is sibling to.

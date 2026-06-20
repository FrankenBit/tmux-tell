# ADR-0016: Canonical-substrate-vs-curated-surface routing

> **Status**: Accepted
> **Date**: 2026-06-20
> **Authors**: Bosun (principle, #426), Pilot (ADR, #462)

## Context

Release artifacts for a software project serve different readers with different
needs. A release has at least three distinct moments: publishing (making the
version exist), recording what changed comprehensively, and narrating the
highlights for the reader who wants to understand the release at a glance.

Without an explicit routing decision, these surfaces collapse into each other:
the release UI body carries the comprehensive record, the CHANGELOG becomes
a pointer list, and the publish act becomes conflated with the narration act.
This creates archaeology problems — PR bodies and release UI text are
UI-level artifacts that get compressed and harder to locate; the "what
exactly changed in v0.17.1" answer requires tracking down a Forgejo release
page rather than reading `CHANGELOG.md` at that tag.

The routing principle emerged from #426 (release-draft body extractor design)
and was first applied in #391 (distillation). It has been cited as authority
by #391, #427, and #454 (contributor density convention). ADR-0006's rubric
identifies this as architectural: it has constrained release-narration design
across three empirical cycles and forecloses live alternatives.

## Decision

Three surfaces, three distinct roles:

1. **Release UI (Forgejo)** — the *publish gate*. Clicking Publish creates the
   tag. The release body is the *curated narrative* (prelude + Headlines digest
   from `CHANGELOG.md`), not the comprehensive record.

2. **`CHANGELOG.md`@tag** — the *comprehensive substrate-of-record*. Every
   merged change that a downstream consumer or future operator needs to
   understand appears here with full detail — no omissions, no "see PR for
   details". The `## [X.Y.Z]` section at the tag commit is the canonical
   answer to "what changed in this version".

3. **Release body** — a *curated extract* of the prelude section from
   `CHANGELOG.md`. `release-draft.yml` extracts everything before the first
   `### ` subsection and uses it as the draft body. The prelude must exist and
   contain substantive prose (#427 hard-fails an empty prelude by design);
   the release body is a reader-facing summary, not an audit record.

These roles are non-overlapping. The publish act (clicking Publish) is
separate from both the comprehensive record (CHANGELOG at tag) and the
curated narrative (release body from prelude). A change that appears in the
release body MUST also appear in full in `CHANGELOG.md`; the release body
may omit or summarise where the CHANGELOG does not.

## Alternatives considered

**Release-body-as-comprehensive** — make the Forgejo release body the full,
authoritative record and demote `CHANGELOG.md` to a pointer list. Rejected
because release bodies live in the Forgejo UI: they are indexed by a web
application, subject to UI layout changes, and harder to access in-repo at
a specific tag. `git show v0.17.1:CHANGELOG.md` is a stable, tool-friendly
access pattern that survives platform migrations.

**CHANGELOG-as-thin-pointers** — `CHANGELOG.md` entries are just PR links;
the PR body is the substrate-of-record. Rejected because PR bodies age into
archaeology (#391's finding): a reader consulting v0.16.0 three years later
should not need to track down a PR thread to understand what changed.
CHANGELOG at a tag must be self-contained.

**Single-surface** — collapse publish + record + narrative into one surface
(the release UI is the only artifact that matters). Rejected because the
Forgejo release UI is optimised for announcement, not forensic archaeology.
A comprehensive record maintained separately (CHANGELOG.md in-repo) is more
durable and format-stable than UI text.

## Consequences

**Cleaner**: each surface has one job. Readers know where to look —
`CHANGELOG.md` at a tag for "what changed", the release body for "what
matters to me in this version". The routing prevents accidental collapse of
the surfaces (a PR that lets release body grow comprehensive at the expense
of CHANGELOG discipline, or vice versa).

**Harder**: the `## [X.Y.Z]` prelude in `CHANGELOG.md` is load-bearing. It
must contain substantive prose (not just bullet lists) because `release-draft.yml`
extracts it as the release body. A release cut without a hand-curated prelude
hard-fails at workflow time (#427). This is a deliberate tax: it forces
release authors to write the narrative once (in CHANGELOG), not twice (in
CHANGELOG and separately in a release-body template).

**Forward inheritance**: new release substrates (post-merge steps, deploy
notes, external dashboards) route to one of the three surfaces by role. A
new substrate that needs the comprehensive record reads `CHANGELOG.md`@tag.
A new substrate that needs the narrative extract reads the prelude section.
Nothing else should be invented.

## What would change the decision

- If the project adopts a doc-hosting system where `CHANGELOG.md` at a tag
  is inaccessible to the primary reader audience (e.g., all documentation is
  generated from a separate pipeline and the in-repo file is never rendered).
  In that case, the comprehensive-record role would need to move.
- If maintaining comprehensive `CHANGELOG.md` prose becomes a practical
  bottleneck (unlikely pre-1.0, but worth revisiting at scale).

## References

- #426 — origin of the routing principle (release-draft body extractor design)
- #427 — honest-hard-fail on empty prelude, the enforcement downstream of this routing
- #391 — first worked application (distillation Parts 1+2 ratifying the routing as authority)
- #454 — contributor-discipline codification (CHANGELOG density convention); cross-references this ADR
- [CONTRIBUTING.md §CHANGELOG entries](../../CONTRIBUTING.md) — operational density convention

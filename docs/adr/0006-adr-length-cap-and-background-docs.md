# ADR-0006: ADR length cap (400 lines) + background docs

> **Status**: Proposed
> **Date**: 2026-06-05 (proposed)
> **Authors**: Quartermaster (author), operator (surfaced the
> length-cap question on PR #115 after ADR-0005 merged; proposed
> 300-400 range; picked 400 after analysis), Binnacle's ADR
> convention (inspiration — adapted upward from ≤100 to ≤400 for
> tmux-msg's deeper-audience material)

## Context

ADR line counts across the project so far:

| ADR | Lines | Type |
|---|---|---|
| 0001 | 307 | Process (discipline-pin discipline) |
| 0002 | 276 | Substrate (chamber-state carry-forward) |
| 0003 | 319 | Principle (substrate-vs-flavor) |
| 0004 | 370 | Application (MCP wire surface) |
| 0005 | 440 | Application (substrate-honest terminology) |

The trend is upward. Each application ADR adds inheritance from
its parent's precedent, the named pattern reference (or its
promotion), the per-surface sub-decisions, and the framing-review-
derived patterns. The substrate-rename arc alone surfaced three
named patterns (substance-vs-reference state cleavage from ADR-0003
round-2; surface-vs-substantive staleness routing from ADR-0004
round-3; wheel-reinvention check on supertype-vs-rename commitments
from ADR-0005 round-2) — all via adversarial framing-review
pressure, all now living inline in their respective ADRs.

The trajectory leads to 600-700 line ADRs as more applications stack.

Binnacle caps ADRs at 100 lines with co-located
`NNNN-<slug>-background.md` files carrying the deeper exploration.
The cap forces skimmability; the background doc preserves depth.
Two audiences served by separate artifacts: the skim-what-was-
decided reader gets the ADR; the deep-why-was-that-the-call reader
opens the background.

Operator surfaced this on 2026-06-05 (PR #115 framing review)
asking whether tmux-msg should adopt Binnacle's pattern. ADR-0006
codifies the convention.

## Decision

**Three sub-decisions:**

### (1) Cap: 400 lines per ADR file, forward-only

Future ADRs (ADR-0006+) cap at **400 lines** including the header
block and references section. Existing ADRs (0001-0005) stay as
written — they predate the convention and are explicitly preserved
per ADR-0004 §Generality's parent-frozen precedent (verbose by a
later-introduced discipline is not the same as substantively
wrong; supersession escape hatch doesn't apply).

400 was picked from operator's 300-400 range as the value that:

- Leaves standard alternative-bearing structure (Context / Decision
  / Alternatives / Consequences / What-would-change / References)
  room at ~50-60 lines per section without squeezing
- Prevents unbounded growth: the trajectory above is bounded by 400
- Forces background-doc only for genuinely deep material
  (multi-axis distinctness analyses, wheel-reinvention checks,
  framing-review threading captured across rounds, pattern arc
  references that would otherwise re-inline per occurrence)
- Avoids inviting the cap-loophole of ultra-aggressive abbreviation
  that would have been a risk at 100 or 200

**Calibration note** (per Surveyor's framing observation on the
draft): the cap is calibrated to the **median** of the existing
ADR range (270-440), not the longest. ADR-0001..0004 sit at
276-370 lines, comfortably within 400. ADR-0005 at 440 exceeds the
cap by ~10% — it stays as written per the forward-only scope below,
and is the empirical anchor for "this is what gets split to
background docs from ADR-0006+ onward." The cap does NOT mean the
longest existing ADR retroactively fits; it means future ADRs are
held to a standard that the longest existing ADR demonstrably
exceeded.

The cap is **soft** — exceeding it doesn't block merge, but it
triggers a question in framing review: "Should this material move
to a background doc?" Authors expecting to exceed should split
preemptively.

### (2) Background-doc convention

Background docs co-locate with their ADR in `docs/adr/`, named
`NNNN-<slug>-background.md` (matching the ADR's slug). One
background doc per ADR; no further nesting. Authors structure the
background doc as they see fit — the ADR-format discipline applies
to the ADR file, not the background.

**What stays in the ADR:**

- Header (Status / Date / Authors)
- Context: 1-2 paragraphs naming the trigger + the principle (if
  application ADR) or the new claim (if process or substrate ADR)
- Decision: the sub-decisions and their direct rationale
- Alternatives: the catalog with one-line rejection reasoning each
- Consequences: cleaner / harder, scoped to the immediate effects
- What-would-change: the supersession triggers (not deep analyses)
- References: links + cross-ADR citations

**What goes to the background doc:**

- Multi-axis distinctness analyses (e.g., the §Pattern structural-
  distinctness exploration that ADR-0005 had inline)
- Wheel-reinvention checks (e.g., ADR-0005's operator-shell scenario
  walk-through)
- Framing-review round-by-round captures (Surveyor must-fix /
  should-consider disposition threads)
- Named-pattern arc references and their cross-ADR threading
- Deep alternative-space exploration beyond one-line rejection
- Empirical methodology details and counter-counts
- Quotes from operator / Surveyor / external review context

The split is by **depth-of-required-engagement**, not by importance:
a reader cares about both; the ADR offers the decision, the
background offers the reasoning.

### (3) Index marker

`docs/adr/README.md` gains a one-line note after the index table
indicating ADRs 0001-0005 predate the cap; ADR-0006+ conforms.
Forward-watch readers know which ADRs to expect at what length
without confusion.

## Alternatives considered

- **100-line cap (Binnacle's value).** Rejected as too aggressive
  for tmux-msg's audience. Binnacle's ADRs serve a primarily
  reader-skim audience (developers approaching unfamiliar code);
  tmux-msg's ADRs serve a substrate-design audience that often
  needs the alternative-bearing structure inline to evaluate the
  decision properly. 100 would force background-doc work on every
  ADR; that overhead is not value-add at this scale.
- **300-line cap.** Defensible alternative; would force background
  splits earlier (ADR-0001 at ~310 would be just over). Picked
  400 because the standard ADR structure fits more comfortably
  and the cap is meant to bound, not squeeze.
- **500+ cap or no cap.** Rejected: doesn't prevent the trajectory.
  The whole point of the convention is to bound growth.
- **No background-doc separation, just enforce shorter ADRs.**
  Rejected: forces compression that loses the round-by-round
  framing-review captures and named-pattern arc material the
  project has demonstrably benefited from. Background docs let
  depth persist where the author wanted depth, without imposing
  it on every reader.
- **Retroactive split of ADR-0001..0005.** Rejected per ADR-0004
  §Generality: substantive content of past ADRs is frozen.
  Splitting them would be a substantive amendment — exactly what
  the parent-frozen precedent prohibits. Forward-only is the only
  compatible scope.

## Consequences

### Cleaner

- **Skim-vs-deep audiences both served.** ADR file answers "what
  was decided"; background doc answers "why was that the call."
  Future readers approach with the right tool.
- **Named patterns get a home.** Surveyor's three patterns from the
  substrate-rename arc (substance-vs-reference, surface-vs-
  substantive, wheel-reinvention check) are the kind of material
  that benefits from background-doc residency — cited by reference
  in future ADRs rather than re-inlined per occurrence.
- **Trajectory bounded.** 400 lines is a hard ceiling on per-ADR
  size; the project doesn't drift to 700-line ADRs by accident.
- **Discipline-as-ADR pattern carries forward.** ADR-0001's
  precedent for process-as-ADR is reinforced; future process
  decisions follow the same shape.

### Harder

- **File-management discipline.** Authors must judge when to split.
  The judgment is "does this require deep engagement to evaluate?"
  Misjudgment cost is low (background doc is added later, or
  inline material that should have moved out stays a bit too long).
- **Cross-reference overhead.** Background docs need to be linked
  from the ADR; cross-citation grows by one indirection per
  reference. Small cost per reference, recurs.
- **Learn-the-pattern cost.** First few ADR-0006+ authors will
  need to absorb the convention from this ADR + ADR-0001's process
  precedent.
- **Inconsistency between ADR-0001..0005 and ADR-0006+.** Existing
  ADRs stay long-form; the convention shift is visible as a
  cliff edge. Index marker (decision 3) mitigates by naming the
  cliff explicitly.

## What would change the decision

Reasons to retract or supersede ADR-0006:

- **400 proves too lax.** If ADRs continue trending toward the cap
  without using background docs (i.e., authors filling 400 lines
  without splitting deep material out), the cap isn't earning
  its keep. Amendment trigger: tighten to 300 via supersession.
- **400 proves too strict.** If authors routinely defer to
  background docs to fit, with the ADR file becoming a near-empty
  index of the background, the cap is forcing artificial split.
  Amendment trigger: relax cap or remove via supersession.
- **Background-doc convention proves unused.** If 6 months in,
  ADR-0006+ ADRs systematically come in under 400 without ever
  spawning a background doc, the background-doc half of the
  convention isn't load-bearing. Retract the background-doc half
  via amendment; keep the cap as a soft guideline.
- **Project audience shifts.** If the substrate stabilizes and
  the ADRs become read primarily by skim-audiences (new
  contributors orienting), Binnacle's 100-line cap becomes more
  appropriate. Supersede toward tighter discipline at that point.

The watch: track the next 4-6 ADRs post-this-one. If 50%+ exceed
400 (cap too lax) or 50%+ fall under 100 without depth (cap
mismatched), the convention isn't fitting and warrants revisit.

## References

- Operator's 2026-06-05 length-cap question on PR #115
- ADR-0001 — process-as-ADR precedent (discipline-pin discipline
  as a test category)
- ADR-0004 §Generality — parent-frozen precedent that prevents
  retroactive splits of ADR-0001..0005
- Binnacle's ≤100-line + background-doc convention (the
  inspiration; tmux-msg adapts upward to 400)
- Surveyor's three named patterns across the substrate-rename arc
  (substance-vs-reference state cleavage, surface-vs-substantive
  staleness routing, wheel-reinvention check) — exemplars of the
  kind of material future ADRs will cite from background docs
  rather than re-inline
- #117 — this ADR's tracking issue

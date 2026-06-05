# ADR-0006: ADR length cap (350 lines) + background docs

> **Status**: Accepted
> **Date**: 2026-06-05 (proposed); 2026-06-05 (accepted on operator
> + Surveyor TICK with three small refinements and one nit folded
> pre-merge)
> **Authors**: Quartermaster (author), operator (surfaced the
> length-cap question on PR #115 after ADR-0005 merged; proposed
> 300-400 range; picked 350 after analysis), Surveyor (framing
> review: empirical-anchor framing folded pre-PR + three refinements
> (bg-doc soft-one-per-ADR, status-lifecycle inheritance, index
> visibility) + a §Worked example subsection + one nit on the
> Binnacle comparison framing), Binnacle's ADR convention
> (inspiration — adapted upward from ≤100 to ≤350 for tmux-msg's
> deeper-audience material)

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

### (1) Cap: 350 lines per ADR file, forward-only

Future ADRs (ADR-0006+) cap at **350 lines** including the header
block and references section. Existing ADRs (0001-0005) stay as
written — they predate the convention and are explicitly preserved
per ADR-0004 §Generality's parent-frozen precedent (verbose by a
later-introduced discipline is not the same as substantively
wrong; supersession escape hatch doesn't apply).

350 was picked from operator's 300-400 range as the value that:

- Leaves standard alternative-bearing structure (Context / Decision
  / Alternatives / Consequences / What-would-change / References)
  room at ~50-60 lines per section without squeezing
- Prevents unbounded growth: the trajectory above is bounded by 350
- Forces background-doc only for genuinely deep material
  (multi-axis distinctness analyses, wheel-reinvention checks,
  framing-review threading captured across rounds, pattern arc
  references that would otherwise re-inline per occurrence)
- Suits tmux-msg's deeper-audience material (substrate-design
  decisions that often need the alternative-bearing structure
  inline for proper evaluation) without inviting the cap-loophole
  of ultra-aggressive abbreviation that a tighter cap might invite

**Calibration note** (per Surveyor's framing observation on the
draft): the cap is calibrated above the **median** of the existing
ADR range (276-440), not above the longest. ADR-0001..0003 sit at
276-319 lines, within 350. ADR-0004 at 370 and ADR-0005 at 440 both
exceed — they stay as written per the forward-only scope below, and
are the empirical anchors for "this is what gets split to background
docs from ADR-0006+ onward." The cap does NOT mean the longest
existing ADR retroactively fits; it means future ADRs are held to a
standard that two of the five existing ADRs demonstrably exceeded.

The cap is **soft** — exceeding it doesn't block merge, but it
triggers a question in framing review: "Should this material move
to a background doc?" Authors expecting to exceed should split
preemptively.

### (2) Background-doc convention

Background docs co-locate with their ADR in `docs/adr/`, named
`NNNN-<slug>-background.md` (matching the ADR's slug). Authors
structure the background doc as they see fit — the ADR-format
discipline applies to the ADR file, not the background.

**One per ADR (soft).** The convention is one background doc per
ADR. If an ADR genuinely needs two background docs for unrelated
deep-dive topics, that's usually a signal the ADR itself should
be split into two ADRs (each with its own bg-doc). Authors may
exceed the one-per-ADR norm with rationale; reviewers may push
back. Not a hard rule.

**Status / lifecycle implicitly inherits from the parent ADR.**
Bg-docs are not independently versioned — they share the parent
ADR's `Status` (Proposed / Accepted / Superseded / Retracted) by
co-location. If the parent ADR is superseded, the bg-doc travels
with it; supersession of the ADR transitively supersedes the
bg-doc's claims. No separate status header is required in
background docs.

**Index visibility.** The `docs/adr/README.md` index lists ADRs
only. Background docs are implicit-child via slug match
(`<NNNN>-<slug>.md` → `<NNNN>-<slug>-background.md` adjacent).
Listing them separately would clutter the index without value;
readers know to check for the adjacent bg-doc.

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

**Worked example: ADR-0005 hypothetically split.** ADR-0005 at 440
lines exceeds the cap by ~10%. Hypothetical split:

- **Move to `0005-substrate-honest-terminology-background.md`**
  (~100 lines): §Decision (1)'s wheel-reinvention check subsection
  (the operator-shell scenario walk-through, ~50 lines) + §Pattern
  promotion's structural-distinctness analysis (~20 lines) + the
  expanded §Alternatives catalog with per-candidate rationale (~30
  lines, beyond the one-line rejection summaries that stay).
- **ADR file shrinks to ~340 lines**: header, §Context (with bg-doc
  pointer for substrate-honest two-readings exploration), §Decision
  (1)'s one-paragraph resolution ("`agent` chosen; rationale and
  wheel-reinvention check in bg-doc"), §Decisions (2)/(3)/(4) as
  is, §Alternatives catalog at one-line rejections each, §Pattern
  promotion as one-paragraph reference to bg-doc analysis,
  §Consequences and §What-would-change unchanged.
- **Result**: skim-reader gets the decision in ~340 lines; deep
  reader follows the bg-doc pointers for ~100 lines of analysis.
  Total content preserved; per-reader cost reduced.

The example is hypothetical because ADR-0005 stays as written per
the forward-only scope below; it serves as the visualisable
worked example for ADR-0006+ authors deciding what to split.

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
- **300-line cap.** Defensible alternative on the tighter side;
  would force background splits earlier (ADR-0001 at 307 would
  also just exceed it). Rejected because the standard ADR
  structure squeezes — 350 leaves slightly more room for
  alternative-bearing structure without inviting unbounded growth.
- **400-line cap.** The initial pick; defensible on the looser
  side. Rejected at operator request as still permitting too much
  growth — only ADR-0005 would exceed it, weakening the discipline
  signal. 350 forces ADR-0004 (370 lines) under the cap too,
  expanding the empirical anchor set from 1/5 to 2/5 existing ADRs
  and giving the convention more bite.
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
- **Trajectory bounded.** 350 lines is a hard ceiling on per-ADR
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

- **350 proves too lax.** If ADRs continue trending toward the cap
  without using background docs (i.e., authors filling 350 lines
  without splitting deep material out), the cap isn't earning
  its keep. Amendment trigger: tighten to 300 via supersession.
- **350 proves too strict.** If authors routinely defer to
  background docs to fit, with the ADR file becoming a near-empty
  index of the background, the cap is forcing artificial split.
  Amendment trigger: relax cap (e.g., back to 400) or remove via
  supersession.
- **Background-doc convention proves unused.** If 6 months in,
  ADR-0006+ ADRs systematically come in under 350 without ever
  spawning a background doc, the background-doc half of the
  convention isn't load-bearing. Retract the background-doc half
  via amendment; keep the cap as a soft guideline.
- **Project audience shifts.** If the substrate stabilizes and
  the ADRs become read primarily by skim-audiences (new
  contributors orienting), Binnacle's 100-line cap becomes more
  appropriate. Supersede toward tighter discipline at that point.

The watch: track the next 4-6 ADRs post-this-one. If 50%+ exceed
350 (cap too lax) or 50%+ fall under 100 without depth (cap
mismatched), the convention isn't fitting and warrants revisit.

## References

- Operator's 2026-06-05 length-cap question on PR #115
- ADR-0001 — process-as-ADR precedent (discipline-pin discipline
  as a test category)
- ADR-0004 §Generality — parent-frozen precedent that prevents
  retroactive splits of ADR-0001..0005
- Binnacle's ≤100-line + background-doc convention (the
  inspiration; tmux-msg adapts upward to 350)
- Surveyor's three named patterns across the substrate-rename arc
  (substance-vs-reference state cleavage, surface-vs-substantive
  staleness routing, wheel-reinvention check) — exemplars of the
  kind of material future ADRs will cite from background docs
  rather than re-inline
- #117 — this ADR's tracking issue

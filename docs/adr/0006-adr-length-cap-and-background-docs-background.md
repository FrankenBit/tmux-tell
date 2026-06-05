# ADR-0006 background — cross-project relationship analysis + framework-register lesson

This background doc carries the deep analysis of the cross-project
relationship between ADR-0006 (tmux-msg) and Binnacle's parallel
ADR-0029 supersession. Co-located with `0006-adr-length-cap-and-background-docs.md`;
status implicitly inherits from the parent ADR per ADR-0006
§Decision (2).

## Cross-project relationship — two chains, distinguished

The cross-project relationship is **not** independent convergence,
but it is also **not** a single propagation chain. Two distinct
chains run in parallel:

### (1) Number-carry via operator

Bosun analyzed first from the Binnacle side (raising Binnacle's
existing 100-line cap), reasoning from:

- First-ADR-size on Binnacle side (310)
- ~15% growth headroom from the first-ADR-size anchor
- Binnacle's empirical background-doc distribution: median 250-350,
  cluster 200-400, two outliers at 424 and 582

Arriving at 350.

The operator carried that number into this ADR's context (asking
Quartermaster to lower 400 → 350) without transmitting Bosun's
reasoning chain — only the choice. This ADR's revision from 400 to
350 was operator-prompted based on the carried number; Quartermaster
did NOT independently re-derive the choice from substrate-survey
before pushing.

### (2) Reasoning-survey direct from Bosun, post-revision

After the mechanical 400 → 350 push (`f5a5576`), Bosun sent his
analytical frame directly to Quartermaster via bus (id 6267). That
frame was folded into this ADR's §Calibration as supporting
context (`395c866`). The reasoning-survey reached this ADR
independently of the operator-carry, just chronologically after.

## Meta-correction trail

The §Calibration paragraph went through three framings as the
substrate of the cross-project relationship became clearer:

1. **`395c866` — "Two actors reasoned independently to the same
   number — substrate-claim-verification family in action."**
   Wrong on two axes: (a) the convergence wasn't independent;
   (b) the family attribution was wrong even if convergence had
   been independent (it would have been independent-triangulation
   epistemics / diversity-of-evidence corroboration, a different
   family).

2. **`c5b26ee` — "Reasoning-propagated, not independently arrived
   at."** Caught the dependence but conflated number-carry with
   reasoning-propagation. The reasoning didn't propagate through
   the operator; only the number did. Bosun's reasoning chain
   reached Quartermaster through a parallel direct-bus channel
   after the revision was already pushed.

3. **Current — "Two chains, distinguished: number-carry via
   operator + reasoning-survey direct from Bosun, post-revision."**
   Honors both the routing graph (which actor analyzed first,
   what exactly was transmitted, through which channels, in what
   order) and the chronology (which message arrived when, what
   the ADR state was at each point).

## Framework-register lesson (the worked instance-of-the-pattern)

**Substrate-claim-verification at the verification-mechanism
layer.** When an apparent "convergence" pattern surfaces in a
multi-actor routed-information environment, verify the **routing
graph**:

- **Which actor analyzed first.** (Here: Bosun on the Binnacle side.)
- **What exactly was transmitted at each hop.** (Here: number from
  Bosun to operator to Quartermaster; reasoning from Bosun to
  Quartermaster directly via bus.)
- **Through which channels.** (Here: operator carried the number
  through an in-conversation prompt; Bosun's reasoning came via the
  semaphore bus.)
- **In what order.** (Here: number-carry preceded reasoning-survey
  by one push.)

Two surface-similar framings — "independent convergence" and
"reasoning-propagation" — can both be wrong in the same direction
if the routing graph isn't probed. The correct framing here is
neither single label; it's a two-chain decomposition with explicit
chronology.

This pattern is **sibling** to:

- **Substance-vs-reference state cleavage** (ADR-0003 round-2):
  when verifying what's frozen vs what's live, distinguish
  substance state from reference state.
- **Surface-vs-substantive staleness routing** (ADR-0004 round-3):
  when handling staleness in an immutability regime, route by
  surface-staleness (handled by successor) vs substantive-wrongness
  (handled by supersession).
- **Wheel-reinvention check on supertype-vs-rename commitments**
  (ADR-0005 round-2): when forward-correctness arguments surface
  for new abstractions, walk through the use case against existing
  primitives before accepting the commitment.
- **Independent-triangulation / diversity-of-evidence corroboration**
  (named-but-not-instantiated in this campaign): when actors
  reasoning from different evidence bases arrive at the same
  conclusion, that's Bayesian-strong corroboration of the
  conclusion's substrate. This family is **distinct** from
  substrate-claim-verification — the latter probes whether the
  conclusion itself is grounded; the former probes whether
  multiple independent groundings converge. ADR-0006 §Calibration's
  original framing claimed this family's pattern; the routing-graph
  probe revealed the actors weren't reasoning independently, so
  the family didn't apply here. The pattern remains a valid
  catalog entry for future cases where the routing graph confirms
  independence.

ADR-0006 contributes the fifth sub-shape: **verification-mechanism-
layer routing-graph probe**. The campaign's substrate-rename arc
(ADR-0003/0004/0005) plus this ADR's process-discipline arc have
together surfaced **five** named verification-discipline patterns
through adversarial review pressure rather than by design — four
instantiated in the campaign's worked examples, one
(independent-triangulation) named-but-not-instantiated because the
apparent example turned out to be a substrate-claim-verification
instance instead. Both are valid distinct families; the catalog
preserves both rather than collapsing them.

## Surveyor's self-banked lesson

In the framing-verify on `395c866` (PR #118 comment 62543),
Surveyor credited the "independent convergence" as
"genuinely Bayesian-strong" without probing the routing graph.
Bosun's id 703a correction caught what Quartermaster mis-framed
and what Surveyor endorsed — the verification-mechanism-layer
pattern at work, banked by Surveyor self-citation: "when
'convergence' claims involve multiple agents in a routed-
information environment, verify the routing graph before
crediting independence."

This is itself a sub-instance of the pattern: the discipline
emerged via three actors (Quartermaster mis-framed; Surveyor
verified-with-blind-spot; Bosun caught the routing fact); each
actor's own correction-loop strengthened the pattern's articulation.

## Cross-project consistency status

Binnacle's parallel PR raises 100 → 350 via ADR-0029 supersession
(Bosun's first attempt directly amended ADR-0001's text, operator
caught the procedure violation; PR-2 honors ADR-0001 §Amendments'
own self-named supersession path). Cross-project consistency at
350 is real — both projects converge on the number, just via a
routed-information chain rather than independent reasoning.

The substantive substrate (350 is empirically grounded for both
projects via the same underlying data — first-ADR-size + growth
headroom + bg-doc distribution clustering) is unchanged through
all three framing revisions. What changed was the framing's
accuracy about HOW both projects arrived there.

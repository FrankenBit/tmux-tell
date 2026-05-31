# ADR-0001: Discipline pins as a test category

> **Status**: Proposed
> **Date**: 2026-05-31
> **Authors**: Admin (author), Surveyor (by-commitment scope per
> issue #34 comment 58662)

## Context

The cli-semaphore test suite currently mixes two categorically
different test classes without distinguishing them in code or
convention:

- **Behavioral / regression tests** verify "does this function compute
  the right output for this input?" Failure means a bug crept in;
  fix the bug.
- **Discipline pins** verify "does this implementation honor an
  architectural commitment?" Failure means the commitment is being
  violated, possibly intentionally; the failing test is the gate
  against silently dropping the commitment.

Treating both classes the same has two costs:

1. **Triage drift**: a discipline-pin failure may be triaged as "the
   test is wrong, update it" when the right response is "the
   commitment is no longer load-bearing, retract the pin via ADR" OR
   "the commitment is intact, fix the implementation that violated it."
2. **Discoverability**: an operator scanning the test suite cannot
   answer "what architectural commitments does this codebase pin?"
   without reading every test individually.

`docs/failure-modes.md` §4.2 enumerates **four architectural
commitments** that pinning tests currently guard, across **eight
test functions** (status as of v0.2.1):

| Architectural commitment                                                              | Existing pin tests                                                                                                                                                                                                  | Source                                              |
|---------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-----------------------------------------------------|
| **`WireShapeSingleSoT`** — wire shape is single source-of-truth (JSON-tag-driven; no manual map construction) | `TestTrackResult_OmitemptyContract`; `TestTrack_WireShape_CLIAndMCPMatch`                                                                                                                                            | Surveyor #28 / #29 reviews                          |
| **`AtomicCapEnforcement`** — caps are ceilings, never floors (atomic under concurrency)                       | `TestInsertMessage_CapEnforcedUnderConcurrency`                                                                                                                                                                      | Surveyor #29 round-3 review                         |
| **`ThreadStructurePrecondition`** — `linkP2ToP1` callers don't pass explicit `reply_to`                       | `TestInsertMessagePair_LinkP2ToP1_RejectsExplicitReplyTo`                                                                                                                                                            | Surveyor #29 follow-up                              |
| **`CanonicalNoSilentGuess`** — never silently guess between canonical-or-alias exact matches                  | `TestLookupByNameWithCanonicals_Ambiguous` (substring variant); `_ExactMatchAmbiguous_AliasCollision`; `_ExactMatchAmbiguous_AliasIsAnotherCanonical`; `TestPaneAgentNameWithCanonicals_ExactMatchAmbiguous`         | Surveyor v0.2.0 Q(a) + v0.2.1 review                |

The right unit of account for this ADR is **the architectural
commitment, not the individual test**. Four commitments exist; eight
tests implement them, possibly multiply. When a fifth commitment
surfaces, the commitment count tells us discipline-pins are earning
their keep across time; by-test framing flattens that signal.

## Decision

**Discipline pins are a distinct test category, governed by three
mechanical conventions and one triage discipline.**

### Mechanical conventions

1. **File**: each package containing pin tests has a `pin_test.go`
   file holding ONLY pin tests. Regression and behavioral tests stay
   in their existing files. Grep handle: `find . -name pin_test.go`
   lists the pinning surface across the codebase.
2. **Function name**: pin test functions are named
   `TestPin_<CommitmentSlug>_<Variant>`. The `TestPin_` prefix is the
   grep handle for the category; the `<CommitmentSlug>` identifies
   which architectural commitment the test guards.
3. **Docstring**: each pin's docstring opens with a single line:
   `// PIN: <one-sentence architectural commitment>`. The `// PIN:`
   marker is a third orthogonal grep handle and makes the
   commitment-to-test traceability mechanical.

Three handles for the same discipline: file (`pin_test.go`),
function (`TestPin_`), docstring (`// PIN:`). Any one of them
locates the pinning surface; together they make miscategorization
self-evident under review.

### Commitment slugs (initial register)

<!-- pin-slug-register-start -->

- **`WireShapeSingleSoT`** — wire shape is single source-of-truth
  (JSON-tag-driven; no manual map construction).
- **`AtomicCapEnforcement`** — caps are ceilings, never floors
  (atomic under concurrency).
- **`ThreadStructurePrecondition`** — `linkP2ToP1` callers don't pass
  explicit `reply_to`.
- **`CanonicalNoSilentGuess`** — never silently guess between
  canonical-or-alias exact matches.

<!-- pin-slug-register-end -->

The slug carries the commitment's essence, not just a label: a
reader of just the slug + test name should be able to infer the
load-bearing claim without consulting the docstring. The marker
comments around the register are a parser anchor for the
CI-enforcement tooling tracked as #51.

Adding a fifth slug is a deliberate act, not an accidental side
effect of writing another test. The slug should be added to this
ADR (via amendment) at the same time the first pin for the new
commitment lands. Until #51 ships, the discipline is convention-only
and rests on reviewer attention; #51 promotes the "deliberate act"
framing from convention to gate.

### Triage discipline

When a discipline pin fails:

1. **Default response is NOT "fix the implementation."** It's:
   triage which of three things happened.
2. **Three possible diagnoses**:
   - **(a) The commitment is intact; the implementation regressed.**
     Standard bugfix path. Fix the code; re-run the pin; verify green.
   - **(b) The commitment was always wrong / no longer load-bearing.**
     Retract the pin via ADR (this one's amendment, or a superseding
     ADR). The pin's removal needs the same gate as adding one: an
     explicit decision, not an accidental delete.
   - **(c) The commitment is intact but the pin's assertion is
     wrong.** The assertion is part of the commitment's expression;
     fixing it requires the strengthenings below to prevent silent
     erosion of the discipline.

3. **Strengthenings on diagnosis (c)** — both required:
    - **(c.1) Co-require a regression test demonstrating the new
      assertion strictly improves coverage.** If (c) loosens the
      assertion (false-positive failure under intact commitment), the
      new pin needs a counter-test showing the loosened pin still
      catches a real commitment violation. If (c) tightens the
      assertion (the prior pin was too loose and let a real bug
      through), the strengthening needs the test case that motivated
      it. Either way: (c) must demonstrate the new assertion
      strictly improves the commitment's coverage. This is the
      operational floor.
    - **(c.2) Co-require an ADR amendment on (c), not just a
      commit-message rationale.** Rationale: if the assertion was
      wrong, the commitment-as-encoded was wrong somewhere — even if
      just at the encoding layer. The ADR's commitment statement may
      itself need clarification (sometimes the slug or one-sentence
      commitment hides an ambiguity that the broken assertion
      exposes). Treating the assertion as part of the commitment's
      expression makes (c) a deliberate act, parallel to adding a
      new commitment slug.

   Without (c.1), (c) becomes "I weakened the assertion until it
   passed and wrote a sentence explaining why." Without (c.2), the
   discipline ceremony is invisible across project history. Both
   gates closed = the discipline holds.

4. **Flake / non-determinism is a (c) variant.** Pin tests should be
   deterministic by construction; a flaking pin means the assertion
   encodes the commitment in a non-deterministic shape (e.g., a
   concurrency pin using a timing window instead of a deterministic
   driver). The right response is **strengthen toward determinism**,
   not retry-into-noise. Triage as (c); apply (c.1) by demonstrating
   the deterministic-rewrite catches the original commitment
   violation; apply (c.2) since the assertion's shape needed
   clarification. A common pattern: replace timing-based concurrency
   checks with deterministic schedulers or barriers.

5. **Failure-site triage hint**. Each pin test must call
   `testpin.Triage(t, "<Slug>", "<one-sentence commitment>")` as
   its first line. This installs a `t.Cleanup` that logs the triage
   pointer when the pin fails:

   ```
   PIN FAILURE [WireShapeSingleSoT] — wire shape is single source-of-truth
     Triage per ADR-0001 §Triage before fixing the assertion.
     Diagnoses: (a) implementation regressed / (b) commitment retracted / (c) pin miswrote.
   ```

   The helper's source-of-truth on the slug (machine-readable) is
   what the CI-enforcement tooling (#51) parses. The `// PIN:`
   docstring marker stays as a human-readable adjacent view.

The triage step lives in the pre-commit flow as a discussion
artifact (commit message rationale; PR description; ADR amendment
on (c) per (c.2); or, for non-trivial cases, an explicit Surveyor
consultation). The cost is small per pin; the value is that a
pin's removal or assertion-change is a deliberate decision rather
than a silent deletion or weakening.

## Alternatives considered

- **No category — treat all tests uniformly.** Rejected because the
  triage drift cost is real and recurring. Surveyor's reviews have
  named four commitments distinct from "what does this code
  compute"; flattening the category loses that signal.
- **Documentation-only convention (CONTRIBUTING.md only, no code
  conventions).** Rejected because docs drift from code:
  convention-encoded-in-test-naming and convention-encoded-in-helper-
  call survives the next refactor; convention-encoded-in-prose
  doesn't. This is the lightest-weight alternative and the closest
  competitor; rejection rests on durability under churn.
- **Build-tag based separation (`//go:build pin`).** Rejected because
  pins should run in every CI pass, not opt-in. The category is a
  semantic distinction, not a runtime distinction.
- **By-test naming only (no file separation).** Rejected because
  grep-by-filename is the cheapest scan; co-locating pins in
  `pin_test.go` makes "show me the discipline surface for this
  package" a one-`ls` operation.
- **By-test counting in the ADR (e.g. "8 pins exist").** Rejected
  per Surveyor's #34 / S4 framing: the right unit of account is
  the **commitment**, not the test. The commitment count is what
  tells you whether discipline-pin is earning its keep across time.
- **External test framework with native pin-class support.**
  Rejected because no such framework exists in the Go ecosystem
  today; standardising on one would be a bet on something
  hypothetical. Recorded as a watch in §What would change the
  decision.

## Consequences

### Cleaner

- **Pin failures get the right triage by default.** The
  `pin_test.go` location + `TestPin_` prefix flag the category at
  every failure; the decision to retract is gated by ADR amendment.
- **Discoverability**: `find . -name pin_test.go` answers "what
  architectural commitments does this codebase pin?" in one command.
- **Commitment register evolves visibly.** Each new commitment slug
  is a recorded decision; the register grows by deliberate ADR
  amendment, not by accidental test addition.
- **Surveyor's cross-project review continuity preserved.** Pins
  emerging from review rounds carry an `// PIN:` marker citing the
  review; future reviewers can trace the commitment to its origin.

### Harder

- **Authoring discipline at write time.** Authors must distinguish
  "this is a discipline pin" from "this is a regression test"
  before they write the file location and function name. Minor
  cognitive cost; the categories are usually obvious.
- **Rename ceremony when commitments shift.** If a commitment slug
  changes (e.g., "WireShape" → "WireContract"), the rename is a
  multi-file pass + ADR amendment. Cost scales with pin count; for
  the current 4 commitments / 8 tests, ~10 lines.
- **One more file per package**. Packages with one pin gain a
  `pin_test.go`. Net file count grows by ~3 (for the current
  layout). Negligible.

## What would change the decision

Reasons to retract or supersede ADR-0001:

- **The category mechanically collapses.** If after some duration
  the `// PIN:` marker, `TestPin_` prefix, and `pin_test.go`
  location all coincide with regression tests by accident, the
  discipline isn't being honored at write time and the category
  isn't functioning. Retract via superseding ADR with the failure
  modes documented.
- **A fifth+ commitment surfaces a structural problem with the
  by-commitment scope.** E.g., a commitment that's necessarily
  cross-package wouldn't fit `pin_test.go`-per-package cleanly.
  Amend ADR-0001 (or supersede) to update the conventions.
- **Tooling absorbs the discipline.** If a future test framework
  natively distinguishes pin-class from regression-class tests
  with built-in triage gates, the manual conventions can retire
  in favor of the framework's. Open watch — no plausible candidate
  in the Go ecosystem today.

## References

- Issue #43 (this ADR's tracking issue + AC list)
- Issue #34 (audit doc — `docs/failure-modes.md` §4.2 sourced
  the eight-tests-four-commitments table verbatim)
- Issue #51 (CI-enforce the slug register — the gate that promotes
  the "deliberate act" framing from convention to mechanical check)
- `docs/failure-modes.md` §4.3 (the conventions section that
  this ADR ratifies)
- `internal/testpin/testpin.go` (the `Triage` helper that wires
  the failure-site triage hint)
- Surveyor review threads carried in the references of
  `docs/failure-modes.md` §6 (the per-commitment pin origins)
- Surveyor #43 structural review (comment 58874) — by-commitment
  scope ratification, (c) strengthenings (c.1)+(c.2), flake-as-(c),
  failure-site triage hint, slug sharpening, alternatives
  completeness

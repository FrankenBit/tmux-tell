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
| **Wire shape is single source-of-truth** (JSON-tag-driven; no manual map construction) | `TestTrackResult_OmitemptyContract`; `TestTrack_WireShape_CLIAndMCPMatch`                                                                                                                                            | Surveyor #28 / #29 reviews                          |
| **Atomic cap enforcement under concurrency** (caps are ceilings, never floors)         | `TestInsertMessage_CapEnforcedUnderConcurrency`                                                                                                                                                                      | Surveyor #29 round-3 review                         |
| **Thread-structure precondition** (`linkP2ToP1` callers don't pass explicit `reply_to`) | `TestInsertMessagePair_LinkP2ToP1_RejectsExplicitReplyTo`                                                                                                                                                            | Surveyor #29 follow-up                              |
| **Never silently guess between canonical-or-alias exact matches**                      | `TestLookupByNameWithCanonicals_Ambiguous` (substring variant); `_ExactMatchAmbiguous_AliasCollision`; `_ExactMatchAmbiguous_AliasIsAnotherCanonical`; `TestPaneAgentNameWithCanonicals_ExactMatchAmbiguous`         | Surveyor v0.2.0 Q(a) + v0.2.1 review                |

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

- **`WireShape`** — wire shape is single source-of-truth.
- **`AtomicCapEnforcement`** — caps are ceilings, never floors.
- **`ThreadStructurePrecondition`** — `linkP2ToP1` callers don't pass
  explicit `reply_to`.
- **`CanonicalResolution`** — never silently guess between
  canonical-or-alias exact matches.

Adding a fifth slug is a deliberate act, not an accidental side
effect of writing another test. The slug should be added to this
ADR (via amendment) at the same time the first pin for the new
commitment lands.

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
     wrong.** Fix the assertion; document why the prior shape
     misspecified. Likely indicates the pin should also be
     strengthened (the regression class slipped through).

The triage step lives in the pre-commit flow as a discussion
artifact (commit message rationale; PR description; or, for
non-trivial cases, an explicit Surveyor consultation). The cost is
small per pin; the value is that a pin's removal is a deliberate
decision rather than a silent deletion.

## Alternatives considered

- **No category — treat all tests uniformly.** Rejected because the
  triage drift cost is real and recurring. Surveyor's reviews have
  named four commitments distinct from "what does this code
  compute"; flattening the category loses that signal.
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
  in favor of the framework's. Likely never happens; recorded
  for completeness.

## References

- Issue #43 (this ADR's tracking issue + AC list)
- Issue #34 (audit doc — `docs/failure-modes.md` §4.2 sourced
  the eight-tests-four-commitments table verbatim)
- `docs/failure-modes.md` §4.3 (the conventions section that
  this ADR ratifies)
- Surveyor review threads carried in the references of
  `docs/failure-modes.md` §6 (the per-commitment pin origins)

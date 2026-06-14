# ADR-0013: Plan-first workflow for size/M+ work

> **Status**: Proposed
> **Date**: 2026-06-14
> **Authors**: operator (ratified 2026-06-14), Bosun (ADR)

## Context

The multi-chamber crew (Engineer, Quartermaster, Pilot, Shipwright,
Herald, Surveyor, Lookout) implements substantive substrate work in
parallel against tmux-tell. Today the dispatch-to-implementation cycle
runs implement-directly: dispatcher hands the chamber an issue, chamber
reads the issue, chamber starts coding, mid-flight pivots happen when
architectural surprises surface during implementation.

A recurring failure mode in v0.17.0's substrate cluster: substantial
mid-flight architectural pivots. Two anchor incidents:

- **#392/#393 trigger-file → dedicated-runner pivot (2026-06-14).**
  Quartermaster reshaped #393 against nimbus's trigger-file pattern per
  the established alcatraz precedent. After ~30 minutes of design work,
  the operator surfaced "would a dedicated host-mode runner solve it?",
  and Quartermaster's evaporation-test analysis confirmed the simpler
  architecture won. The trigger-file design work was preserved as
  substrate-engineering archaeology but didn't ship.
- **#401 settle-delay → two-Enter reframe (2026-06-14).** Engineer's
  initial framing (codex paste failures are settle-timing) had a clear
  immediate-mitigation that turned out wrong; a witnessed sweep
  surfaced the actual root cause (codex needs two Enters for
  collapsed pastes). The reframe added cycles that catching-earlier
  could have avoided.

Nimbus historically ran a plan-first workflow (chamber composes plan →
review → APPROVED → implementation). The operator's recollection:
optional by convention, default-on for issues above size/S. The
workflow's value was catching architectural disagreements before code
was written, when the cost of revising the design was low.

The substrate is ready to support the same pattern. Bus messaging
(tmux-tell), filesystem-local file access (chambers share alcatraz),
issue commenting (Forgejo) — all in place.

## Decision

**Plan-first workflow for size/M+ work, dispatcher-triggered, with
plans composed at `/tmp/tmux-tell-plans/<N>-<title>.md` during the
cycle and archived as issue comments at implementation completion.**

Ratified by the operator 2026-06-14.

### When plan-first fires

- **Default-on for size/M+ work** (size/M, size/L, size/XL). The
  dispatcher signals "plan-first" in the dispatch message; chamber
  composes plan before implementing.
- **Default-off for size/S work** and below. Bug fixes with clear root
  cause, AC-tick passes, mechanical changes, CHANGELOG-only edits, and
  similar implement-directly without a plan phase.
- **Both sides can override.** Dispatcher signals plan-first on a
  size/S architectural choice; chamber announces "scope clearer than
  expected, skipping plan-first" on a size/M with no design
  ambiguity. Overrides are surfaced explicitly so reviewers can
  redirect.

### Plan location

- **`/tmp/tmux-tell-plans/<issue-N>-<short-title>.md`** during the
  cycle. Chamber composes the plan there.
- All chambers run as `alex` on alcatraz; same `/tmp` namespace
  permits filesystem-local read by reviewers.
- Ephemeral by design — `/tmp` clears on reboot. The discipline pin
  "plan must be on the issue before in-progress plan can be lost"
  forces archival of finalized plans before reboot risk accumulates.

### Plan content shape

Every plan file opens with a **stable metadata header** for grep + UI
discovery:

```
# Plan: <issue title>

> Issue: <issue-N> · Status: Draft | InReview | Approved | Superseded
> Chamber: <name> · Created: YYYY-MM-DD · Last revised: YYYY-MM-DD
```

Body sections:

- **Context** — what's the issue, what constraints apply
- **Approach** — proposed implementation direction in enough detail to
  reveal architectural decisions
- **Design decisions** — explicit choices, with the alternatives
  considered + rationale
- **Out of scope** — explicit deferrals to keep scope honest
- **ACs** — proposed acceptance criteria the implementation will close

Not a complete spec. The level of detail is "decisions a future reader
would want to know to understand why the implementation is the way it
is." Roughly mirrors the ADR template at one issue-tier down.

### Review cycle

1. Chamber **atomically creates** the plan file at the `/tmp` location.
   Use `set -C` (noclobber) or equivalent so creation **fails loud** if
   `/tmp/tmux-tell-plans/<N>-<title>.md` already exists — prevents
   silent overwrite of an in-flight plan from a parallel chamber or
   stale prior session.
2. Chamber bus-pings Surveyor + Bosun with the file path
   (`/tmp/tmux-tell-plans/<N>-<title>.md`).
3. Reviewers read the file filesystem-local. Lookout — when actively
   reviewing per the parallel-review protocol — reads the same file.
4. Reviewers post review verdict + observations via bus message:
   APPROVED, REQUEST_CHANGES, or COMMENT.
5. Plan iterations: chamber edits file in place (flips the metadata
   header's `Last revised` date + `Status` if the state changed);
   reviewers re-read + re-stamp.
6. **APPROVED → chamber archives the APPROVED plan to the work
   issue immediately, then starts implementation.** Posting the
   approved plan to the issue at this moment closes the `/tmp` loss
   window — even if reboot or cleanup wipes the file, the
   approved plan is durable on the issue. Archive comment header:

   ```
   ## Plan archive — APPROVED at <YYYY-MM-DD HH:MM>, implementation starting
   ```

7. Subsequent commits land on a normal feature branch; PR review runs
   against the implementation per the existing protocol
   (substrate-state-level by Surveyor, containment-strength by
   Lookout, code-level by Bosun).
8. **At PR merge or implementation completion**, chamber posts a
   second comment on the issue noting completion (or, if the plan
   was superseded mid-implementation, a revised-plan comment with the
   supersession context). The /tmp file may then be deleted (or left
   to natural `/tmp` cleanup).

### Plan supersession

When implementation surfaces evidence that the plan was wrong, the
chamber edits the plan file in place (status flips to `Superseded`
in the metadata header) AND posts a supersession comment on the
issue. The supersession comment names what changed, why, and which
prior comment it supersedes. Format:

```
## Plan supersession — <reason>

Supersedes the APPROVED plan archived at <prior-comment-link>.

<new plan content or revised sections, with the "what changed" diff>
```

If a plan is substantively superseded mid-cycle (e.g., the
#392 trigger-file → dedicated-runner pivot), the chamber announces the
supersession on the bus, edits the plan to the new direction, posts
the supersession comment on the issue, and the review cycle re-runs
against the revised plan. The substrate-archaeology on the issue
captures the full evolution.

## Alternatives considered

- **Plan as issue comment** — operator-pushed-back: plan is
  implementation-level, belongs near code substrate, not issue
  substrate. Issue stays focused on WHAT; plan-substrate captures HOW.
- **Plan as repo file in `docs/plans/<N>.md`** — rejected: accumulates
  data pressure in the repo (rough estimate ~2-3 MB/year of markdown);
  requires ceremony commits (empty or cleanup) that clutter history.
  The repo-as-plan-substrate trade-off swung against repo growth and
  ceremony cost.
- **Plan as Forgejo wiki page** — rejected: review semantics on
  wiki pages are weaker than PR-substrate review; cross-chamber
  filesystem-local-read mechanism is simpler.
- **No plan step (status quo, implement-directly)** — rejected for
  size/M+ work: the cost of mid-flight architectural pivots
  substantively exceeds the cost of plan-cycle for the work that
  actually warrants it.

## Consequences

**Cleaner.** Architectural disagreements surface before code commits.
Reviewers engage the design at the cheapest possible moment. Operator
gets a checkpoint to redirect before chamber sinks substantive work
into the wrong direction. The chamber-throughput-vs-operator-
availability discipline still applies — the operator can review plans
at their cadence; chambers wait at the APPROVED gate per the existing
park-cleanly pattern.

**Cost.** Dispatcher gains a decision (plan-first vs implement-directly)
per dispatch. Reviewers gain an extra review surface (plans, distinct
from PRs). `/tmp/tmux-tell-plans/` accumulates files that need
archival-to-issue discipline. The convention is alcatraz-local —
filesystem-local-read assumes all chambers share `/tmp`.

**Discipline pins this commits to:**

- **"Approved plan on issue before implementation starts"** — archive at
  APPROVED-time, not only at completion. Closes the `/tmp` loss
  window: even if reboot or cleanup wipes the file, the
  approved plan is durable on the issue from the moment chamber
  starts implementing.
- **"Atomic create, fail-loud on existing"** — use `set -C` or
  equivalent when creating the plan file. Prevents silent overwrite
  of an in-flight plan from a parallel chamber or stale prior session.
- **"Stale-file triage, not delete-by-age"** — periodic walks (e.g.,
  weekly) over `/tmp/tmux-tell-plans/` surface files that need
  attention. Each surfaced file is **triaged manually**: archive to
  issue if it's a finalized plan that didn't get archived, rescue via
  bus-ping to the chamber if the work is still in-flight, or delete
  with explicit rationale. Never bulk-delete by age — `mtime` is a
  signal to investigate, not authority to remove.
- **"Plan supersession leaves no git trail; substrate-archaeology is
  on the issue"** — accept this trade-off; the APPROVED comment +
  any supersession comments on the issue capture the evolution;
  iteration history within a single plan-state lives in bus messages
  between reviewers + chamber.

## What would change the decision

- **Multi-machine development.** The filesystem-local-read mechanism
  assumes all chambers share `/tmp` on alcatraz. If chambers ever run
  on different machines (operator-fork, cloud-deployed crew,
  external-contributor plans), plans need a substrate that doesn't
  require shared filesystem. Likely candidates at that point: repo-
  based (Option B from the design discussion), BookStack pages, or
  dedicated planning substrate (Notion, etc.).
- **Forgejo gains line-level comment on issue-comments.** Currently
  Forgejo issue comments don't support inline file-line review. If
  that surfaces (or another tool offers it), the filesystem-file plan
  + issue-archive shape could compose with richer review affordances.

## References

- ADR-0001 — discipline-pins-as-test-category framing this convention
  inherits
- v0.17.0 substrate cluster (2026-06-14) — anchor incidents above
  (#392/#393 trigger-file pivot, #401 reframe)
- Nimbus historical plan-first workflow — operator-recall framing
- Bosun-side memory pins:
  - `feedback_plan_first_workflow.md` — operational shape for
    dispatcher decisions
  - `feedback_plan_archive_at_completion.md` — `/tmp` location +
    issue-archival discipline
- `feedback_chamber_throughput_vs_operator_availability.md` — the
  ambient discipline this composes with at the operator-availability
  gate
- `feedback_register_separation` (Surveyor's banking) — the
  multi-reviewer-tier pattern this composes with at the plan-review
  surface

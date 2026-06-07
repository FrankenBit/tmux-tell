# ADR-0008: Deprecation policy — two-minor-cycle floor (post-1.0)

> **Status**: Accepted
> **Date**: 2026-06-07
> **Authors**: operator (ratified 2026-06-07), Herald (ADR)

## Context

The CHANGELOG's 1.0-trigger notes require, before the 1.0 cut, **a
committed deprecation policy** — post-1.0 breaks must go through a
deprecation cycle rather than landing abruptly. It is one of three
explicit 1.0 blockers in the **Sea-trials** milestone. Until now no
such policy existed: the pre-1.0 cadence notes (CHANGELOG) govern
`0.x` minor/patch semantics but say nothing about post-1.0 grace.

Two things make this load-bearing rather than ceremonial:

- **ADR-0007 (coexist)** committed tmux-msg's exported Go API and DB
  schema as an **external contract** Binnacle (and any third-party
  consumer) relies on. A deprecation grace period is the teeth behind
  that contract.
- The audience is no longer just the maintainer — it's operators,
  future contributors, and speculative public users. The policy
  signals predictability to all of them.

## Decision

**Moderate-with-floor: at least two minor release cycles between a
deprecation announcement and removal, with explicit discretion to
extend.** Ratified by the operator 2026-06-07.

- **Floor commitment (post-1.0).** Deprecated public surfaces remain
  functional for **at least two minor release cycles** before removal
  in a subsequent minor. Concretely: deprecate in `v1.X`, earliest
  removal `v1.X+2`.
- **Discretion clause.** Maintainers may extend the deprecation period
  at their discretion for high-impact changes. The floor is a
  guarantee, not a ceiling.
- **Runtime warning.** A deprecated surface emits
  `WARN deprecated_surface_used name=<surface> removal=<v1.X+2>` when
  invoked — distinguishable and greppable, distinct from other warnings.
- **CHANGELOG.** Every deprecation announcement gets a
  `[Unreleased] ### Deprecated` entry (Keep a Changelog convention)
  naming the surface, the earliest-removal version, and the
  forward-compatible replacement — or an explicit "no migration; this
  surface goes away."
- **JSON `deprecated: true`.** Set on MCP tool schemas and CLI
  `--format json` shapes where the surface is consumed programmatically.
- **Pre-1.0 (current state).** Retains semver-explicit looseness —
  minor bumps may carry breaks, always called out in the CHANGELOG.
  The two-cycle policy applies starting at **v1.0**.

**Surfaces covered:** MCP tool schemas; CLI subcommand args / flags /
exit codes; `--format json` shapes; DB schema columns + the message /
agent **state vocabulary**; and the public Go API for the `discover` /
`store` / `tmuxio` packages (and any `pkg/` extracted per ADR-0007).

**Removal mechanics.** The surface is deleted from source at the
earliest-removal version; through the pre-removal cycles it keeps
functioning and emits the WARN. Any announcement must name a
forward-compatible replacement or explicitly state there is none.

## Alternatives considered

- **One-minor "cut rotting ropes" cycle.** Rejected: under a fast
  release cadence a single minor cycle can be only weeks — too little
  grace for the external-contract audience to migrate.
- **Exactly-two (a ceiling, not a floor).** Rejected in favor of "at
  least two": the floor locks the user-facing guarantee while leaving
  the maintainer room to extend for high-impact changes.
- **No policy / ad-hoc per-break judgment.** Rejected: the 1.0 trigger
  explicitly requires a committed policy, and ad-hoc removal defeats
  the predictability ADR-0007's contract promises downstream.

## Consequences

**Cleaner.** Downstream gets a predictable migration window —
Binnacle's `go.mod` pin cadence stays sane across the K=3 stability
tracker (#163), and the external contract (ADR-0007 / CONTRIBUTING.md)
has a concrete, enforceable grace.

**Cost.** Post-1.0, every deprecation carries a real obligation before
removal: keep the surface functional for two minors, emit the WARN,
set the JSON `deprecated: true` flag, and file the CHANGELOG
`### Deprecated` entry. Removal can no longer be impulsive. The
WARN-log and JSON-flag *implementation* for each surface is follow-up
work (separate issues), not part of this ADR.

## Worked example

**`claude-msg` → `tmux-msg-claude` (#177)** is the first concrete
beneficiary. The rename is pre-1.0 (where a hard break is permitted),
but it adopts the policy's cycle as the inaugural worked example —
dogfooding the commitment before 1.0:

1. Ship `tmux-msg-claude` as the canonical binary; keep `claude-msg`
   as a functioning **alias**.
2. Announce in CHANGELOG `### Deprecated`: *"`claude-msg` binary name —
   replaced by `tmux-msg-claude`; earliest removal two minors out."*
3. The alias emits `WARN deprecated_surface_used name=claude-msg
   removal=<v1.X+2>` on each invocation.
4. Remove the alias no earlier than two minor cycles later.

A second beneficiary, **#140** (renaming the `delivered_unverified`
classifier), exercises the same cycle on the **state-vocabulary**
surface: the old classifier value stays emitted + documented, with the
replacement named, until the two-cycle floor elapses.

## What would change the decision

- If the release cadence slows enough that two minors is an
  unreasonably long grace (or speeds up enough that it's too short),
  revisit the floor via a superseding ADR.
- If a downstream consumer needs a stronger guarantee (e.g. an LTS
  surface), extend rather than retract — the floor framing already
  permits that.

## References

- #162 — this ADR's tracking issue (operator ratification 2026-06-07)
- `CHANGELOG.md` — the 1.0-trigger criteria + the pre-1.0 cadence notes
- ADR-0007 — Binnacle coexist; the external contract this policy protects
- CONTRIBUTING.md — states the stability commitment this ADR governs
- #163 — K=3 release-stability tracker (sibling Sea-trials work)
- #177 / #140 — first concrete beneficiaries (worked examples above)
- Keep a Changelog `### Deprecated`:
  <https://keepachangelog.com/en/1.1.0/>

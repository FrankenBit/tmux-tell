# ADR-0008: Deprecation policy — two-minor-cycle floor (post-1.0)

> **Status**: Accepted (amended 2026-06-08 — see Amendment A + Amendment B)
> **Date**: 2026-06-07
> **Authors**: operator (ratified 2026-06-07), Herald (ADR), Quartermaster (amendments)

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

## Amendment A — 2026-06-08 (K-counter interaction)

The original Decision section (above) says **what** the deprecation policy is
but leaves implicit **how** deprecation/removal interact with the
K-counter (#163) — the consecutive-non-breaking-releases tracker that gates
the road to 1.0. The interaction was settled by the operator on 2026-06-07
during the v0.9.0 cut conversation (the `claude-msg → tmux-msg-claude`
rename arc, #177); recording it here now so the policy doc itself, not just
the README + tracker, is the source of truth.

### Reading B — deprecation-with-functioning-alias preserves K

- **Deprecation announcement** that ships with a functioning alias (the
  surface above) does **NOT** reset the K-counter. Existing operator config
  keeps working at the cutover — every old invocation, env var, JSON shape,
  CLI flag emits the WARN but does not break. The release is a clean cut for
  K-counter purposes, K increments.
- **Removal** of a deprecated surface (alias goes away) **DOES** reset the
  K-counter. That's the moment existing config stops working — by the
  definition of a public-surface break.

### Why this reading (operator-impact alignment)

The K-counter measures **operator-impact breaks**, not policy-change
announcements. A deprecation announcement with a functioning alias is *policy
hygiene*, not operator-impacting breakage. The alternative reading (Reading A:
any deprecation resets K) would punish responsible policy execution — the
fallback would be *not* deprecating and dropping straight to removal, which is
strictly worse for operators yet would reset K identically. Aligning the
counter with what operators actually feel (does existing config still work?)
is the substrate-honest framing.

### Worked example — the v0.9.0 → v0.11.0 trajectory

The `#177` rename arc (claude-msg → tmux-msg-claude + CLAUDE_AGENT_NAME →
TMUX_AGENT_NAME + claude-mailman@ template rename), shipped in **v0.9.0**:

- **v0.9.0** (2026-06-07) — *deprecate with functioning aliases.* Old names
  still work; new names canonical. Each old invocation emits the WARN. **K
  preserved → K=3 (Sea-trials gate clears).**
- **v0.10.0** (2026-06-08, this cut) — *still under aliases.* Old names
  continue to work, WARN still emitted. No new deprecation, no removal. **K
  preserved → K=4.**
- **v0.11.0** (earliest) — *remove aliases.* `claude-msg` is no longer on
  disk; `CLAUDE_AGENT_NAME` no longer resolves identity; `claude-mailman@`
  template is gone. Existing operator config that hadn't migrated now breaks.
  **K resets → K=0.**

The two-minor floor (v0.9.0 → v0.11.0 earliest removal) and the K-counter
interaction compose cleanly: every announcement is K-preserving (provided the
alias machinery is honored), every removal is the next deliberate K-reset.

### Out-of-scope consequences (deferred to follow-ups)

- **Structured `### Deprecated` CHANGELOG format + derive-script.** The
  per-release "what's eligible for removal at v<X>" view is filed at #209
  (deprecation bookkeeping, Option C hybrid). Each `### Deprecated` entry
  carries a "Deprecated in v<X>; earliest removal v<Y>" line that the script
  parses to drive the eligibility view. *(Landed via §Amendment B,
  2026-06-08.)*
- **Cap-vs-keep-raising K-counter past gate.** Operator decided 2026-06-07:
  K keeps raising past 3 (no cap), retires at 1.0. Recorded in #163 but
  beyond the scope of this addendum.

## Amendment B — 2026-06-08 (structured `### Deprecated` CHANGELOG format)

The Decision section requires every deprecation announcement to land as a
`[Unreleased] ### Deprecated` entry naming the surface, the earliest-removal
version, and the replacement. As deprecations stack across releases — feature
A deprecated in v0.9.0 (earliest removal v0.11.0), feature B in v0.10.0
(earliest removal v0.12.0), etc. — the question "*which surfaces are eligible
for removal at v<X.Y.Z>?*" becomes a grep-and-reason exercise that scales
poorly and is silently easy to miss.

This amendment codifies a machine-parseable shape for each `### Deprecated`
entry so a thin derive-script can produce the removal-eligibility view per
cut. The CHANGELOG remains the **single source of truth** — no separate
registry. Ratified by the operator 2026-06-07 (Option C hybrid: CHANGELOG-
as-SoT + derive-script).

### Structured entry format

Each `### Deprecated` entry has TWO structured lines (the rest is free-form
prose):

1. **Title line.** A markdown bullet whose bold opening names the deprecated
   surface(s) in backticks, the replacement, and the issue ref:

       - **`<surface-name>` — replaced by `<replacement>` (#<issue>).**

2. **Version-pin line.** Immediately after the title line, on its own line,
   indented to nest under the bullet:

       Deprecated in v<X.Y.Z>; earliest removal v<A.B.C>.

Following lines are free-form (rationale, migration prose, sub-surface
enumeration). The derive-script greps for the two structured lines; the
free-form body is human-only.

### Worked example (canonical)

    ### Deprecated

    - **`claude-msg` binary name + `claude-mailman@` systemd template —
      replaced by `tmux-msg-claude` / `tmux-msg-claude-mailman@` (#177).**
      Deprecated in v0.9.0; earliest removal v0.11.0.

      For the cycle, `install.sh` installs a `claude-msg → tmux-msg-claude`
      binary symlink … (rationale + migration prose continues)

The pre-amendment v0.9.0 and v0.10.0 entries (`claude-msg` /
`$CLAUDE_AGENT_NAME` / `delivered_unverified` family) carry a
near-equivalent shape — title line + an "Earliest removal v<X.Y.Z>" pin
embedded in body prose — that the derive-script handles permissively as
legacy-conforming entries (surfaces with a soft warning). Going forward,
new entries should follow the canonical shape above.

### Derive-script — `scripts/deprecations.sh`

A bash script consuming `CHANGELOG.md` and producing the removal-
eligibility view per release. Modes:

- `--for v<X.Y.Z>` — list surfaces whose earliest_removal ≤ v<X.Y.Z>
  (cleared for removal at that cut); split into **cleared** / **not-yet-
  eligible** / **legacy-needs-eyeballing**.
- `--all` — full table (section × deprecated-in × earliest-removal ×
  surface) across the whole CHANGELOG.

The script is permissive: entries that don't fit the canonical shape fall
back to section-header inference (deprecated-in = section version) or
surface as legacy entries needing manual confirmation, rather than being
silently dropped.

### Release-cut runbook step

Before cutting v<X.Y.Z>, the runbook adds a step (in `CONTRIBUTING.md`
§Release cuts):

> Run `./scripts/deprecations.sh --for v<X.Y.Z>` and confirm the cleared
> list matches intent. If a listed surface is NOT being removed this cut,
> document the extension reason in the cut PR — the floor is a guarantee,
> not a ceiling (per Decision §Discretion clause).

### Out of scope (deferred to follow-ups)

- **CI auto-fail on un-removed eligible surfaces at a release-cut PR.**
  Could be a future hardening; not load-bearing for this amendment.
- **Separate registry file** (`docs/deprecations.yaml` or equivalent) —
  explicitly rejected in favor of CHANGELOG-as-single-source-of-truth.
- **Parsing `### Removed` blocks to subtract already-removed surfaces** from
  `--for` output. The script reports eligibility from `### Deprecated`
  entries only; the operator manually cross-references against past
  `### Removed` blocks. Could be a future improvement once the deprecation
  list grows enough that manual cross-reference becomes load-bearing.

### References

- #209 — this amendment's tracking issue
- `scripts/deprecations.sh` — the derive-script
- `CONTRIBUTING.md` §Release cuts — the runbook step
- Keep a Changelog `### Deprecated`: <https://keepachangelog.com/en/1.1.0/>

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

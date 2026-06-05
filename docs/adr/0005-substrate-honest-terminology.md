# ADR-0005: Substrate-honest terminology (chamber → ?)

> **Status**: Proposed
> **Date**: 2026-06-05 (proposed)
> **Authors**: Quartermaster (author), operator (surfaced the
> trigger nit on PR #113 + terminology candidate `session`),
> ADR-0003 (the principle this ADR applies), ADR-0004 (sets the
> generalized parent-ADR-stays-frozen precedent this ADR inherits)

## Context

ADR-0003 established the substrate-vs-flavor architectural
commitment. ADR-0004 applied that commitment to the MCP wire
surface. This ADR applies it to a third surface: the **terminology**
the substrate uses to describe its own primitives in code
identifiers, MCP method names, and doc prose.

The current substrate uses **`chamber`** to refer to a per-pane
agent unit (the CLI tool process running in a tmux pane, observed
through its rendered chrome). `chamber` is not part of tmux's own
vocabulary — tmux talks about `session`, `window`, `pane`. The
term came in from project-local Binnacle / Nimbus lineage, where
each operator-side Claude instance running in a tmux pane is
colloquially called a "chamber."

Under ADR-0003's framing, the substrate's vocabulary should be
**substrate-neutral** — names that any consumer (Claude Code,
Codex, Copilot) would read consistently, anchored in the substrate
the implementation actually uses (tmux + the CLI tool process
running in a pane).

### Pattern promotion

ADR-0004 §Context flagged "ADR-0003 application" as a pattern
candidate under anchor-before-pin discipline: ADR-0004 was
instance-1 (MCP wire surface). ADR-0005 surfaces the same shape on
a structurally distinct surface (terminology vs wire-level
naming; different cutover mechanics; different blast radius). The
two instances together justify promoting the pattern from
anchor-before-pin to a **named pattern**:

**"ADR-0003 application" ADRs apply a parent ADR's framing to a
specific surface.** Each application ADR makes its own
sub-decisions (the parent provides the principle; the application
provides the per-surface choices); each inherits the parent-frozen
precedent (per ADR-0004 §Precedent, generalized); each can promote
the pattern by adding a §Pattern subsection referencing the named
pattern rather than re-anchoring it. Future application ADRs cite
this section instead of repeating the framing.

### Inherited precedent from ADR-0004

Per ADR-0004 §Precedent (generalized to any parent ADR), ADR-0005
does NOT amend ADR-0003 or ADR-0004 prose. Both stay
accurate-to-time. Their in-text references to `chamber` (and
ADR-0004's references to `.mcp.json` per-chamber namespacing, etc.)
remain as written; current voice on terminology lives here in
ADR-0005.

## Decision

Four sub-decisions, one with an open review-surface question:

### (1) Terminology choice: TBD via framing review

The operator surfaced `session` as the candidate on PR #113. The
ADR-grade exploration surfaces a real tension worth resolving at
the framing review rather than codifying without consideration:

- **`session`** (operator's pick). Matches tmux's own vocabulary
  for the long-running shell context. Generic enough to read
  consistently across CLI flavors. **Concern**: dual meaning —
  "session" can mean the tmux session OR a Claude Code conversation
  session. The substrate's current `ChamberState` describes the
  state of a per-pane agent, not the state of a tmux session (a
  tmux session can host multiple panes; each pane hosts one
  agent). Renaming to `SessionState` carries a small semantic
  drift: the type's scope is per-pane-agent, but the name suggests
  per-tmux-session.

- **`agent`** (alternative worth weighing). The substrate's
  existing identifier vocabulary already uses `agent`: the
  `agents` SQL table, the `--agent` CLI flag, the mailman service
  template `claude-mailman@<agent>.service`. Renaming `ChamberState`
  to `AgentState` keeps the type's scope (per-agent state) and the
  identifier (`agent`) in sync. `agent.X` MCP methods compose
  naturally too — though the methods aren't being renamed at the
  verb level (those are post-ADR-0004 `tmux-msg.<verb>`).

- **`pane`**. Names where the state is observed. Rejected at
  decision time: leaks substrate internals (the pane is HOW state
  is observed via `capture-pane`; not WHAT the state semantically
  describes). Consumers shouldn't have to know the substrate
  observes via tmux panes to read the type's purpose.

- **`instance`** / **`endpoint`**. Too generic; could mean any
  process or any network surface. Rejected.

- **Keep `chamber`**. Rejected per the substrate-honest principle:
  project-local jargon that doesn't generalize across CLI flavors.

**Review-surface question for operator**: between `session` and
`agent`, which carries the substrate's per-pane-agent unit better?
My slight lean is `agent` (consistency with existing `agents`
table + no dual-meaning), but the operator's `session` pick is
reasonable and may carry a framing I haven't considered. ADR-0005
defers this until framing review; the rest of the decisions assume
`<term>` as a placeholder.

### (2) Substrate scope

The rename covers:

- **Go code identifiers**: `ChamberState` (the enum type in
  `internal/tmuxio/state.go`), `chamber_state` (variable names),
  `chamber-state` (kebab-case in CLI surface), `chamberState`
  (camelCase), and all derived forms across `cmd/claude-msg/` and
  `internal/`. Issue #107 was filed pre-ADR for this sweep; this
  ADR provides its upstream architectural rationale.

- **MCP tool method name**: `chamber_state` → `<term>_state`.
  Post-ADR-0004 sweep this is rendered as
  `tmux-msg.<term>_state` (was `semaphore.chamber_state`).
  Bundled in the same Claude Code restart cycle as ADR-0004's
  MCP-wire-surface implementation (one cutover instead of two).

- **DB column names** (if any contain `chamber`): swept via
  schema migration in the implementation PR.

- **Doc prose**: `README.md`, `docs/`, in-code comments, help
  text. Post-ADR-0005 prose uses `<term>` consistently.

- **Future ADR prose**: written with `<term>` from inception. New
  ADRs starting at ADR-0006 use `<term>`.

The rename explicitly **does not** cover:

- **ADR-0001 through ADR-0004 prose**. Per the inherited parent-
  frozen precedent: substantive content of prior ADRs stays
  accurate-to-time. Index metadata (`docs/adr/README.md` index
  table) stays live and may update for supersession markers etc.

- **Chamber-level CLAUDE.md files in `frankenbit/alcatraz-infra`**.
  The chamber-level CLAUDE.md files (Bosun, Quartermaster,
  Surveyor, Engineer, Pilot, Herald) are project-local operator
  configuration; "chamber" is the operational name for these
  Claude instances in the project's own lexicon. Substrate-honest
  naming extends to the substrate's vocabulary, not to project-
  local naming for instances of consumers.

- **Operator memory entries**. Same reasoning — project-local
  naming for consumers, downstream of the substrate.

- **Binnacle / Nimbus repos**. Separate projects with their own
  identifier vocabularies. Tracked as Bosun follow-up alongside
  the cli-semaphore → tmux-msg rename (see #97-out-of-scope).

### (3) Inheritance of ADR-0004's parent-frozen precedent

ADR-0005 explicitly inherits ADR-0004's generalized §Precedent:
application ADRs do not amend their parent ADRs. ADR-0003 prose
references to `chamber` (e.g., line 87's `semaphore.control`
example uses the pre-ADR-0004 wire name; ADR-0003's per-chamber
namespacing examples use `chamber`) stay frozen. ADR-0004 prose
references to `chamber` (e.g., decision (1)'s per-chamber
namespacing constraint) stay frozen.

The current voice for terminology lives in ADR-0005 going forward.
A reader following any prior ADR's chamber-references finds the
current voice here; the cost is the one extra reading hop named
by ADR-0004's §Precedent.

### (4) Migration: bundled hard cutover with ADR-0004 implementation

ADR-0004's MCP wire-surface implementation PR (task #197) is the
upcoming mechanical PR. ADR-0005's terminology rename **bundles
into the same implementation PR + same operational cutover**:

- One Claude Code restart per chamber (operator-side, per
  ADR-0004 decision 4 sequencing), not two.
- One v0.6.0 release cut, not two.
- One CHANGELOG entry covering both rename surfaces (with clear
  attribution: MCP wire surface per ADR-0004, terminology per
  ADR-0005).
- One operational window of MCP-bus-quiet (~5 minutes) across all
  six chambers.

Bundling is appropriate because both surfaces are substrate-honest
naming sweeps that hit the same set of files (Go source, MCP
registrations, control commands, doc prose), require the same
operational sequence (binary install + chamber `.mcp.json` updates
+ Claude Code restarts), and have the same blast radius (every
chamber's MCP tool calls fail until restart). Splitting them would
duplicate the operational cost without separating the architectural
concerns — the ADRs already separate the principle from the
application from the implementation.

## Alternatives considered

### Terminology

(Surfaced in §Decision (1) and reproduced for the §Alternatives
catalog. The choice between `session` and `agent` is the review-
surface question; the rejected candidates are below for
completeness.)

- **`session`** — see §Decision (1).
- **`agent`** — see §Decision (1).
- **`pane`** — rejected: leaks substrate internals (observation
  mechanism, not semantic meaning).
- **`instance`** — rejected: too generic to carry substrate
  context; could mean any process or service.
- **`endpoint`** — rejected: networking connotation conflicts
  with the substrate's process-in-pane reality.
- **`client`** — rejected: bidirectional substrate (agents both
  send and receive); "client" implies asymmetric server/client.
- **Keep `chamber`** — rejected: project-local jargon that doesn't
  generalize. The whole point of substrate-honest naming is to
  use terms readable by any consumer.

### Substrate scope

- **Maximal sweep (include ADR prose + chamber-level CLAUDE.md +
  operator memory + Binnacle)**. Rejected: ADR prose is frozen per
  inherited precedent; project-local naming is out of substrate
  scope; Binnacle is a separate project.
- **Minimal sweep (Go code only, leave MCP method name)**.
  Rejected: the MCP method name is the wire-visible terminology
  that consumers see in tool calls; leaving it as `chamber_state`
  while renaming the code identifier creates a permanent code-vs-
  wire-name divergence.
- **Chosen: substrate-runtime + wire surface + doc prose; exclude
  frozen-history surfaces** — see §Decision (2).

### Migration shape

- **Separate ADR-0005 implementation PR after ADR-0004's lands**.
  Rejected: doubles the operational cost (two restart cycles, two
  release cuts) without separating the architectural concerns.
  The two ADRs are sequentially decided; the implementations
  share files, sequence, and blast radius.
- **Alias period** (`chamber_state` + `<term>_state` MCP methods
  both registered for one minor version). Rejected per the same
  reasoning as ADR-0004 §Migration: alias-period machinery's
  carry cost outweighs the convenience at single-tmux-instance
  scope.
- **Chosen: bundled hard cutover with ADR-0004's implementation
  PR + same v0.6.0 cut** — see §Decision (4).

## Consequences

### Cleaner

- **Substrate vocabulary aligns with the substrate's substrate.**
  tmux's own terms (or close kin) replace project-local jargon.
  Readers from outside the project (or from the future when
  Binnacle / Nimbus context has faded) see substrate-honest
  terminology in code, wire surface, and docs.
- **The "ADR-0003 application" pattern is now named.** Future
  application ADRs cite the pattern; the anchor-before-pin
  observation in ADR-0004 graduates to a named-pattern reference.
- **Substrate identifier coherence.** If `agent` is chosen,
  `AgentState` matches the existing `agents` table + `--agent`
  flag + mailman service template. If `session` is chosen, the
  type's scope is slightly broader than its tmux-strict meaning
  but maintains tmux-vocabulary alignment.
- **One Claude Code restart cycle, not two.** Bundling with
  ADR-0004's implementation halves the operational window cost.

### Harder

- **Operational migration window is the same as ADR-0004's** (~5
  minutes of MCP-bus-quiet) but the rename pass is larger (~300+
  `chamber` mentions across code + tests + docs vs. ~110 for the
  MCP wire surface alone). Implementation PR is larger but the
  cutover blast radius is identical.
- **Existing memory entries citing `ChamberState`,
  `chamber_state`, etc.** remain accurate-to-time. Forward-tracking:
  post-cutover entries use `<term>`; pre-cutover entries are
  preserved per the same forward-tracking discipline ADR-0004
  established.
- **Project-local lexicon (CLAUDE.md, memory) diverges from
  substrate lexicon.** Chambers are still called "chambers" in
  operator-side files (Bosun, Quartermaster, etc.); the substrate
  refers to `<term>`s. A short bridge note in chamber-level
  CLAUDE.md files (operator-side) can disambiguate: "Chambers
  (project lexicon) = agent-sessions / agents (substrate lexicon)."
- **Type-name change requires updating any consumer code** that
  imports the type. Within `tmux-msg` itself, the sweep is
  internal. Downstream consumers (Binnacle's substrate bindings)
  carry the rename as part of the deferred-to-Bosun follow-up.

## What would change the decision

Reasons to retract or supersede ADR-0005:

- **Substrate-honest naming proves wrong on `<term>`-as-chosen.**
  If `session` is chosen and operational experience reveals the
  dual-meaning ambiguity is friction-causing in practice (e.g.,
  every `SessionState` reference needs disambiguation prose), the
  amendment trigger is structurally-distinct instance-2 of
  "SessionState was ambiguous here" — mirroring ADR-0003 / ADR-0004
  amendment-trigger framing. Same retraction-via-supersession
  shape per ADR-0004 §Generality.
- **A sibling CLI flavor needs a different terminology.** Unlikely
  but possible: if Codex's lexicon or Copilot's lexicon resists
  the chosen term, the substrate-vs-flavor cut might need to
  surface here too. Today's expectation is that terminology
  translates; if it doesn't, ADR-0005 amends via supersession.
- **The substrate's substrate changes.** If `tmux-msg` ever moves
  off tmux (e.g., to a different multiplexer like zellij or a
  native CLI-tool IPC), the "tmux-honest" half of substrate-honest
  naming retires. The chosen term may or may not need to follow
  the substrate change. ADR-0005 retracts via supersession at
  that point.

The watch: same shape as ADR-0004's prefix-friction watch — track
the first ~one month post-cutover for terminology-friction
complaints. If no structurally-distinct issues surface in that
window, the chosen term is durable.

## References

- ADR-0003 (`docs/adr/0003-substrate-vs-flavor-naming.md`) — the
  parent principle this ADR applies (third application surface)
- ADR-0004 (`docs/adr/0004-mcp-wire-surface-naming.md`) — the
  prior application; established the generalized parent-frozen
  precedent ADR-0005 inherits; introduced the "ADR-0003
  application" pattern under anchor-before-pin which ADR-0005
  promotes to named-pattern
- #107 — pre-existing tracker for `ChamberState → SessionState`
  Go identifier rename; ADR-0005 provides the upstream
  architectural rationale (#107 becomes the implementation arm
  bundled into ADR-0004's #197 mechanical PR)
- #114 — this ADR's tracking issue
- README.md §Substrate vs CLI-tool-flavor — current voice of the
  framing ADR-0005 applies to the terminology layer
- Operator's 2026-06-05 nit on PR #113 — the trigger

# ADR-0005: Substrate-honest terminology (chamber → agent)

> **Status**: Accepted
> **Date**: 2026-06-05 (proposed); 2026-06-05 (accepted on operator
> + Surveyor sign-off after a substantive cross-current on `session`
> vs `agent` resolved in favor of `agent`)
> **Authors**: Quartermaster (author), operator (surfaced the
> trigger nit on PR #113; initially leaned `session`; the
> wheel-reinvention check on the operator-shell-participant scenario
> resolved to `agent`), Surveyor (framing review with independent
> `agent` lean — three-reason rationale baked into §Decision (1);
> round-2 cross-current deepening on supertype-vs-rename architectural
> commitments shaped the wheel-reinvention check), ADR-0003 (the
> principle this ADR applies), ADR-0004 (sets the generalized
> parent-ADR-stays-frozen precedent this ADR inherits)

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
**substrate-honest** — names that any consumer (Claude Code,
Codex, Copilot) would read consistently. "Substrate-honest" admits
two complementary readings that pull in different directions on a
specific term:

- **Anchored-in-tmux**: use tmux's own terms (`session`, `pane`)
  because tmux is the technical substrate. Reads tend to favor
  `session`-flavored choices.
- **Consistent-with-existing-substrate-identifiers**: use what the
  substrate's own code already calls things (`agent`, in this
  codebase, from the `agents` table through `--agent` flags through
  the mailman service template). Reads tend to favor `agent`-flavored
  choices.

Both readings are coherent applications of ADR-0003's principle.
Where they conflict on a specific term, §Decision (1) resolves
which reading is load-bearing for this surface (the §Decision (1)
rationale below picks consistent-with-existing-identifiers for
terminology, and explains why).

### Pattern promotion

ADR-0004 §Context flagged "ADR-0003 application" as a pattern
candidate under anchor-before-pin discipline: ADR-0004 was
instance-1 (MCP wire surface). ADR-0005 surfaces the same shape on
a structurally distinct surface — different in **what the surface
is** (wire-shape names vs terminology in code/docs) and **what
per-surface sub-decisions it raises** (server identity / tool
prefix / control-names / migration for ADR-0004 vs terminology
choice / scope / precedent-inheritance / migration for ADR-0005).
The cutover mechanic and operational blast radius are **identical**
between the two instances — that's what justifies bundling per
§Decision (4), not what justifies the n=2 promotion. The two
instances together justify promoting the pattern from
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

### (1) Terminology choice: `agent`

`ChamberState` → `AgentState`. `chamber_state` (MCP method) →
`agent_state` (rendered as `tmux-msg.agent_state` post-ADR-0004).
All derived forms across Go code, MCP wire surface, and doc prose
follow.

Three reasons in order of weight:

1. **The substrate already calls these "agents" everywhere.**
   281 mentions of `agent` in Go source under `cmd/` and
   `internal/`; the `agents` SQL table; the `--agent` CLI flag;
   the mailman service template `claude-mailman@<agent>.service`.
   Under the consistent-with-existing-substrate-identifiers reading
   of substrate-honest (§Context above), `agent` is what we
   already use — `chamber` was the outlier that should align with
   the existing convention, not the other way around. The substrate
   doesn't have to invent a new term; it already has one.

2. **Type scope matches.** `ChamberState` describes the state
   observed for one CLI tool process running in one tmux pane.
   `AgentState` keeps the type's scope (per-agent) and the
   identifier (`agent`) in lockstep with each other and with the
   rest of the substrate. The alternative `SessionState` reads
   broader than the actual scope — a tmux session can host
   multiple panes, each hosting one agent — so the name would
   suggest per-tmux-session state when the actual state is
   per-agent.

3. **`session` carries dual-meaning friction.** Claude Code uses
   "session" for conversation-scoped contexts; tmux uses "session"
   for multiplexer-scoped contexts; a substrate "session" would be
   a third. `SessionState` in code, talking to a reader who's
   already thinking about either Claude Code sessions or tmux
   sessions, requires per-occurrence disambiguation. Cost is small
   per reference but recurs everywhere `SessionState` would appear.

#### Wheel-reinvention check on the operator-shell-participant scenario

The operator surfaced (2026-06-05, during the framing review): a
scenario where the operator's own tmux shell becomes a bus
participant — sending messages to chambers from a shell,
potentially receiving via a comment-prefix or send-only mode.
Initially this read as an argument *for* `session` as a supertype
(session > {agent, shell}) — a forward-looking commitment that the
substrate's participant abstraction is broader than today's
all-CLI-tool deployment.

The wheel-reinvention check, however, shows the supertype
expansion isn't justified:

- **Sending FROM a shell** already works via `claude-msg send …` —
  the shell is not a registered participant; it's a CLI invocation
  point with bus credentials. No substrate change needed.
- **Watching the response** is naturally done in the destination
  chamber's own pane, where the agent's reply renders normally.
  No paste-back-to-operator-shell needed.
- **Receiving INTO a shell** (if the operator wants their shell to
  *be* a destination chambers can target) can be handled by one
  flag on the existing `agents` table: `delivery_mode` ∈
  {`paste-and-enter`, `mailbox-only`}. The shell registers as
  `operator` with `mailbox-only`; chambers `send to=operator`; the
  operator polls `claude-msg inbox` when convenient. No supertype
  abstraction needed — the existing primitive bends to cover the
  case with a single config flag.
- **Operator-not-disturbed-when-typing** is already pane-level via
  the input-row gate (ADR-0001 `OperatorInputRowGate` pin). It's
  substrate-agnostic about what's running in the pane — works for
  shells for free.

Net: the operator-shell scenario expands the **configuration
space** for agents (a `delivery_mode` field), not the **abstraction
hierarchy**. Shells become agents-with-different-config, not a
different kind of thing. `agent` remains the substrate's primitive.

The operator-shell-participant feature is filed separately as a
substrate enhancement that adds `delivery_mode`. It does not
require ADR-0005 to anticipate it as a taxonomy split.

#### Alternatives (terminology)

- **`session`** — rejected. Argued under the anchored-in-tmux
  reading of substrate-honest (§Context); rejected under the
  consistent-with-existing-substrate-identifiers reading. The
  wheel-reinvention check above resolved the cross-current that
  briefly made `session` the forward-correct call.

- **`pane`** — rejected. Names where state is observed
  (capture-pane mechanism). The objection that this "leaks
  substrate internals" is contestable (Surveyor noted the state
  IS observed per-pane; naming what it actually is doesn't leak
  — it describes), but `pane` is rejected on a different ground:
  the substrate's existing identifier vocabulary uses `agent`,
  not `pane`. Switching to `pane` would create the same identifier
  inconsistency that `session` would, just toward a different
  term. Consistency-with-existing-identifiers is the load-bearing
  reading.

- **`instance`** / **`endpoint`**. Too generic; could mean any
  process or any network surface. Rejected.

- **`client`** — rejected. Bidirectional substrate (agents both
  send and receive); "client" implies asymmetric server/client.

- **Keep `chamber`** — rejected per the substrate-honest principle:
  project-local jargon that doesn't generalize across CLI flavors.

### (2) Substrate scope

The rename covers:

- **Go code identifiers**: `ChamberState` (the enum type in
  `internal/tmuxio/state.go`), `chamber_state` (variable names),
  `chamber-state` (kebab-case in CLI surface), `chamberState`
  (camelCase), and all derived forms across `cmd/claude-msg/` and
  `internal/`. Issue #107 was filed pre-ADR for this sweep; this
  ADR provides its upstream architectural rationale.

- **MCP tool method name**: `chamber_state` → `agent_state`.
  Post-ADR-0004 sweep this is rendered as
  `tmux-msg.<term>_state` (was `semaphore.chamber_state`).
  Bundled in the same Claude Code restart cycle as ADR-0004's
  MCP-wire-surface implementation (one cutover instead of two).

- **DB column names** (if any contain `chamber`): swept via
  schema migration in the implementation PR.

- **Doc prose**: `README.md`, `docs/`, in-code comments, help
  text. Post-ADR-0005 prose uses `agent` consistently.

- **Future ADR prose**: written with `agent` from inception. New
  ADRs starting at ADR-0006 use `agent`.

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

(Resolved in §Decision (1). Catalog reproduced here for reference;
the substantive rationale lives there.)

- **`agent`** — **chosen** per §Decision (1).
- **`session`** — rejected per §Decision (1); the
  wheel-reinvention check on the operator-shell scenario closed
  the cross-current that briefly made it the forward-correct call.
- **`pane`** — rejected per §Decision (1)'s §Alternatives
  subsection.
- **`instance`** / **`endpoint`** / **`client`** — rejected per
  §Decision (1)'s §Alternatives subsection.
- **Keep `chamber`** — rejected per the substrate-honest principle
  in §Context.

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
- **Alias period** (`chamber_state` + `agent_state` MCP methods
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
- **Substrate identifier coherence.** `AgentState` matches the
  existing `agents` table + `--agent` flag + mailman service
  template `claude-mailman@<agent>.service`. The substrate's code
  reads consistently from primitive to type to wire surface.
- **One Claude Code restart cycle, not two.** Bundling with
  ADR-0004's implementation halves the operational window cost.

### Harder

- **Operational migration window is the same as ADR-0004's** (~5
  minutes of MCP-bus-quiet). Empirical scope on apples-to-apples
  Go source only: ~111 `chamber` mentions vs. ~110 `semaphore`
  mentions — essentially identical. With docs included: ~200
  `chamber` vs. ~150 `semaphore`. The implementation PR's
  mechanical scope is comparable to ADR-0004's, not dramatically
  larger; the cutover blast radius is identical.
- **Existing memory entries citing `ChamberState`,
  `chamber_state`, etc.** remain accurate-to-time. Forward-tracking:
  post-cutover entries use `agent`; pre-cutover entries are
  preserved per the same forward-tracking discipline ADR-0004
  established.
- **Project-local lexicon (CLAUDE.md, memory) diverges from
  substrate lexicon.** Chambers are still called "chambers" in
  operator-side files (Bosun, Quartermaster, etc.); the substrate
  refers to `agent`s. A short bridge note in chamber-level
  CLAUDE.md files (operator-side) disambiguates: "Chambers
  (project lexicon) = agents (substrate lexicon)." Filed as a
  follow-up issue in `frankenbit/alcatraz-infra` so the bridge-note
  update is tracked, not left dangling in this ADR's §Consequences
  prose (per operator-action-items-as-issues discipline).
- **Type-name change requires updating any consumer code** that
  imports the type. Within `tmux-msg` itself, the sweep is
  internal. Downstream consumers (Binnacle's substrate bindings)
  carry the rename as part of the deferred-to-Bosun follow-up.

## What would change the decision

Reasons to retract or supersede ADR-0005:

- **The participant set splits along lines that require different
  chrome-state machinery or delivery semantics — not just config
  differences.** This is the supersession trigger the
  wheel-reinvention check identified. Today's operator-shell
  scenario is config-differences-only (one `delivery_mode` flag);
  if a future participant kind genuinely needs a different state
  machine (e.g., chrome states that don't apply), different
  identity model (e.g., per-session-per-pane), or different
  delivery contract (e.g., streaming instead of paste-and-Enter),
  the substrate's primitive really is no longer `agent` — it's
  a supertype with `agent` as one subtype. At that point ADR-0006+
  (or a successor ADR) supersedes ADR-0005 with the participant
  supertype framing. Same supersession-not-amendment shape per
  ADR-0004 §Generality.
- **A sibling CLI flavor needs a different terminology.** Unlikely
  but possible: if Codex's lexicon or Copilot's lexicon resists
  `agent`, the substrate-vs-flavor cut might need to surface here
  too. Today's expectation is that terminology translates; if it
  doesn't, ADR-0005 retracts via supersession.
- **The substrate's substrate changes.** If `tmux-msg` ever moves
  off tmux (e.g., to a different multiplexer like zellij or a
  native CLI-tool IPC), the "tmux-honest" half of substrate-honest
  naming retires. `agent` may or may not need to follow the
  substrate change. ADR-0005 retracts via supersession at that
  point.
- **`agent` carries friction in practice.** If `AgentState` and
  related names cause real reader-friction in code/docs/conversation
  (e.g., readers from outside the project consistently misread
  `agent` as something else), the amendment trigger is
  structurally-distinct instance-2 of the friction shape, mirroring
  ADR-0003 / ADR-0004's threshold framing.

The watch: same shape as ADR-0004's prefix-friction watch — track
the first ~one month post-cutover for terminology-friction
complaints. If no structurally-distinct issues surface in that
window, `agent` is durable.

## References

- ADR-0003 (`docs/adr/0003-substrate-vs-flavor-naming.md`) — the
  parent principle this ADR applies (third application surface)
- ADR-0004 (`docs/adr/0004-mcp-wire-surface-naming.md`) — the
  prior application; established the generalized parent-frozen
  precedent ADR-0005 inherits; introduced the "ADR-0003
  application" pattern under anchor-before-pin which ADR-0005
  promotes to named-pattern
- #107 — pre-existing tracker for the `ChamberState → ?` Go
  identifier rename (originally titled "→ SessionState" before
  ADR-0005 resolved the terminology to `agent`); ADR-0005
  provides the upstream architectural rationale (#107 becomes
  the implementation arm bundled into ADR-0004's #197 mechanical
  PR, sweeping `Chamber → Agent` throughout)
- #114 — this ADR's tracking issue
- README.md §Substrate vs CLI-tool-flavor — current voice of the
  framing ADR-0005 applies to the terminology layer
- Operator's 2026-06-05 nit on PR #113 — the trigger

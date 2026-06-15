# ADR-0014: tmux-tell scope — what the substrate IS, IS NOT, and the SSH-back-tunnel

> **Status**: Accepted
> **Date**: 2026-06-16
> **Authors**: Bosun (operator-ratified IS / IS NOT / planned-but-distinct lists, 2026-06-16 00:25)

## Context

The project started as a "small and trivial idea" (operator framing during v0.17.0 retro,
2026-06-15) and has accumulated substrate the original framing didn't anticipate: an
observe-gate, a hook-context delivery mode, an MCP server surface, codex-adapter
parity, four-workflow cut chains, and the first deploy lane. Each addition was
substrate-honest in isolation; the cumulative weight has changed the project's
center of gravity without an explicit project-scope statement to anchor against.

The **issue-level scope-fence** discipline is sharp: when a new gap surfaces during
review, the question "fold or follow-up?" gets asked + decided. The **project-level
scope-fence** is missing. Every new issue surface currently gets a tracker by default,
rather than first being measured against "does this even belong in tmux-tell?"

Operator-named friction during v0.17.0 retro:

> *"A project that started with a small and trivial idea has become something much
> more heavy."*

The absence of a project-scope ADR means every "should we add X?" proposal carries
the burden of proof on the rejecter, not the proposer. That defaults toward
substrate-accretion. The decision-by-omission discipline (this ADR) flips that:
once landed, "X is out-of-scope per ADR-0014" becomes the default answer unless the
proposer can name a load-bearing reason X falls within scope.

Composes with [ADR-0009](0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
(substrate-vs-adapter boundary) + [ADR-0010](0010-tool-name.md) (tool name
rename arc) + [#441](https://git.frankenbit.de/frankenbit/tmux-msg/issues/441)
(origin tracker).

## Decision

`tmux-tell` is a **per-user TUI-paste coordination bus for operator-launched LLM-CLI
sessions attached to tmux panes on a single host**, with cross-host reach planned via
SSH-back-tunnels that compose with host-locality rather than replicating the bus.

The decision is captured as three lists, each binding:

### What tmux-tell IS

1. **A peer-style coordination bus** for operator-launched LLM-CLI sessions (Claude,
   Codex, …) attached to tmux panes on a single host.
2. **TUI-paste delivery substrate** with operator-presence-aware deferral (the
   observe-gate).
3. **SQLite-backed persistence** for offline/idle agents.
4. **Substrate-vs-adapter boundary** (per ADR-0009) — multi-LLM-CLI coexistence
   on a single substrate.
5. **Hook-context delivery mode** for adapters with paste-incompatible TUIs (an
   alternative delivery channel that keeps the substrate uniform).
6. **MCP server surface** for agent self-registration + sending.
7. **Host-local trust boundary** (per-user UID, per-host SQLite, OS-level
   isolation).
8. **Forward extensible at the adapter axis** (new LLM-CLI adapter = new
   `cmd/tmux-tell-<name>` binary + Profile flags; substrate unchanged).

### What tmux-tell is NOT

1. **Generic message broker** — NATS / RabbitMQ / Redis-style use cases are
   out-of-scope. The tmux-paste delivery mechanism is the load-bearing
   differentiator; substrates without that constraint already exist + are better.
2. **Real-time streaming** — paste-and-enter is point-in-time, not stream. If
   token-level streaming becomes desired, that's a different substrate.
3. **Multi-tenant / network-addressable agents** — per-user UID assumption is
   load-bearing for the trust model + the SQLite scoping. Cross-user coordination
   requires a different substrate.
4. **Browser / web UI** — TUI substrate is intentional. The operator-presence model
   + the observe-gate + the paste-into-TUI mechanism all rely on terminal-as-surface.
5. **E2E encryption** — host-local + per-UID means the trust boundary is OS-level.
   Cross-host reach (see below) carries its own transport security separately; the
   substrate doesn't layer E2E on top.
6. **Persistent service for non-LLM consumers** — the substrate is calibrated for
   LLM-CLI TUIs (paste-and-enter, observe-gate's working-detection, hook-context
   for non-paste-capable adapters). Generic process IPC is out-of-scope.

### Planned but NOT host-local-bus-replication

7. **SSH-back-tunnel to remote operators** — cross-host reach via one-way SSH
   carriage that composes WITH host-locality rather than replicating the bus.
   The substrate stays host-local; SSH tunnels are the operator-facing reach
   mechanism for sending messages TO an alcatraz-hosted agent from a remote
   working environment (operator's Caymans laptop, Admin desktop, etc.). This
   is distinct from "cross-host bus" which IS out-of-scope (item 3 above).

   The SSH-back-tunnel is **planned-but-distinct**: not yet implemented, but
   anticipated as the canonical cross-host reach shape. When it lands, it'll
   compose with the host-local bus, not extend it.

### Decision-by-omission discipline

Once this ADR lands, the default answer to "should we add X?" becomes:

> "X is out-of-scope per ADR-0014 unless you can name a load-bearing reason X
> falls within the IS list."

The burden of proof flips from "rejecter justifies the no" to "proposer justifies
the yes against the IS list." Sibling discipline to the issue-level scope-fence
pattern: project-level decision-by-default → issue-level decision-by-empirical-surface.

## Alternatives considered

- **No scope ADR (status quo).** Every "should we add X?" gets re-derived from first
  principles, with the default toward substrate-accretion. Rejected: operator-named
  the heaviness friction; the absence of a scope-fence is the cause, not a
  consequence.

- **Lighter scope statement (BookStack page or CONTRIBUTING.md section).** Could
  work for the IS / IS NOT statement, but doesn't carry the architectural-commitment
  weight that flips the default. ADRs constrain future work + have the discipline-pin
  enforcement shape (per ADR-0001). BookStack would document; only an ADR
  decides-by-omission.

- **Extend ADR-0010 (tool name rename) with scope language.** ADR-0010 is about
  the rename arc itself, not about scope. Folding would over-load that ADR.
  Separate decisions land in separate ADRs per the [ADR-0006 length-cap + co-location
  convention](0006-adr-length-cap-and-background-docs.md).

- **Defer the scope ADR until cross-host substrate question is settled.** The
  SSH-back-tunnel shape isn't fully specified yet (it's planned-but-distinct, not
  implemented). Rejected because the scope-friction operator named is present NOW;
  waiting for cross-host details would let substrate-accretion continue
  unanchored. SSH-back-tunnel can land as a follow-up ADR specifying the mechanism
  once it's needed.

- **Looser "IS NOT" list (fewer rejections, more open scope).** Each IS NOT item
  was operator-ratified as load-bearing-out-of-scope. Loosening would reintroduce
  the substrate-accretion default the ADR is meant to fix.

## Consequences

**Upside:**

- **Forward scope-fence at issue-filing time.** New "should we add X?" proposals
  measure against the IS / IS NOT lists first. Most proposals that don't fit get
  closed-as-out-of-scope with a single ADR reference; substrate-accretion friction
  drops.
- **Substrate-flat principle made concrete.** The IS list bounds the substrate's
  growth axis at "what the substrate provides," not "what convention or chamber
  behavior adds on top." Conventions and chamber-discipline pins compose with the
  substrate; the substrate doesn't grow to embed them.
- **SSH-back-tunnel framing pre-empts a recurring scope-creep risk.** Cross-host
  reach is a natural request; this ADR establishes that the reach mechanism is
  separate from the bus, so future "should we make the bus cross-host?" questions
  get answered consistently.
- **Decision-by-omission lowers cognitive load.** Reviewers don't re-derive scope
  from first principles each time; the ADR carries the load-bearing reasoning.

**Cost:**

- **Some valuable extensions get foreclosed.** Items on the IS NOT list (generic
  broker, multi-tenant, web UI) may have valid use cases for someone, somewhere.
  This ADR commits us to not pursuing them inside tmux-tell. The cost is real
  but bounded: a separate project could carry those use cases without
  contaminating tmux-tell's substrate.
- **The decision-by-omission discipline puts burden on proposers.** Someone with
  a genuinely-in-scope-but-not-obviously-IS-list-aligned proposal has to argue
  for it explicitly. That's intentional friction; the trade-off is anchored
  forward scope, not lighter proposer burden.
- **SSH-back-tunnel deferred means cross-host reach has a known gap.** Operator
  flagged this as planned; until the SSH-back-tunnel ADR + implementation land,
  cross-host reach is manual. Acceptable because the substrate's host-local
  trust boundary is itself load-bearing; rushing cross-host would erode it.

## What would change the decision

This ADR would warrant revisiting when:

- **A consistent operator pattern emerges for an IS NOT item.** If, e.g., generic
  process IPC requests recur with substrate-honest reasoning that the existing
  alternatives don't fit, the IS NOT list might shift. Single-instance asks do
  NOT trigger revisiting (per the decision-by-omission discipline); pattern does.
- **The host-local trust boundary stops being load-bearing.** If a future
  architectural shift made per-host SQLite + per-user UID isolation a hindrance
  rather than a contract, the IS list's items 3 + 7 would need re-examination.
- **A new substrate primitive emerges that subsumes paste-and-enter** (e.g., a
  TUI-native message protocol). The IS list's item 2 (TUI-paste delivery
  substrate) would shift from load-bearing-mechanism to one-of-N mechanisms;
  scope would need redrawing.
- **SSH-back-tunnel design proves incompatible with host-locality** when
  implementation begins. Either: the ADR establishing SSH-back-tunnel resolves
  the tension by amending this one, OR the host-locality contract proves
  load-bearing and SSH-back-tunnel gets rescoped.

## References

- [#441](https://git.frankenbit.de/frankenbit/tmux-msg/issues/441) — origin
  tracker, operator-named scope friction at v0.17.0 retro.
- [#442](https://git.frankenbit.de/frankenbit/tmux-msg/issues/442) — comparative
  source-reading of AutoGen / CrewAI / Claude Agent SDK; evidence-grounding for
  the substrate-flat tenet; lands separately, may amend this ADR if its evidence
  warrants.
- [#440](https://git.frankenbit.de/frankenbit/tmux-msg/issues/440) — rename arc
  to `tmux-tell`; this ADR + the rename together establish the substrate's
  forward identity.
- [ADR-0009](0009-hook-context-delivery-substrate-vs-adapter-boundary.md) —
  substrate-vs-adapter boundary; this ADR specifies what the substrate IS at
  the project level, ADR-0009 specifies where the substrate ENDS at the adapter
  boundary.
- [ADR-0010](0010-tool-name.md) — tool name rename; this ADR defines what the
  renamed substrate is, ADR-0010 defines the name.
- Operator v0.17.0 retro (2026-06-15) — surfaced the heaviness friction and
  named the SSH-back-tunnel framing as cross-host reach.
- Operator ratification (2026-06-16 00:25) — IS / IS NOT / SSH-back-tunnel
  planned-but-distinct lists ratified verbatim from #441 body.

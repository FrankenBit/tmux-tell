# ADR-0011: Mailman scope-expansion — the three-fence test, applied to chamber respawn

> **Status**: Accepted
> **Date**: 2026-06-11 (proposed); 2026-06-11 (accepted — operator ratification via Quartermaster, #306)
> **Authors**: Engineer (ADR draft), operator (ratified the three-fence framing 2026-06-11), Quartermaster (design framing + ratification routing), operator + Quartermaster (design framing — #285 provenance, 2026-06-09 design conversation), Surveyor (framework-quality review on PR #306)

## Context

tmux-msg is a **message-bus substrate**: pane registry, identity, paste-and-Enter
delivery, the per-agent mailbox store, and the per-agent **mailman daemon** that
carries messages from the store to a pane (observe-gate, deferred-delivery #227,
`RecoverDelivering`). ADR-0003 draws the substrate-vs-flavor line (what's general
vs consumer-specific); ADR-0009 draws the substrate-vs-adapter line (what's
delivery-method-agnostic). Neither answers a third, recurring question:

> **When may tmux-msg expand its scope *outward* — from "deliver messages" toward
> adjacent concerns like chamber-process lifecycle — without becoming a
> general-purpose orchestrator?**

The forcing case is #285. Long-running chamber Claude processes accumulate heap
that `/compact` does not release (alcatraz, 2026-06-09: 9-day uptime → 5.7–5.8 GB
RSS per chamber while auto-compaction behaved correctly; the box eventually
segfaulted tmux during an unrelated workload — alcatraz-infra#31). `/compact`
shrinks the *conversation*; only a **process restart** releases the *heap*, after
which `claude --resume <name>` reloads the just-compacted (small) transcript. The
proposal: extend the mailman to **respawn a chamber's process after N successive
context-shrink events**.

That is a deliberate scope-expansion beyond "message bus." Without a principled
boundary, "the mailman could also do X" recurs per-PR and the bus accretes into a
process supervisor. This ADR establishes the boundary as a **reusable test** and
applies it to respawn (the first of two cases; ADR-0012 is the second).

## Decision

**Adopt the three-fence test for substrate scope-expansion, and admit chamber
respawn under it.**

A capability that reaches *outside* message delivery (process lifecycle, session
storage, …) belongs in tmux-msg **only if all three fences hold**:

1. **The trigger is observable, not invented.** The capability fires only in
   response to events the substrate already sees — never as a standalone
   primitive the bus newly originates. (Respawn fires on existing in-substrate
   events: a compact-done signal, or a bus-delivered clear. It is never a thing
   you *ask* the bus to do out of nowhere.)
2. **The carriage work is already in the mailman's hands.** The mechanism reuses
   substrate machinery the mailman already owns — pane handle, observe-gate,
   deferred-delivery — rather than introducing a new subsystem. (Respawn is
   `tmux respawn-pane` sequenced through the observe-gate + #227 handoff the
   mailman already drives.)
3. **No standalone lever.** There is **no** subcommand and **no** MCP tool that
   performs the capability on demand. (No `respawn` subcommand, no
   `restart_chamber` MCP tool. A wedged chamber is fixed with `tmux respawn-pane`
   directly — outside tmux-msg.)

A future contributor asking "should this go in tmux-msg?" checks these three. If
**any** fails, the answer is no.

### How respawn satisfies the fences

- **Fence 1 (observable trigger).** A single per-chamber counter increments on
  EITHER in-substrate event: a `/compact` (detected via Claude Code's
  `PostCompact` hook — preferred — or transcript-JSONL polling as fallback) or a
  bus-delivered clear (counted on delivery; the bus *is* the source of truth, no
  detection needed). At threshold `N` the mailman respawns and resets the counter
  to 0. There is no "I want a restart" event.
- **Fence 2 (carriage already owned).** The respawn pathway inserts into the
  existing #227 post-compact flow: wait-for-idle (observe-gate) → `/exit` →
  wait-for-exit (bounded, force-kill fallback) → `tmux respawn-pane -k` (pane
  preserved, original cmdline) → wait-for-ready (observe-gate) → **deliver
  deferred messages to the *restarted* process**. Every step is substrate the
  mailman already drives; only the respawn-pane call and the counter are new.
- **Fence 3 (no standalone lever).** Memory pressure is surfaced to the operator
  via alcatraz-infra#33 (RSS metric) as *observation*, not a trigger. If the
  operator wants an out-of-band restart they use `tmux respawn-pane`. The bus
  exposes no restart verb.

### Status of the feature vs. the test

This ADR commits to **the test** and to **respawn being admissible under it**. The
implementation contract (config flag `--respawn-after-n-shrinks <N>` default 3,
per-chamber; hook + polling-fallback detection; the #227 sequencing) lives in
#285 and its implementation PR, not here. The ADR is the fence; #285 is the build.

## Alternatives considered

- **No test — decide scope-expansion ad hoc per PR.** Rejected: "the mailman could
  also do X" has no stopping rule without a named boundary; the bus drifts into a
  process supervisor one reasonable-looking PR at a time.
- **A standalone `respawn` subcommand / `restart_chamber` MCP tool.** Rejected by
  fence 3: a standalone restart lever makes tmux-msg a process orchestrator and
  invites "restart any pane" scope the substrate has no business owning. Coupling
  respawn to observed context-shrink keeps it a *carriage* concern.
- **A separate orchestration layer/daemon for chamber lifecycle.** Rejected: the
  mailman already holds per-chamber pane + observe-gate + deferred-delivery state;
  a second supervisor would duplicate that state and re-introduce the
  single-writer-per-recipient race the mailman exists to avoid.
- **Memory-threshold (RSS) trigger.** Rejected as a *trigger* (fence 1): RSS is
  not an in-substrate event and reading it couples the mailman to host
  process-introspection. Kept as operator-facing observation (alcatraz-infra#33).
- **Do nothing — rely on `/compact` + operator-driven `tmux respawn-pane`.**
  Rejected: `/compact` provably does not release heap (the 5.8 GB measurement),
  and a purely manual restart does not bound RAM growth for unattended fleets or
  smaller boxes.

## Consequences

### Cleaner

- **Scope-expansion gets a stopping rule.** The recurring "should the mailman also
  do X?" question has a mechanical answer; reviewers carry the three fences in
  working memory the way ADR-0003's substrate-vs-flavor line is carried.
- **Respawn rides existing substrate.** No new orchestration layer, no new tool
  surface, no second per-chamber supervisor; the #227 deferred-delivery handoff is
  reused, not reimplemented.
- **Opt-in and bounded.** `--respawn-after-n-shrinks 0` disables the feature
  entirely; the per-chamber default (3) bounds RAM growth without inventing a
  policy engine.

### Harder

- **The mailman now spans message-carriage *and* a slice of process lifecycle.**
  That is a real widening of the daemon's responsibility; the fences are what keep
  it a *slice* (respawn-on-observed-shrink) rather than general supervision. The
  cost is that every future lifecycle-adjacent proposal must be argued against the
  fences explicitly, not waved through because "the mailman already restarts
  things."
- **Detection has two modes.** Hook-preferred with polling-fallback means two code
  paths and an operator-visible "which mode is active" surface; the fallback's
  mtime-poll is bounded but non-zero CPU.
- **Sequencing is delivery-correctness-critical.** Deferred messages must arrive
  at the *restarted* process, not the dying one (#227 handoff threaded through the
  respawn pathway). A sequencing bug here silently drops post-respawn messages —
  the #285 test harness must prove arrival-at-new-process.

## What would change the decision

- **A second standalone-lever need surfaces** — e.g. a legitimate case for
  restart-on-demand that can't be expressed as response-to-observed-event. That
  would mean fence 1/3 are too strict; revisit whether tmux-msg should own an
  explicit lifecycle verb (and accept becoming, in part, an orchestrator).
- **Claude Code (or a sibling CLI) ships native process-lifecycle management** —
  built-in heap-release-on-compact or a supervised-restart mode. Then respawn-in-
  the-mailman is redundant carriage; retract in favor of the native mechanism (the
  same "standards obsolete substrates" logic as ADR-0003 §What-would-change).
- **The fences stop being checkable in practice** — if "observable trigger" or
  "carriage already owned" become so elastic that everything passes, the test has
  failed as a boundary and must be tightened or replaced.

The watch (mirroring ADR-0003's structurally-distinct-case threshold): each new
scope-expansion PR is tested against the three fences. ADR-0012 is the **second**
structurally-distinct application (session rename on clear); two clean applications
are what promote the fences from "respawn's local justification" to an extracted,
load-bearing project pattern. A *failed* application — a proposal that's clearly
desirable yet fails a fence — is the signal to amend this ADR (relax the fence and
record why) rather than quietly route around it.

## References

- #285 (this feature — post-compact chamber respawn; carries the implementation
  contract and the AC list)
- #227 (deferred-delivery primitive — the post-compact handoff the respawn pathway
  threads through)
- #249 / ADR-0009 (hook-context delivery — the `PostCompact` hook composes with
  this substrate as the symmetric outbound channel)
- ADR-0003 (substrate-vs-flavor naming — the prior boundary ADR whose
  structurally-distinct-case promotion-threshold this ADR's watch mirrors)
- ADR-0012 (session rename on bus-mediated clear — the second application of this
  test)
- alcatraz-infra#31 (the tmux segfault that surfaced the heap-growth problem),
  alcatraz-infra#33 (per-chamber RSS observation surface — operator-facing, not a
  trigger)

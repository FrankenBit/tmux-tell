# ADR-0002: Chamber-state carry-forward spec for Binnacle's M6b

> **Status**: Accepted
> **Date**: 2026-06-04
> **Authors**: Quartermaster (drafting), Bosun + operator (framing on #69)

## Context

`cli-semaphore`'s chamber-state primitive (#69) ships as a temporary
bridge: a tmux-capture-based "knock at the door" probe that classifies
each chamber as one of `working` / `idle` / `at-rest-in-compaction` /
`awaiting-operator` / `unknown`. The substrate landed in
[PR #75](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/75)
(`tmuxio.ChamberState` v1), [PR #76](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/76)
(consumer surfaces #72 + #73), [PR #77](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/77)
(cursor-aware v2), and [PR #78](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/78)
(NBSP encoding fix that made the gate fire in production). The CLI
(`claude-msg state`) and MCP (`semaphore.chamber_state`) consumer
surfaces are byte-identical via the shared `resolveChamberState`
helper.

Binnacle's M6b dashboard / operator API is the long-term home for the
same observation surface. Binnacle ships, this primitive retires. The
question this ADR answers: **what stays the same across the
transition, and what is bridge-specific?**

Framing came through operator + Bosun on the parent #69:

> *"This all would probably have more a temporal use, until Binnacle
> is ready to orchestrate. But maybe we could still use some pieces
> there too."*

The "pieces that carry forward" need to be named explicitly, in this
repo, before Binnacle's M6b picks up the question — otherwise the
risk is that M6b silently invents a new vocabulary and the operator-
facing UI text fragments across the cli-semaphore → Binnacle
transition.

## Decision

**The chamber-state vocabulary, result schema, and the
`unknown`-as-advisory convention carry forward verbatim to Binnacle's
M6b. The tmux-capture detection mechanism, the 200ms temporal-delta
latency floor, and the specific Evidence struct shape do NOT.**

Binnacle M6b should adopt the durable parts as a typed contract on its
operator-facing API; the bridge-specific parts are internal substrate
that M6b is free to replace without affecting consumers.

### Durable parts (carry forward to Binnacle)

#### (1) Five-state vocabulary

The state names are operator-facing — they appear in dashboard
columns, MCP tool responses, CLI output, log lines. Adopting them
verbatim keeps the operator's mental model continuous across the
cli-semaphore → Binnacle transition.

| State                   | Semantics                                                                                              |
|-------------------------|--------------------------------------------------------------------------------------------------------|
| `working`               | Chamber is actively processing — captures differ across temporal delta (or working markers detected).  |
| `idle`                  | Chamber is at a clean prompt waiting for input (sentinel found + cursor at sentinel position).         |
| `at-rest-in-compaction` | Chamber is mid-compaction — detected via compaction-specific UI markers.                               |
| `awaiting-operator`     | Chamber is paused awaiting human input (selection menu, AskUserQuestion popup, mid-typing draft).      |
| `unknown`               | Probe-honest indeterminacy — implementation could not confidently classify.                            |

#### (2) Result schema

```typescript
type ChamberStateResult = {
  agent: string;                // The chamber's canonical name.
  state: ChamberState;          // One of the five values above.
  evidence: Record<string, unknown>;
                                // Opaque blob; implementation-specific
                                // shape. Consumers MAY display or
                                // log it; they MUST NOT branch logic
                                // on its inner fields.
  captured_at: string;          // ISO 8601 UTC, microsecond precision.
};
```

The `evidence` field is an opaque blob by design. The
cli-semaphore implementation populates it with
`{Reason: string, RegisteredPane: string, CapturedAt: time}`;
Binnacle's implementation can populate whatever fits its substrate.
Consumers that need to make decisions branch on `state`, not on
`evidence`'s inner structure.

#### (3) `unknown`-as-advisory convention

`unknown` is **probe-honest indeterminacy**, NOT a silent roll-up to
a known state. Consumers MUST treat `unknown` as a gating signal:

- Decision logic that fires on `state == 'idle'` MUST NOT fire on
  `state == 'unknown'`.
- Display surfaces SHOULD show `unknown` as visually distinct from
  the four known states (different colour / icon / label).
- Aggregation surfaces (e.g., "how many chambers are idle right
  now?") MUST NOT count `unknown` as either idle-or-not — count it
  in its own bucket.

This is the same shape as the cli-semaphore#65 playbook's
"advisory-not-authoritative" framing: a layer that returns
`unknown` is honestly admitting it couldn't tell, and downstream
consumers preserve that uncertainty rather than collapsing it.

#### (4) API surface shape (both primitives)

The cli-semaphore implementation ships only the **per-agent query**
primitive (`semaphore.chamber_state {agent}`). Binnacle's M6b
dashboard naturally wants the **enumeration-over-all-agents**
primitive too (one call returns the state of every registered
chamber). Both belong in the durable contract:

```typescript
// Per-agent (what cli-semaphore ships).
function chamberState(agent: string): ChamberStateResult;

// Enumeration (what Binnacle M6b adds).
function chamberStates(): ChamberStateResult[];
```

`chamberStates()` MUST return a result per registered chamber,
including those classified as `unknown`. Filtering happens at the
consumer.

### Bridge-specific parts (do NOT carry forward)

#### (a) tmux-capture-based detection mechanism

cli-semaphore observes chambers by parsing `tmux capture-pane`
output + `tmux display-message '#{cursor_x}'` for cursor position.
The substrate-class property is *read-only-observe at the tmux
layer* — "knock at the door without waking" is the framing.

Binnacle's substrate is likely Go-side process supervision +
session-state events (per the Phase-0 / M1 architecture work). The
detection mechanism is bridge-specific and Binnacle should
implement whatever fits its substrate.

#### (b) ~200ms temporal-delta latency floor

cli-semaphore distinguishes `working` from `idle` via a 200ms
temporal delta between two `capture-pane` calls. This polling shape
is required because tmux doesn't push pane-change events to the
mailman.

Binnacle's session-state event stream (M6b's natural substrate)
eliminates the polling floor entirely — Binnacle knows when a
chamber's state changes because the session emits the event
directly. The latency floor disappears with the bridge.

#### (c) Evidence struct's specific fields

cli-semaphore's `evidence.Reason` carries strings like
`"sentinel found, cursor at col 2 == sentinel width"` —
substrate-specific diagnostic text that ties back to the
tmux-capture mechanism. Binnacle's evidence shape will say
different things because it's looking at different signals.

The contract is the field's **opacity** to consumers, not its
inner shape.

## Alternatives considered

### Alt 1: defer the carry-forward decision until M6b actually starts

Rejected. The vocabulary needs to be locked before two implementations
exist, otherwise the second one silently drifts. The cost of writing
this ADR now is low; the cost of fixing a vocabulary fork later (after
operator-facing UI text has shipped under two names) is high.

### Alt 2: typed enum in the wire schema instead of stringly-typed states

Rejected for the cross-implementation contract. The wire is JSON; both
cli-semaphore (Go) and Binnacle (Go) can map strings to typed values
internally, but the wire shape is shared with the MCP layer where
schema-bound enums add friction (some MCP clients don't enforce enum
constraints; the consumer-side burden moves but doesn't decrease).
String values + a documented vocabulary list is the practical shape.

### Alt 3: subscription primitive as v1, polling as v2

Rejected for the cli-semaphore implementation specifically. cli-
semaphore's substrate is tmux capture-pane (no native push channel),
so polling is the only shape it can ship. Binnacle's M6b SHOULD
implement subscription as a sibling to the query primitives once
the substrate supports it; the carry-forward contract names this as
a v2 extension, not a v1 requirement.

## Consequences

**Upside:**

- Binnacle's M6b doesn't have to redesign the vocabulary or schema
  from scratch — the durable parts are already named and tested in
  production.
- Operator-facing UI text stays consistent across the cli-semaphore
  → Binnacle transition. No "wait, in the new dashboard `idle` means
  something different" moment.
- The `unknown`-as-advisory convention is named once; consumers that
  follow the convention work against either implementation.
- The bridge-specific parts list is explicit, so Binnacle doesn't
  feel obligated to preserve mechanism-level details (tmux capture,
  cursor position) that are substrate-specific.

**Cost:**

- The vocabulary is now a load-bearing commitment. A future "actually,
  we need a sixth state" insight requires either an extension that
  preserves the existing five (sibling state) or a superseding ADR.
- If Binnacle's M6b discovers a structural reason the durable parts
  list is wrong (e.g., `evidence` needs a typed sub-field for some
  consumer use case), this ADR needs amendment, not just a Binnacle-
  side decision.
- The `chamberStates()` enumeration primitive is named but
  unimplemented in cli-semaphore. Either it stays a Binnacle-only
  primitive (acceptable — cli-semaphore consumers don't currently
  ask for it), or cli-semaphore adds it for parity with Binnacle's
  M6b on the same ADR's contract.

## What would change the decision

- **A sixth state emerges as load-bearing.** A real chamber-state
  signal that doesn't fit any of the five categories (and isn't
  `unknown` as a fallback). Amendment-shaped, not superseding.
- **The opaque-evidence contract becomes load-bearing for a
  consumer.** If a real Binnacle M6b consumer needs to branch on
  evidence inner fields, the schema needs typed sub-fields, not an
  opaque blob. Amendment-shaped.
- **Binnacle's M6b ships with an incompatible vocabulary.** This
  ADR is *recommendation*, not enforcement — Binnacle could ignore
  it. If that happens and the operator-facing UI fragments, the
  retraction trigger fires and a new ADR documents the fork.
- **A native push-channel substrate** that eliminates polling AND
  preserves cli-semaphore as the implementation. Unlikely (Binnacle
  is the natural home for push), but if it shipped, the
  polling-as-v1 framing would need amending.

## References

- [#69](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/69)
  — parent tracker; operator + Bosun framing on durable-vs-bridge
- [#71](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/71)
  — `tmuxio.ChamberState` substrate; informs the bridge-specific
  parts list
- [#72](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/72)
  / [#73](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/73)
  — CLI + MCP consumer surfaces under the shared
  `resolveChamberState` helper
- [PR #75](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/75)
  / [PR #76](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/76)
  / [PR #77](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/77)
  / [PR #78](https://git.frankenbit.de/frankenbit/cli-semaphore/pulls/78)
  — substrate-progression that landed the implementation this ADR
  freezes the operator-facing parts of
- ADR-0001 — discipline-pins-as-test-category; sibling ADR; the
  `Substrate-class evolution preserving invariant` framework
  observation (Surveyor's O65) emerged from the same PR-77-vs-PR-66
  evolution
- Binnacle M6b (when filed) — the consumer of this ADR's
  recommendations
- cli-semaphore#65 playbook `docs/diagnostic-playbook.md` —
  same `advisory-not-authoritative` framing for the `unknown` state

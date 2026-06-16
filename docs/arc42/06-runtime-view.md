---
arc42-section: 6
revisit-triggers:
  - a locked-down delivery flow changes shape (not just a verify-detail)
  - a new runtime flow is added (e.g. a new cascade)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §6 Runtime View

Phase 1 documents **one flow** in depth — the core send → mailman → observe-gate →
paste → verify path. The other locked-down flows (hook-context delivery, the
`refresh-all-mcps` cascade) are noted with pointers and deepen in a later phase.
Flows are stable for the locked cases; verify-detail changes (e.g. the
cursor-anchor refinement #369) don't change the flow shape.

## Core flow — deliver a message (paste-and-enter)

```mermaid
sequenceDiagram
    participant S as Sender (agent/operator)
    participant DB as store (SQLite)
    participant M as Mailman (recipient's)
    participant P as Recipient pane (tmux)

    S->>DB: send → row {queued}
    M->>DB: poll → claim {delivering}
    M->>P: observe-gate: classify pane state
    Note over M,P: 5 states — working / idle /<br/>awaiting-operator / at-rest-in-compaction / unknown
    alt idle (or working+immediate)
        M->>P: paste buffer + Enter
        M->>P: verify-token round-trip
        alt verified
            M->>DB: {delivered}
        else not confirmed
            M->>DB: {delivered_in_input_box} (honest, not over-claimed)
        end
    else awaiting-operator / compaction / unknown
        M->>P: drop 📫 (typing notification, #95)
        M->>M: re-poll, multiplicative backoff 3s→…→15s
        Note over M: deliver once safe; if budget exhausted,<br/>deliver anyway + log (fail-loud)
    end
```

**Key invariants in this flow:**

- The gate is **near-read-only** — two `capture-pane` snapshots + one cursor query,
  plus at most one `📫` paste; it does not mutate the recipient's work.
- The cursor-position distinction (**at**-sentinel = idle vs **past**-sentinel =
  mid-typing) is what separates "deliver now" from "wait" ([observe-gate](../observe-gate.md)).
- Delivery **never silently over-claims**: the verify-token + `delivered_in_input_box`
  notice is the post-hoc safety net for the small race between observing a state and
  the paste landing ([§10](10-quality-requirements.md) substrate-state-claim integrity).

## Other locked-down flows (pointers; deepen later)

- **Hook-context delivery** — for a paste-incapable adapter, the message is injected
  via the CLI's hook channel at a turn boundary instead of pasted into a live pane
  ([ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)).
- **`refresh-all-mcps` cascade** — the cap-protected, operator-explicit rebind of
  every chamber's MCP after a deploy ([§7](07-deployment-view.md)).

> Depth: [docs/observe-gate.md](../observe-gate.md) (the five states + tuning) +
> [docs/failure-modes.md](../failure-modes.md) (what the gate redesign fixed).

---
arc42-section: 9
revisit-triggers:
  - a new ADR is accepted (this index gains an entry)
  - an ADR is superseded or retracted (supersession chain updates)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §9 Architecture Decisions

The Arc42 entry point to the decision records under [`../adr/`](../adr/). That
directory is the **source of truth**, indexed by [`../adr/README.md`](../adr/README.md)
(active decisions, statuses, the length-cap + historicity notes). This section
adds the architect's-eye relationship view; it does not duplicate the index. An
auto-index-gen script is a deferred follow-up (#386 out-of-scope).

## The decision set at a glance

The ADR discipline itself — format, the 350-line cap, lifecycle, amendment policy —
is [ADR-0006](../adr/0006-adr-length-cap-and-background-docs.md) (and the test-category
discipline, [ADR-0001](../adr/0001-discipline-pins-as-test-category.md)). All ADRs
are currently **Accepted** except [ADR-0013](../adr/0013-plan-first-workflow.md)
(*Proposed*). No ADR has been **superseded** yet.

## Relationship / supersession chain

ADRs aren't a flat list — several *apply* or *amend* an earlier decision:

```
0003 substrate-vs-flavor naming
   └─ 0004 MCP wire-surface naming        (application of 0003)
   └─ 0005 substrate-honest terminology   (chamber → agent; applies the naming law)
0009 hook-context / substrate-vs-adapter boundary
   └─ anchors §2 / §3 / §8 (architectural law)
0011 mailman scope-expansion (three-fence test)
   └─ 0012 session rename on bus-mediated clear  (application of 0011)
0010 tool name (the rename arc)  ──► realized in v0.18.0 (#440)
0014 tmux-tell scope-fence  ──► anchors §3 Context & Scope
0015 adopt Arc42 spine  ──► this spine (mirrors Binnacle ADR-0007)
```

The one **amended** ADR is [ADR-0008](../adr/0008-deprecation-policy.md) (deprecation
policy), amended 2026-06-08 — Amendment A (K-counter interaction) + Amendment B
(structured `### Deprecated` CHANGELOG format), both in-file per the ADR-0006
amendment convention.

Standalone decisions (no application chain): 0001 (discipline pins), 0002 (chamber
state carry-forward), 0006 (length cap), 0007 (Binnacle external-module coexist),
0008 (deprecation policy — see the amendment note above), 0013 (plan-first, *Proposed*).

> **Numbering note:** a parked duplicate `0014-comparative-source-reading.md`
> shares the 0014 number with the indexed scope ADR; the renumber is tracked in
> #500 (it will move the duplicate to the next free number, not 0015).

**The live, authoritative index is always [`../adr/README.md`](../adr/README.md).**

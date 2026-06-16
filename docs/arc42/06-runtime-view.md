---
arc42-section: 6
revisit-triggers:
  - a locked-down delivery flow changes shape (not just a verify-detail)
  - a new runtime flow is added (e.g. a new cascade)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §6 Runtime View

**Phase-1 status: spine placeholder.** Content lands in **PR-B** of #386. The
file exists now so the [Arc42 index](README.md) is complete and this section
reads as *planned*, not *missing*.

**Will cover (Phase 1, one flow):** a sequence diagram for the core
send → mailman → paste → verify flow. The other locked-down flows (hook-context
delivery, the `refresh-all-mcps` cascade) are noted with pointers and deepen in a
later phase — per the operator-ratified "one flow now, deepen later" scope-call
(#386). Flows are stable for the locked cases; the cursor-anchor verify detail
(#369) is a verify-detail change, not a flow change.

**Interim source:** [docs/observe-gate.md](../observe-gate.md) +
[docs/failure-modes.md](../failure-modes.md).

---
arc42-section: 4
revisit-triggers:
  - a core substrate mechanism changes (mailman loop, verify-token loop, WAL/concurrency model)
  - a new delivery mode is added alongside paste-and-enter / hook-context
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §4 Solution Strategy

**Phase-1 status: spine placeholder.** Content lands in **PR-B** of #386. The
file exists now so the [Arc42 index](README.md) is complete and this section
reads as *planned*, not *missing*.

**Will cover:** the high-level technical shape of how the architecture meets the
§1 goals — SQLite-backed mailbox + per-agent mailman daemon; atomic tmux delivery
via paste-and-enter; verify-token loop as the substrate-state-claim-integrity
mechanism; WAL + `BEGIN IMMEDIATE` for write-concurrency under single-writer-per-
recipient; systemd-user as daemon manager; cap-bounded admission control;
hook-context as complementary delivery. Distinct from §1 (*what* we want) and §10
(*how well* we do it) — §4 is *the shape of the answer*.

**Interim source:** [ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
+ code comments in `internal/`.

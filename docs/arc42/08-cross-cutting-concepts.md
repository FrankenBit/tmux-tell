---
arc42-section: 8
revisit-triggers:
  - a new cross-cutting concept emerges (one that touches multiple components)
  - an ADR lands that should anchor a subsection here
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §8 Crosscutting Concepts

**Phase-1 status: spine placeholder.** Content lands in **PR-B** of #386. The
file exists now so the [Arc42 index](README.md) is complete and this section
reads as *planned*, not *missing*.

**Will cover (link-first subsections, mirroring Binnacle §8):** the concepts that
pervade multiple components — identity resolution chain (`TMUX_AGENT_NAME` →
`TMUX_PANE` → registry); error classification (delivery-failure classes, ping
reachability classes); caps + admission control; the verify-token loop discipline;
substrate-state-claim integrity (the never-over-claim invariant); the `Profile`
abstraction; the WAL + concurrency model; logging/observability conventions. Each
subsection frames the concept inline and links its ADR / `docs/security.md` /
code home for depth.

**Interim source:** [docs/security.md](../security.md) + the `docs/adr/` set.

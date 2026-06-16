---
arc42-section: 5
revisit-triggers:
  - a top-level component is added or removed
  - the PaneProfile shape changes
  - the substrate-vs-adapter boundary moves
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §5 Building Block View

**Phase-1 status: spine placeholder.** Content lands in **PR-B** of #386. The
file exists now so the [Arc42 index](README.md) is complete and this section
reads as *planned*, not *missing*.

**Will cover:** the static system component view — the bus DB, the per-agent
mailmen, the MCP servers, the adapter binaries, the hook-helper, and the
observe-gate substrate, with each component's responsibility and its links. The
component shape is stable; per-component internal evolution (e.g. `Profile`
content) does not invalidate the static view, so this is a thin component map in
Phase 1 — deep per-component internals are a later phase.

**Interim source:** [docs/reference.md](../reference.md) + the `internal/` and
`cmd/` package layout.

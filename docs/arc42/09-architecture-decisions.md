---
arc42-section: 9
revisit-triggers:
  - a new ADR is accepted (this index gains an entry)
  - an ADR is superseded or retracted (supersession chain updates)
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §9 Architecture Decisions

**Phase-1 status: spine placeholder.** The Arc42 entry-point framing + the
supersession-chain view land in **PR-B** of #386; an auto-index-gen script is a
later phase. The file exists now so the [Arc42 index](README.md) is complete.

By construction, §9 points at the existing decision records under
[`../adr/`](../adr/) — that directory is the source of truth, indexed by
[`../adr/README.md`](../adr/README.md) (active decisions + status + supersession
notes). The ADR discipline itself (format, 350-line cap, lifecycle) is defined in
[ADR-0006](../adr/0006-adr-length-cap-and-background-docs.md); the decision to
adopt this Arc42 spine is [ADR-0015](../adr/0015-adopt-arc42-architecture-spine.md).

**Interim source (already the real index today):** [`../adr/README.md`](../adr/README.md).

---
arc42-section: 11
revisit-triggers:
  - a new risk / soft-fail class is discovered (append — continuous-add register)
  - a documented risk is resolved (archive it, don't delete)
  - a latent bug is promoted to a tracked issue or fixed
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A spine stub — #386) -->

# §11 Risks and Technical Debt

**Phase-1 status: spine placeholder.** The initial risk register + the
continuous-add discipline land in **PR-B** of #386. The file exists now so the
[Arc42 index](README.md) is complete and this — the highest gap-fill section
(nothing documents project risks today) — reads as *planned*, not *missing*.

**Will cover:** an initial, categorized risk register treated as a
**continuous-add register** (new risks append, resolved risks archive — not a
point-in-time snapshot), seeded from the known classes: cosmetic soft-fail races
(`delivered_in_input_box`), latent/deferred bugs (e.g. the P3 codex under-clear,
#360), known-stale substrate (codex paste-capability will evolve), and
substrate-state-claim-integrity gaps (#357 closed one such class). Each risk
names the design choice (or open issue) that addresses it, mirroring Binnacle §11.

**Interim source:** [docs/failure-modes.md](../failure-modes.md) +
[docs/diagnostic-playbook.md](../diagnostic-playbook.md).

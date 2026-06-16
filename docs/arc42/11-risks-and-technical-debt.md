---
arc42-section: 11
revisit-triggers:
  - a new risk / soft-fail class is discovered (append — continuous-add register)
  - a documented risk is resolved (archive it, don't delete)
  - a latent bug is promoted to a tracked issue or fixed
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-B content pass — #386) -->

# §11 Risks and Technical Debt

This is a **continuous-add register**, not a point-in-time snapshot: a newly
discovered risk *appends* here, a resolved one is *archived* (struck/dated, not
deleted) so the history stays legible. Each entry names the design choice or
tracking issue that addresses it. This is the highest gap-fill section — nothing
documented project risk before #386. Depth: [docs/failure-modes.md](../failure-modes.md),
[docs/diagnostic-playbook.md](../diagnostic-playbook.md).

## §11.1 Cosmetic soft-fail races

| Risk | Addressed / status |
|---|---|
| A paste that reaches the input but can't be verify-confirmed as submitted | **By design, not a bug** — honestly marked `delivered_in_input_box`; `resend` recovers (#157). Substrate-state-claim integrity ([§8.2](08-cross-cutting-concepts.md)) makes this honest rather than silent. |
| Small race between observing a pane state and the paste landing | Covered by the post-hoc verify-token + `delivered_in_input_box` net ([§6](06-runtime-view.md)). |

## §11.2 Latent / deferred bugs

| Risk | Addressed / status |
|---|---|
| P3 codex multi-line under-clear (residual lines compound with a paste) | Mitigated by clear-by-line-count (2 presses/line); deeper per-adapter compat tracked in **#360 / #420**. |
| Per-(command, adapter) `/mcp` `/cost` `/compact` compat surface | Narrow fix landed (#419); broad surface tracked in **#420**. |

## §11.3 Known-stale substrate (will evolve)

| Risk | Addressed / status |
|---|---|
| Codex paste-capability is a v1 assumption; the adapter will evolve | Isolated behind the `Profile` abstraction ([§8.5](08-cross-cutting-concepts.md)) so evolution is adapter-local, not a substrate change. |
| `RELEASE_TOKEN` master-PAT stopgap in the release workflows | Least-privilege follow-up tracked in **#423**. |

## §11.4 Substrate-state-claim-integrity gaps

| Risk | Addressed / status |
|---|---|
| Post-deploy MCP/DB-binding divergence (process writes an orphaned inode) | `doctor` detects divergence + exits non-zero; `refresh-all-mcps` rebinds (#348). |
| A delivery-claim class that over-claimed (now closed) | Closed by #357; recorded here as a resolved-but-historical integrity class. |

## §11.5 Documentation debt

| Risk | Addressed / status |
|---|---|
| The Arc42 spine itself can rot if not maintained | Mitigated by the link-first principle + the two-layer revisit discipline ([README](README.md)) — the doc-state-claim-integrity NFR ([§10](10-quality-requirements.md)). |
| Off-PR-net doc surfaces drift on a rename/path change | The #495 docs-coherence gate (cut-time checklist) surfaces them; the Arc42 Layer-1 staleness check is its sibling. |

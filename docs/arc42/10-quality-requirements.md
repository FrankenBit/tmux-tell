---
arc42-section: 10
revisit-triggers:
  - the collaborative working session refines or adds an NFR
  - a quality goal gains or loses a substrate mechanism that realizes it
  - operator pivot on what "good" means for the project
---
<!-- last-reviewed: 2026-06-16 (Phase-1 PR-A seed — #386; NOT canonical) -->

# §10 Quality Requirements

> **SEED — not the canonical set.** Per #386, §10 content is decided in a
> **collaborative working session** (operator + Quartermaster + Herald +
> Surveyor, the last for substrate-claim-integrity validation), scheduled
> separately; Herald scribes the consensus into the canonical set in **PR-C**.
> What follows is the substrate-empirical *seed* — the starting list for that
> session, not the answer. Do not cite this as settled.

The quality goals are deliberately **substrate-honest**, not enterprise-QAR: they
fit tmux-tell's single-host, paste-delivery shape — no p99/SLO frameworks (#386
§What-this-does-NOT-do).

## Seed NFRs (for the working session)

1. **Reliability** — every paste is verify-confirmed or honestly marked
   `delivered_in_input_box`; mailmen self-recover from stuck-state (backoff +
   park, #297); the DB is durable through crashes (WAL + `BEGIN IMMEDIATE`, #29).
2. **Substrate-state-claim integrity** — never claim `delivered` without verify
   (the load-bearing invariant); classify failures with structured reasons (#358
   reachability classes); attribute senders provably (no "the mailman did it"
   black hole).
3. **Non-clobber safety** — the observe-gate defers during operator typing (#105);
   paste-incapable adapters force-defer (#333); pre-paste cursor + agent_state
   probe; codex ghost-text recognized as empty (#369).
4. **Bounded latency** — verify-token ≤ 5s budget; mailman ~3–15s loop cadence;
   hook-context drains on next turn; `refresh-all-mcps` completes within ~30s for
   a ~9-agent fleet (empirical).
5. **Operator-recoverability** — `refresh-all-mcps` (stale binding), `resend`
   (soft-fail #157), `install.sh` hard-cut (#349), `db migrate` (atomic #349),
   `doctor` (divergence #348).
6. **Cap-bounded resource use** — `capRecipientQueue` (2/recipient) +
   `capSenderBacklog` (2/(sender,recipient), #296) prevent runaway-storm DoS;
   `refresh-all-mcps` is cap-protected + operator-explicit-only.
7. **Identity guarantees** — sender resolves to exactly one agent via
   `TMUX_AGENT_NAME` ‖ `TMUX_PANE` → `agents.pane_id`; codex MCP env-block
   discipline (#320/#384); identity-precedence chain documented + enforced.
8. **Compositionality** — substrate-vs-adapter boundary ([ADR-0009](../adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md));
   per-adapter `Profile` (#322); hook-context-vs-paste delivery modes (#249);
   adapter binaries are thin entry-points around shared `internal/`.
9. **Doc-state-claim integrity** — the docs don't over-claim freshness; the
   revisit-trigger discipline (this spine's `revisit-triggers` frontmatter +
   `last-reviewed` markers + the Layer-1 cut-time check) **is** this NFR's
   mechanism — the same shape as the substrate's verify-token integrity, applied
   to the documentation layer.

**Interim source for the seed:** the [#386](https://git.frankenbit.de/frankenbit/tmux-tell/issues/386)
§10 brainstorm seed.

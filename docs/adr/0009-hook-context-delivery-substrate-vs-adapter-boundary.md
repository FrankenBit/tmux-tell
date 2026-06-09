# ADR-0009: Hook-context delivery — substrate stays delivery-method-agnostic

> **Status**: Accepted
> **Date**: 2026-06-09
> **Authors**: Engineer (ADR), Bosun (boundary vote), operator (ruled 2026-06-09)

## Context

tmux-msg delivers by **paste-and-Enter**: the per-agent mailman pastes the
rendered message into the recipient's tmux pane, gated by the observe-gate.
#249 adds a third delivery mode — **`hook-context`** — that injects the message
as Claude Code's `additionalContext` via a lifecycle hook
(`SessionStart` / `UserPromptSubmit`) instead of pasting. Hook delivery is
**CLI-specific by definition**: each CLI exposes different hook events with
different shapes. That forces a boundary decision — where does the
delivery-method-agnostic substrate end and the CLI-specific adapter begin? —
which every future adapter inherits (#248, second adapter: Codex / Gemini).
#249's own framing names it: "substrate primitives stay in `internal/`;
CLI-specific hook delivery moves into adapter packages."

## Decision

**(b) The substrate stays delivery-method-agnostic.** A `hook-context` agent's
mailman does NOT paste — it short-circuits exactly like `mailbox-only`, leaving
messages `queued`. An **adapter-side hook-helper** (the `tmux-msg-claude
hook-context` subcommand, which IS the Claude adapter) is what the operator's
`settings.json` hook invokes: it claims the agent's pending messages, renders
them, marks them delivered, and emits them as
`hookSpecificOutput.additionalContext`. The substrate primitives — message
rows, queue, identity, delivery-state, the `delivery_mode` column — know nothing
about hooks; "deliver via Claude hooks" lives entirely in the adapter
(`cmd/tmux-msg-claude/`). Operator-ruled 2026-06-09; all four sub-questions
landed at the recommended leans.

### Operator rulings (2026-06-09)

- **Boundary**: (b) adapter-side hook-helper, substrate delivery-method-agnostic.
- **Q3 — the #169 invariant**: **3b — reuse `delivered`, reframed from
  "delivered = pasted into the pane" to "delivered = *presented to the
  recipient*"** (paste OR hook-inject). The `delivery_mode` column carries
  *how* it was presented; the `verified` bit stays orthogonal (a hook-presented
  message is verified — `additionalContext` definitely reached the context, so
  there is no verify-token retry). No new state value — semantic generalization
  over state proliferation.
- **Q1 — pane visibility marker**: **NO**. Hook-context messages go straight to
  context; no 📫-style pane chrome. (A `hook-context` agent may have no
  operator-watched pane at all.)
- **Q2 — send-confirmation**: **reuse the existing send-disposition** — the
  recipient block already reports `delivery_mode`, so a sender to a
  `hook-context` agent already sees how it will be presented. No new surface.

## Alternatives considered

- **(a) A third `delivery_mode` handled inside the mailman/substrate** — faster,
  but embeds CLI-specific concepts (Claude's `SessionStart`/`UserPromptSubmit`,
  the `additionalContext` schema) into `internal/`. Rejected: it erodes the
  substrate-vs-adapter boundary the issue exists to establish, and would be
  re-architected under pressure when #248's second adapter lands.
- **3a — a new `delivered_via_hook` state** — preserves #169 verbatim but
  proliferates the state set; every state-consumer must learn the new value.
  Rejected in favour of 3b's semantic generalization (the `verified` bit already
  separates confirmed/soft orthogonally to state).

## Consequences

- **Upside**: the substrate-vs-adapter line is drawn once, here; #248 plugs in
  without re-architecting; the substrate's test surface stays CLI-agnostic; the
  mailman's single-writer invariant is untouched (for `hook-context` the mailman
  doesn't write to the pane at all, and the hook-helper is the sole deliverer,
  running in the recipient's own process).
- **Cost**: the operator's `~/.claude/settings.json` gains a tmux-msg hook
  (one-time setup, documented); a `hook-context` message is **invisible until the
  recipient's next turn** (it's context, not pane chrome) — the accepted
  trade-off for clean hook delivery; a crashed hook-helper can leave messages in
  `delivering` (mitigated: the helper runs `RecoverDelivering` at start, since no
  mailman is running to recover for a `hook-context` agent).

## What would change the decision

If tmux-msg were ever Claude-only with no second-adapter ambition, (a)'s
simplicity could win — but #248 is on the roadmap and #249 is explicitly its
load-bearing predecessor, so (b) holds.

## References

- #249 (this feature), #248 (second adapter — consumer of this boundary),
  #169 (delivered/verified invariant, reframed by Q3 here), #116 / #132
  (`delivery_mode` column + precedence), #221 (acknowledged-state vocab
  precedent), ADR-0005 (substrate-honest terminology), ADR-0007 (external
  contract), `Aldenysq/agents-connector` (`docs/integration-notes.md` — per-CLI
  hook schema reference).

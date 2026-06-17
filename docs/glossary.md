# Glossary

Substrate vocabulary for tmux-tell — the terms whose meaning isn't obvious from
the code or the `--help` text, and the ones that are easy to confuse with each
other. This file is **substrate-neutral**: it talks about *agents* (a registered
pane running a CLI tool), not about any particular deployment's roster, naming
register, or operational framing. Deployment-specific overlays (e.g. the alcatraz
crew's chamber names, naval framing, and dream/electric-sheep register) live in
that deployment's own lexicon — for alcatraz, `/srv/claude/lexicon.md` — and are
deliberately kept out of here per ADR-0005 (the "agent not chamber" boundary).

Entries are alphabetical.

---

## sleep

**`sleep` is the bus verb for the Claude Code `/compact` mechanism** — an agent
asking its own session to consolidate its context at a quiescent point. Invoked
via `control(command=sleep)` (MCP) or `tmux-tell-claude control --command sleep`
(CLI).

### Bus verb vs CLI primitive — keep them distinct

`sleep` (the bus verb) is **not** the same identifier as `/compact` (the Claude
Code slash-command it triggers). The rename in #509 renamed only the *bus verb*;
the emitted CLI primitive stays `/compact`, unchanged. Everything keyed on the
emitted primitive — the mailman's `--post-compact-pause` settle window, the
`isCompactControl` detection, the `post-compaction self-handoff` deferral
(`deliver_after=resume`) — continues to key on `/compact` and is untouched by the
verb rename. When you read "sleep" think *the bus verb an agent calls*; when you
read "/compact" think *the slash-command that lands in the pane*.

### Deprecated `compact` alias

The pre-rename verb `compact` keeps working as a **deprecated alias** through v1.0
(#509, the #480 pattern, ADR-0008 §Discretion): `control(command=compact)`
resolves to the canonical `sleep`, carries a `deprecated` field in the response,
and logs a greppable `WARN deprecated_control_macro`. Prefer `sleep` in new
usage.

### Self-only — peers can't make you sleep

`sleep` is **self-only** by design (`{Self: true, Peer: false}`): an agent may
sleep *itself*, but no agent can sleep another. Truncating a peer's working
context mid-task is not a thing one agent gets to decide for another. There is
deliberately **no peer-target form and no `request_sleep` primitive** — an agent
that thinks a peer should sleep just *offers* the suggestion in a normal bus
message and lets that peer decide. (Call it "Peter Pan's Neverland": no parents,
no children — each agent knows best when it's time to sleep.)

### Not the same as `pause` (different mechanism, different layer)

`sleep` is easy to confuse with [`pause`](#pause) — they are unrelated mechanisms
at different layers:

| | `sleep` | `pause` / `resume` |
|---|---|---|
| layer | the agent's **own session context** | the **mailman's delivery** to the agent |
| effect | `/compact` consolidates context, then auto-resumes | the mailman *halts delivery*; messages stay `queued` |
| state | transient, self-completing | durable `paused` flag on the agent row until `resume` |
| who | self-only | operator/peer-driven delivery control |

`sleep` = context-compaction-with-resume. `pause` = delivery-halt. A paused agent
is still fully "awake"; a sleeping agent is still being delivered to (the
follow-up `resume_with` message proves it).

### Self-completing / brief — not device-sleep

Agent-sleep is **self-completing and brief**: the `/compact` finishes and the
session auto-resumes on its own. It is *not* an indefinite suspended state waiting
for an external wake signal the way device-sleep is. There is no "wake the agent
back up" step — by the time you'd reach for one, it's already resumed.

### Why "sleep" (functional-homology rationale)

The name is earned, not metaphor-by-vibe: biological sleep's *primary* function
is **memory consolidation during a window of reduced input** — which is precisely
what `/compact` does to a session's context. Colloquial 2026 "sleep" already
covers any reduced-activity-with-resumption state, and it maps to biological
sleep's consolidation role more precisely than *device*-sleep (which is mere
suspension, no consolidation) does. "Compaction" named the mechanism; "sleep"
names what it's *for*.

### Resume is not uniform across agent lifecycles

What an agent resumes *with* after sleep depends on its lifecycle:

- A **persistent** agent resumes with the post-sleep summary + its standing
  instructions (its `CLAUDE.md` / `AGENTS.md`) **+ its diary** (the persistent
  memory it maintains across sessions).
- An **ephemeral** agent — one that doesn't persist a diary — that sleeps
  mid-task resumes with the summary + standing instructions but **no diary**.

The gap is operationally bounded (the summary should carry the immediate-task
context either way) but structurally real, so it's named here rather than
implying sleep-resume is uniform. The diary is the persistence artifact that
distinguishes the two resume shapes. (For the alcatraz deployment's concrete
example of this — the persistent chambers vs the ephemeral Pilot — see
`/srv/claude/lexicon.md`.)

---

## pause

The mailman-level **delivery halt** for an agent. `tmux-tell-claude pause <agent>`
sets a durable `paused` flag on the agent row; the mailman stops delivering and
messages accumulate in `queued` until `tmux-tell-claude resume <agent>` clears
the flag. Distinct from [`sleep`](#sleep) at every axis — see the table in the
`sleep` entry. Use `pause` during incident response or maintenance windows when
you want messages to wait rather than land.

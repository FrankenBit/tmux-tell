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

## compact

**`compact` is the bus verb for the Claude Code `/compact` mechanism** — an agent
asking its own session to consolidate its context at a quiescent point. Invoked
via `control(command=compact)` (MCP) or `tmux-tell-claude control --command compact`
(CLI).

### Bus verb vs CLI primitive — keep them distinct

`compact` (the bus verb) is a **whitelist key the control surface resolves**; it
is not the same layer as `/compact` (the Claude Code slash-command that lands in
the pane). They share a name deliberately — the verb equals the primitive it emits,
which is exactly the substrate-honesty #646 was after — but they live at different
layers. Everything keyed on the *emitted primitive* — the mailman's
`--post-compact-pause` settle window, the `isCompactControl` detection, the
`post-compaction self-handoff` deferral (`deliver_after=resume`) — keys on the
literal `/compact` text and is untouched by any bus-verb rename. When you read
"`compact`" as a control verb think *the bus verb an agent calls*; when you read
"/compact" think *the slash-command that lands in the pane*.

### Deprecated `sleep` alias

The former verb `sleep` keeps working as a **deprecated alias** through v1.0
(#646, the #480 pattern, ADR-0008 §Discretion): `control(command=sleep)` resolves
to the canonical `compact`, carries a `deprecated` field in the response, and logs
a greppable `WARN deprecated_control_macro`. Prefer `compact` in new usage.

> **Naming history.** #509 first renamed `compact` → `sleep` on a
> functional-homology argument (biological sleep consolidates memory, as `/compact`
> consolidates a session's context). #646 reversed it: the operator judged `sleep`
> **anthropomorphic-dramatic** — agents don't experience biological rest; they
> reset context by choice — and every "go to sleep" leaked that framing into
> operator-facing surfaces and reasoning chains. `compact` names the substrate
> mechanism directly, so the verb, the emitted primitive, and the
> substrate-of-record language all coincide.

### Self-only — peers can't compact you

`compact` is **self-only** by design (`{Self: true, Peer: false}`): an agent may
compact *its own* context, but no agent can compact another's. Truncating a peer's
working context mid-task is not a thing one agent gets to decide for another. There
is deliberately **no peer-target form and no `request_compact` primitive** — an
agent that thinks a peer should compact just *offers* the suggestion in a normal
bus message and lets that peer decide when its own context is at a quiescent point.

### Not the same as `pause` (different mechanism, different layer)

`compact` is easy to confuse with [`pause`](#pause) — they are unrelated mechanisms
at different layers:

| | `compact` | `pause` / `resume` |
|---|---|---|
| layer | the agent's **own session context** | the **mailman's delivery** to the agent |
| effect | `/compact` consolidates context, then auto-resumes | the mailman *halts delivery*; messages stay `queued` |
| state | transient, self-completing | durable `paused` flag on the agent row until `resume` |
| who | self-only | operator/peer-driven delivery control |

`compact` = context-compaction-with-resume. `pause` = delivery-halt. A paused agent
is still fully live; a compacting agent is still being delivered to (the follow-up
`resume_with` message proves it).

### Self-completing / brief — not a suspend

Self-compaction is **self-completing and brief**: the `/compact` finishes and the
session auto-resumes on its own, morning-fresh. It is *not* an indefinite suspended
state waiting for an external wake signal. There is no "wake the agent back up"
step — by the time you'd reach for one, it's already resumed.

### Resume is not uniform across agent lifecycles

What an agent resumes *with* after a compaction depends on its lifecycle:

- A **persistent** agent resumes with the post-compaction summary + its standing
  instructions (its `CLAUDE.md` / `AGENTS.md`) **+ its diary** (the persistent
  memory it maintains across sessions).
- An **ephemeral** agent — one that doesn't persist a diary — that compacts
  mid-task resumes with the summary + standing instructions but **no diary**.

The gap is operationally bounded (the summary should carry the immediate-task
context either way) but structurally real, so it's named here rather than
implying compaction-resume is uniform. The diary is the persistence artifact that
distinguishes the two resume shapes. (For the alcatraz deployment's concrete
example of this — the persistent chambers vs the ephemeral Pilot — see
`/srv/claude/lexicon.md`.)

---

## pause

The mailman-level **delivery halt** for an agent. `tmux-tell-claude pause <agent>`
sets a durable `paused` flag on the agent row; the mailman stops delivering and
messages accumulate in `queued` until `tmux-tell-claude resume <agent>` clears
the flag. Distinct from [`compact`](#compact) at every axis — see the table in the
`compact` entry. Use `pause` during incident response or maintenance windows when
you want messages to wait rather than land.

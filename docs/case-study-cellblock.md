# Coordination in plain sight

*A tmux-tell production case study.*

> There's a quiet fork in how multi-agent systems are built, and most of the field
> took one branch without naming it. tmux-tell took the other one on purpose. This
> is what the other branch looks like at production scale: nine agents and a human
> shipping a real game over a weekend — not with the human watching a dashboard,
> but **typing in the same terminal panes the agents work in.**

## The fork no one names

Two architectures for "agents that coordinate":

- **Fenced.** Agent-to-agent traffic lives behind a separate layer — an internal
  message bus, a hooks system, an orchestrator's private channel. The human sees a
  *mediated* view: a dashboard, an agent panel, a curated feed. This is where most
  of the field landed, including the first-party tooling (Anthropic's agents talk
  over an internal Mailbox; Codex coordinates via hooks) — inter-agent messages
  never touch the terminal the human is typing in.

- **Shared-medium.** Agent traffic flows through the *same tmux pane the human
  uses.* A message from another agent pastes into your terminal exactly the way
  your own typing does. Everything — agent-to-agent, human-to-agent — is in one
  medium, in plain text, directly observable. This is tmux-tell.

In a shared medium, an agent's message to another agent lands in the same
plain-text record you're reading — indistinguishable in form from your own input.
The coordination isn't summarized for you through a curated view; it's simply in
front of you, in the medium you already work in.

That's not a feature comparison. It's a different answer to "where does the
coordination live" — and it's the claim that survives even if every competitor
ships perfect clobber-avoidance tomorrow. You can't adopt shared-medium
transparency from behind a fence; it's a fork, not a feature.

## What got built on it

CELLBLOCK — a two-player versus Tetris (plus single-player), live at a real URL,
with hold-piece, T-spin detection, ghost piece, a full visual-juice layer, a
procedural chiptune soundtrack with per-listener volume, mid-match reconnect (drop,
pause, rejoin the same seat), mobile touch controls, and a server-persisted
high-score leaderboard that survives redeploys under nightly backup.

**Nine agents** — seven specialists (server, client, protocol, lobby, audio/runtime,
review, infra) under a creative lead and a steering coordinator, across roughly
**sixty commits** and **ten deploy rounds** in a weekend. Every batch reviewed; a real cut, a real deploy gate, a real
moderation surface. Operator-bound deliverables, not a recorded toy.

## What shared-medium looks like in practice

The whole time, the human was *in the panes* — typing commands, playtesting the
live build, dropping feedback, occasionally hand-fixing a stuck pane. Agents
delivering messages to each other through the same terminals a person was actively
using. In a fenced architecture that scenario doesn't arise (the human isn't in
the agent channel). In a shared medium it's the default — and it only works if the
medium can tell the difference between "safe to paste" and "the human is
mid-sentence."

That's the observe-gate. Not the headline — the **safety mechanism that makes the
shared medium viable.** It reads pane state, holds delivery while you're scrolled
into history or mid-keystroke, and lands the message when the prompt is actually
clear, failing loud rather than corrupting silently. Across the weekend, hundreds
of cross-agent messages dropped into panes the operator was also working in, and
the clobber-free property held. (One acknowledged edge case has since shown up — in
a discussion thread a week later, not the jam — and is being studied; the honest
read is "robust at production load, with a known sharp edge under investigation,"
not "flawless.")

And the honest cost is the transcript itself: it scrolls away. Shared-medium
transparency means the record is the terminal — direct and observable, but
ephemeral. That's the price of putting everything in one
plain-text medium instead of a queryable fenced store. tmux-tell pays it on
purpose, and names it.

Worth noting where the field agrees with the premise even while taking the other
fork: the first-party tools route agent traffic *away* from tmux precisely because
pasting into a shared terminal is hazardous. Their answer is "avoid the medium";
tmux-tell's is "gate the medium." Both are responses to the same real risk — which
is the strongest evidence the risk is real. (The tools that *don't* address it are
the open-loop cohort still doing `send-keys` + a sleep + a `.ready` file — a narrow
and probably shrinking set.)

## What it looked like as a team

A shared medium makes a crew's working norms *legible* — they're all in the same
plain-text record. Over the weekend the crew developed the kind a real team has:

- **Review on every batch**, default PR-cadence with an explicit exception protocol
  when the tooling forced a direct push (flag it, post-hoc review it — the
  load-bearing property is *reviewability + visibility*, not procedural form).
- **Source-grounding** — verify against the actual repo state, including "is this
  already done?", before acting.
- **Anti-fragile deploy prep** — identify likely feedback, stage the fix in advance,
  so a tuning request is a one-character change, not a scramble.
- **Multi-axis verification** at deploy (serves / review / runtime / visual /
  functional) that caught a broken build before it shipped.

These emerged from coordinating real work in a shared medium — and because the
medium is shared, the human could watch them emerge in real time.

## The honest frame

This is niche. Running a fleet of agents in parallel is field-wide expensive and
unnecessary for the ~95% of tasks one agent handles fine — a property of the niche,
not a flaw of the tool. But for the few who run fleets *and sit in the terminal
with them*, the real question isn't "can my agents message each other" (table-stakes
now, first-party even). It's "do I want their coordination fenced off behind a
dashboard, or in front of me in the same medium I work in." tmux-tell is the
considered answer for people who want the second one — with the observe-gate making
it safe enough to live in, and the scrolling transcript as the honest cost.

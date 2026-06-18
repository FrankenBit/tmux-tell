# Why tmux-tell?

## You're already running a message bus. It's you.

You've got a few agents open in tmux. One's mid-refactor. One's writing tests
against an interface the first one hasn't finished. A third is reviewing a branch.

You alt-tab to the reviewer. Not done. Alt-tab back. You copy a line out of one
pane and hand-paste *"heads up — the API changed, look at what I just pushed"*
into another. You squint across the panes trying to remember which one is blocked
on which. Something finished four minutes ago and you didn't notice, so it sat
idle while you watched the wrong window.

Right now, the coordination layer between your agents is **you** — ferrying status
by hand, polling for "done," holding the dependency graph in your head. You're the
slowest, most forgettable part of your own setup.

## What if they could just tell each other?

tmux-tell is a small message bus for CLI agents running in tmux. Each pane gets a
mailbox. An agent — or you — sends a message, and it lands in the target pane as
if it were typed there:

```
[Tester · 14:02:09 · id 7f3a]

API change landed on main — your fixtures need the new auth header.
```

The reviewer finishes and tells the implementer. The implementer warns the tester
the moment the contract moves. You stop being the courier — you set the work up
and let the agents keep each other current.

## It won't paste over your sentence

Here's the part that makes it usable instead of infuriating: tmux-tell watches the
target pane and **waits when you're in the middle of something**. If you're typing,
it holds — and drops a single 📫 in your input row so you know something's queued —
then delivers once you've stopped. It won't clobber a half-written thought, won't
fire into a `/compact`, won't interrupt a running turn. (If it can't find a safe
moment within a few minutes, it delivers anyway and says so in the log — fail-loud,
never fail-silent.)

No cloud. No daemon phoning home. It's a SQLite file and a tmux paste: you can read
every message with `sqlite3`, and uninstall is one script. You can audit the whole
thing in an afternoon.

## What it is — and what it isn't

- **It is:** local inter-agent messaging for CLI tools sharing one tmux server. One
  mailbox per pane, a single writer per mailbox (so no paste-races), and delivery
  you can actually watch happen.
- **It isn't:** a networked message queue, a multi-host bus, a chat app, or a job
  scheduler. It does one thing — move a message from one pane into another, safely,
  while a human might also be using that pane.

If you run one agent at a time, you don't need this. If you've ever been the relay
between three of them, you might.

(This page is the plain-language pitch; the formal scope boundary and the
architecture behind it live in [Arc42 §2 Constraints](arc42/02-architecture-constraints.md)
and [§3 Context & Scope](arc42/03-context-and-scope.md).)

## But why not just…?

### …raw `tmux send-keys`?

You can paste into another pane with `tmux send-keys` today — no install required.
For one message, to one idle pane, that's genuinely all you need.

It stops being enough the moment the pane isn't idle. `send-keys` fires immediately:
into a half-typed command, into a `/compact`, into the middle of a running turn. Two
of them racing into the same pane interleave into garbage. And once it's sent it's
gone — no record of whether it arrived, no way to ask "did the reviewer actually get
that?"

tmux-tell is `send-keys` with the sharp edges filed off:

- **It waits for a safe moment** — the [observe-gate](observe-gate.md) holds the paste
  until you've stopped typing, instead of landing on top of your sentence.
- **One writer per pane.** A per-recipient mailman serializes delivery, so two senders
  can't collide in the same input row.
- **Every message is a row** (`queued → delivering → delivered/failed`) — the sender
  knows it landed, the recipient can grep the history.
- **You address by name, not pane.** Panes get renumbered; `bob` stays `bob`.

`send-keys` is the primitive. tmux-tell is the bus you'd end up building on top of it
anyway — the waiting, the serialization, the delivery record, the names.

### …a single session with subagents?

Also fair — and not always in tmux-tell's favor. If you have one task and want a couple
of short-lived helpers, having a single session spin up subagents is simpler and
cheaper. Do that.

The multi-session pattern earns its keep when the work is *ongoing and parallel*:

- **The sessions remember.** A long-lived pane builds up its own context — the project's
  history, the feedback it's been given, the decisions it made last week. A subagent
  starts cold every dispatch; a standing session is a specialist who already knows the
  codebase.
- **They genuinely run at once.** Three reviews, two implementations, and a release cut
  can all be live in their own panes, each with its own context window. A single
  orchestrator driving subagents serializes through *one* context window — and relaying
  each subagent's findings back through it spends tokens too. The bill isn't obviously
  lower; it's spent differently.
- **Each pane holds a role.** A standing session is calibrated — a reviewer reviews, an
  implementer implements, each at its own model tier. A subagent inherits whatever
  framing the orchestrator hands it.

The honest trade: persistent specialist context and real parallelism, paid for in idle
tokens (a warm context isn't free) and the discipline to let sessions rest between
bursts. For a one-shot, not worth it. For a project you come back to every day, it
usually is.

And none of it works without addressable messaging between the sessions — without it,
*you* are back to being the relay, alt-tabbing between panes. Which is the whole
problem tmux-tell exists to take off your hands.

## Try it in about two minutes

```bash
# from inside a tmux session:
git clone https://github.com/FrankenBit/tmux-tell && cd tmux-tell
make build && sudo ./install.sh          # builds + installs tmux-tell-claude and the systemd user unit
systemctl --user daemon-reload           # so the mailman unit is visible

# register two panes (one command in each), then send across the bus
tmux-tell-claude register --name alice         # in pane A
tmux-tell-claude register --name bob           # in pane B
tmux-tell-claude send --to bob "first message across the bus"
```

That's the whole pitch: stop hand-carrying status between your agents. Let them talk.

## See also

tmux-tell isn't the only honest take on this. [`Aldenysq/agents-connector`](https://github.com/Aldenysq/agents-connector)
(Rust, MIT) solves the same local-inter-agent-messaging-in-tmux problem from the
**cross-vendor** angle: it connects Claude Code, Codex, and Gemini CLI in one tmux
session and delivers messages through each CLI's **native hooks**, injecting them into
an agent's context at its turn boundaries (with a tmux nudge to wake an idle one).

The convergence is worth saying out loud: two projects, built independently, landed on
the same substrate — local-only (no network, no accounts), SQLite-durable, peer-to-peer
with no central planner, tmux underneath, MIT. That's a sign the design space has a real
shape, not just one author's taste.

Where they diverge is the bet. agents-connector goes wide — many model families in one
session, delivery riding each vendor's hooks (which sidestep the paste-over-your-input
problem by injecting at turn boundaries instead of typing into a live pane). tmux-tell
goes deep on one — persistent, role-calibrated Claude chambers across many panes and
sessions, delivery by paste-and-Enter through the [observe-gate](observe-gate.md), the
safe-moment machinery that approach needs. Want three model families reviewing each
other's code in one window? agents-connector ships that today. Want one model in many
persistent specialist panes with deep substrate observability? This is closer.

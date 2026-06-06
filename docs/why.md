# Why tmux-msg?

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

tmux-msg is a small message bus for CLI agents running in tmux. Each pane gets a
mailbox. An agent — or you — sends a message, and it lands in the target pane as
if it were typed there:

```
[tester · 14:02:09 · id 7f3a]

API change landed on main — your fixtures need the new auth header.
```

The reviewer finishes and tells the implementer. The implementer warns the tester
the moment the contract moves. You stop being the courier — you set the work up
and let the agents keep each other current.

## It won't paste over your sentence

Here's the part that makes it usable instead of infuriating: tmux-msg watches the
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

## Try it in about two minutes

```bash
git clone https://github.com/FrankenBit/tmux-msg && cd tmux-msg
make build && ./install.sh

# register two panes (one command in each), then send across the bus
claude-msg register --name alice     # in pane A
claude-msg register --name bob       # in pane B
claude-msg send --to bob "first message across the bus"
```

That's the whole pitch: stop hand-carrying status between your agents. Let them talk.

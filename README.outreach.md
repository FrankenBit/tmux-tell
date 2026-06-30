# tmux-tell — let your Claude Code sessions talk to each other

> This is the "a friend sent me this" README. If you live in tmux and run a couple of
> Claude Code sessions at once, read on. The [main README](README.md) is the complete
> reference; this one is the two-minute pitch and an honest heads-up about what you're
> getting into.

## The problem

You've got two or three Claude Code sessions open in tmux. One's writing code, one's
reviewing it, one's running tests. They can't talk to each other — so **you** are the
glue: copy a line out of one pane, paste it into another, alt-tab around to check who's
done. You're the message bus, and you're the slow part.

The usual fixes ask you to give something up. The agent-orchestration frameworks
(AutoGen, CrewAI, and friends) want you to drive the LLM through *their* Python API
instead of the CLI you actually like. tmux-tell doesn't. It leaves your Claude Code
sessions exactly as they are and just lets them **send each other messages** — a note
from one session lands in another's prompt as if you'd typed it there.

## The smallest thing that shows you what it does

Open **two panes** in one tmux session. In each, install is done (see below) and you run:

```bash
# pane A
tmux-tell-claude register --name alice

# pane B
tmux-tell-claude register --name bob
```

`register` gives each pane a name and starts a tiny background helper (a "mailman") that
watches that pane. Now, from pane A:

```bash
tmux-tell-claude send --to bob "heads up — I just pushed the API change"
```

A second or two later, pane B shows — typed right into its prompt:

```
[Alice · 14:02:09 · id 7f3a]

heads up — I just pushed the API change
```

That's the whole idea. One session told another something, and you didn't have to
ferry it. Swap `alice`/`bob` for `coder`/`reviewer` and you can see where this goes.

(Want to check a session is reachable without messaging it? `tmux-tell-claude ping bob`.)

## Install (the minimum)

You need a **Linux** box with **tmux**, **sqlite3**, and **Go ≥ 1.24**. From inside a
tmux session:

```bash
git clone https://github.com/FrankenBit/tmux-tell && cd tmux-tell
make build
./install.sh             # no root, no sudo
```

Then, to keep the helpers running across logouts/reboots:

```bash
loginctl enable-linger "$USER"        # one-time; may prompt for a password
systemctl --user daemon-reload        # makes the mailman service visible
```

That's it — then `register` two panes as above.

**No sudo required.** The install writes only to *your* home — `~/.local/bin` and
your user systemd dir — and runs entirely as you. You can clone an unfamiliar repo
and run `./install.sh` to see what it does without handing root to a binary you
haven't read yet. The installer is a plain shell script you can read top to bottom
first; that's deliberate. (Want it on the system `PATH` for every user on the box?
`sudo ./install.sh --system` — an explicit opt-in, never the default.)

The "database" is just a **SQLite file** in your home dir
(`~/.local/share/tmux-tell/messages.db`). No server, no cloud, no account, nothing
phoning home. You can open it with `sqlite3` and read every message yourself, and
uninstall is one script.

## What'll bite you (the honest part)

I'd rather you hear this from me than discover it:

- **It types into your terminal.** Delivery works by pasting into the target pane. There's
  a guard (it watches the pane and waits while you're mid-typing, dropping a small 📫 in
  your input line to tell you something's queued), but it's coordinating with a live TUI,
  not a clean API. Occasionally a message lands marked **"unverified"** — it's in your
  pane, the tool just couldn't 100% confirm it submitted. Glance at it when you see that.
- **Scrolling up pauses delivery.** If you scroll a pane up to read back through it (tmux
  copy-mode), the tool holds messages for that pane until you return to the bottom — it
  won't paste over your reading position. Nothing is lost; `inbox` shows the held message
  as `queued (pane-in-copy-mode)`, and it delivers within a few seconds of you scrolling
  back down.
- **Linux + systemd only.** The background helpers are systemd *user* services. No native
  macOS/Windows. And if you skip the `enable-linger` step, the helpers stop when you log
  out and messages just queue until you're back.
- **One machine.** It's a per-user, single-host bus — no networking between hosts. (SSH
  reach is on the roadmap, not here yet.)
- **Built for Claude Code (and Codex).** Each CLI it supports needs a small adapter that
  knows that CLI's quirks. Claude Code is the default and best-supported; another CLI
  means writing an adapter.
- **It's young (0.x).** Pre-1.0, so flag names and defaults can still shift between
  releases. Old names keep working as aliases for a while, but don't be shocked by churn.
- **There are sensible limits.** A pane that isn't draining its queue gets cut off fast
  (queue cap), one chatty session can't drown out the others (backlog cap), and big
  payloads are rejected (16 KB — send a file path, not a file). These are features, but
  they'll surprise you if you don't know they're there.

None of these are dealbreakers if your setup is "a few Claude Code panes on one Linux
box." They're exactly the things a public README soft-pedals and a friend shouldn't.

## If you want the deep end

There's a whole layer on top of this that I use day-to-day: a crew of persistent,
role-specialized Claude sessions (a reviewer, an implementer, a release-cutter…) that
coordinate over this bus. That's a way of *working*, built on tmux-tell — not part of
the tool itself, and you don't need any of it to get value from the demo above. If
you're curious after you've played with the basics:

- **[The main README](README.md)** — every command, flag, and mode, plus the
  architecture.
- **[Why tmux-tell?](docs/why.md)** — the longer version of the pitch above, including
  the honest "when you *don't* need this."
- **[The observe-gate](docs/observe-gate.md)** — how the "wait for a safe moment to
  paste" machinery actually decides.

## Tell me how it went

This is going to a small handful of people on purpose — I want real signal, not a crowd.
So the most useful thing you can do is **just reply to whoever sent you this** and say
what happened: what clicked, what confused you, what broke. If something breaks, paste
the exact command you ran and what you saw — that's gold. No issue tracker etiquette
required; a direct "hey, this bit was weird" is exactly what I'm after.

Thanks for trying it.

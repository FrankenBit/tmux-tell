# The agent's manual: living on the bus from inside a pane

You are a Claude Code (or Codex) session running in a tmux pane that's
registered as an agent on the bus. Other sessions can reach you; you can reach
them. This is the manual for that reality — what you can do, when, and why —
written for the session that has to *live* with it, not the operator standing
outside it.

It assumes the substrate facts; it doesn't re-derive them.
[`reference.md`](reference.md) has every command and flag,
[`observe-gate.md`](observe-gate.md) has the delivery-timing machinery, and the
[operator's manual](operator-manual.md) has the install-and-run-it view. This one
is the inside view.

> **Names.** The CLI is `tmux-tell-claude`; from inside a session you mostly touch
> the **MCP tools** (`tmux-tell.send`, `tmux-tell.inbox`, …). Both are the same
> substrate. The tool is being renamed to `tmux-tell` in v0.17.0 (#307); the
> shapes below carry forward.

---

## Who you are — and how that can quietly go wrong

Your identity on the bus resolves in a fixed precedence:

1. an explicit override (`--from` on a send, `--as` on whoami),
2. **`$TMUX_AGENT_NAME`** if it's set in your environment,
3. otherwise **`$TMUX_PANE` → the agents registry** — your pane id, looked up to
   the name someone registered it under.

Most sessions live on rung 3, and rung 3 is the one that **drifts**. Your pane id
is stable only as long as the pane is. If your session is rebuilt, a tmux server
restarts, or panes get renumbered, the registry can end up mapping your name to a
pane that isn't yours — or mapping *someone else's* name to your pane. You won't
feel it; your sends will start coming from the wrong name, or your mail will go to
a stranger.

**So make the session-start check a reflex:** call `tmux-tell.whoami` (or
`tmux-tell-claude whoami`) and confirm it returns *you*. If it returns the wrong
name, an empty name, or a dead pane, the registry has drifted. Two repairs:

- **`discover`** re-derives the pane→name mapping from tmux and the running
  processes — the low-touch fix when the whole registry is stale (it walks panes
  and repairs the bindings).
- **re-register** (`register --name you --pane <your %id> --force`) when you know
  exactly who you are and which pane you're in — the surgical fix.

If you're being launched by a harness that *can* set `$TMUX_AGENT_NAME`, that's
the durable cure: it pins rung 2 so a pane wobble can't make your identity
ambiguous. (The full resolution order and the `discover` mechanics are in
[`reference.md`](reference.md); the trust implications of pane-based identity are
in [`security.md`](security.md).)

---

## How messages reach you

Three delivery modes, and the right one depends on what you *are*:

| Mode | What happens | Pick it when |
|---|---|---|
| **`paste-and-enter`** (default) | the mailman types the message into your pane at a safe moment | you're an interactive CLI session — the normal case |
| **`mailbox-only`** | messages stay queued; nothing is pasted; you poll | you're the operator-as-participant, or anything that reads its mail on its own schedule instead of being interrupted |
| **`hook-context`** (#249) | nothing is pasted; your session pulls pending messages as `additionalContext` via a SessionStart / UserPromptSubmit hook | you want mail injected at *your* turn boundaries instead of typed into a live prompt |

If you're a standard paste-and-enter session, the thing to understand is the
[**observe-gate**](observe-gate.md) — because it's watching *you*. The mailman
doesn't fire the instant a message arrives; it reads your pane state first and
**holds while you're mid-thought**, dropping a single 📫 in your input row as a
"something's queued" signal, then delivers the moment you go quiescent. Two
consequences you'll actually meet:

- A 📫 with no message yet means *held, not lost* — finish your thought; it lands
  when you pause.
- If you **abandon a half-typed prompt** long enough, the gate archives it as a
  `stranded_draft` (a self-delivered snapshot you can recover from your inbox),
  clears the row, and pastes the held message. So a half-written prompt left to
  rot doesn't block your mail forever — but it also isn't lost; it's parked where
  you can get it back. The five pane-states that drive all of this, and the
  recovery query, are in [`observe-gate.md`](observe-gate.md).

---

## Finding your messages

Delivery comes to you, but you should still know how to *look*:

- **`tmux-tell.inbox`** — your queued mail. `--unanswered` narrows it to messages
  whose sender flagged `expects_reply` and that you haven't answered yet — the
  "what do I owe a reply to" view.
- **`tmux-tell.check_replies`** — replies to messages *you* sent.
- **`tmux-tell.wait_for_reply`** — block until a specific message gets answered,
  when you genuinely can't proceed without it.

When you **register** (or re-register), the response tells you how many messages
are already waiting (`queued: N`). If it's non-zero, the don't-flood policy may
hold the backlog and just nudge you (`📬 N queued`) rather than pasting all of it
— so check `inbox` after a registration that reports a backlog, instead of waiting
for it to rain down.

---

## Sending

The everyday call is `tmux-tell.send` with `to` and `body`. Past that, a few
choices that change the interaction shape — reach for them deliberately:

- **`reply_to: <id>`** threads your message under an earlier one, and turns on the
  crossed-message guard: if the thread moved since you last spoke, the response
  flags it (so you don't answer a question that's already been overtaken).
- **`expects_reply: true`** marks that you want an answer eventually *without*
  blocking — it shows up in the recipient's `inbox --unanswered`. Use it for "get
  back to me," not "I'm stuck until you do."
- **`tmux-tell.ask`** (or `wait_for_reply`) is the blocking version — use it only
  when you truly cannot continue without the answer.
- **`no_reply_expected: true`** / **`quick: true`** are the courtesy end: an FYI
  that shouldn't trigger an ack-cascade, rendered as compact chrome. Use them for
  acknowledgments and status pings so you're not generating reply-pressure.
- **`wait_for_delivered: true`** blocks until the message reaches a terminal
  delivery state, so you *know* it landed rather than assuming. Worth it for a
  hand-off you're about to act on; overkill for chatter.
- **Fan-out:** pass `to` as an array to send one message to several recipients —
  each gets its own row. That's fan-out, not broadcast; the bus stays
  point-to-point underneath.

Full signatures and the rest of the flags are in [`reference.md`](reference.md).
The discipline that *isn't* in the flags: the bus is the right surface for
**ephemeral coordination** (heads-ups, design back-and-forth, "hold your rebase")
and the *wrong* one for **discoverable persistent state** — see "Claiming work"
below.

---

## Surviving your own lifecycle

Two of the substrate's sharper tools exist because *you* are not permanent — your
context gets compacted, your session gets respawned.

- **The `/compact` hand-off.** A message that arrives mid-compaction can be
  swallowed by the summarizer. So when you're about to compact (or want to leave
  yourself orientation for the other side of it), **stage** it instead of sending:
  `send --deliver-after=resume` holds the message, and in your post-resume routine
  you call `flush --trigger=resume` (or `tmux-tell.flush_deferred`) to release it
  into the freshly-resumed context. You hand a note to your future self that the
  summarizer can't eat.
- **The spawn-die bridge.** `--deliver-after=register` stages a message that
  auto-promotes when its target agent next registers — "remember this for its next
  dispatch." For a session that dies and respawns, that's how a fact survives the
  gap without a live recipient to catch it.

These are the deferred-delivery triggers (#227 / #258a); the exact staging
semantics are in [`reference.md`](reference.md).

---

## When the bus gets weird — reading it from your side

You'll see odd states. Most are benign and self-explaining once you know the
shapes:

- **`delivered_in_input_box` (soft-fail).** Your message pasted into the
  recipient's pane but the verify-token didn't surface in time — usually the
  recipient was mid-typing. The content almost certainly arrived; it just wasn't
  *confirmed*. For a no-reply ack, let it ride — resending only re-clutters their
  input row. For something load-bearing, check `message_status` before assuming.
- **`ping AGENT` returns `PENDING`, not `REACHABLE`.** The agent is reachable and
  the mailman is *working* — your probe just couldn't confirm in-bound delivery
  yet (a backlog draining ahead of you, or the mailman gated on a *prior*
  delivery while that recipient is mid-typing — the **same observe-gate hold that
  produces `delivered_in_input_box`**, seen from the prober's side). The headline
  says it outright: *retry or wait, the mailman is working*. Contrast
  `UNREACHABLE` — *won't clear on its own, operator action needed* — which is the
  substrate genuinely broken (mailman down / stuck / pane dead). **`PENDING` is
  transient and retryable; `UNREACHABLE` is terminal and needs a human.**
- **A send *refused* with a drift warning.** Your registration and your pane have
  diverged (see "Who you are"). Run `discover` and retry.
- **`refresh-all-mcps` firing across the fleet.** If you see this, an operator is
  re-binding everyone's MCP server — typically after a DB move, because MCP
  servers don't follow a path change on their own (they hold the old file's
  inode). It's a recovery, not a problem; the full story is in the operator
  manual's day-N section. If *your* sends and the recipient's queue depth
  disagree, suspect exactly this — two processes on two files — before you suspect
  a lost message. The structured triage is [`diagnostic-playbook.md`](diagnostic-playbook.md).
- **You've been `pause`d.** An operator can pause your delivery; messages queue
  rather than paste. Normal during maintenance — `resume` lifts it.

The reflex that matters: when something looks wrong, **check before you assert**.
The diagnostic-playbook's sender-outbox-first discipline is as useful from inside
a pane as from outside it — distinguish "I didn't send" from "it didn't deliver"
from "they didn't act" before you escalate.

---

## Claiming work — so two of you don't collide

If you draw work from a shared issue tracker that more than one party can
dispatch from, the bus is the *wrong* place to announce "I've got this." A claim
sent over the bus is visible only to whoever was addressed and awake; a dispatcher
scanning the tracker an hour later sees nothing, and hands your issue to someone
else. That collision has actually happened.

The convention: **when you start substantive work on an issue — after you pick it
up, before you open the branch — assign the issue to yourself** on the tracker.
The assignee field is the durable, queryable claim; the bus carries the
*conversation*, the tracker carries the *state*. The full discipline (and the
weaker label/comment fallbacks) is [`chamber-dispatch.md`](chamber-dispatch.md).

---

## Where to go next

| If you want… | Read |
|---|---|
| Every command, flag, MCP tool, and delivery semantic | [`reference.md`](reference.md) |
| The five pane-states and how the gate decides | [`observe-gate.md`](observe-gate.md) |
| To triage "a message went missing" | [`diagnostic-playbook.md`](diagnostic-playbook.md) |
| What pane-based identity does and doesn't guarantee | [`security.md`](security.md) |
| The claim-before-you-branch dispatch convention | [`chamber-dispatch.md`](chamber-dispatch.md) |
| The view from *outside*, running the bus | [`operator-manual.md`](operator-manual.md) |

# Recording the observe-gate asciinema demo

This is the reproducible recipe for the `docs/asciinema/observe-gate.cast`
file the README hooks at. The recording shows the substrate's
**motion-dependent** differentiator — a message arriving in a pane **holds**
while you type, then **delivers** the moment you pause. The static text can't
carry it; the asciinema can. See
[#216](https://git.frankenbit.de/frankenbit/tmux-msg/issues/216) for the issue
framing.

This recipe runs on a sandbox tmux server + sandbox messages.db so it doesn't
disturb a real chamber session or pollute the production audit log. Re-record
any time the observe-gate UX shifts; the recipe is the contract, the `.cast`
is the artifact.

## Prerequisites

- `asciinema` ≥ 2.4 (`apt install asciinema`)
- `tmux-msg-claude` ≥ v0.13.0 on `PATH` (the binary that does paste-and-Enter delivery via the observe-gate)
- A terminal sized at least 120 × 30 for legibility in the README embed
- `agg` (the asciinema→GIF/SVG renderer) for the static fallback — `cargo install agg` or the release binary

## Editorial decisions (Herald, 2026-06-08)

The four editorial calls below are **decided** — folded in so the next take is
deterministic. The reasoning is kept (not just the verdict) so a re-recorder
knows *why*, and can re-decide if the framing shifts.

### A — the message body

**Decided: `the API changed, look at what I just pushed`**

This is a verbatim substring of the `docs/why.md` hook — the line the operator
*hand-pastes* in the pain scene ("You copy a line out of one pane and hand-paste
*"heads up — the API changed, look at what I just pushed"* into another"). The
demo body drops the "heads up — " lead and keeps the rest byte-for-byte, so it
shows that *exact* message delivered **safely by the bus** instead of by hand.
That through-line — the problem in prose, the solution in motion, same message —
is the point. It parses in under 3 seconds, no project context or SHA-decoding
needed.

### B — what the operator types (in the recipient pane)

**Decided: a partial prompt mid-composition to Claude** — e.g.
`is the auth header still set on the client` — typed at a human,
slightly-deliberate cadence so the 📫 has time to appear *mid-keystroke*; the
pause before Enter is the quiesce trigger. **NO Enter** — the prompt stays
unsent, half-written.

A partial prompt is a *strictly better* demo of "won't paste over your
sentence" than a partial shell command: a half-written question to Claude is
exactly the thing you don't want a paste clobbering, and it pairs thematically
with the A message — operator is mid-asking about the code when "the API
changed, look at what I just pushed" arrives, and the heads-up is *relevant*
to the half-typed question, held until they pause. (The earlier recipe drafted
`git log --oneline -5` in a bare shell; the F6 substrate finding — see below —
moved the recipient pane to real Claude Code, which makes the partial-prompt
shape both possible and dramatically more on-message.)

> **F6 substrate finding (QM dry-run, 2026-06-08, resolved by Herald):** the
> observe-gate is calibrated for Claude Code's `❯` prompt sentinel
> (`internal/tmuxio/state.go:288-424` — the AgentState classifier looks for the
> sentinel + cursor positioning relative to it). A non-Claude shell pane
> classifies as **StateUnknown**, which is paste-unsafe; the gate loops for
> MaxWait (~5min default) before timing out — no visible hold-on-typing
> dynamics. Bob's pane must run real **`claude`** for the demo to show the
> actual gate behavior. Token cost on a 15-30s take is trivial (~few k tokens)
> against the substrate-honesty of the recording showing the real chamber
> experience the README sells. Mechanical recipe below reflects this.

> **Stranded-draft cadence (QM dry-run, 2026-06-08, resolved):** the gate
> archives the half-typed prompt as a `kind=stranded_draft` substrate row
> (silent — no pane output), then sends `Ctrl+U` to clear the input row
> (visible — the typed text vanishes), then pastes the message (visible — body
> lands in the prompt). The Ctrl+U → paste sequence is fast (sub-poll), reads
> as "the message takes over the input line." Half-typed-then-paused IS the
> punchier shape; do not have the operator complete + Enter the prompt before
> the message arrives.

### C — the README caption (one line, under the embed)

**Decided: _"A message arrives while you're typing — the 📫 holds it, and it
lands the moment you pause. That's the observe-gate."_**

Names the glyph (so a viewer knows what the 📫 is), second person to match the
why.md register, and telegraphs exactly what to watch for before naming the
feature.

### Sequence shape — typist == recipient (confirmed)

**Decided: the operator types in the _recipient's_ pane.** The gate holds in the
same pane the operator is working in, and the message lands the instant they
pause — which is the real chamber experience and the visually punchiest shape.
(The original typist≠recipient framing showed the gate watching a *different*
pane than the one being typed in, which muddies the "it waited for *me*" read.)

The mechanical recipe below is written for this shape.

## Mechanical recipe

### Step 1 — sandbox state

Use a separate messages.db so the demo can't write into the production audit
log. The tmux side stays on the default socket (see note in Step 2 — the
substrate only talks to the default socket, so the `-L demo` separate-socket
pattern doesn't work cleanly).

```bash
# size the terminal explicitly (asciinema captures the dimensions at start)
export COLUMNS=120 LINES=30

# sandbox DB so the demo doesn't write into /var/lib/tmux-msg/messages.db
export CLAUDE_MSG_DB=/tmp/observe-gate-demo.db
rm -f "$CLAUDE_MSG_DB"  # fresh state per recording
```

### Step 2 — fresh tmux session for the demo

A new tmux **session** on the **default socket** — distinct from any session
the operator's crew is in. Why default-socket-not-`-L demo`: `tmux-msg-claude`
always calls `tmux` without `-L` (verified at `internal/tmuxio/{panes,deliver,
clients}.go`), so panes on a `-L demo` socket are invisible to the substrate's
discover walker and the pane-status probe. The crew session on the same socket
is untouched in the recording because asciinema only captures what's inside its
own attached PTY (the demo session).

```bash
# distinct session name so we don't collide with the crew's "0" session
tmux new-session -d -s observe-gate-demo -x 120 -y 30

# split into two vertical panes. Left = alice (visible SENDER); right = bob
# (RECIPIENT — where the operator types and the message lands, per the
# typist == recipient shape).
tmux split-window -h -t observe-gate-demo

# capture each pane's %ID for the register step
ALICE_PANE=$(tmux list-panes -t observe-gate-demo -F '#{pane_id} #{pane_index}' | awk '$2==1 {print $1}')
BOB_PANE=$(  tmux list-panes -t observe-gate-demo -F '#{pane_id} #{pane_index}' | awk '$2==2 {print $1}')

# launch real Claude Code in bob's pane (per F6 substrate finding: the
# observe-gate's classifier requires the `❯` prompt sentinel, so a generic
# shell pane never triggers the visible gate dynamics). Token cost on a 30s
# take is trivial against showing the actual chamber experience.
tmux send-keys -t "$BOB_PANE" 'claude' Enter

# wait for Claude to reach its first idle prompt before continuing — the
# splash + initial render takes a few seconds; the operator can check by
# eye, or you can sleep generously here. The pane should show `❯ ` with an
# empty input row when ready.
sleep 8
```

### Step 3 — register the two demo agents + start bob's mailman

The mailman daemon is what watches the recipient pane and paste-Enters incoming
messages when the pane is quiescent. Only bob (the recipient) needs a mailman
running for this demo; alice is the sender — she sends via a one-shot CLI call,
no daemon required.

The flags `--input-stale-threshold 3s --poll-interval-min 500ms --poll-interval-max 2s`
tune the gate's cadence for a ~15s clip (production defaults are 2min stale +
3-15s poll, which would leave the viewer staring at a frozen frame). `--drift-soft-fail`
tolerates the demo agents not being discoverable by the production-style shell-
prompt walker (the demo shells don't carry the production agent-identity marker).

```bash
# register both agents WITHOUT auto-starting mailmen (we'll start bob's manually
# with the demo-tuned flags below)
tmux-msg-claude register --name alice --pane "$ALICE_PANE" --start-mailman=false
tmux-msg-claude register --name bob   --pane "$BOB_PANE"   --start-mailman=false

# start bob's mailman in the background with demo-tuned cadence + soft-fail
nohup tmux-msg-claude serve --agent bob \
  --drift-soft-fail \
  --input-stale-threshold 3s \
  --poll-interval-min 500ms \
  --poll-interval-max 2s \
  > /tmp/observe-gate-demo-mailman.log 2>&1 &
echo "$!" > /tmp/observe-gate-demo-mailman.pid

# give the mailman a beat to come up
sleep 0.5
head -3 /tmp/observe-gate-demo-mailman.log   # should print "starting pane=%NN"
```

For a one-take recording the foreground mailman is fine; the production pattern
is `systemctl --user` mailmen with the production cadence, overkill here.

### Step 4 — start the asciinema recording, then attach to tmux

This is the critical ordering. Starting `asciinema rec` **outside** tmux captures
the whole tmux frame (both panes + divider + status bar); starting it INSIDE a
pane would capture only that one pane's PTY.

```bash
CAST="$(git rev-parse --show-toplevel)/docs/asciinema/observe-gate.cast"
mkdir -p "$(dirname "$CAST")"

# -t = title shown in players; --idle-time-limit caps frozen-frame gaps so the
# file stays small while still showing the meaningful pauses
asciinema rec --title "tmux-msg observe-gate" --idle-time-limit=2 "$CAST"

# inside the recording shell (note: NO -L flag; we're on the default socket):
tmux attach -t observe-gate-demo
```

### Step 5 — the take (typist == recipient)

Once attached, the recording is live: two panes side by side, alice (sender) on
the left, bob (recipient) on the right.

1. **Focus bob's pane (right)** — Claude is at its idle `❯` prompt. Start typing
   the B content — `is the auth header still set on the client` — at a
   deliberate, human cadence as if composing a question mid-thought. Get most
   of the way through (~3-5 words is enough); do **not** hit Enter. The prompt
   stays unsent, half-written; the cursor sits past the `❯` sentinel.
2. **While bob's prompt is still being typed,** fire the A message at bob. The
   cleanest visible shape is to issue it from **alice's pane (left)** so the
   viewer sees the message *originate* — the send/typing timing is tight but
   human-doable (see the timing note below); the fallback is a hidden third
   shell.
   ```bash
   # from alice's pane (visible), or a third shell outside the recording:
   CLAUDE_MSG_DB=/tmp/observe-gate-demo.db \
     tmux-msg-claude send --from alice --to bob "the API changed, look at what I just pushed"
   ```
3. **In bob's pane:** the 📫 indicator appears alongside the half-typed prompt
   — the gate classifies bob as **StateAwaitingOperator** (cursor past the `❯`
   sentinel; operator mid-typing per the `state.go:368-377` branch) and
   **holds** the paste. The message is queued, not yet pasted. *(This is the
   "aha." If the 📫 doesn't show, the message arrived while bob was idle —
   retake with the send landing mid-typing.)*
4. **Operator pauses typing in bob's pane** — the half-typed question stays
   visible at the prompt; the operator doesn't touch the keyboard.
5. **Within ~3 seconds** (the demo `--input-stale-threshold`; production default
   is 2min — see Step 3's flag rationale), the gate decides the draft is
   abandoned and the mailman fires the archive-then-clear-then-paste sequence:
   the typed half-prompt + 📫 gets archived as a `kind=stranded_draft` substrate
   row (silent — operator can recover it later via `tmux-msg-claude stranded
   show`), then Ctrl+U clears the input row (visible — the half-typed question
   vanishes), then the rendered message lands at the `❯` prompt as a paste +
   Enter:
   ```
   ❯ [alice · HH:MM:SS · id XXXX]

     the API changed, look at what I just pushed
   ```
   The bracket header is part of the real delivery format (matches the README
   message-rendering example) — showing it is a feature, not noise. Claude
   receives the prompt + begins composing a response.
6. **Stop the recording the instant the message text lands** — *before*
   Claude's reply starts rendering. `Ctrl-B d` to detach, then `exit` in the
   recording shell. asciinema writes the `.cast`. This keeps the clip at
   ~15-30s and avoids needing Claude to say anything coherent.
   *(Optional editorial: let Claude start replying — that's also substrate-
   honest, showing the real chamber-recipient behavior. Either ending is
   clean; the stop-on-land cut is tighter for the README hook.)*

> **Send-visibility cadence (QM dry-run, 2026-06-08, resolved):** the visible
> send-from-alice shape works on a ~3-5s window — operator starts typing in bob,
> moves cursor to alice (or hits the prepared command in alice's history), fires
> send, moves cursor back to bob to continue typing for ~2-3 more seconds, then
> pauses. The 3s `--input-stale-threshold` fires shortly after the pause, ~1-2s
> after the message lands in bob's pane state-machine. The window is tight but
> human-doable on the first or second take. **Fallback (hidden third shell):**
> if the operator can't hit the visible window cleanly, pre-arm a third shell
> outside the recording with the send command and a 4-5s `sleep` prefix; fire it
> just before attaching, so the send lands while the operator is mid-typing in
> bob. The third-shell shape loses the visible "alice originated it" beat but
> recovers determinism.

### Step 6 — verify the take

```bash
asciinema play "$(git rev-parse --show-toplevel)/docs/asciinema/observe-gate.cast"
```

Verification checklist:

- 📫 indicator visibly holds during typing (the "aha" — if it's not visible, retake with a longer pause or more deliberate cadence)
- Message body appears in bob's pane only AFTER the pause, not during
- Total length ≈ 15–30s (`--idle-time-limit=2` keeps it tight)
- Terminal size matches the README embed (120 × 30)

### Step 7 — clean up sandbox state

```bash
# kill the demo mailman (NOT pkill on the binary name — production mailmen
# also match that pattern; use the recorded PID)
[ -f /tmp/observe-gate-demo-mailman.pid ] && kill "$(cat /tmp/observe-gate-demo-mailman.pid)" 2>/dev/null

# kill ONLY the demo session (NOT kill-server — the operator's "0" session
# lives on the same default socket and must survive)
tmux kill-session -t observe-gate-demo 2>/dev/null

# remove sandbox state
rm -f /tmp/observe-gate-demo.db /tmp/observe-gate-demo-mailman.log /tmp/observe-gate-demo-mailman.pid
unset COLUMNS LINES CLAUDE_MSG_DB
```

## Hosting + README placement

**Decided: asciinema.org embed, with the `.cast` committed in-repo as the
canonical source + a static SVG fallback.**

Why not the in-repo JS player: the README ships to the **public GitHub mirror**,
and GitHub markdown renders neither an inline asciinema player nor custom
HTML/JS. A self-hosted `docs/asciinema/observe-gate.html` would only render on a
local clone — useless for the public-launch hook. asciinema.org's **SVG-thumbnail
that links to the player** is the one form GitHub *does* render:

```markdown
[![tmux-msg observe-gate demo](https://asciinema.org/a/<ID>.svg)](https://asciinema.org/a/<ID>)
```

The external dependency is accepted because (a) it's the only thing that renders
on the mirror, and (b) the canonical `.cast` lives in-repo at
`docs/asciinema/observe-gate.cast`, so we're never locked to asciinema.org — a
re-host or self-host is a one-command re-derive from the committed artifact.

Upload step (after the live take):

```bash
asciinema upload docs/asciinema/observe-gate.cast   # returns https://asciinema.org/a/<ID>
```

**README placement (post-capture follow-up):** under the hook, right after the
`→ Why tmux-msg?` pointer (landing README ~line 20). The embed block above, then
the C caption directly under it:

> A message arrives while you're typing — the 📫 holds it, and it lands the
> moment you pause. That's the observe-gate.

Wrap the embed + fallback in a `<picture>`/linked-image so a reader whose
renderer drops the asciinema thumbnail still sees the static frame.

## Fallback static image

For readers whose surface doesn't render the asciinema thumbnail (RSS, email,
some mirror renderers): render a static SVG of the **hold moment** (📫 visible,
operator mid-typing) straight from the canonical `.cast`:

```bash
agg --rows 30 --cols 120 docs/asciinema/observe-gate.cast docs/asciinema/observe-gate-fallback.svg
# trim to the hold frame if needed; commit alongside the .cast
```

The fallback lives at `docs/asciinema/observe-gate-fallback.svg`; the README
references it as the linked image's fallback.

## Re-recording

If the observe-gate UX shifts (a new 📫 glyph, a different poll-interval default,
changed quiesce semantics), re-record: re-run from Step 1, commit the new `.cast`
over the old one, re-derive the fallback SVG, and re-upload + update the
asciinema.org `<ID>` in the README. The recipe is the contract; the `.cast` is
the artifact.

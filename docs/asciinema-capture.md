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

**Decided: `git log --oneline -5`**, typed at a human, slightly-deliberate
cadence so the 📫 has time to appear *mid-keystroke*; the pause before Enter is
the quiesce trigger.

It reads as genuine mid-work (you're checking "what just got pushed" — which
pairs with the A message), a half-typed command makes the "don't clobber my
input" stakes legible, and it's benign if it does run. It is **not** a
`tmux-msg-claude send …` (the recursive option) — that confuses a first-time
viewer about what's the tool vs what's the demo.

> **Dry-run flag for QM (mechanical):** validate how the gate handles the
> half-typed line on quiesce — does it archive it as a `stranded_draft` before
> pasting, and is that legible or distracting in a ~15s clip? Pick the cadence
> that shows **📫-hold → clean-land** most clearly. If the stranded-draft step
> intrudes, have the operator *complete + Enter* `git log --oneline -5` (it runs
> harmlessly), then pause on the fresh prompt where the message lands cleanly.

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

Use a separate tmux socket + a separate messages.db so the recording can't see
(or disturb) the production chamber session.

```bash
# size the terminal explicitly (asciinema captures the dimensions at start)
export COLUMNS=120 LINES=30

# sandbox DB so the demo doesn't write into /var/lib/tmux-msg/messages.db
export CLAUDE_MSG_DB=/tmp/observe-gate-demo.db
rm -f "$CLAUDE_MSG_DB"  # fresh state per recording
```

### Step 2 — fresh tmux server with the two demo panes

```bash
# new tmux server on a dedicated socket (-L demo), isolated from any other
# tmux server already running; new session at the chosen size
tmux -L demo new-session -d -s observe-gate -x 120 -y 30

# split into two vertical panes (left / right). Left = alice (the visible
# SENDER); right = bob (the RECIPIENT, where the operator types and the
# message lands — the typist == recipient shape).
tmux -L demo split-window -h -t observe-gate
```

### Step 3 — register the two demo agents + start their mailmen

The mailman daemon is what watches each pane and paste-Enters incoming messages
when the pane is quiescent.

```bash
# capture each pane's %ID so register knows where each agent lives
ALICE_PANE=$(tmux -L demo list-panes -t observe-gate -F '#{pane_id}' | sed -n '1p')
BOB_PANE=$(tmux  -L demo list-panes -t observe-gate -F '#{pane_id}' | sed -n '2p')

# register both agents + start mailmen (foreground; they stop when the script
# ends, so no leftover state)
tmux-msg-claude register --name alice --pane "$ALICE_PANE" --start-mailman=true
tmux-msg-claude register --name bob   --pane "$BOB_PANE"   --start-mailman=true
```

For a one-take recording the foreground mailmen are fine; the production pattern
is `systemctl --user` mailmen, overkill here.

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

# inside the recording shell:
tmux -L demo attach -t observe-gate
```

### Step 5 — the take (typist == recipient)

Once attached, the recording is live: two panes side by side, alice (sender) on
the left, bob (recipient) on the right.

1. **Focus bob's pane (right)** and start typing the B content —
   `git log --oneline -5` — at a deliberate, human cadence. Get a few characters
   in; do **not** hit Enter yet.
2. **While bob is still being typed in,** fire the A message at bob. The cleanest
   visible shape is to issue it from **alice's pane (left)** so the viewer sees
   the message *originate* — but the send/typing timing is tight (see the timing
   flag below); the fallback is a hidden third shell.
   ```bash
   # from alice's pane (visible), or a third shell outside the recording:
   CLAUDE_MSG_DB=/tmp/observe-gate-demo.db \
     tmux-msg-claude send --from alice --to bob "the API changed, look at what I just pushed"
   ```
3. **In bob's pane:** the 📫 indicator appears — the gate sees bob's pane is
   actively being typed in, so it **holds** the paste. The message is queued, not
   yet pasted. *(This is the "aha." If the 📫 doesn't show, the message arrived
   while bob was idle — retake with the send landing mid-typing.)*
4. **Operator pauses typing in bob's pane.**
5. **Within one poll interval** (default ~3–15s, per-agent configurable — see
   [`docs/observe-gate.md`](observe-gate.md) §Latency), the mailman observes the
   quiesce and pastes the message into bob's pane + Enter. The 📫 clears; the
   message body appears as if typed.
6. **Stop the recording** — `Ctrl-B d` to detach, then `exit` in the recording
   shell. asciinema writes the `.cast`.

> **Dry-run flag for QM (timing):** step 2's "send from alice's pane while bob is
> mid-typing" is the legible shape but timing-sensitive — if bob goes idle (pane
> switch) before the send lands, the gate delivers immediately and no hold shows.
> If a live operator can't reliably hit the window, fall back to the hidden
> third-shell send fired on a beat while they type in bob. Settle this in a dry
> run before the live take so the operator isn't fighting timing on camera.

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
tmux -L demo kill-server 2>/dev/null     # no effect on the real crew session
rm -f /tmp/observe-gate-demo.db
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

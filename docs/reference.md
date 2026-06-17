# tmux-tell — operator reference

The full operator manual: every command, flag, and edge-case semantic. The
[README](../README.md) is the landing page (pitch → install → first message); this
is the reference you reach for once you're running the bus. For the observe-gate's
decision matrix and tuning knobs see [`observe-gate.md`](observe-gate.md); for
missing-message triage see [`diagnostic-playbook.md`](diagnostic-playbook.md).

## Send and reply

The `recipient` block reports the recipient's **send-time disposition** so the
sender knows where the message is headed: `registered`, `alive` (pane present),
`delivery_mode`, `mailman_running`, and `pane_status` (`live`/`paused`/`unknown`). An
**unregistered** recipient is always fail-loud (the message is *not* queued — a typo
shouldn't sit unclaimed forever). Two opt-in flags refine this:

- `--strict` — also fail (`ok:false`) when the recipient is registered but **not
  reachable** (pane gone). Without it, a registered-but-dead recipient still queues
  (the message waits for the pane to return) and the block reports `alive:false`.
- `--wait-for-delivered [--timeout 10s]` — block until the message reaches a terminal
  delivery state, then return a `delivery` block (`state` + `verify_ms`) — the
  synchronous "delivered?" confirmation without a follow-up `track`/`message_status`
  poll. The `state` is the display-state, so a soft-fail surfaces as
  `delivered_in_input_box` (verified=0) rather than plain `delivered` (#230).
- `--block-on-stale` — with `--reply-to`, refuse the send (`ok:false`) when the thread
  has moved since you last spoke. See the `thread_freshness` block below.

When the send carries `--reply-to <id>`, the response adds a **`thread_freshness`**
block — the crossed-message guard. Async bus traffic means replies cross in
flight: you `reply_to` a thread-state that an inbound you haven't read may already have
superseded. The block reports `{stale, newer_in_thread[], you_replied_to,
latest_in_thread}`, where `newer_in_thread` lists messages in the reply chain that are
**addressed to you and arrived after your own last message in that chain** — "the thread
moved since you last spoke." That's a substrate-knowable signal (reply_to walk + arrival
order + to/from); it deliberately does *not* claim "messages you haven't *processed*",
which the substrate can't know — a `delivered` paste is in your context stream but may
not be attended-to. By default `stale`
is informational and the send still succeeds; `--block-on-stale` turns it into a hard
refusal so you can re-read before replying.

The same fields are available over MCP (`tmux-tell.send` with `strict` /
`wait_for_delivered` / `timeout` / `block_on_stale`). The response schema is a named
struct contract that later disposition features extend.

To confirm a freshly-registered agent is reachable *without* sending it a message,
`tmux-tell-claude ping bob` probes daemon-up + pane-live (no pane paste) — see
[Reachability probe](#commands) under Operating the bus.

## Message rendering

Headers come in two shapes:

**Compact** — `[Sender · HH:MM:SS · ⇢<dur> · id XXXX]` — an unthreaded message (no
`reply_to`); the common case, a new thread:

```
[Alice · 14:02:09 · ⇢2s · id 7f3a]

please check CI on the latest push
```

**Threaded** — `[Sender → Recipient · re YYYY · ⇢<dur> · id XXXX]` — when `reply_to=YYYY`
is set; surfaces the direction *and* the parent message for thread-following:

```
[Bob → Alice · re 7f3a · ⇢4s · id 9c1d]

on it — green in ~3 min
```

**Delivery duration** — the `⇢<dur>` field (#368) is how long the message sat between
send and delivery — the substrate-honest signal of the gap between "queued" (persisted)
and "delivered" (in the recipient's context). It sits between the send-time and the id
(or, on a threaded header, after `re <id>`), as a single most-significant unit:

| Span | Field | Typical cause |
|---|---|---|
| < 60s | `⇢3s`, `⇢45s` | standard paste-and-enter cycle |
| < 60m | `⇢2m`, `⇢15m` | held by the observe-gate while the recipient typed |
| < 24h | `⇢2h`, `⇢14h` | hook-context not pulling, mailman stuck |
| ≥ 24h | `⇢2d`, `⇢5d` | substrate-blocked recipient |

It's the duration from `created_at` to `delivered_at` — or to the render moment for a
message rendered before delivery completes (the paste itself, or a still-queued row in
`log`), so a pending message reads as "has been waiting ⇢X". A sub-second span is
**omitted** (its absence reads as "instant"; a `⇢0s` would be noise). Header value is
operational signal, not audit precision — the exact `delivered_at` is in
`tmux-tell.message_status`.

**No-reply marker** — either shape can carry a trailing `· 🔕` when the sender sets
`--no-reply-expected` (CLI) or `no_reply_expected=true` (MCP); a discipline aid for
FYI / status messages that would otherwise accumulate ack-cascades:

```
[Alice · 11:04:12 · id 7f3a · 🔕]

FYI: tagged v0.8.0 — no ack needed
```

The recipient's Claude reads but doesn't acknowledge — content is still judged on
its own merits; the marker is a hint, not a hard rule.

**Length marker** — either shape gains a trailing `· <size>` when the body exceeds a
byte threshold (default 512 bytes), so a reader scrolling history can tell a two-line
ack from a 3K wall of review text, and a sender sees the size cost of what they're
about to send:

```
[Surveyor → Quartermaster · re abad · id 4825 · 2.3k]

<a long, substantive review body…>
```

The count is the body's byte length. Sizes read `<n>b` under 1000 bytes and `<n.n>k`
above (×1000, so `2.3k` is 2300 bytes — the lowercase suffix borrows the `du -h`/`ls -h`
look, but the math is decimal, not 1024-based, so a threshold maps cleanly back to a
marker). The threshold is configurable via the `render-byte-marker-threshold` TOML key
(fleet `[defaults]` + per-`[agent.<name>]` override), e.g. `render-byte-marker-threshold = "2k"`;
set it above any realistic message size to suppress the marker entirely.

**Compact chrome** — set `--quick` (CLI) or `quick=true` (MCP) to collapse the full
bracket-header block to a single line; for routine acks where typing-overhead-to-signal
ratio is high:

```
✓ Bosun · 14:02:09 · ⇢2s · acked, ⚓
✓ Quartermaster · re bd19 · 14:03:00 · ⇢3s · acked, ⚓
```

The compact form preserves the load-bearing fields — sender, optional thread linkage (`re
<id>` when `reply_to` is set), the send-time + `⇢<duration>` (#368), and content — and
drops the spatial framing (no message id, no blank line between envelope and body). The
send-time was previously dropped too; it's kept now so a quick ack's timing stays
verifiable from the message itself. The `✓` prefix marks the shape
at a glance so a reader scrolling history can distinguish it from a regular bracket-header
message. `no_reply_expected`, if set, is preserved as a `🔕` prefix on the body. The
length marker is not applied to quick messages (single-line chrome is already the
compactness signal). Sister to `--no-reply-expected`: `--no-reply-expected`
reduces unnecessary acks; `--quick` reduces the overhead of necessary acks.

System-generated messages (`delivery_failure_notice`, `dedupe_notice`, `stranded_draft`) carry their
own chrome so they're distinguishable from agent traffic. Sender names render
title-cased in the header; stored agent names are lowercase by convention.

## Delivery modes

Each registered agent has a `delivery_mode`:

| mode | what the mailman does | the recipient's view |
|---|---|---|
| **`paste-and-enter`** *(default)* | pastes into the agent's pane through the observe-gate | messages **appear in the pane** — no inbox polling needed; the substrate pushes |
| **`mailbox-only`** | does not paste (no pane to push into); messages stay queued | the recipient **polls** `tmux-tell-claude inbox` / `tmux-tell.inbox` to read them |
| **`hook-context`** | does not paste; messages stay queued for the recipient's own hook to pull | the recipient's Claude session **injects** pending messages as `additionalContext` on its next turn, via a SessionStart/UserPromptSubmit hook (#249) |

`mailbox-only` makes a plain shell a bus *destination* without an always-on agent
session — e.g. your own shell: agents `send to=you` and you read when you choose. Set
it via MCP (`register … delivery_mode=mailbox-only`), CLI (`register --name you
--delivery-mode mailbox-only`), or a per-agent TOML block. Precedence (highest wins):
**per-agent block > `[defaults]` > the DB column > compiled default (`paste-and-enter`)**.
`tmux-tell-claude config show` prints the resolved value per agent.

### Flipping delivery_mode (pre-flip queued messages, #390)

Re-running `register --name <agent> --delivery-mode <new>` on an agent that already
exists **flips** its mode (a flip also requires `--force`, since you're overwriting an
existing registration). Messages that were queued under the *prior* mode are **orphaned**
by the flip: they were emitted for the old delivery semantics and sit below the new
mailman's backlog floor, so the mailman silently skips them (a `hook-context` chamber's
queued rows, for instance, must not be pasted verbatim into a paste-served pane). To keep
the substrate from silently fencing — or unilaterally discarding — operator-addressed
messages, a flip with queued orphans requires an **explicit disposition**:

| flag | effect |
|---|---|
| `--purge-stale-queue` | ack the orphaned rows (they were for the old mode; drop them) |
| `--keep-stale-queue` | leave them queued — they stay **backlog-fenced** (not auto-delivered); clear later with `inbox --ack-all` |

Without one of these flags, a flip that would orphan `N > 0` messages **errors** and names
the count. A flip with zero orphans, or a re-register to the **same** mode (e.g. a chamber
restart), proceeds without a flag. `--force` is orthogonal — it authorizes overwriting the
registration, not a queue disposition.

**Seeing fenced rows:** `inbox` annotates orphaned rows as `queued (backlog-fenced)` in the
text table. On the JSON surface every inbox row carries a stable **`backlog_fenced`**
boolean (always emitted, `true`/`false`) so downstream tooling can detect the
won't-auto-deliver state programmatically. A row is fenced when it is `queued`, below the
agent's `backlog_epoch_id`, and not a promoted-deferred row (#227 deferred rows bypass the
floor and deliver regardless).

### Hook-context delivery (Claude Code)

`hook-context` (#249, [ADR-0009](adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md))
delivers via Claude Code's **lifecycle hooks** instead of pasting into the pane: the
recipient's own Claude session pulls pending messages and injects them as
`additionalContext` on its next turn. Like `mailbox-only`, the mailman doesn't paste (it
short-circuits at startup) — but unlike it, the recipient doesn't have to poll: a hook
does the pull automatically.

**Substrate-vs-adapter boundary** (ADR-0009 decision (b)): the substrate stays
delivery-method-agnostic — messages just sit `queued`. The CLI-specific hook delivery
lives entirely in the adapter (the `tmux-tell-claude hook-context` subcommand). "Delivered"
is reframed from "pasted into the pane" to **"presented to the recipient"** (paste OR
hook-inject); the `delivery_mode` column carries *how*, and a hook-presented message is
`verified` by construction (additionalContext definitely reaches the context).

Wire it up: register the agent `hook-context`, then add a hook to the operator's
`~/.claude/settings.json` that runs the helper:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [ { "type": "command", "command": "tmux-tell-claude hook-context" } ] }
    ]
  }
}
```

On each `UserPromptSubmit` (and/or `SessionStart`), the helper resolves the session's
agent identity, claims its pending messages (honoring the #204 backlog floor + #227
deferred staging, same as the pane path), marks them delivered, and emits
`{"hookSpecificOutput": {"hookEventName": …, "additionalContext": …}}`. It is a clean
no-op (empty JSON) when nothing is pending, so it is safe to wire unconditionally. A
`hook-context` message is **invisible until the recipient's next turn** (it's context, not
pane chrome) — the accepted trade-off for clean hook delivery (ADR-0009 Q1). The **Codex**
adapter delivers the same way (#248) — see [Adapter integration](#adapter-integration) for
its `~/.codex/config.toml` wiring; Gemini's differing hook schema is future work.

### Draining a mailbox-only queue: `inbox --watch` (#149)

A `mailbox-only` queue only drains when something marks messages consumed — the mailman
deliberately doesn't paste, so nothing auto-advances the lifecycle. `inbox --ack <id>` /
`--ack-all` drain by id from a one-shot list; `inbox --watch` is the **interactive**
counterpart — a full-screen TUI that lists the queue, refreshes as mail lands, and acks
under the cursor with one keystroke:

```bash
tmux-tell-claude inbox you --watch                 # live drain surface for agent "you"
tmux-tell-claude inbox you --watch --watch-interval 5s
```

| key | action |
|---|---|
| `↑`/`↓` (or `k`/`j`) | move the cursor between rows |
| `space` | **ack** the selected message — transitions it `queued → acknowledged` (the same #221 transition `--ack` drives) and drops it from the queue |
| `enter` | expand the selected row to show the full body inline (toggles) |
| `r` | **reply** to the selected message — opens `$EDITOR` (`$VISUAL` → `$EDITOR` → `vi`); the saved body is sent threaded under the original (`reply_to`), addressed to its sender. Save an empty reply to abandon. Write above the scissors line; everything below (the quoted original + instructions) is ignored, so a reply line that starts with `#` survives. The original stays queued — replying isn't acking. |
| `q` / `Ctrl-C` / `Esc` | exit cleanly |

The list **refreshes by polling** (default every 2s, `--watch-interval` to tune), not by
a push hook: the writing mailman is a separate process, and SQLite's `update_hook` only
fires for the connection that registered it — so polling `state=queued` is the only
cross-process-visible mechanism (the same call `tail` makes, #148). New arrivals appear
on the next tick; the cursor stays anchored to its message as rows above it drain. On
exit a one-line summary (`N drained this session, M still queued`) is left on the normal
screen, so the session's work is preserved in scrollback.

`--watch` is interactive: it requires a real terminal (errors if stdout isn't a TTY) and
can't be combined with `--format json` or `--ack`/`--ack-all`. The reply send reuses the
`send` substrate (caps enforced in-transaction, like `send --reply-to`); it does not
replicate the send-CLI's thread-freshness / `--strict` guards, which are sender-side
ergonomics rather than reply-from-your-own-inbox needs.

An operator-reject / mark-failed action was considered (#149's original `D` key) and
deliberately **not** built: a `queued` message has no `queued → failed` transition, and
`failed` is the sender-facing delivery-failure state — reusing it for operator-side
rejection would muddy the state vocabulary. For a `mailbox-only` operator, `space →
acknowledged` already IS the drain. A distinct `rejected`/`dismissed` state would be a
new forever-commitment to the state vocabulary with no current consumer, so it's left to
a future forcing-function rather than baked speculatively (see #268 for the full
decision-record).

## Adapter integration

tmux-tell is a substrate with **per-CLI adapter binaries**. The binary name encodes
`tmux-tell` (substrate) + the CLI tool it adapts: `tmux-tell-claude`, `tmux-tell-codex`.
Every adapter is a thin wrapper over the same adapter-agnostic core (`internal/cli`):
message storage, queueing, identity, delivery-state, and the whole subcommand surface are
shared and identical. What differs per adapter is narrow — the binary/unit name and the
CLI's native **hook** wiring for `hook-context` delivery. [ADR-0009](adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
draws this substrate-vs-adapter line; #248 proves it by adding the second binary with zero
substrate changes.

Pick the adapter at install time (both can coexist):

```bash
sudo -A ./install.sh --adapter=claude   # default
sudo -A ./install.sh --adapter=codex
```

Each adapter gets its own mailman unit template (`tmux-tell-<adapter>-mailman@.service`)
and shares the one message DB, so a `claude` agent and a `codex` agent register/send/
receive on the same bus.

### Claude Code — `tmux-tell-claude`

The canonical adapter. Hook-context wiring lives in `~/.claude/settings.json` (see
[Hook-context delivery](#hook-context-delivery-claude-code) above). Claude Code sends the
firing event name on stdin as `hook_event_name`, which the helper echoes back into
`hookSpecificOutput.hookEventName`.

### Codex — `tmux-tell-codex`

Codex (the OpenAI CLI) supports **both** delivery modes, like Claude:

- **`paste-and-enter`** *(default, #360)* — the mailman pastes into the Codex pane through
  the observe-gate. #322 taught the gate to read Codex's `› ` sentinel + cursor (so it
  defers while a Codex operator is typing) and #336 made the verify signal cursor-anchored
  (so it confirms delivery even when a >1KB paste collapses to `[Pasted Content]` and the
  verify token never renders literally). With both in place, Codex is `PasteCapable = true`
  and the register-time default works for it — no extra wiring needed. **See the
  [dual-prompt note](#codex-dual-prompt-the-submit-visual) below** so the submit visual
  isn't misread as a lost delivery.
- **`hook-context`** — no paste; the recipient's own Codex session injects pending messages
  on its next turn. Codex's hook output schema (`hookSpecificOutput.hookEventName` +
  `additionalContext`) matches Claude's, so the same `hook-context` helper presents messages
  unchanged. Choose this when you'd rather not have the mailman paste into the pane.

To use `hook-context`, register the agent `hook-context`, then wire a Codex hook in
`~/.codex/config.toml`:

```toml
[features]
hooks = true        # or run codex with `--enable hooks`

[[hooks.UserPromptSubmit]]
[[hooks.UserPromptSubmit.hooks]]
type = "command"
command = "tmux-tell-codex hook-context --event-name UserPromptSubmit"

[[hooks.SessionStart]]
[[hooks.SessionStart.hooks]]
type = "command"
command = "tmux-tell-codex hook-context --event-name SessionStart"
```

After editing `~/.codex/config.toml`, Codex will ask you to approve or deny each new hook
command in its UI. Both hooks must be **explicitly enabled** (the approval flow sets an
`enabled = true` flag per hook in `~/.codex/config.toml`'s `[hooks.state]` section);
hooks left in the `trusted_hash`-but-no-`enabled` state are not run.

**Identity resolution:** hooks run in the operator's shell (not in a subprocess like the
MCP server), so `$TMUX_PANE` **is** set in the hook's environment. Omitting `--from` is
the recommended wiring — the helper resolves the agent name from `$TMUX_PANE` via the
registry on every hook fire, which keeps the hook command pane-independent (one command in
`config.toml` works for any pane). If you supply `--from <name>`, the name must be a
registered agent — passing an unregistered name hard-errors rather than silently no-oping.

**Why `--event-name`:** Codex *requires* the output's `hookEventName` to match the firing
event — it rejects a mismatch (`hook returned invalid user prompt submit JSON output`).
Codex's hook **stdin** schema (whether, and under what key, it passes the event name) is
not documented, so rather than trust the stdin echo, pin the event name in the hook
command with `--event-name`; the helper then emits it deterministically regardless of the
CLI's stdin shape. Wire one hook block per event you enable (`SessionStart`,
`UserPromptSubmit`, `PostToolUse`), each pinning its own `--event-name`.

The mailman short-circuits for a `hook-context` agent (it never pastes); a
`paste-and-enter` Codex agent exercises the full observe-gate paste path, the same one
Claude uses (#360). Subset verified working: `register` / `send` / `inbox` / `serve` +
the hook-context round-trip (`cmd/tmux-tell-codex` end-to-end test) + paste-and-enter
delivery to a live Codex pane.

**Paste-capability safe-default guard (#323, generalized #360).** The mailman refuses to
paste-and-enter into a pane whose adapter declares `PasteCapable = false` — paste-and-enter
relies on the observe-gate reading the pane's prompt sentinel + cursor to defer during
operator-typing, and an unreadable pane would clobber in-progress input. This guarded Codex
specifically while its `› ` sentinel was unreadable (#323); #322 + #336 dissolved that
premise and #360 flipped Codex to `PasteCapable = true`, so **Codex now passes this guard**.
It remains as the general safe-default for any *future* paste-incapable adapter: such an
adapter's mailman force-defers at startup (leaves messages queued, exits cleanly, logs the
migration command). Recover by moving that agent to a non-paste mode:

```sh
tmux-tell-<adapter> register --name <agent> --delivery-mode hook-context   # or mailbox-only
systemctl --user restart tmux-tell-<adapter>-mailman@<agent>
```

**Pane-observation: the per-adapter `PaneProfile` (#322).** The observe-gate / `agent_state`
classifier no longer hardcodes Claude's `❯` sentinel: each adapter supplies a `PaneProfile`
(`Profile.Pane`, installed process-globally by `cli.Run`) carrying its prompt sentinel +
compaction / awaiting-operator / status-line snippets. Claude's is `ClaudePaneProfile`
(`❯` + NBSP); Codex's is `CodexPaneProfile` with its substrate-verified `› ` sentinel
(U+203A + a regular space — *not* NBSP). With this, `agent_state` classifies Codex panes
correctly and the observe-gate defers paste-and-enter while a Codex operator is typing —
the read side of the substrate-vs-adapter pane-observation contract is adapter-uniform.

This is what unblocked `PasteCapable = true` (#360). The historical blocker was **verify-token
robustness**, not pane-reading: both adapters collapse a pasted message to a `[Pasted …]`
placeholder (Codex by size ~1KB, Claude by line-count), hiding the verify token until the
message is submitted, so the old whole-pane token-match verify was fragile to that collapse
plus the mid-turn case (Enter queued while the recipient is busy). #336 replaced it with a
**cursor-anchored input-emptied signal** (delivery is confirmed when the input row empties,
read from the cursor position, independent of whether the token rendered) plus a per-adapter
clear/submit (`InputControl`) contract. That signal is collapse-robust and cross-adapter, so
Codex inherits it the same as Claude — the verify fragility is no longer a Codex-specific gate.

#### Codex dual-prompt: the submit visual

When a Codex agent submits a pasted message, its TUI shows a visual that is easy to misread
as "the delivery vanished": the submitted prompt **lingers in place** as a transcript row
(Codex expands the collapsed `[Pasted Content]` there) while a **new, empty input prompt
opens below it** and the cursor jumps down to the new row. So a Codex pane briefly shows
*two* `› ` rows — the lingering submitted one and the fresh input.

This is purely cosmetic. The message **did** submit. The mailman's verify signal is
cursor-anchored (#336): it reads the input-row state from the cursor's position, and the
cursor is on the new bottom input, so the lingering submitted row above is irrelevant to the
confirmation. An operator watching the pane should read the dual-`›` layout as "submitted —
new input ready", not "stuck". (Pinned by `TestDeliverySubmitted_CodexDualPrompt`.)

#### Codex two-Enter submit + the resubmit (#401)

A codex paste large enough to collapse to `[Pasted Content N chars]` needs **two** Enters
to submit when delivered via `tmux paste-buffer`: the **first Enter expands** the collapsed
block (it does NOT submit), the **second Enter submits** the now-expanded content
(operator-witnessed + Engineer-tested). A single Enter — what the mailman originally sent —
leaves the paste sitting unsubmitted, and the cursor-anchor verify *false-positives* on it
(codex parks the cursor on an empty sub-line while the `[Pasted Content]` lingers a line
above, so the input row reads "empty").

The mailman handles both, driven by the codex `PaneProfile.PasteCollapseMarker`
(`[Pasted Content`):

- **Verify override** — while the marker is present in the LIVE input (scoped to the
  bottom-most `› ` row, so a *submitted* paste's lingering transcript copy doesn't count),
  the paste is definitively **not** submitted; this overrides the cursor-anchor false-positive.
- **Resubmit** — while the marker persists the mailman re-sends Enter each verify retry; the
  marker survives both the collapsed and expanded states, so the loop keeps Entering until the
  paste truly submits (typically two), then the marker clears and verification confirms.
  Enter-on-empty is a safe no-op, so a resubmit racing an already-submitted paste is harmless.
  Bounded by the verify-retry budget. Claude has no collapse marker → it never resubmits
  (it submits on the first Enter), so its behavior is unchanged.

Pinned by `TestPasteStillInInput`, `TestDeliver_Codex_ResubmitsStuckCollapsedPaste`, and
`TestDeliver_Claude_NoResubmit`.

**Codex MCP env contract (#355, #356).** The Codex MCP server process is
spawned by the Codex CLI and does **not** automatically inherit the calling
shell's full environment. Two env-completeness requirements apply:

1. **Identity resolution** — `tmux-tell.send`, `tmux-tell.inbox`, and most
   other MCP tools resolve the caller's identity from `$TMUX_AGENT_NAME`
   (preferred) or `$TMUX_PANE` → registry lookup. Because `$TMUX_PANE` is
   set by tmux for every pane but is **not** propagated to the Codex MCP
   child, the `$TMUX_PANE` path silently fails. Set `TMUX_AGENT_NAME=<name>`
   explicitly in the MCP wrapper so the child has a fixed identity — see
   [#320](https://git.frankenbit.de/frankenbit/tmux-tell/issues/320) for the
   recommended `~/.codex/config.toml` MCP wrapper block.

2. **Mailman daemon start** — `tmux-tell.register` calls `systemctl --user`
   to start the mailman daemon. `systemctl --user` requires
   `DBUS_SESSION_BUS_ADDRESS` and `XDG_RUNTIME_DIR`; these are set by
   `pam_systemd` at login and inherited by shell processes, but **not**
   automatically by the Codex MCP child. If either is absent, `register`
   returns `ok:true` (the agent row is written) but `mailman: "skipped"` with
   `mailman_error` naming the missing vars and the recovery path. Add both
   vars to the MCP wrapper `env` block, or use `start_mailman=false` and
   start the daemon yourself:
   ```sh
   tmux-tell-codex serve --agent <name>   # or: nohup … &
   ```

Both gaps share the same root cause (MCP env isolation) and the same fix
(explicit `env` block in the Codex MCP config).

*Verified against codex-cli 0.130.0 (2026-05-10), per the [`Aldenysq/agents-connector`](https://github.com/Aldenysq/agents-connector)
integration notes — Codex hook events with `additionalContext` support: `SessionStart`,
`UserPromptSubmit`, `PostToolUse`.*

## Bus host-locality

The bus is **host-local**: one SQLite DB per host, per user (#308). There is no
substrate-default cross-host bus. The MCP server and the DB share a machine;
the mailmen and the panes they paste into share a machine. Substrate scope is
exactly the operator's tmux server on that host — see
[`security.md` §3.2](./security.md) for the load-bearing identity invariant
this rests on, and [`security.md` §4](./security.md) for what cross-host would
require if the substrate ever expanded to it.

This is a **deliberate scope-boundary**, not a gap. Federating SQLite mailboxes
across hosts would dissolve the auditability simplicity the project commits to
(read every message with `sqlite3`, uninstall is one script, no replication
state machine to reason about). Cross-host messaging is a substantively
different problem with substantively different tradeoffs; if a future
deployment ships under a "cross-host tmux-tell" label, it'll be an explicit
deviation from this scope, not the default behavior.

### Default tmux socket only

Within that host, the substrate talks to exactly **one** tmux server: the
**default socket**. Every tmux call the binary makes is a bare `tmux …` with no
`-L`/`-S` flag (`internal/tmuxio/{panes,clients,deliver}.go`), so `register`,
`discover`, the pane-status probe, and delivery all see only panes on the
default socket. Panes on a **`-L <name>`** socket are invisible to the bus: a
mailman can't find them (it spawn-fails or loops), and `discover` won't register
them.

To run the bus on an **isolated** socket anyway — a sandbox, a demo rig, CI —
set **`TMUX_TMPDIR`**, not `-L`. Bare `tmux` honors `TMUX_TMPDIR` for its
default-socket directory, so the whole stack (server, mailmen, `discover`,
delivery) lands on the isolated socket together. `-L`/`-S` do **not** work for
this; only `TMUX_TMPDIR` does.

### SSH'd panes are one-way carriers

When a tmux pane runs an SSH session to a remote host, the bus sees it as a
regular pane: the mailman pastes bytes into the pane, SSH transports those
bytes to the remote-end, the remote process receives them as terminal input.
**This is bus-on-host → SSH-transport → remote-input — not bus-to-bus
communication.** The return path (remote process trying to participate in the
bus on the alcatraz side) is **not** substrate-default behavior.

The framing matters: an operator encountering an SSH'd pane in their tmux
might assume the substrate offers cross-host messaging, expect a reply path,
and wonder if the substrate is broken when none appears. It's not broken;
it's substrate-scope. The substrate did one-way carriage (local → SSH →
remote) cleanly; bidirectional participation needs the Remote MCP mode opt-in
([#310](https://git.frankenbit.de/frankenbit/tmux-tell/issues/310) for the
design discussion).

### Three patterns for SSH'd panes

When a chamber's pane runs an SSH session, the operator has three
substrate-honest choices:

**Pattern A — Leave unregistered.** The bus doesn't try to deliver to the
pane; operator watches the pane manually and relays SSH'd content back to
local bus chambers as needed. Cleanest separation; lowest substrate-coupling.
Best for short-lived remote experiments.

**Pattern B — Register `mailbox-only`.** Local bus chambers can `send` to the
remote pane; messages queue but the substrate doesn't paste them. Operator
reads them via `inbox --watch` or similar and relays replies manually. Good
for operator-mediated bidirectional exchange where the operator is the
human-in-the-loop translator between local-bus and remote-terminal.

**Pattern C — Register `paste-and-enter`.** Local mailman pastes bytes
through the SSH session; one-way carriage to the remote chamber's input
stream. The remote-end can't participate in the local bus by default — for
that, see [#310](https://git.frankenbit.de/frankenbit/tmux-tell/issues/310)
(Remote MCP mode, opt-in).

The 2026-06-11 Caymans-Admin substrate-witness experiment used pattern C — a
one-way carriage that worked exactly as advertised. The post-hoc observation
that locked in this section's framing:

> *"From this side of the wire it doesn't feel like 'tmux-msg from Alcatraz.'
> It feels like an operator on a remote host pasting into my terminal via a
> transport I can't see. That's exactly what's happening, of course, and it's
> a fine demo, but the framing matters: this isn't bus-to-bus communication,
> it's bus-on-Alcatraz → SSH → my-input-stream."*

### The Remote MCP mode exception

A separate opt-in mode where the remote-end's MCP routes its tool calls back
to the bus via reverse-SSH tunnel is tracked at
[#310](https://git.frankenbit.de/frankenbit/tmux-tell/issues/310).
**Explicitly not "cross-host bus"** — the bus stays host-local; the MCP
becomes a remote-router via operator-configured tunnel. Remote MCP mode is
*not* default substrate behavior — it requires explicit operator configuration
(SSH tunnel setup, reverse-port allocation, MCP-route override).

Operators who want bidirectional participation from a remote chamber should
treat Remote MCP mode as the canonical path, *not* expect the default bus to
extend across hosts.

## Verified vs unverified deliveries

**Verified vs unverified deliveries.** After a paste+Enter, the mailman looks
for a verify token to confirm the message actually surfaced. If it does, the delivery
is *verified*; if the token never appears in the retry budget (typically the recipient
was mid-turn and Enter was queued), the message still landed in the pane but the
delivery is *unverified* — a soft outcome, logged `WARN delivered_in_input_box`. Both
are `state = delivered`: the message IS in the recipient's pane either way, so the
state isn't a failure. The distinction is carried by a durable `verified` column on
the row (`1` = verified, `0` = unverified, `NULL` = delivered before this marker
existed — never retroactively guessed), so the split is queryable from the DB rather
than only from the journal. Every consumer surface now reads the column (#230):
`sent`, `inbox`, `track`, `get`, `thread`, and the MCP `message_status` / `inbox`
tools all render a soft-fail as `delivered_in_input_box` (the `thread` tree marks it
`⚠`); `stats` reports the verified / in-input-box / pre-marker split; `status --today`
sources its verified counts from the column (failed / crash / cap-exceeded counts stay
journal-sourced); and `resend <id>` recovers a `delivered_in_input_box` message
**without** `--force` (the column confirms the soft-fail, so the explicit recovery is
sanctioned) — `--force` is still required to replay a *confirmed* (`verified = 1`) or a
*pre-marker* (`verified = NULL`) delivery, where the substrate can't claim the message
wasn't seen.

**Verify-retry budget — per-agent tunable.** The retry window for the verify-token
check is `~5s` by default (a 7-attempt 100ms / 250ms / 500ms / 1s / 1.5s / 1.65s
backoff schedule). The total budget is configurable per agent via the
`verify-retry-budget` knob — `15s` triples each delay, `2s` halves them, etc.
Precedence (highest wins): **`--verify-retry-budget` CLI flag > per-agent block >
`[defaults]` > compiled default `5s`**. Inspect production verify-attempt latency
via the `tmux_tell_delivery_verify_attempt_seconds` histogram (Prometheus, served on
each mailman's `/metrics` endpoint) before tuning. Tune for large-payload hubs (e.g.
Bosun's heavy review pane) if the p99 attempt-latency approaches the budget under
load.

## Commands

```
tmux-tell-claude send   --to Y[,Z,...] [--reply-to ID] [--expects-reply] [--strict] [--wait-for-delivered] [--block-on-stale] "body"  # one-shot; --to a,b,c fans to multiple recipients; --expects-reply stamps reply intent without blocking (#270)
tmux-tell-claude resend ID [--force]                     # replay a failed/unverified message
tmux-tell-claude ping   AGENT [--timeout D] [--format json]   # reachability probe (no pane paste)
tmux-tell-claude inbox  AGENT [--state STATE]            # list messages for AGENT
tmux-tell-claude inbox  AGENT --unanswered               # only expects_reply=1 messages the recipient hasn't replied to yet (#270)
tmux-tell-claude inbox  AGENT --ack <id>                 # mark one queued message acknowledged (#221)
tmux-tell-claude inbox  AGENT --ack-all                  # acknowledge all announce-skipped backlog residue (#221)
tmux-tell-claude inbox  AGENT --watch [--watch-interval D]  # interactive TUI: live list + cursor-nav + space-ack (mailbox-only drain; #149)
tmux-tell-claude sent   [--since DUR] [--state STATE] [--to AGENT] [--awaiting-reply]  # sender's outbox; --awaiting-reply filters to unanswered expects_reply messages (#270)
tmux-tell-claude track  ID [--watch]                     # delivery state of one message
tmux-tell-claude get    ID                               # fetch a processed message by id
tmux-tell-claude status [--today]                        # paused state + queue depths per agent
tmux-tell-claude stats  [--window all|7d|1h] [--agent X] [--pair]  # on-demand bus-traffic aggregates
tmux-tell-claude digest [--since today|week|24h] [--counterparty X]  # campaign-arc narrative summary
tmux-tell-claude tail   [--from X] [--to Y] [--kind K] [--state S]   # live cross-chamber firehose
tmux-tell-claude state  --agent AGENT                    # probe an agent's current activity
tmux-tell-claude health [--since DUR]                    # per-agent operational audit
tmux-tell-claude pause  AGENT | --all                    # halt delivery (queue keeps filling)
tmux-tell-claude resume AGENT | --all
tmux-tell-claude reset  --confirm [--hard]               # purge queued; --hard wipes audit log
tmux-tell-claude log    --thread ID                      # a reply chain, flat-chronological
tmux-tell-claude thread ID [--format tree|json]          # a reply chain, as a parent→child tree
tmux-tell-claude stranded list|show|prune                # recover flushed operator paste snapshots
tmux-tell-claude discover                                # re-derive agents.pane_id from tmux
tmux-tell-claude register --name <agent> [--pane <id>] [--force]  # register a pane on the bus
tmux-tell-claude unregister --name <agent> [--purge-queue] [--force]  # remove agent + stop mailman (#289)
tmux-tell-claude flag-operator "<body>"                  # signal this chamber needs operator attention (#224)
tmux-tell-claude clear-operator-flag                     # clear this chamber's awaiting_operator flag
tmux-tell-claude flush  [--trigger resume|register]      # promote your own deferred messages for a trigger (#227)
```

### Deferred delivery (#227 / #258a)

`send --deliver-after=<trigger>` **stages** a message instead of queuing it: it
sits in `state='deferred'`, invisible to inbox + mailman, until its trigger
fires. Single-recipient only. Two triggers exist:

- **`resume`** (#227) — post-compaction self-handoff. Before `/compact`, send
  *yourself* orientation with `--deliver-after=resume`; in your resume routine
  call `flush --trigger=resume` (or `tmux-tell.flush_deferred`) so the staged
  text lands in the freshly-resumed context instead of being absorbed by the
  summarizer. You can only flush messages addressed to yourself.
- **`register`** (#258a) — spawn-die session bridge. Send *another* agent a
  message with `--deliver-after=register` ("remember this for its next
  dispatch", e.g. Pilot's dispatch-across-sessions pattern). It auto-promotes
  when that agent next (re)registers — **no explicit flush needed**, the
  register *is* the trigger fire. The register response reports
  `deferred_promoted` (count, non-zero only). Promoted register rows deliver
  immediately (they bypass the #204 backlog floor via the `deliver_after`
  exemption), so they are *delivered*, not announced as backlog.

Timestamp/duration triggers and `OR`-composition are a #295 follow-up.

## Chamber → operator attention signal

Today's `tmux-tell-claude agents` output shows whether a chamber is reachable
(`pane_status: live` + `mailman_running` + `registered`), but it does not tell
the operator the load-bearing distinction: *"this chamber has presented a
choice and is waiting for me to weigh in"*. From the operator's side scanning
panes, an idle-because-done chamber looks identical to an idle-because-
awaiting-input chamber.

The attention signal (#224) closes that gap with a three-value state column on
each registered agent: `idle` (default; no operator action pending), `busy`
(reserved for future hook-driven mid-tool-call tracking), and
`awaiting_operator` (chamber explicitly flagged it needs the operator).

**Chamber side.** When a chamber presents a choice it wants the operator to
weigh in on, it calls `flag_operator(body)` — either as the MCP tool
`tmux-tell.flag_operator` or via `tmux-tell-claude flag-operator "<body>"`. The
body is the question / choice text. Two substrate mutations land in sequence
(best-effort):

1. A message is posted to the reserved `operator-attention` recipient (so the
   operator can read the actual prompt by tailing that mailbox).
2. The chamber's `attention_state` flips to `awaiting_operator`.

If step 1 fails (operator-attention not registered, body too large), no
substrate mutation lands — the call fail-louds and the chamber's state stays
unchanged. If step 1 succeeds and step 2 fails, the response carries a
`state_error` field so the chamber sees the partial outcome rather than
treating it as a silent failure.

The flag clears implicitly on the chamber's next `register` call (back from
`/compact`, a restart, or a spawn-die cycle) or explicitly via
`tmux-tell-claude clear-operator-flag` / `tmux-tell.clear_operator_flag`. The
auto-clear-on-register matches the substrate-honest semantic: a chamber that
re-registered is alive and ready, so whatever it was waiting on is presumed
resolved.

**Operator side.** One-time setup: register the reserved recipient as
`mailbox-only` (no mailman daemon needed; the operator polls):

```bash
tmux-tell-claude register --name operator-attention --delivery-mode mailbox-only
```

Then the operator's two surfaces:

- `tmux-tell-claude agents` includes an ATTENTION column listing each chamber's
  state — quick scan of "who needs me?"
- `tmux-tell-claude inbox operator-attention` (or `--watch` for live tailing
  once #149 lands) shows the actual questions chambers have flagged

The reserved-recipient convention is enforced: `flag_operator` fails-loud if
`operator-attention` is not registered — substrate-honest about the setup
prerequisite rather than silently swallowing the attention request.

## Recovering a stuck mailman (#291)

A mailman delivers by probing the recipient's tmux pane before each paste. When
that probe fails with `can't find pane` — a stale registration, a respawned
pane, or the wrong tmux server — the mailman reverts the message to `queued` and
retries. If the failure is *persistent*, an un-bounded retry would hammer the
tmux server (the 2026-06-10 incident: ~100 probes/sec wedged the server). Two
mechanisms bound it:

- **Exponential backoff.** Consecutive `can't find pane` failures back off
  `1s → 2s → 4s → … → 60s` (capped). Even the first failure waits 1s, so a
  persistent failure can never exceed ~1 probe/sec, dropping to 1/60s. A
  transient outage (you restarted tmux, a pane is respawning) self-heals: the
  next successful probe resets the streak.
- **Stuck-state parking.** After `stuck-threshold` consecutive failures
  (default 10), the mailman parks itself: it writes `stuck_reason = 'pane-not-found'`
  and **stops probing tmux entirely** for that agent. Queued messages stay
  queued — no loss — but nothing is delivered until you intervene.

A parked agent shows a non-`-` value in the **STUCK** column:

```bash
tmux-tell-claude agents
# NAME   PANE  STATUS  PAUSED  QUEUED  ATTENTION  STUCK
# bob    %3    stale   no      2       idle       pane-not-found
```

**To recover, re-register the agent with a correct pane** — this clears the
stuck state and the mailman resumes on its next loop:

```bash
tmux-tell-claude register --name bob --pane %7 --force
```

The clear also fires on the MCP `tmux-tell.register` tool (`force: true`), so a
chamber that re-registers itself after a respawn un-parks automatically.

Both knobs are per-agent TOML-configurable (`stuck-threshold`,
`stuck-poll-interval`); `stuck-threshold = 0` disables parking (backoff-only).

**Prometheus gauge.** When metrics are enabled (`--metrics-addr`), the
`tmux_tell_mailman_stuck{agent,reason}` gauge reflects the park state in
real-time: it is set to `1` on the loop iteration where parking is first
detected and drops to `0` when the stuck state is cleared. The gauge also
initialises correctly when a mailman starts against an already-parked agent
(e.g. after a daemon restart) — the first loop iteration reads the DB state
and sets the gauge before any delivery is attempted. Use this in a Grafana
alert to get paged when an agent parks.

## `serve` exit codes (#340)

`tmux-tell-claude serve --agent NAME` distinguishes **substrate-permanent**
failures from **transient** ones in its exit code so the systemd unit
template (`Restart=on-failure`) does the right thing for each:

| Condition                              | Exit code   | systemd behavior        |
| -------------------------------------- | ----------- | ----------------------- |
| Agent not registered in DB             | `0` (OK)    | record success, stop    |
| Agent row exists but `pane_id` empty   | `0` (OK)    | record success, stop    |
| `delivery_mode` is mailbox-only / hook-context | `0` (OK)    | record success, stop    |
| Adapter is paste-incapable but mode = paste-and-enter (#323) | `0` (OK)    | record success, stop    |
| Tmux `can't find pane` (persistent)    | mailman parks itself in stuck-state; process stays up | n/a (see above) |
| DB open error / get_agent error        | `70` (INTERNAL) | restart per Restart=on-failure |
| Mailman loop crash                     | non-zero     | restart                |

The substrate-honest framing: agent-not-found and empty `pane_id` are
**substrate-permanent for this unit instance** — a restart cannot resolve them,
only an operator-side `register` / `discover` invocation can. Exit cleanly so
systemd records success instead of restart-looping, and the log line tells the
operator how to recover.

Pre-#340 behavior returned `69` (UNAVAILABLE) for the agent-not-found and
empty-pane cases, which `Restart=on-failure` treated as a transient failure and
restarted every 2 seconds. Under enough orphan units (e.g. surviving a
chamber-name change without `unregister`, [alcatraz-infra#39](https://git.frankenbit.de/frankenbit/alcatraz-infra/issues/39))
the restart-flood hammered SQLite into a contention freeze. The #340 fix
prevents the flood at the substrate layer; #338's `unregister` cleanup is the
other half of the defense-in-depth.

## Verifying which DB a process is bound to (#290)

Both `tmux-tell-claude serve` and `tmux-tell-claude mcp` emit a startup log line
naming the resolved DB path and how it was resolved:

```
mcp: claude_msg_db=/tmp/crew-demo.db source=env(CLAUDE_MSG_DB)
serve: claude_msg_db=/home/alex/.local/share/tmux-tell/messages.db source=default(env unset)
```

The `source` label is one of:
- `env(CLAUDE_MSG_DB)` — the `CLAUDE_MSG_DB` environment variable was set.
- `flag(--db)` — the `--db <path>` CLI flag was passed.
- `default(env unset)` — neither was set; the user-home default (#308) is in use
  (`$XDG_DATA_HOME/tmux-tell/messages.db`, or `~/.local/share/tmux-tell/messages.db`
  when `$XDG_DATA_HOME` is unset).

This line is the **canonical way to confirm which DB a process is actually
bound to**, journalctl-visible on alcatraz:

```bash
journalctl -u tmux-tell-claude-mailman@bosun --no-pager | grep claude_msg_db
```

Motivation: a sandbox rig that sets `CLAUDE_MSG_DB` in the launching shell
but fails to propagate it to MCP children silently falls back to the
production DB — the 2026-06-10 crew-demo misdelivery. The log line makes this
propagation failure immediately visible.

## Moving the DB safely (#343)

The bus DB at `~/.local/share/tmux-tell/messages.db` (or wherever `CLAUDE_MSG_DB`
points) is a SQLite database in **WAL mode**. The on-disk representation is
actually three files:

```
messages.db        ← the canonical database
messages.db-wal    ← write-ahead log (pending commits, not yet checkpointed)
messages.db-shm    ← shared-memory index for WAL access
```

A plain `mv messages.db /new/location/` **leaves the `-wal` and `-shm`
sidecars at the old path**. Mailmen restarted against `/new/location/messages.db`
read the pre-checkpoint state, losing every write since the last automatic
checkpoint (typically minutes-to-hours of bus history). This actually happened
during the v0.16.0 alcatraz deploy ([#343](https://git.frankenbit.de/frankenbit/tmux-tell/issues/343)
provenance) — ~14 hours of bus messages stranded in an orphaned WAL.

**The substrate-honest deploy recipe** is checkpoint-then-move, with all
mailmen stopped so no one is writing to the WAL while the move runs:

```bash
# 1. Stop everything that writes to the DB.
systemctl --user stop 'tmux-tell-claude-mailman@*.service'
pkill -f 'tmux-tell-claude mcp' || true

# 2. Consolidate the WAL into messages.db. After TRUNCATE the .db-wal file
#    exists but is zero bytes — every committed transaction is now in
#    messages.db proper, safe to move alone.
sqlite3 ~/.local/share/tmux-tell/messages.db 'PRAGMA wal_checkpoint(TRUNCATE);'

# 3. Move the file. The sidecars at the source can be deleted; SQLite will
#    recreate them at the destination on first open.
mv ~/.local/share/tmux-tell/messages.db /new/location/messages.db
rm -f ~/.local/share/tmux-tell/messages.db-wal \
      ~/.local/share/tmux-tell/messages.db-shm

# 4. Update $CLAUDE_MSG_DB (or the unit-file Environment=) to the new path,
#    restart mailmen.
systemctl --user daemon-reload
systemctl --user start 'tmux-tell-claude-mailman@*.service'

# 5. Refresh every chamber's MCP server so they re-bind against the new DB.
#    Chamber MCP servers are stdio-spawned by Claude Code at session start,
#    NOT systemd-managed — restarting mailmen does not restart them.
#    A long-lived MCP server holds the OLD DB inode open (file handle survives
#    the rename), so writes from that chamber's MCP land in a ghost-inode DB
#    invisible to the new path. refresh-all-mcps fires the mcp-restart-tmux-tell
#    macro per registered agent → each chamber's Claude Code re-initializes its
#    tmux-tell MCP stdio against the current binary + canonical DB.
tmux-tell-claude refresh-all-mcps
```

**Alternative** (preferred when the source DB can be locked momentarily): use
SQLite's `.backup` dot-command, which is WAL-aware and copies all three files
in a single atomic snapshot:

```bash
systemctl --user stop 'tmux-tell-claude-mailman@*.service'
sqlite3 ~/.local/share/tmux-tell/messages.db ".backup /new/location/messages.db"
# Update $CLAUDE_MSG_DB; restart mailmen. No -wal/-shm cleanup needed.
systemctl --user start 'tmux-tell-claude-mailman@*.service'
tmux-tell-claude refresh-all-mcps  # same MCP-rebind reason as above
```

**Why the MCP-restart step is required (substrate-honest framing).** The deploy
recipe touches *two* sets of `tmux-tell-claude` processes, not one:

- **Systemd-managed mailmen** (`tmux-tell-claude-mailman@<agent>.service`) — the
  stop/start cycle above handles these cleanly; they read the new DB on the
  fresh process's first open.
- **Chamber MCP servers** (`tmux-tell-claude mcp` stdio-spawned by Claude Code
  per chamber session) — these are NOT systemd-managed; they survive the
  mailman restart cycle. A long-lived MCP server retains its open file handle
  on the old DB *inode* even after `mv` renames the dirent. SQLite happily
  writes to that handle; the resulting messages live on the orphan inode and
  are invisible to mailmen reading the canonical path.

Without `refresh-all-mcps`, the substrate ends up in **two-DB split-brain**:
chamber MCPs write to the ghost inode, mailmen read from the canonical path,
neither side surfaces the divergence. This actually happened during the
v0.16.0 alcatraz deploy ([#349](https://git.frankenbit.de/frankenbit/tmux-tell/issues/349)
provenance) — 2+ hours of bus messages from one chamber stranded on a ghost
inode until the post-deploy investigation surfaced it.

**Release-notes touching DB-path moves must mention the
`refresh-all-mcps` step.** It's not optional and not always-implied by
"restart mailmen"; the substrate-honest deploy procedure has to call it out
explicitly. Same shape for any future deploy that touches DB-binding
substrate-state.

The invariant in one line: **`messages.db` always has invisible siblings;
never move the file alone.** And: **mailmen are not the only readers; refresh
the MCPs too.**

**Substrate-vs-adapter note: codex chambers.** `refresh-all-mcps` fires the
`mcp-restart-tmux-tell` macro per registered agent, but the macro is
claude-only per [#248](https://git.frankenbit.de/frankenbit/tmux-tell/issues/248)
(B)'s substrate-vs-adapter decision: the paste-and-enter control surface that
delivers slash-commands is genuinely Claude-bound. For codex chambers,
`refresh-all-mcps` walks the registry row but the macro lands as a no-op at
the recipient's codex pane (the codex CLI doesn't process `/mcp` slash
commands). **The Unix-file-semantics invariant applies symmetrically across
adapters** — a codex MCP server has the same open-fd-on-old-inode problem as
a claude one — **only the macro-delivery surface differs**. After the move,
**restart the codex CLI manually** in each codex chamber's session so its
child MCP server re-binds against the canonical DB.

## Operator-presence routing — `send --to operator`

Sister substrate to the attention signal above (#228). When a chamber wants to
reach the operator directly — *"Bosun has an urgent question for whoever's at
the keyboard"* — addressing `operator` as a recipient routes the message to the
chamber the operator is currently attached to, or was last attached to.

```bash
tmux-tell-claude send --to operator "PR #999 needs your read before merge"
```

The substrate resolves `operator` at send time via two steps (always in order):

1. **`tmux list-clients` observation.** The substrate asks tmux which pane each
   attached client is currently focused on. If any client is on a registered
   chamber's pane, that chamber wins — substrate-honest "the operator is here
   right now."

2. **Last-seen-in fallback.** When tmux reports no client at a chamber pane
   (operator stepped to their own shell, or detached), the substrate falls back
   to a single-slot record of where the operator was most recently observed. The
   slot is updated automatically on every successful step-1 resolution, so
   subsequent sends route to the last-known chamber even with no client
   currently attached.

If neither step yields a registered chamber (substrate has never observed the
operator at a chamber, or the slot's target has been unregistered), the send
fails-loud rather than silently routing to nowhere. The operator's first
attached observation lights up the slot for all future sends.

**Substrate-honest single-operator assumption.** A tmux session is per-user;
multi-operator-per-session is not modeled. If tmux reports multiple attached
clients (e.g. operator on laptop + a phone forwarder), the substrate uses the
first match — the next observation pass corrects it if the picture changes.

**Composes with `--to a,b,operator`.** Multi-recipient sends substitute every
`operator` entry independently and pass other recipients through unchanged.

**Composes with [`flag_operator`](#chamber--operator-attention-signal).**
A chamber that needs operator attention can pair `flag_operator(prompt)`
(declarative signal) with `send --to operator "<urgent reply or follow-up>"`
(operator-routed message) — the first surfaces the chamber in the operator's
ATTENTION column; the second lands a message in whichever chamber pane the
operator is reading.


**Kill switch & retention.** `pause` sets `agents.paused = 1`; the mailman stops
injecting (messages keep queuing up to the cap) until `resume`. History is free —
SQLite on disk; on mailman start, any row left `delivering` from a crashed run is
reset to `queued`. `reset` purges `queued` + `delivering`; `--hard` also wipes the
delivered audit log; `--confirm` is mandatory. `reset --older-than <window>` prunes
terminal-state rows (`delivered`, `failed`, `acknowledged`) older than the given
duration (e.g. `7d`, `24h`) — a one-off manual flush.

**Automatic retention (TOML).** The mailman runs a background sweep when
`retention` is configured. Default is `"infinite"` (no sweep, zero behavior change).
Set a per-agent or fleet-wide window in `/etc/tmux-tell/config.toml`:

```toml
[defaults]
retention = "30d"                 # delete delivered+failed rows older than 30 days
retention-sweep-interval = "1h"   # how often to sweep (default 1h)

[agent.operator]
retention = "infinite"            # operator audit log: never auto-delete
```

Precedence: per-agent block > `[defaults]` > hardcoded `"infinite"`. The sweep only
deletes `delivered` and `failed` rows for the agent it serves (single-writer
invariant); in-flight rows (`queued`, `delivering`) are never touched. The one-off
`reset --older-than` and the daemon sweep are independent — both can run
simultaneously without interference.

**Reachability probe.** `tmux-tell-claude ping <agent>` answers "is the daemon up + the
agent registered + its pane reachable?" without the side effect a test `send` has —
it queues a `kind=ping` row the mailman picks up (proving the daemon is alive) and
resolves via substrate-health checks (agent registered, pane live), transitioning
straight to `delivered`/`failed` **without pasting into the recipient's pane**. The
clean "is this chamber wired up?" check for new-agent setup and post-restart sanity.
States (and exit codes): `delivered` reachable (`0`), `failed` registered-but-
unreachable (`69`), `timeout` no answer in `--timeout` — daemon down/paused/
backlogged (`75`). Pinging a non-registered agent fails loud. From MCP, call
`tmux-tell.ping {"agent": "surveyor"}`. (A `mailbox-only` agent has no mailman, so a
ping to it reports `timeout`.)

**Tracking delivery.** `tmux-tell-claude track <id>` shows where a message is
(`queued → delivering → delivered`, or `failed` with the reason in `error`);
`--watch` re-renders on each state change until terminal. From MCP, call
`tmux-tell.message_status {"id": "9c1d"}`.

**Diagnosing a failed or unverified message — `resend`.** When a message
lands `failed`, or lands `delivered` but you can't tell whether it actually
surfaced in the recipient (a `delivered_in_input_box` — the paste landed but the
verify-token never came back in budget), the recovery path is `tmux-tell-claude resend
<id>`. It replays the original to its recipient as a *new* message whose body is
byte-identical to the original, carrying a `↻ Replayed: original sent at <ts>`
chrome marker so the recipient sees it's a re-send, not fresh content. The
response adds a `replay` block (`original_id`, `original_sent_at`,
`original_state`, `forced`). From MCP: `tmux-tell.resend {"id": "9c1d"}`.

The duplicate guard keeps an accidental re-run from spamming:

- A **`failed`** message replays directly — it never arrived.
- A **`delivered_in_input_box`** (delivered-but-unverified) message replays
  **directly too** — the `verified = 0` column confirms the soft-fail, so `resend`
  recovers it without `--force` (#230). Passing `--force` here still works but is
  deprecated (it's no longer needed) and emits a one-time
  `WARN deprecated_surface_used name=resend_force_unverified`.
- A **confirmed `delivered`** (`verified = 1`) or a **pre-marker `delivered`**
  (`verified = NULL`, from before the column existed) message is refused without
  `--force` — the substrate can't claim the message wasn't seen, so `--force` is the
  deliberate "yes, I know it may already have arrived" signal.
- A still **in-flight** message (`queued`/`delivering`) is likewise refused
  without `--force` — wait for a terminal state, or force a duplicate knowingly.

**Automatic replay deduplication (TOML).** After a `resend`, the mailman closes
the ambiguity loop automatically. Before delivering any message, it checks whether
a prior `delivered_in_input_box` row from the same sender with the same body exists
within the `dedupe-window` (default 60 s). If found, it re-verifies the original's
`id <public_id>` token against the recipient's pane scrollback:

- **Original now visible** — the message was actually processed. The original is
  promoted to confirmed-`delivered` (`verified = 1`), the replay is absorbed (marked
  `failed` with reason `dedupe_absorbed`), and a `dedupe_notice` is inserted back to
  the sender confirming the resolution.
- **Original not visible** — the message genuinely never landed. The replay proceeds
  through normal delivery.

Configure per-agent or fleet-wide:

```toml
[defaults]
dedupe-window = "60s"   # default; set to "0s" to disable

[agent.operator]
dedupe-window = "0s"    # operator pane: disable dedupe (scrollback too short)
```

Precedence: per-agent block > `[defaults]` > hardcoded 60 s. `dedupe-window = "0s"`
disables the check entirely — zero behavior change for existing deploys. The check
is scoped to the serving agent (single-writer invariant) and never touches in-flight
rows.

**Reading a reply thread.** Two views of the same `reply_to` chain (both resolve
the whole chain from *any* id in it — walk to root, then all descendants):
`tmux-tell-claude log --thread <id>` renders it **flat-chronological** (an audit view);
`tmux-tell-claude thread <id>` renders it as a **parent→child tree** (a navigation /
diagnostic view — "who replied to what, and did it land?"):

```
○ id=6970 from=quartermaster to=bosun kind=message state=delivered  (PR #397 ready for merge)
└─ ✓ id=7501 from=bosun to=quartermaster kind=message state=delivered  (PR #397 merged)
   ├─ ✓ id=6d35 from=quartermaster to=bosun kind=delivery_failure_notice state=delivered  (…)
   └─ ✗ id=01ff from=quartermaster to=bosun kind=message state=failed  (merge acked)
      └─ … id=ac44 from=bosun to=quartermaster kind=message state=queued  (state-sync ack)
```

Glyphs: `○` root · `✓` delivered · `⚠` delivered_in_input_box (soft-fail) · `✗` failed
· `…` queued/delivering. The `⚠` glyph reads the `verified` column (#230): a delivered
node with `verified = 0` renders `⚠` and `state=delivered_in_input_box`, distinct from a
confirmed `✓`. `--format json` emits the nested tree for tooling. `thread` is read-only
and never touches a pane.

When you *write* into a chain with `send --reply-to <id>`, the substrate runs the same
walk to warn you if the thread moved since you last spoke — the `thread_freshness`
block, described under [the send loop](#send-and-reply). `thread`/`log` *read*
the chain; `thread_freshness` *guards a write* against replying to a superseded state.

**Request-reply — `ask` / `wait_for_reply` / `check_replies`.** The reply-to chain
above is *asynchronous*: you send, the other side answers whenever. Request-reply
(#250) bundles the **wait** so you can pause your own turn until the answer comes:

```bash
ask_id=$(tmux-tell-claude ask --to bob "is CI green on main?" | jq -r .id)
tmux-tell-claude wait-for-reply "$ask_id" --timeout 60s   # blocks until bob replies (or times out)
```

- **`ask --to <agent> "question"`** is a single-recipient `send` that marks the row
  `expects_reply` and returns the message id as the **`ask_id`**. Bob answers by
  replying to it (`send --reply-to <ask_id> --to <asker>`).
- **`wait-for-reply <ask_id> [--timeout <dur>]`** blocks until a reply addressed to
  you with `reply_to = ask_id` arrives, then returns `{ok, ask_id, reply, timed_out}`.
  `reply` is `{id, from, body, state, unverified, created_at}`. `unverified: true`
  (#169) means the reply landed but its delivery to you wasn't verify-confirmed — it's
  returned anyway, you decide how much to trust it. It does **not** auto-acknowledge
  the reply (`ack` stays a separate, explicit action).
- **`check-replies <ask_id> [--since <id>]`** is the non-blocking poll: returns all
  replies so far. Pass `--since <highest-id-seen>` for the accumulation pattern (do
  other work, periodically check). Complements `wait-for-reply` when you'd rather not
  block.

The same three are MCP tools (`tmux-tell.ask` / `wait_for_reply` / `check_replies`).
Implementation note: in tmux-tell's multi-process bus the reply is written by a
*different* process than the one waiting, so `wait_for_reply` is a substrate-side
**poll-backed** blocking seam (a literal sqlite `update_hook` only fires
intra-connection); the blocking-call shape is the contract, the poll is an
implementation detail behind it. Out of scope for v1: multi-recipient `ask`
(broadcast a question to N agents).

**Lightweight reply intent** (#270) — when you want to flag "I expect a reply" without
the blocking wait machinery, pass `--expects-reply` to `send`:

```bash
tmux-tell-claude send --to bob --expects-reply "please confirm deploy"
```

This stamps `expects_reply=1` on the row. Delivery is unchanged — it is a pure
metadata marker. The two complementary filter surfaces close the loop:

- **`inbox --unanswered`** — shows the recipient only the expects_reply=1 messages
  they haven't replied to yet. Scoped by `--state` as usual (default `queued`).
- **`sent --awaiting-reply`** — shows the sender the expects_reply=1 messages where
  the recipient hasn't replied. Overrides `--state` (the filter is meaningful
  regardless of delivery state). Also available as `tmux-tell.inbox` `unanswered`
  and `tmux-tell.send` `expects_reply` MCP parameters.

Note: `ask` also sets `expects_reply=1` — the column is shared. `send --expects-reply`
is the non-blocking form; `ask` is the blocking form that additionally waits for the
reply via `wait_for_reply`.

**Bus-traffic stats.** `tmux-tell-claude stats` is the in-terminal "show me the bus
right now" surface — on-demand aggregates computed straight from the local
`messages.db`, complementing the continuous observability stack that owns
dashboard trends. The default reports a per-agent table (sent / received /
delivered / failed / queued + p50 delivery latency) plus window totals for the
last 24h; `--window` takes `all`, `<N>d` (e.g. `7d`), or any Go duration
(`1h`/`90m`); `--agent X` scopes to one agent; `--pair --top N` shows the
busiest sender→recipient pairs; `--format json` emits machine-readable output
(also carrying `p95_latency_ms`). Below the table, `stats` now prints the
delivered verified-vs-unverified split sourced from the `verified` column (#230) —
`Delivered split: verified N, in-input-box M, pre-marker K` (pre-marker = `verified =
NULL` rows from before the column existed). For the per-message breakdown use
`tmux-tell-claude sent --state delivered_in_input_box`; for a per-agent since-midnight
split, `status --today`.

**Campaign digest.** `tmux-tell-claude digest` is the *qualitative* sibling to `stats`:
where `stats` answers "how much / how fast," `digest` answers "what conversations
happened and what's still owed." It reports a by-counterparty table (sent /
received / threads / closed / in-flight) plus an **in-flight threads** section
listing the reply-chains whose last word still awaits an answer — the day's-end
"what do I need to follow up on?" view. `--since` takes the calendar shortcuts
`today` / `yesterday` / `week` (alongside `all`, `<N>d`, and any duration);
`--counterparty X` scopes to conversations involving one agent; `--format json`
emits the structure. A thread counts as **closed** when its latest message is
marked `🔕` no-reply-expected (or the send failed) and **in-flight** otherwise —
a heuristic, not ground truth: the substrate can't know if a conversation is
*semantically* done, so the output says "likely needs follow-up," and setting
`--no-reply-expected` on a genuine last word is what keeps a closed thread out of
the list. System chrome (`delivery_failure_notice`, `dedupe_notice`, `stranded_draft`, `ping`) is
excluded from thread analysis.

**Live tail.** `tmux-tell-claude tail` is the cross-chamber firehose — all bus traffic,
live, filtered to what you care about. It's the view the per-mailman journals and
single-message `track` couldn't give: when a bug spans two chambers, `tail --from X
--to Y` shows the correlated stream in one terminal. New rows print as they're
inserted and `queued → delivered/failed` transitions print on the same id (a
multi-line lifecycle). Filters compose (AND):
`--from` / `--to` / `--kind` / `--state` / `--since`. `--since` defaults to `now`
(start live, no backfill) but takes any `parseWindow` spec (`5m`, `today`, `all`) to
backfill first. `--format json` emits one object per line for piping. Ctrl-C exits
cleanly.

**Delivery-failure notifications.** When an outbound message hits a terminal-failure
state (`failed` or `delivered_in_input_box`), the mailman auto-inserts a
`delivery_failure_notice` back to the sender (original id, recipient, failure class,
reason, 200-char preview). These bypass the queue caps so they're never dropped, and
a notice that itself fails does not generate another (no wedged-pane cascade). Both
`--notify-on-failed` and `--notify-on-delivered-in-input-box` default on.

**Recovering a flushed paste.** When the observe-gate archives your in-flight
input before pasting over it (see below), it stores the snapshot as a
`stranded_draft` bookmark. `tmux-tell-claude stranded list` shows your bookmarks (id,
pane, timestamp, byte-size); `tmux-tell-claude stranded show <id>` prints the recovered
content (`-o file` writes it out, for long pastes); `tmux-tell-claude stranded prune
--older-than 7d` clears old ones. Note: the snapshot holds whatever the substrate
captured from the input row — for a large bracketed paste tmux may have shown only
its `[Pasted text #N +M lines]` placeholder rather than the literal text, so
recovery is best-effort on big pastes.

**When a message seems to go missing,** walk the sender-first triage in
[`docs/diagnostic-playbook.md`](docs/diagnostic-playbook.md) — it starts from the
SQLite store (did the send reach the bus at all?) before the receiver's mailman
journal.

## Use from Claude Code (MCP): details

These extend the MCP setup in the [README](../README.md#use-from-claude-code-mcp).

### How identity works

When the CLI tool in a pane spawns the MCP server, the child inherits `$TMUX_PANE`
(tmux sets it for every pane — `%1`, `%3`, …). The server looks that pane id up in the
`agents` table and uses the matching name as the session's identity. So onboarding a
**new pane** is one call from that pane:

> **Codex note (#355):** The Codex MCP server is spawned differently and does
> **not** inherit `$TMUX_PANE` from the shell. Set `TMUX_AGENT_NAME=<name>`
> in the Codex MCP wrapper `env` block instead — the substrate checks
> `$TMUX_AGENT_NAME` first in the identity chain and skips the pane lookup.
> See the [Codex adapter section](#codex----tmux-tell-codex) for the full env
> contract.

> *call `tmux-tell.register name=myname`*

The pane is auto-detected, the row inserted, and the mailman started in the same step.
Equivalent CLI: `tmux-tell-claude register --name myname`.

The register response includes a **`queued`** count — the number of messages already
waiting for this agent at register time. A fresh or post-restart session (e.g.
the spawn-per-task pattern, or a chamber that lost its pane and re-registers) learns it
has backlog without a separate `tmux-tell.inbox` poll: if `queued > 0`, run
`tmux-tell.inbox` to read it. The count is informational and never blocks registration;
on the rare event the count can't be read, the response carries `queued_error` instead
and registration still succeeds.

**Caveat — systemd-managed mailman uses the unit-file environment, not the caller's**
(#293). When `register` is called with `start_mailman=true` (the default for
`paste-and-enter` agents), the daemon is started via `systemctl --user enable --now
<binary>-mailman@<name>.service`. The systemd-managed mailman that launches inherits
its `Environment=` from the unit file — it does **not** inherit the env of whoever ran
`register`. That means a caller who set `CLAUDE_MSG_DB=/sandbox.db` and ran
`register --start-mailman=true` would write the agent row to the sandbox DB while the
mailman polls the production DB (the unit file's default); the two never meet,
and delivery silently mismatches the caller's intent.

To prevent that silent footgun, `register` **refuses** the combination of
`--start-mailman=true` with a non-default `CLAUDE_MSG_DB` / `--db` path. On the CLI
the refusal is an `ok:false` error (exit 65); on the MCP path the registration still
succeeds but `mailman` is `skipped` with `mailman_error` naming the resolved-vs-default
DB divergence. To run an agent against a non-default DB, use `--start-mailman=false`
and run the mailman as a foreground subprocess that inherits your environment:
`<binary> serve --agent <name>` (or `nohup <binary> serve --agent <name> &`).

**Don't-flood the backlog.** A pane that comes back after a restart with a deep
backlog shouldn't have the whole queue pasted into it at once. So when a `paste-and-enter`
agent (re)registers with `queued > 0`, the register handler stamps a per-agent
**claim-floor** and the mailman skips queued messages at or below it. The policy is the
`on-register-backlog` TOML knob:

| `on-register-backlog` | what the mailman does |
|---|---|
| **`announce`** *(default)* | leaves the whole backlog queued and delivers one `📬 N queued — run tmux-tell.inbox` nudge |
| **`auto-deliver`** | pastes the newest `on-register-backlog-cap` messages (default 3) and announces the older remainder; if the backlog fits the cap, all of it delivers and no nudge is sent |

The register response surfaces what happened: `backlog_policy`, `backlog_skipped` (how
many were left queued), and `backlog_nudge` (the nudge's id). The skipped backlog stays
in state `queued` — you still read it with `tmux-tell.inbox`; the nudge just makes sure a
freshly-resumed session *knows* it's there. An unrecognized policy value falls back to
`announce`. Mailbox-only agents are unaffected (they never get a paste). Precedence is
the usual **per-`[agent.<name>]` block > `[defaults]` > compiled default**; an
unrecognized value resolves to `announce`.

**Draining announce-skipped backlog residue (#221).** Announce-skipped messages stay
`queued` indefinitely (the mailman never re-delivers them; a re-register only advances
the floor). To clear the residue once you've seen the `📬` nudge, use the ack path:

```bash
# mark all backlog-residue messages acknowledged (scope = ≤ backlog_epoch_id)
tmux-tell-claude inbox --ack-all

# mark one specific message acknowledged (idempotent)
tmux-tell-claude inbox --ack <id>
```

Acknowledged messages move to state `acknowledged` (a substrate-honest terminal state:
they were never pasted, so they do not carry `delivered`). They are excluded from the
default inbox view (`--state queued`) but remain retrievable by ID via `tmux-tell-claude
get` / `tmux-tell.get` (audit-preserving). The MCP surface is `tmux-tell.inbox` with
`ack_all: true` or `ack_ids: ["id1", "id2"]`.

### Canonical name mapping

The same tool is referred to by different sanitized names at different layers — worth
a glance when writing runbooks or invoking tools from a shell:

| Layer | Example name |
|---|---|
| Wire protocol (`tools/list` JSON-RPC) | `tmux-tell.register` |
| Source (`srv.RegisterTool(...)`) | `tmux-tell.register` |
| Claude Code tool-call slug | `mcp__tmux-tell__tmux-tell_register` |
| Documentation / prose | `tmux-tell.register` *(preferred)* |

Prefer the wire-protocol name (`tmux-tell.register`) in prose; use the slug when
invoking from Claude Code's tool surface. The Claude Code sanitization rule: dots →
underscores, dashes preserved, server-name prefix repeated as
`mcp__<server>__<server>_<tool>`. You can read the live wire names directly:

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | tmux-tell-claude mcp
```

> A Claude session started before an MCP-tool rename keeps the names it was
> initialized with until it restarts — so an older session may still surface the
> pre-v0.6.0 `mcp__semaphore__semaphore_*` names (same handler). See
> [New tools require a session restart](#new-tools-require-a-session-restart).

### Whitelisted control commands

`tmux-tell.control` types a vetted slash-command into a pane — the caller's own (most
commonly an agent asking itself to `/compact` at a quiescent point) or, for benign
peer nudges, another's. The string is typed directly (no chat header) so the CLI tool
parses it as if you'd typed it.

The whitelist is three-axis: each command opts in to *self*, *peer*, and — for
destructive commands needing a narrow exception to a blanket peer-deny — a per-edge
allowlist of specific (sender, recipient) pairs.

| command | self | peer | note |
|---|---|---|---|
| `sleep`   | ✓ | ✗ | self-only — peers can't truncate your context (the bus verb for `/compact`; see the [glossary](glossary.md#sleep)) |
| `rename`  | ✓ | ✓ | useful for `<Project> #<Issue>` tab automation |
| `cost`    | ✓ | ✗ | self-only — output goes to the recipient |
| `help`    | ✓ | ✓ | harmless either way |
| `clear`   | ✗ | ✗ | **edge-only** rescue path when `sleep` (`/compact`) can't recover from token exhaustion; loses in-flight work |
| `mcp-enable-tmux-tell`  | ✓ | ✓ | refresh the tool surface after deploying a new `tmux-tell.*` tool — no context loss |
| `mcp-disable-tmux-tell` | ✓ | ✗ | self-only: raw peer-disable is a DoS surface; use the restart macro |
| `mcp-restart-tmux-tell` | ✓ | ✓ | macro: `disable` + `enable` as two rows for a peer-safe reconnect |

The pre-rename `mcp-{restart,disable,enable}-tmux-msg` names — and the pre-rename `compact`
verb (#509) — keep working as deprecated aliases through v1.0 (#480, ADR-0008 §Discretion):
an invocation resolves to the canonical form (`…-tmux-tell` / `sleep`), carries a
`deprecated` field in the response, and logs a greppable `WARN deprecated_control_macro`.

Adding a command, flipping a scope, or adding an edge requires a code change
(`internal/control/control.go`) — the audit surface is intentionally small. The same
surface is a CLI subcommand (`tmux-tell-claude control --to … --command …`) for scripts and
sessions whose MCP isn't loaded.

**Self-sleep with a follow-up.** `/compact` leaves the session at an empty prompt;
`command=sleep` accepts a `resume_with` string (self-invocation only). The handler
queues the `/compact` plus the resume message (threaded via `reply_to`), and the
mailman holds the queue for `--post-compact-pause` (default 120s) so the follow-up
lands after the CLI tool has settled, not into the slash-command parser mid-compaction.
(`--post-compact-pause` keeps its name: it keys on the emitted `/compact` primitive,
which the verb rename leaves unchanged.)

### Removing a pane (#289)

`tmux-tell.unregister name=oldname` (or `tmux-tell-claude unregister --name oldname`) is the
reciprocal of `register`. It stops the agent's mailman, then drops the agent row from the
registry.

```bash
# Remove a stale agent that no longer has a live pane.
tmux-tell-claude unregister --name alcatraz

# Drop queued messages too (default: preserve them so they deliver if re-registered).
tmux-tell-claude unregister --name alcatraz --purge-queue

# Override the "you have N queued messages" guard.
tmux-tell-claude unregister --name alcatraz --force --purge-queue
```

**Semantics:**

- **Mailman first.** `stopMailman` runs `systemctl --user disable --now
  tmux-tell-claude-mailman@<agent>.service` before the row is deleted so the
  daemon doesn't observe a dangling agent reference. `disable` removes the
  `default.target.wants/` symlink so the unit also doesn't restart at next
  boot — the cleanup that was missing before #338 and let a stale
  visitor-mailman unit survive a chamber rename and trigger
  alcatraz-infra#39.
- **Soft-fail on systemctl error (#338).** If the user systemd manager is
  unavailable, full-disk, or otherwise unhappy, the DB row removal still
  proceeds — the agents-table row is authoritative state and a surviving
  unit is now caught by #340's serve-exit-on-missing-agent path. The
  response surfaces `mailman: "warn"` + `mailman_error: "<systemd output>"`
  instead of the usual `mailman: "stopped"` so the operator sees what
  needs follow-up.
- **Idempotent.** Unregistering an absent agent returns `{ok: true, removed: false}` —
  safe to call from cleanup scripts without a pre-check.
- **Queue guard.** If the agent has queued messages, the default is to fail loudly with
  the count. Pass `--force` / `force: true` to override. This prevents accidentally
  discarding mail that hasn't been delivered yet.
- **`--purge-queue`** drops only `queued` messages addressed to the agent. Delivered and
  failed audit rows (the bus's forensic history) are preserved regardless.
- **Sender history.** Messages *sent by* the unregistered agent (where it was
  `from_agent`) stay in the `messages` table — `from_agent` doesn't reference a live
  agents row. The bus history is forensic record, not live state.

**MCP:** `tmux-tell.unregister({name, purge_queue?, force?})`

**Response fields:** `{ok, name, removed, mailman: "stopped" | "warn", deleted: N, mailman_error?: string}`

### New tools require a session restart

MCP tool lists are sent once during the `initialize` handshake and not refreshed.
Updating the binary and restarting the mailmen makes new tools available to *future*
sessions; sessions started earlier stay pinned to the tool surface they initialized
with. `mcp-restart-tmux-tell` re-initializes one session's MCP stdio without
losing context; for a fleet, `tmux-tell-claude refresh-all-mcps` fires it per registered
agent (operator-only — a peer-invokable bulk restart would be a DoS amplification
class).

## Identity, names & aliases

**Identity precedence** (shared by the MCP server and the CLI): (1) explicit override
— `--from` on `send`, `--as` on `whoami`, or `$TMUX_AGENT_NAME`; (2) `$TMUX_PANE` →
`agents.pane_id` → name (the default for a registered pane, no env var needed);
(3) neither → an actionable error pointing at registration. `whoami` reports a
`source` field (`explicit` / `env` / `pane`) so you can see how identity resolved.

> **Trust model.** `$TMUX_PANE` is settable by anything with shell access, and the
> registry has no per-pane authentication. This widens *convenience*, not *security* —
> the model is "whoever has shell access is trusted," same as the rest of the bus.
> Don't run it on a box where that isn't true.

**Canonical names & aliases.** The bus addresses agents by canonical short name. The
discover walker, though, reads the name from `<cli> --resume <name>` in the process
tree — so a session launched as `--resume "My Long Session Name"` produces a running
name that won't match a short canonical. Register an alias to bridge it:

```text
tmux-tell.register name=alice alias='My Long Session Name'
```

After that, `discover` and the mailman's drift-check resolve the long name back to
`alice`. Multiple aliases per canonical are supported. If two canonicals both
substring-match one running value, the resolver logs `drift_check_ambiguous` rather
than guess — add an explicit alias on the one you meant.

## Storage schema

SQLite (WAL mode), three tables. The DB lives under the operator's user-home
(#308): `$XDG_DATA_HOME/tmux-tell/messages.db` when `$XDG_DATA_HOME` is set, else
`~/.local/share/tmux-tell/messages.db`. Override with `--db` or `$CLAUDE_MSG_DB`.
The binary creates the directory lazily on first open. This keeps the bus's
trust boundary congruent with tmux's per-user model (no shared-space path, no
install-time chown) and lets sandbox-by-default adapters (codex) write the DB
without per-write escalation.

```sql
CREATE TABLE messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  public_id     TEXT NOT NULL UNIQUE,           -- 7f3a — short, copy-pastable
  from_agent    TEXT NOT NULL,
  to_agent      TEXT NOT NULL,
  reply_to      TEXT REFERENCES messages(public_id),
  body          TEXT NOT NULL,
  state         TEXT NOT NULL DEFAULT 'queued', -- queued|delivering|delivered|failed|acknowledged|deferred
  created_at    TEXT NOT NULL DEFAULT (datetime('now','subsec')),
  delivered_at  TEXT,
  error         TEXT,
  verified      INTEGER,                        -- 1=verified, 0=unverified (delivered_in_input_box), NULL=unmarked (pre-migration, or not yet delivered)
  deliver_after TEXT,                           -- #227 deferred-delivery trigger; non-NULL only on state='deferred' (and the row it's promoted into); 'resume' (#227) or 'register' (#258a)
  expects_reply INTEGER NOT NULL DEFAULT 0       -- 1 = sender flagged reply intent: set by `ask` (#250) OR `send --expects-reply` (#270); 0 = plain send with no explicit reply expectation
);
CREATE INDEX ix_msg_queue ON messages(to_agent, state, id);

CREATE TABLE agents (
  name             TEXT PRIMARY KEY,
  pane_id          TEXT,                          -- "%3", refreshed by discovery
  paused           INTEGER NOT NULL DEFAULT 0,    -- the kill switch
  updated_at       TEXT NOT NULL DEFAULT (datetime('now','subsec')),
  aliases          TEXT NOT NULL DEFAULT '[]',    -- JSON list of alternative names (#38)
  delivery_mode    TEXT NOT NULL DEFAULT 'paste-and-enter',  -- 'paste-and-enter' or 'mailbox-only' (#116/#132)
  backlog_epoch_id INTEGER,                       -- #204 claim-floor (NULL = no epoch)
  attention_state  TEXT NOT NULL DEFAULT 'idle',  -- 'idle' | 'busy' | 'awaiting_operator' (#224)
  stuck_reason     TEXT NOT NULL DEFAULT ''       -- '' = healthy; 'pane-not-found' = mailman parked (#291)
);

CREATE TABLE presence (
  key        TEXT PRIMARY KEY,                -- e.g. 'operator.last_seen_in' (#228)
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
```

The `presence` table holds substrate-level observation slots that are
neither per-message nor per-agent. Today the only slot is
`operator.last_seen_in`, written by the `send --to operator` resolver
each time it observes the operator attached at a registered chamber's
pane (see §Operator-presence routing).

## Install internals: what runs as root

`sudo ./install.sh` asks for root, but root's reach is deliberately narrow.
**As root** the script does exactly one privileged thing: installs the
binary to `/usr/local/bin/tmux-tell-claude` (mode `0755`, owned `root:root`).
**As you** — never as root — it runs `go build`, installs the systemd
template to your `~/.config/systemd/user/`, and (after install) the mailman
daemons run in your linger-enabled `systemctl --user` session. The DB needs no
install-time step at all: it lives under your user-home (#308) and the binary
creates it lazily on first open. No daemon ever runs as root; root touches
nothing but the binary path.

The operator account is resolved from `$SUDO_USER` (set by `sudo`), falling
back to `$USER`. There is **no hardcoded fallback** — if neither resolves
(or resolves to `root`), the installer fails loud rather than guessing an
owner. To install for a different target user without `sudo`, set it
explicitly: `OPERATOR_USER=alice ./install.sh`.

That boundary is the whole point of shipping the installer as a readable
shell script: the same "audit it in an afternoon" property the bus itself
offers applies to the install story too — you can confirm exactly which
two operations need root before you grant it.

## Versioning and the K-counter

The road to `1.0` is gated on **K=3**: three consecutive releases with no
breaking change across any of the five public surfaces — MCP tool schemas, CLI
subcommand args/flags/exit codes, `--format json` shapes, the DB schema, and the
exported Go API (`discover` / `store` / `tmuxio`). Each clean cut increments K;
any break on a tracked surface resets it to 0.

**Current K: 8** (Sea-trials K=3 gate cleared at v0.9.0; the counter keeps
raising past the gate and retires at v1.0). The `cli-semaphore → tmux-msg`
substrate rename (v0.5.0) and the MCP wire-protocol rename (v0.6.0) were the
last deliberate breaks; v0.7.0, v0.8.0, v0.9.0, v0.10.0, v0.11.0, v0.12.0,
v0.13.0, and v0.14.0 have each been non-breaking. v0.13.0 introduced a new
alias-preserving deprecation (`resend --force` against
`delivered_in_input_box`, #230; earliest removal v0.15.0) — additive
deprecation that does not reset K per Reading B. v0.14.0 reframed the #169
delivery invariant from "delivered = pasted" to "delivered = presented"
(`delivery_mode` carries paste-vs-inject); the state name doesn't change,
only the invariant widens — substrate-honestly K-preserving under
ADR-0009's substrate-vs-adapter boundary. v0.10.0's second K-preserving deprecation arc —
`delivered_unverified → delivered_in_input_box` with CLI flag / TOML
key / `--state` value / JSON shadow-field aliases per ADR-0008's two-minor
floor (originally earliest removal v0.12.0) — was extended at the v0.12.0
cut to the v1.0 stability boundary (ADR-0008 §Discretion clause, operator
decision 2026-06-08), composing with the same v0.11.0 extension of the
v0.9.0 `claude-msg → tmux-tell-claude` arc. Both alias families continue to
function through v1.0. Per
ADR-0008's [Reading B amendment](docs/adr/0008-deprecation-policy.md#amendment-a--2026-06-08-k-counter-interaction):
deprecation-with-functioning-alias preserves K-counter progress; only removal
resets it. The live per-release record lives in the tracker at
[#163](https://git.frankenbit.de/frankenbit/tmux-tell/issues/163).

## Migrating from `tmux-msg`

The project was renamed `tmux-msg` → **`tmux-tell`** in v0.18.0 (#440) — the
second half of the `cli-semaphore → tmux-msg → tmux-tell` arc. A fresh install
lands on `tmux-tell` everywhere with nothing to migrate; an in-place operator
keeps working untouched. Every legacy surface is honored as a deprecated alias
(each emits a `WARN`) through the **v1.0** stability boundary per
[ADR-0008](docs/adr/0008-deprecation-policy.md) §Discretion, then hard-cuts.

| Surface | Canonical (`tmux-tell`) | Legacy (honored → v1.0) | WARN on use |
|---|---|---|---|
| Binary | `tmux-tell-claude` / `tmux-tell-codex` | `tmux-msg-claude` / `tmux-msg-codex` (+ `claude-msg`) | `deprecated_surface_used` |
| DB env var | `$TMUX_TELL_DB` | `$CLAUDE_MSG_DB` | `deprecated_env_var_used` |
| Config env var | `$TMUX_TELL_CONFIG` | `$CLAUDE_MSG_CONFIG` | `deprecated_env_var_used` |
| Default DB path | `~/.local/share/tmux-tell/messages.db` | `~/.local/share/tmux-msg/messages.db` | `legacy_data_path_in_use` |
| Default config path | `/etc/tmux-tell/config.toml` | `/etc/tmux-msg/config.toml` | `legacy_data_path_in_use` |

**Binaries + env vars** need no action — `install.sh` keeps the `tmux-msg-*`
symlinks and the resolver reads the legacy env vars as fallbacks; each emits its
WARN naming the successor.

**Data + config paths are lazy-migrated**: the binary uses the new `tmux-tell`
path if it exists, else falls back to the legacy `tmux-msg` path if *that* exists
(so nothing moves under you), emitting `WARN legacy_data_path_in_use` with the
move recipe. To migrate explicitly:

```bash
mv ~/.local/share/tmux-msg ~/.local/share/tmux-tell      # the DB + its WAL sidecars
sudo mv /etc/tmux-msg /etc/tmux-tell                      # or just config.toml
```

Move the DB with the mailmen stopped — see *Moving the DB safely* above for the
WAL-safe checkpoint-then-`mv` recipe. Everything resolves to `tmux-tell`
afterward and the WARNs go quiet. **At v1.0 every legacy surface above is
removed** — migrate before then.

## Migrating from `claude-msg`

A fresh install has nothing to migrate — skip this. If you ran a release before
v0.9.0, the adapter binary was renamed there: `claude-msg` → `tmux-tell-claude`, the
systemd template (`claude-mailman@` → `tmux-tell-claude-mailman@`), and the agent-name
env var (`$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME`) — all to encode the substrate plus
its adapter. The aliases stay functional through the v1.0 stability boundary
(extended at the v0.11.0 cut from the two-minor-floor earliest of v0.11.0 per
[ADR-0008](docs/adr/0008-deprecation-policy.md)'s §Discretion clause; operator
decision 2026-06-08): `install.sh` keeps `claude-msg` and `claude-mailman@` working
as aliases, and the identity layer still reads `$CLAUDE_AGENT_NAME` as a fallback —
each emits a `WARN deprecated_surface_used … removal=v1.0` when used. Migrate
scripts, units, and env to the new names at your leisure before then.

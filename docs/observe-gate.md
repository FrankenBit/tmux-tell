# The observe-gate: operator's guide

When you send a bus message to an agent that's mid-turn, mid-`/compact`, or
mid-typing, tmux-tell doesn't just paste it in and hope. Before each delivery the
mailman runs the **observe-gate** — a near-read-only check (one optional `📫`
nudge when you're typing, opt-out via `notify-emoji-disabled`; see §"When
you're typing" below) that watches the recipient's pane and waits for a safe
moment to deliver. This page is the operator-facing
companion to the README's [§Delivery semantics: the observe-gate](../README.md#delivery-semantics-the-observe-gate);
the README is the concise reference, this is the deeper "what it's doing and how to
steer it" guide.

The gate shipped in **v0.3.0** (#92/#93) and replaced the older probe-and-watch
quiet-pane gate; the legacy primitives + knobs were swept out in **v0.4.0** (#94),
the 📫 notification landed alongside (#95), and the multi-line stranded-draft fix
followed in **v0.4.0** (#96). It's **on by default** for every agent.

## What it is, in one paragraph

The observe-gate is **near-read-only**: it inspects the recipient pane via
two `capture-pane` snapshots plus one `display-message` cursor query, and
makes **at most one optional, opt-out-able mutation** before the actual
delivery — the `📫` typing-notification nudge (see §"When you're typing"
below; turn it off with `notify-emoji-disabled = true` and the gate is
**strictly read-only**). The nudge fires at most once per delivery cycle.
(The old probe-and-watch gate injected `─` probe dashes into your input
row on every poll cycle to test the waters; the observe-gate's one nudge
is bounded, opt-out-able, and absent unless the gate observes that you're
actively typing.) The gate classifies the pane into one of five states,
and either delivers immediately or waits and re-checks. Typical latency
on an idle pane is **~3–5s**; the old gate's single backoff cycle was ~72s.

## How it decides: the five agent states

Each poll classifies the recipient into one `AgentState`
(`internal/tmuxio/state.go`), checked in **precedence order** — the first rule that
matches wins:

| # | State | How it's detected | Gate decision |
|---|---|---|---|
| 1 | **at-rest-in-compaction** | the `Compacting conversation…` marker is in the pane (#70) | wait |
| 2 | **working** | the two snapshots differ across a 200ms window (streaming output, a spinner, any paint) | wait — *or* **deliver now** when `working-deliver-immediately` is on (#106) |
| 3 | **idle** | cursor sits *at* the `❯ ` prompt sentinel — empty prompt *or* an auto-suggestion ghost-text (you haven't engaged) | **deliver now** |
| 4 | **awaiting-operator** | cursor sits *past* the sentinel (you're mid-typing), or an AskUserQuestion-style popup footer is present (#79) | notify + wait (see below) |
| 5 | **unknown** | capture failed, or the pane is stable in some UI the heuristic doesn't recognize | wait (treated as advisory — never delivered into blindly) |

The precedence matters: a pane mid-`/compact` is *animating* (the spinner glyph and
percentage tick), so without the compaction check running before the "working"
check, it would look like ordinary working — the marker check at precedence 1 fixes
that. The cursor-position distinction (at-sentinel vs past-sentinel) is what lets
the gate tell an *idle prompt with ghost-text* apart from *you actively drafting* —
the two cases the older heuristic conflated.

> **Note on the markers.** The three substrings the gate keys on — the prompt
> sentinel `❯ ` (NBSP), the compaction phrase, and the popup footer — are
> **Claude-Code-version-dependent**. If a Claude Code TUI update changes any of
> them, the affected branch degrades gracefully (toward `unknown`, the safe
> default), and the canary tests in `state_canary_test.go` fail loudly so the
> constant gets re-verified. If you ever see agents that should be idle classifying
> as `unknown` after a Claude Code upgrade, this is the first thing to check.

## Latency: fast when idle, patient when busy

- **Idle pane** → delivered on the first poll, ~instant.
- **Busy pane** (working / compaction / awaiting-operator that's still changing) →
  the gate re-polls with **multiplicative backoff**: `3s → 4.5s → 6.75s → … → 15s`
  cap. The interval resets to the 3s floor whenever your input content changes
  (fresh activity → fresh cadence), so it stays responsive while you're actively
  working.
- **Safety cap** → a total `MaxWait` of **5 minutes** bounds the loop. If it
  expires (the recipient was continuously busy the whole time), the mailman
  **delivers anyway** and logs `WARN gate_max_wait` — *fail-loud, not fail-stop*.
  Crossing the cap is rare; it means the pane never went idle and your draft (if
  any) never went stale for the full window.

The gate does **not** try to close the tiny race between "gate decided idle" and
"mailman pastes." That residual window is covered by the same post-hoc safety net
as always — the verify-token check and the `delivered_in_input_box` notice. What the
observe-gate removes is the *pane mutation* the old gate inflicted while observing.

## When you're typing: the 📫 notification (#95)

The old gate's probe dashes had an accidental virtue: they were a *visible* sign
that a bus message was queued behind your typing. The observe-gate is invisible by
design — so v0.4.0 added a deliberate, minimal signal in its place.

The first time the gate sees you mid-typing (`awaiting-operator`) in a delivery
cycle, the mailman drops a single **📫** (closed mailbox with raised flag) into your
input row. That's it:

- **Once per delivery cycle**, not per poll — no dash-creep.
- **No cleanup.** The mailman never tracks or removes the 📫. You either notice and
  backspace it (the gate keeps waiting for you to finish), or you notice and finish
  your message (the 📫 rides along into what you send — and recipients know it means
  *"the sender had a bus message waiting while they typed"*), or you don't notice at
  all. All three are fine by design.

To turn it off for an agent, set **`notify-emoji-disabled = true`** (default
`false`).

## The stale-draft flush (and why it's careful)

If you start typing in an agent's pane and then walk away, a queued bus message
can't wait forever. After your input row sits **unchanged for `input-stale-threshold`
(default 2 minutes)**, the gate decides the draft is abandoned and proceeds — but it
will **not** silently destroy what you typed. The flush is a three-path decision
(`cmd/tmux-tell-claude/serve.go`, `internal/tmuxio/observe_gate.go`):

1. **(c) Clear-paste-archive — the primary path.** The gate snapshots your input-row
   content into the bus as a `kind=stranded_draft` row, **self-addressed to the same
   agent** (cap-bypass, so a full inbox can't drop it), then sends `Ctrl+U` to clear
   the row and pastes the bus message.
2. **(a) Append — the fallback.** If that archive write fails, the mailman *skips the
   clear* and pastes onto your content (you get a compound message) rather than risk
   losing your draft.
3. **(b) Clear-and-discard — rejected.** Blindly `Ctrl+U`-ing without archiving is
   explicitly refused in the code, because the content in your input row might be a
   *half-delivered bus message* from a prior failed delivery — a blind clear could
   destroy bus content, not just operator content. The safe paths always archive
   before clearing.

**Multi-line drafts** are captured in full (#96). The gate walks from the sentinel
row down through continuation rows until it hits the input-area boundary (Claude
Code's `⏵⏵` status line, or a long `────` separator). Before #96 it captured only
the first line, so a multi-line draft got truncated at flush — that's fixed.

## Recovering a stranded draft

Because the snapshot is **self-addressed**, the agent's own mailman delivers it
right back into the agent's pane — so the **primary recovery is inline**: your
cleared draft reappears as a `kind=stranded_draft` bus message, carrying the content
verbatim plus the `public_id` of the delivery that triggered the flush. Usually you
just see it.

To find it again in the store *after* that self-delivery (the row has moved past
`queued`), use the SQLite inbox:

```bash
tmux-tell-claude inbox <agent> --state delivered   # the self-delivered snapshot
tmux-tell-claude inbox <agent> --state ""          # all states, if you're unsure
```

## Tuning knobs

All six are **CLI flags** on `tmux-tell-claude serve` *and* **TOML knobs** (per-agent or
`[defaults]`), resolved through the standard precedence chain — most specific wins:

> **CLI flag > `[agent.<name>]` block > `[defaults]` block > compiled-in default**

| Knob (flag / TOML) | Default | What it does |
|---|---|---|
| `--gate-disabled` / `gate-disabled` | `false` | Bypass the gate entirely — deliver immediately on every queue head. The escape hatch if the gate ever misbehaves for an agent. |
| `--poll-interval-min` / `poll-interval-min` | `3s` | Initial (and floor) sleep between polls. |
| `--poll-interval-max` / `poll-interval-max` | `15s` | Backoff ceiling per poll. |
| `--input-stale-threshold` / `input-stale-threshold` | `2m` | How long your draft must sit unchanged before the gate flushes it. |
| `--notify-emoji-disabled` / `notify-emoji-disabled` | `false` | Suppress the 📫 typing notification for this agent. |
| `--working-deliver-immediately` / `working-deliver-immediately` | `false` | Opts `working` out of the backoff into the same fast-path as `idle` (#106). Eligibility is `working` ONLY — `awaiting-operator` / `at-rest-in-compaction` / `unknown` stay hard-deferred regardless. Useful for crew-coordination workflows where the recipient's mid-turn keystroke buffer is the right delivery target. The verify-token + `delivered_in_input_box` notice is the safety net for the small race window between observing `working` and the paste landing. |

The two delivery-failure toggles (`--notify-on-failed`,
`--notify-on-delivered-in-input-box`) are **independent of the gate** — they govern the
sender-side failure notices, not delivery timing.

Run `tmux-tell-claude config show --agent <name>` to see the resolved values for an agent
and trace where each one came from.

## Checking an agent's state yourself

The same `AgentState` classification the gate uses is available on demand — handy
before dispatching to a busy agent, or when debugging delivery timing:

```bash
tmux-tell-claude state --agent <name>              # text
tmux-tell-claude state --agent <name> --format json
```

From a Claude session, the MCP tool is **`tmux-tell.agent_state`** (input
`{agent}`, output `{agent, state, evidence, captured_at}`). The `evidence.reason`
field explains *why* a state was chosen — useful when a classification surprises
you. Treat `unknown` as advisory: it means "couldn't substantiate," not "idle."

## Migration from a v0.2.x config

Heads up if you're carrying an old config: as of **v0.4.0 (#94)**, TOML decoding is
**strict** — any unknown key makes the mailman's config load **fail** with an error
naming the offending key, rather than silently ignoring it. The legacy probe-and-watch
knobs are unknown keys now, so a config block like:

```toml
[agent.bosun]
prompt-sentinel-gate = true
```

will stop the mailman from starting:

```
config: parse /etc/tmux-tell/config.toml: unknown key(s): agent.bosun.prompt-sentinel-gate
```

**Delete** the legacy keys to fix it — `quiet-disabled`, `prompt-sentinel-gate`,
`quick-presence-probe`, `quiet-observe-window`, `quiet-input-backoff`,
`quiet-max-wait`. A block that existed only to hold one of them can go entirely; the
observe-gate is on by default with no per-agent config. (This strict-fail behavior
supersedes the older "no-op + startup WARN" note still in the README's migration
paragraph — that described v0.3.0; v0.4.0 made it fail-loud. README correction
tracked in [#124](https://git.frankenbit.de/frankenbit/tmux-tell/issues/124).)

## See also

- README [§Delivery semantics: the observe-gate](../README.md#delivery-semantics-the-observe-gate) — the concise reference
- [`diagnostic-playbook.md`](diagnostic-playbook.md) — when an agent says "I missed a message" (sender-side vs bus-side triage)
- `CHANGELOG.md` `[0.3.0]` (#92/#93 gate), `[0.4.0]` (#94 sweep + strict TOML, #95 📫, #96 multi-line)
- Source: `internal/tmuxio/observe_gate.go`, `internal/tmuxio/state.go`, `cmd/tmux-tell-claude/serve.go`
- Releases: [v0.3.0](https://git.frankenbit.de/frankenbit/tmux-tell/releases/tag/v0.3.0) · [v0.4.0](https://git.frankenbit.de/frankenbit/tmux-tell/releases/tag/v0.4.0)

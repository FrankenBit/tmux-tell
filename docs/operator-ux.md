# Operator UX audit — tmux-tell at v0.2.1

> **Status: DRAFT.** Admin-led per the lead/verify split in bus
> message id 8117. Surveyor shape-verify complete (issue #36,
> comment 58673 — approved shape with four formatting/dimensional
> notes plus the §4 `message_status` 1.0-candidate verdict). This
> version applies N1-N4 + the §7 ordering disclosure.
>
> **Scope**: catalog rough edges from heavy operator use across two
> days of running the bus for four agents. Each finding has a
> severity tag (`paper-cut` / `friction` / `blocker`). Concrete
> follow-up issues will be filed for everything `friction` or above;
> `paper-cut` items are recorded here for future batching.

## Method

The operator (Alex) ran the bus across four agents (Bosun, Surveyor,
Pilot, Admin) through two cycles of:

- Routine sends + replies
- Cross-project review threads (3 rounds with Surveyor)
- Incident response (6 production incidents — see `failure-modes.md`)
- A full host reboot for the tmux-tell epic-#1 closure test

Findings collected by the operator-side admin pane during and after
each cycle. Cross-checked against the actual CLI output and
journalctl entries at v0.2.1.

## 1. Error message clarity

For each error class the operator has seen, the verdict is `clear`
(self-explanatory + actionable), `unclear` (needs to grep code or
docs to interpret), `actionable` (says what to do), or `not
actionable` (says what's wrong, not what's next).

| Error                                                                                                     | Verdict                       | Severity   | Note                                                                                                          |
|-----------------------------------------------------------------------------------------------------------|-------------------------------|------------|---------------------------------------------------------------------------------------------------------------|
| `cannot resolve sender identity: set $TMUX_AGENT_NAME, or register this pane (TMUX_PANE=%2) ...`        | clear, actionable             | —          | Exemplary; lists both recovery paths inline with the live $TMUX_PANE value                                    |
| `store: recipient queue full: bob (5/5, need 1 slot(s))`                                                  | clear, actionable             | —          | Good baseline; depth and cap both shown                                                                       |
| `control: command not on whitelist; self-invokable: [cost help ... sleep]`                                | clear, actionable             | —          | Lists the alternatives. Good                                                                                  |
| `body required`                                                                                           | clear, not actionable         | paper-cut  | Could be `--body required (or pass body as positional args after the flags)`                                  |
| `no such message: ghost`                                                                                  | clear, not actionable         | paper-cut  | Could suggest `tmux-tell-claude log --limit 20` to find a recent id                                                 |
| `WARN delivered_in_input_box id=X — paste+Enter completed but token not surfaced in time (Claude likely mid-turn); message is in recipient's input box pending submit` | clear, actionable | — | Exemplary; explains what happened AND what to do |
| `WARN quiet_cap_exceeded id=X pane=Y — delivering anyway`                                                 | clear, not-quite-actionable   | paper-cut  | What does the operator do with this WARN? Add: "rerun `tmux-tell-claude discover` if frequent" or similar           |
| `WARN drift_check_ambiguous ... multiple canonicals ... (resolve via: tmux-tell.register name=<canonical> alias=<unique-suffix> force=true; #47)` | clear, actionable (post-#47) | —          | Recipe now inline in the WARN per #47; operator gets the fix command without needing to grep docs. WARN string is generic enough to cover both the post-v0.2.1 Q(a) exact-collision path and the substring path |
| `WARN drift_detected_unrecoverable ... discover couldn't find X anywhere`                                 | clear, half-actionable        | paper-cut  | Add a hint: "(is the agent running? `tmux-tell-claude agents` shows current panes)"                                 |
| `store: alias %q is the canonical name of agent %q` (ErrAliasCollision)                                   | clear, actionable             | —          | Names both colliding agents; operator can fix the alias directly                                              |

### Proposed batch fix (`friction` + `paper-cut`)

Single PR adding next-step hints to the WARN-class log lines and the
error-class JSON `error` fields. Roughly 5-10 line touches across
the relevant files. Worth doing once the next operator-friction
issue surfaces something larger; until then, batched.

## 2. CLI ergonomics

### 2.1 Subcommand-level review

| Subcommand              | Verdict          | Severity   | Note                                                                                                       |
|-------------------------|------------------|------------|------------------------------------------------------------------------------------------------------------|
| `send`                  | Good             | —          | `--from` auto-resolves via identity, positional body works as expected                                     |
| `control`               | Good (post-#44)  | —          | Closed by `reorderFlagsFirst` helper + positional-auto-binds-to-`--to`. Operator's natural typing `tmux-tell-claude control alice --command sleep` works as expected.                                                                          |
| `track`                 | Good             | —          | Age computation is nice. Could add `--watch` for poll-until-state-change                                   |
| `inbox`                 | Good             | —          | Self-default works, `--state` filter useful                                                                |
| `status`                | Sparse           | friction   | Shows paused state + queue depths. Doesn't show: today's delivered count, today's failed count, today's `delivered_in_input_box` count, mailman crash count. Adding these would make morning-coffee health-checking one command instead of grepping journalctl |
| `agents`                | Good             | —          | Pane liveness is the right summary                                                                         |
| `whoami`                | Good             | —          | Source field (env/pane/explicit) is the nice touch                                                         |
| `serve`                 | n/a              | —          | Operator doesn't run this directly; systemd does                                                           |
| `pause` / `resume`      | Good             | —          | Useful during incident response                                                                            |
| `reset`                 | Safe-by-default  | —          | `--confirm` requirement is right                                                                           |
| `log`                   | Good             | —          | Thread inspection is rare but valuable                                                                     |
| `discover`              | Confusing        | friction   | After a tmux restore, the operator runs `discover` expecting it to repair the registry. Instead it CREATES new agent rows (long names from `--resume`) without remapping the existing canonical short-name rows. Documented as `force=true` workaround in README post-v0.2.0, but the subcommand's behaviour itself is unintuitive |
| `mcp`                   | n/a              | —          | Operator doesn't invoke this directly; Claude Code's MCP machinery does                                    |

### 2.2 Concrete CLI improvements

Filed-or-to-be-filed issues:

- [ ] **`control` flag-ordering trap.** Either (a) document at the
  top of `--help` output, or (b) reorganize the subcommand to use
  positional `command` (`tmux-tell-claude control sleep --to bosun`).
  Severity: friction, size/S.
- [ ] **`status` augmentation: deliver-today / fail-today /
  unverified-today / crash counters.** Same data the
  monitoring-recommendations in `failure-modes.md` propose. Could
  share an implementation with a new `tmux-tell-claude health` if filed
  together. Severity: friction, size/S.
- [ ] **`track --watch`** for polling delivery state. Useful for the
  "I just sent a long autonomous task; ping me when it's been
  consumed" pattern. Severity: paper-cut, size/S.
- [ ] **`discover` operator-mode improvements.** When the registry
  has canonical short-names and discover sees long `--resume`
  values, offer to add the long names as aliases on the existing
  canonical rows instead of creating new rows. Severity: friction,
  size/M, depends on #38 which already shipped the alias mechanism.

## 3. Monitoring blind spots

Today's six production incidents were all diagnosed by running
`journalctl --user -u 'tmux-tell-claude-mailman@*' --since '...'` and `grep`.
That's high operator-friction. What would have surfaced each
incident faster:

| Incident                              | What would have surfaced it                                                                            |
|---------------------------------------|--------------------------------------------------------------------------------------------------------|
| Watchdog crash (×4 on bosun)          | Crash counter on `status`; alarm at >1/day                                                             |
| Probe accumulation                    | `DeltaProbeMissing` count via per-verdict metrics                                                      |
| Unverified delivery                   | Deliver-time histogram (95th/99th percentile would have spiked)                                        |
| Enter-not-firing                      | Same — deliver-time histogram                                                                          |
| Silent misdelivery                    | `drift_detected` count (was zero pre-#37 because no detection existed; post-#37 these now log loudly)  |
| Discover row duplicates               | No good signal short of running `tmux-tell-claude agents` and noticing 8 rows. Documented as known limitation |

The pattern: most blind spots collapse to **"per-verdict / per-WARN
metric exposed via `status`."** A `tmux-tell-claude health` subcommand
that does this scan-and-report once would close the biggest gap.
Worth its own issue.

### Delivery-failure notifications (post-#53)

The "Bosun spent half a day waiting" scenario on 2026-05-31 surfaced
a blind spot the per-verdict metrics don't fix: the **sender** doesn't
get a push-signal when their outbound message hits `failed` or
`delivered_in_input_box`. Polling `tmux-tell-claude track <id>` works but
requires the sender to remember to check.

Post-#53 (currently `[Unreleased]`), the mailman auto-generates a
`delivery_failure_notice` back to the original sender on every
terminal-failure transition. Two toggles control the behavior:

- `--notify-on-failed` (default on) — hard failures
- `--notify-on-delivered-in-input-box` (default on) — soft failures

The notice carries the original message id, the recipient, the
failure class, the reason, and a 200-char body preview. Loop
prevention: a notice that itself fails to deliver does NOT generate
another notice.

## 4. MCP tool naming + discoverability

Current surface:

- `tmux-tell.send`
- `tmux-tell.control`
- `tmux-tell.inbox`
- `tmux-tell.status`
- `tmux-tell.agents`
- `tmux-tell.whoami`
- `tmux-tell.register`
- `tmux-tell.unregister`
- `tmux-tell.message_status`

Consistency observations:

- **Naming inconsistency: `message_status` vs `whoami`.** The
  former is `<noun>_<state>`; the latter is `<verb>`. Both
  internally consistent; not consistent with each other. **Verdict
  (Surveyor pass on #36): 1.0-candidate, not paper-cut.** Rationale:
  the CLI/MCP naming asymmetry is internal noise that doesn't trip
  operators in practice (they use one surface at a time — Claude
  calls MCP, operator calls CLI). The fix cost is real (deprecation
  cycle + tooling muscle memory), so it fits the 1.0-break window
  per `CHANGELOG.md`'s 1.0-trigger criteria.
- **Verb-noun split: `agents` (list) vs `register` / `unregister`
  (mutate).** Acceptable; `agents` reads as a noun-as-list which is
  the convention for many MCP tool suites. No change recommended.
- **`message_status` could be `track` to match the CLI subcommand
  name.** Symmetry would be nice. Defer to the 1.0 break window
  rather than introducing it now (same reasoning as the
  `message_status`/`whoami` verdict above).

**Recommendation:** record these in the project's "1.0 break
candidates" list (when that list exists; CHANGELOG `[Unreleased]`'s
1.0-trigger section is the natural home). No immediate action.

## 5. Documentation operator-walkthrough

A new operator picking up the bus tomorrow: what would they hit?

| Step                                | Documented?     | Friction                                                                                                    |
|-------------------------------------|-----------------|-------------------------------------------------------------------------------------------------------------|
| Install on a new host               | Yes             | `make build` + `sudo ./install.sh` — clean                                                                  |
| Register the first pane             | Yes             | README "## Use from Claude Code (MCP)" section is clear                                                     |
| Send the first message              | Yes             | Quick-start example near top of README                                                                       |
| Understand the mailman model        | Partial         | README mentions "per-recipient mailman"; the design rationale (single-writer-per-recipient to avoid races) is in `docker/...` design notes. Could be on the README itself for 1.0 |
| Recover after a tmux restore        | Yes¹            | README's "Canonical names and aliases" section explicitly explains the recipe                              |
| Investigate a failed message        | Partial         | `tmux-tell-claude track <id>` is documented; the WARN log discovery flow ("grep journalctl") is folklore           |
| Tune the probe-and-watch gate       | Yes             | `serve --quiet-*` flags are self-documenting via `--help`; rationale in README                              |
| Add a new agent                     | Yes²            | `tmux-tell.register name=X alias=Y` recipe                                                                  |

¹ Supported since v0.2.0 (silent-drift detection + canonical-name resolution).
² Supported since v0.2.1 (alias collision detection + fail-loud drift).

### Proposed README improvements

- [ ] **Add "Diagnosing a failed/unverified message" section.** One
  page describing the `track` → `journalctl grep` → fix workflow.
  Currently folklore. Severity: friction, size/S.
- [ ] **Promote the mailman-design rationale to the README** (or to
  `docs/`). Currently buried; helps new contributors not "simplify"
  the single-writer property back to a race condition. Severity:
  paper-cut, size/S.
- [ ] **CHANGELOG.md link in README.** Already linked via the
  Versioning section but worth a more prominent cross-reference for
  operators trying to figure out "what changed since the binary I
  have installed?" Severity: paper-cut, size/XS.

## 6. What the operator wished worked differently but it's actually fine

Honest section per the AC ("guard against scope creep on
follow-ups").

- **The 500ms settle delay.** It feels slow when sending many small
  messages back-to-back; but every attempt to shorten it has reopened
  the Enter-not-firing class (#4). Stays.
- **The 5-minute MaxWait on probe-and-watch.** Feels long when the
  operator is watching live; but the empirical data (deliver-time
  histogram once we have it) will tell us whether it's actually
  hitting. Don't tune from intuition. Stays.
- **`tmux-tell.agents` includes paused agents with `paused=true`
  status.** Initial reaction: "why are paused agents in the list?"
  Realized: pausing is the kill-switch, the operator needs to see
  them to un-pause. Correct as designed.
- **`reset --confirm` requires the literal flag, not interactive
  prompt.** Initial reaction: "verbose." Realized: scripts and
  the bus shouldn't be able to fire this by accident; the literal
  flag is the right safety. Stays.

## 7. Follow-up checklist

For the operator (Alex) to batch into Forgejo issues. The three
buckets below are on **two orthogonal axes**: severity (friction /
paper-cut / blocker — how badly it stings) and timing (file-now vs
defer-to-break-window — when the fix should land). A friction-class
item that requires a breaking change defers to the 1.0 bucket
regardless of severity. The buckets reflect timing first, then
severity-within-timing.

Friction-class items ordered most-impactful-first per Admin's
operator-side intuition; future operators may prioritize
differently. The ordering is informative, not canonical.

**Friction (file now):**
- `control` flag-ordering trap
- `status` augmentation: per-day counters + crash count
- `discover` operator-mode improvements
- `drift_check_ambiguous` WARN: add registration recipe inline
- README: "Diagnosing a failed/unverified message" section

**Paper-cut (batchable; file when one becomes worth a dedicated PR):**
- Error-message next-step hints (single PR covering several)
- `track --watch`
- Promote mailman-design rationale to README
- CHANGELOG cross-reference

**1.0-candidate (defer to the 1.0 break window regardless of severity):**
- `message_status` → `track` rename for CLI/MCP symmetry
- Naming consistency review for the full `tmux-tell.*` surface

## What this audit does NOT cover (out of scope)

- Big UX redesigns (a TUI for the bus, a web dashboard for the
  audit log, etc.). Different conversation.
- Re-litigating naming decisions that are stable (the `tmux-tell.*`
  namespace is keeping; the open question is consistency for new
  additions).
- Anything requiring a rewrite of core mechanics.

## Glossary

| Term           | Meaning                                                                                              |
|----------------|------------------------------------------------------------------------------------------------------|
| Paper-cut      | **Severity axis.** A minor annoyance the operator routes around; doesn't block work. Batchable.       |
| Friction       | **Severity axis.** A repeated annoyance; the operator builds workarounds or memos. Worth a dedicated fix. |
| Blocker        | **Severity axis.** The operator can't proceed without a fix. None of those at v0.2.1.                |
| 1.0-candidate  | **Timing axis** (orthogonal to severity). Worth fixing in the 1.0 break window per `CHANGELOG.md`'s 1.0-trigger criteria; not before. A friction-class item can still be 1.0-candidate if the fix involves a break. |

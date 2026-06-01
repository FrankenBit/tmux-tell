# Changelog

All notable changes to cli-semaphore are recorded here. The format
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
the project adheres to [Semantic Versioning](https://semver.org/).

Cadence (clarified per Surveyor review of v0.2.0):

- **Minor bumps** (`0.1.0` → `0.2.0`) carry **feature batches**.
  Strict semver would let backward-compatible additions ride patch
  bumps; we lift them to minor so an operator reading the version
  number knows "something genuinely new shipped." Minor bumps MAY
  break compatibility while we settle the post-MVP shape; the
  CHANGELOG entry calls out any break explicitly.
- **Patch bumps** (`0.2.0` → `0.2.1`) carry **bug-only fixes and
  policy corrections** — including default-behaviour changes that
  fix a bug class (e.g. v0.2.1's fail-loud drift policy). Patch
  bumps avoid removing or renaming existing fields/flags.
- **`0.x.y`** signals pre-1.0 instability. See `[Unreleased]`'s
  1.0 trigger discussion for the criteria we'd want before the
  major bump.

Run `claude-msg --version` to see what's installed.

## [Unreleased]

### Added

- **ADR-0001 amended with two new commitment slugs (#55):**
  - **`OperatorInputRowGate`** — the pre-delivery probe-and-watch gate
    gates on operator-input-row quiet, NOT pane-quiet (#52). Recipient
    mid-conversation / TUI animations / streaming output above the
    input row are explicitly out of scope. Two new pins in
    `internal/tmuxio/pin_test.go`.
  - **`CapExemption`** — operationally-critical signals (currently:
    delivery-failure notices) bypass `MaxRecipientQueue` and
    `MaxSenderBacklog` (#53). One new pin in
    `internal/store/pin_test.go`.

  Register grows 4 → 6 commitments / 8 → 11 pinning tests. The
  marker-block parser anchor (`<!-- pin-slug-register-start -->` /
  `-end`) is unchanged so #51's CI lint picks up the new slugs
  automatically. `docs/failure-modes.md` §4.2 table updated.

- **`check-pin-slugs` CI lint (#51).** Enforces ADR-0001's
  discipline-pin slug register against the slugs actually in use
  across the codebase. The lint parses the marker-block-delimited
  slug list from `docs/adr/0001-discipline-pins-as-test-category.md`
  + scans every `_test.go` file for `testpin.Triage(t, "<slug>", ...)`
  calls; any slug in use that isn't registered fails the lint with a
  clear pointer to the ADR for amendment vs typo-correction.
  - `make check-pin-slugs` runs the check locally
  - `.forgejo/workflows/test.yml` runs it on every CI pass
  - Promotes ADR-0001's "deliberate act" framing for adding a fifth
    slug from convention-only to mechanical gate.

- **`claude-msg discover --apply-aliases` (#46).** Detects long
  `--resume` values that contain an existing canonical short name as
  a whitespace-bounded substring and ADDs them as aliases on the
  existing canonical, rather than creating duplicate registry rows.
  - Without the flag: alias proposals are surfaced as
    `alias_proposed` status rows + a hint to re-run with the flag.
    No changes made.
  - With the flag: alias added via `store.AddAlias` AND the
    canonical's pane_id rebound to the discovered pane so future
    deliveries land correctly.
  - Ambiguous cases (multiple canonicals match as whole-word tokens
    inside the long name) are explicitly NOT proposed — those still
    create new rows so the operator can manually disambiguate.
  - Closes the post-tmux-restore duplication described in
    `docs/operator-ux.md` §2.2.

- **Host-level config file (#54).** `/etc/cli-semaphore/config.toml`
  (overridable via `CLAUDE_MSG_CONFIG` env var) carries per-host
  mailman settings — notification toggles, drift policy, quiet-gate
  tuning. Per-agent override via `[agent.<name>]` sections.
  - **Precedence chain (most specific wins)**: CLI flag > per-agent
    block > [defaults] block > hardcoded compile-time default.
  - **Missing-file**: silent fallback to hardcoded defaults (no
    error on fresh-from-install setups).
  - **Malformed-file**: WARN logged to stderr; mailman falls back to
    hardcoded defaults so a bad config doesn't take the mailman down.
  - **`claude-msg config show --agent NAME`** subcommand prints the
    resolved config so the operator can debug precedence without
    tracing through TOML manually. Both `--format text` and `--format
    json` supported.

- **Monitoring stack (#42, #45, #39, #41).** New `internal/healthscan`
  package + `claude-msg health` subcommand + `--today` flag on
  `claude-msg status`. Sources operational state from journalctl +
  systemd rather than persistent in-process counters, so CLI tools and
  mailmen stay decoupled.
  - **`claude-msg health [--since DURATION] [AGENT...]`** (#42) —
    one-command per-agent operational audit. Counts: delivered,
    delivered_unverified, failed, quiet_cap_exceeded, drift_ambiguous,
    drift_detected_unrecoverable. Deliver-time percentiles
    (p50/p95/p99) computed from `delivering id=X` ↔ `delivered id=X`
    pairs. systemd NRestarts surfaces as crash count. Defaults to
    24-hour window across every registered agent. Highlights
    actionable signals in a NOTES block below the table.
  - **`claude-msg status --today`** (#45) — augments the per-agent
    status output with a today block (since 00:00 local) covering the
    same counters + crash count + deliver-time percentiles. Same
    healthscan source.
  - **Deliver-time histogram** (#39) — the percentile computation in
    healthscan is the histogram primitive; surfaced via both health
    and status --today.
  - **Per-mailman crash counter** (#41) — sourced from systemd's
    NRestarts property; surfaced via both subcommands.

- **`claude-msg track --watch` (#49).** Polls the message state every
  `--watch-interval` (default 5s) and re-renders on each transition.
  Exits when the message reaches a terminal state (`delivered` /
  `failed`) or `--watch-timeout` fires. Clean SIGINT handling. The
  "I just sent a long autonomous task; ping me when it's been
  consumed" pattern now needs no wrapper script.

### Fixed

- **`WARN drift_check_ambiguous` carries the fix recipe (#47).** The
  log line now ends with `(resolve via: semaphore.register
  name=<canonical> alias=<unique-suffix> force=true; #47)` so the
  operator gets the actionable recipe inline without needing to grep
  docs.

### Changed

- **Probe-and-watch quiet-pane gate is now opt-in (default OFF).**
  `--quiet-disabled` default flipped from `false` to `true`; the
  hardcoded fallback in `internal/config/config.go` for unconfigured
  agents also flipped. Empirical use during the Binnacle M2.11
  exchange showed the gate adding up to 5 min worst-case latency
  while not preventing the mid-turn collisions it was designed to
  guard against — the post-#53 verify-token retry +
  `delivered_unverified` notice path (independent toggle, on by
  default) is the load-bearing transparency safety net. Re-enable
  per agent with TOML `quiet-disabled = false` or
  `--quiet-disabled=false` if the polite-wait shape is wanted for a
  specific recipient. README + flag-help text record the decision
  context. Operator call 2026-06-01 after Surveyor's
  `delivered_unverified` triage surfaced two timeouts in ~6h of
  moderate-traffic exchange.

- **README "Diagnosing a failed or unverified message" section added
  (#48).** Walks through the `track` → journalctl → fix flow with
  common cause patterns (`drift_check_ambiguous`,
  `drift_detected_unrecoverable`, `quiet_cap_exceeded`, mailman
  down). The probe-and-watch gate section is also updated to reflect
  the post-#52 two-dash design + the post-#53 notification surface.

### Fixed

- **CLI flag-ordering trap closed (#44).** Operator typing
  `claude-msg control alice --command compact` (recipient-first, the
  natural English order) used to silently drop `--command` because
  Go's `flag.Parse` stops at the first non-flag positional. The
  resulting "command required" error confused operators every time.
  - New `reorderFlagsFirst(fs, args)` helper in
    `cmd/claude-msg/flagorder.go` pre-reorders args so flag tokens
    land at the front and positionals at the back, regardless of how
    the operator typed them. The FlagSet is consulted (via
    `Lookup` + the `IsBoolFlag()` interface) so the helper knows
    whether a flag swallows the next token as its value.
  - Applied to every subcommand that takes positional args: `send`,
    `control`, `track`, `inbox`, `log`, `pause` (and therefore
    `resume`, which shares the handler).
  - `control` additionally auto-binds a trailing single positional to
    `--to` when `--to` is empty — closes the operator's actual
    friction case where the agent name was typed positionally.
  - Handles `--flag value`, `--flag=value`, bool flags, the `--`
    terminator, and unknown flags (which assume no value-swallow so
    unknown-flag errors surface cleanly rather than eating positionals).

### Added

- **Delivery-failure notification (#53).** The mailman now auto-inserts
  a `delivery_failure_notice` back to the original sender when one of
  its outbound messages transitions to a terminal-failure state. The
  notice carries the original message id, recipient, failure class,
  reason, and a body preview. Closes the "Bosun spent half a day
  waiting" failure mode where senders had no push-signal of dropped
  messages.
  - New message `Kind`: `KindDeliveryFailureNotice` in
    `internal/store/types.go`.
  - New store method: `Store.InsertNotice` — bypasses
    `MaxRecipientQueue` and `MaxSenderBacklog` caps. Notifications are
    operationally critical; losing them on cap would defeat the point.
  - Two independent CLI toggles on `claude-msg serve`:
    - `--notify-on-failed` (default `true`) — hard `failed` state
      transitions (drift unrecoverable, MarkFailed, paste error, etc.)
    - `--notify-on-delivered-unverified` (default `true`) — soft
      `delivered_unverified` state (paste+Enter completed but verify
      token didn't surface).
  - **Loop prevention**: a notice that itself fails to deliver does
    NOT generate another notice. Check by kind at the failure hook
    site (`maybeInsertFailureNotice`).
  - Cap-exemption commitment is worth pinning as ADR-0001 amendment
    if/when the discipline matters across the codebase's life.

### Changed (behavioral break — pre-1.0 minor break per cadence rules)

- **Probe-and-watch gate redesigned to operator-only two-dash check
  (#52).** The v0.2.1 four-way verdict
  (`DeltaQuiet`/`DeltaInputActivity`/`DeltaTUINoise`/`DeltaProbeMissing`)
  is replaced by a simpler two-way verdict
  (`DeltaQuiet`/`DeltaInputActivity`). The gate's contract is now
  explicit: protect against operator-typing on the receiving pane,
  ignore everything else.
  - **Wire (per-iteration):** paste `─` (dismisses ghost-text
    suggested prompt) → wait `ObserveWindow` → paste `─` (the actual
    probe) → wait `ObserveWindow` → capture. Input row must end with
    exactly `N` trailing probes (`prevAccumulated + 2`) AND the
    `before` capture's matching row equals the stripped result.
    Otherwise → `DeltaInputActivity` → back off.
  - **Probes NEVER backspaced between iterations.** Probes accumulate
    in the input box as a visible "I see you" stack until the operator
    clears them or the gate exits (quiet or cap).
  - **Conversation-area streaming no longer blocks delivery.** The
    2026-05-31 28ca incident (30× `tui_noise` over 5 minutes during
    heavy Claude Code work) would deliver on first cycle under the
    redesign.
  - **First-probe `input_activity` false positive fixed.** The
    2026-05-31 3c0c / 496e pattern (70s wait per delivery on idle
    panes) goes away because dash #1 dismisses the ghost-text
    suggested prompt before dash #2 lands.
  - **`QuietOpts.TUINoiseBackoff` removed.** No more TUI noise
    verdict; no more separate backoff for it. `ObserveWindow` default
    drops from 5s to 3s (now applied twice per iteration, between
    dash #1 → dash #2 and dash #2 → after-capture).
  - **CLI surface:** `--quiet-tui-backoff` flag removed from
    `claude-msg serve`. `--quiet-observe-window` semantics updated
    (now per-probe, not per-iteration).
  - **Discipline-pin implications:** the gate's "input-row-only,
    not pane-wide" claim is a real architectural commitment worth
    considering for ADR-0001 amendment + an `OperatorInputRowGate`
    slug. Deferred to a follow-up touch.

### Known limitations (recorded, not blocking)

- **`store.AddAlias` / `SetAliases` cross-canonical collision check
  has a TOCTOU window.** The check (`checkAliasCollisions`) reads
  the agent registry outside the UPDATE transaction. Concurrent
  registrations could both pass the check before either's UPDATE
  commits, allowing a collision that the runtime ambiguity-detection
  then has to catch. Mitigated in practice by:
  1. The `_txlock=immediate` DSN setting (#29) — each writer's
     `BEGIN IMMEDIATE` blocks until prior writers commit, so the
     window is microseconds, not seconds.
  2. The alex-as-sole-registrar reality on alcatraz — concurrent
     registers don't happen in practice.
  Worth tightening (pull the check inside the UPDATE transaction)
  if/when concurrent register becomes a real pattern. Per Surveyor
  v0.2.1 review acknowledgment.

### Notes for 1.0 trigger (Surveyor review of v0.2.0)

Before bumping to 1.0 we want **K=3 release stability across all
public surfaces** — MCP tool schemas, CLI subcommand args/flags/exit
codes, `--format json` shapes, the database schema, and the public
Go API for `discover` / `store` / `tmuxio` packages — AND a
deprecation policy committed (post-1.0 breaks require a deprecation
cycle: deprecate in N.x, remove in N+1.0) AND the
Binnacle-absorb-or-coexist decision settled. Tracked informally
here until it becomes actionable.

## [0.2.1] — 2026-05-31

### Fixed

- **Q(a): exact-match alias ambiguity in canonical resolution.** The
  Pass-1 exact-match logic in
  `discover.Walker.{LookupByName,PaneAgentName}WithCanonicals`
  walked canonicals in slice order and returned the first hit. When
  two canonicals shared an alias (or one canonical's name was
  another's alias), the resolver silently picked by slice order
  instead of flagging ambiguous. New `exactMatches` helper collects
  ALL canonical matches; >1 returns ambiguous=true.
- **Q(a): registration-time alias collision rejection.** New
  `store.ErrAliasCollision`. `SetAliases` / `AddAlias` now reject
  cross-canonical collisions at registration time (an alias already
  claimed as another agent's name OR as another agent's alias). The
  `semaphore.register` MCP handler surfaces the error verbatim so
  the operator knows immediately. Self-rebind (re-adding the agent's
  own alias) stays idempotent.

### Changed

- **Q(b): drift-ambiguous + drift-unrecoverable now MarkFailed by
  default.** Previously these paths logged WARN and delivered to the
  drifted (or ambiguous) pane — re-creating the silent-bad-delivery
  class for autonomous receivers (the 2026-05-31 misdelivery
  scenario). v0.2.1 changes the default: MarkFailed surfaces the
  issue immediately to the sender. New `--drift-soft-fail` flag
  preserves the pre-v0.2.1 behaviour for ops that need it.

## [0.2.0] — 2026-05-31

### Added

- `--version` flag on `claude-msg` (with `-v` / `version` aliases).
  Built-time stamping via `-ldflags` from `git describe`. Bare
  `go build` reports `dev`.
- Pre-delivery silent-drift detection (#37). The mailman now reads
  the registered pane's `--resume` argument before delivery; if it
  doesn't match the expected agent, runs discover to find where the
  agent moved to, updates the registry, and retries on the new pane.
  Closes the silent-misdelivery gap surfaced 2026-05-31.
- Discover canonical-name + alias resolution (#38). `agents` table
  gains an `aliases` column. `semaphore.register` accepts an optional
  `alias` field. Resolution order: exact canonical → exact alias →
  case-insensitive substring fallback. Ambiguous matches are logged
  rather than guessed.
- `store.SetAliases` / `store.AddAlias` (idempotent).
- `discover.Walker.PaneAgentName` (raw, no canonicals).
- `discover.Walker.LookupByNameWithCanonicals` and
  `PaneAgentNameWithCanonicals` for canonical-aware lookup.

### Changed

- The mailman serve loop now opts into the silent-drift guard via
  `serveOpts.DriftCheckDisabled` (default off in production, set
  true by tests that don't fake `ListPanesWithPID`).
- Probes are now backspaced on `DeltaTUINoise` so they don't
  accumulate in the input box during long agent-busy stretches
  (cd969ea — was visible to operator as "probe creep").


## [0.1.0] — 2026-05-31

Initial tagged release. The repository contains the full MVP plus
two days of post-MVP hardening; this tag is the baseline going
forward, not a "first release" cut. The list below is intentionally
condensed — see git log for full audit.

### Core (MVP, M1-M7)

- SQLite store package: schema migration, WAL pragmas, agents +
  messages CRUD (#2).
- `send` subcommand with cap enforcement + JSON contract (#3).
- `inbox` + `status` subcommands (#4).
- `agents` + `whoami` discovery subcommands (#15).
- tmux delivery primitive: named-buffer paste + post-paste
  verification (#5).
- `serve` mailman daemon: loop, orphan recovery on startup (#6).
- systemd template unit + per-agent journal logging (#7).
- `pause` / `resume` operator controls (#8).
- `reset --confirm` for wedged-state recovery (#9).
- `--reply-to` flag + threaded headers (#10).
- `log --thread <id>` inspection (#11).
- Boot-time pane discovery (#12).
- Install script + alcatraz-infra integration (#14).
- MCP subcommand: native semaphore.* tools (#16).

### Post-MVP

- Identity precedence helper: explicit override / `$CLAUDE_AGENT_NAME`
  / `$TMUX_PANE`→registry (#27).
- `claude-msg control` CLI subcommand mirroring `semaphore.control`
  (#26).
- Whitelisted control commands with two-axis (self/peer) scope
  (#24): `compact`, `rename`, `cost`, `help`,
  `mcp-enable-semaphore`, `mcp-disable-semaphore` (self-only after
  scope flip), `mcp-restart-semaphore` (peer-safe macro).
- `semaphore.message_status` + `claude-msg track <id>` for delivery
  state (#31).
- Atomic cap enforcement: `_txlock=immediate` + in-transaction depth
  check; `ErrRecipientQueueFull` / `ErrSenderBacklogFull` sentinels
  (#29).
- Pre-delivery quiet-pane gate (probe-and-watch). Four-way verdict:
  Quiet / InputActivity / TUINoise / ProbeMissing. Input row
  identified by where the probe landed (not cursor_y). Probes
  cleaned up on every exit path. Watchdog-aware sleeps so long
  backoffs don't trip WatchdogSec (#30, #32, hotfixes 5a0f0ee
  through cd969ea).
- Verify-after-Enter soft-fail: `ErrUnverifiedDelivery` returns when
  paste/Enter mechanically completed but the token wasn't surfaced
  within ~5s. Mailman marks delivered + WARN rather than failed
  (510e74c).
- 500ms settle delay between `paste-buffer` and `send-keys Enter` so
  the TUI ingests the paste before the submit keystroke (f01c370).
- Probe backspaced on `DeltaTUINoise` so probes don't accumulate
  during long agent-busy stretches (cd969ea).

### Tests

- Concurrent regression for cap-as-ceiling property
  (`internal/store/messages_concurrent_test.go`).
- Wire-shape contract tests for `trackResult` (CLI/MCP byte-identity
  + `omitempty` contract).
- Verdict regression tests for the probe-and-watch gate, including
  the rendering-cursor-elsewhere case from the 2026-05-31 Bosun
  incident.

### Operator surface

- Documentation, README, install path documented for alcatraz.
- Forgejo Actions CI workflow (without `-race` — CI runner lacks
  cgo; local pre-commit uses `-race`).

### Known limitations

- `discover` matches `--resume` argument values verbatim; canonical
  short names (`bosun`, etc.) don't auto-resolve to long names
  (`Master Bosun of Nimbus`). Operator workaround: `semaphore.register
  name=<canonical> force=true` after a tmux restore. Tracked as #38.
- Silent pane drift at delivery time isn't caught by auto-heal —
  the existing recovery only fires on "can't find pane" errors.
  Tracked as #37.

[Unreleased]: https://git.frankenbit.de/frankenbit/cli-semaphore/compare/v0.2.1...main
[0.2.1]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.2.1
[0.2.0]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.2.0
[0.1.0]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.1.0

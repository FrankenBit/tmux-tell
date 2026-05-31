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

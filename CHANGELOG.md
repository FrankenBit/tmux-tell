# Changelog

All notable changes to tmux-msg (originally `cli-semaphore`,
re-grounded on its substrate primitive in v0.5.0) are recorded here.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and
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

- **Post-1.0 deprecation** follows the two-minor-cycle floor in
  [ADR-0008](docs/adr/0008-deprecation-policy.md): a deprecated public surface
  stays functional for at least two minor cycles (deprecate in `v1.X`, earliest
  removal `v1.X+2`), emits a `WARN deprecated_surface_used` log, and gets a
  `### Deprecated` entry here. Pre-1.0 keeps the looseness above.

Run `tmux-msg-claude --version` to see what's installed (`claude-msg` works as a
deprecated alias through the v1.0 stability boundary — earliest removal extended
at the v0.11.0 cut per ADR-0008 §Discretion clause; operator decision 2026-06-08).

## [Unreleased]

### Changed

- **Mailman unit templates add `StartLimitBurst=5` + `StartLimitInterval=60s`
  ([alcatraz-infra#40](https://git.frankenbit.de/frankenbit/alcatraz-infra/issues/40)).**
  Belt-and-suspenders against the restart-flood class behind
  [alcatraz-infra#39](https://git.frankenbit.de/frankenbit/alcatraz-infra/issues/39).
  v0.16.1's #338 + #340 substantively closed the original failure mode by
  preventing both the orphan unit (#338 disables on unregister) and the
  restart-loop (#340 exits with 0 on agent-not-found so `Restart=on-failure`
  doesn't trigger). The unit-template cap is insurance against future failure
  modes that exit non-zero in a tight loop: after 5 starts in 60s, systemd
  marks the unit `failed` and stops restarting until manual `systemctl
  --user reset-failed`. Same directives added to both adapter templates
  (`tmux-msg-claude-mailman@.service` + `tmux-msg-codex-mailman@.service`)
  for symmetry.

- **Delivery-failure notices are now a compact one line — #362.** When a
  message lands `failed` or `delivered_in_input_box`, the auto-notice back to
  the sender was a verbose six-line block (`:warning: Delivery failure / id /
  recipient / class / reason / body-preview`) that cluttered the pane (operator
  feedback). It's now a single greppable line —
  `:warning: <id> → <recipient> <class>: <reason> — resend <id>` (the
  `delivered_in_input_box` soft-fail renders as `unverified`). The original body
  is no longer inlined; full detail stays recoverable on demand via
  `track <id>` / `get <id>`, so compacting **relocates** verbosity to a query
  rather than dropping it. Same trigger, same `notify-on-*` knobs, same
  cap-exemption. The out-of-band hook channel + agent-discretion directions from
  #362 are deferred to #379 (gated on #336's hook-context-ack).

### Added

- **`install.sh` becomes a substrate-honest hard-cut — #349 Fix 2.**
  Adds a `bootstrap` subcommand to the binary + a new orchestration path
  in `install.sh` so an operator gets a fully-wired bus in one
  invocation instead of remembering a manual post-install ritual. The
  bootstrap path runs as the operator (install.sh drops privs) and
  fires six steps: (1) `systemctl --user daemon-reload`, (2) stale-DB
  detect — if the pre-#308 `/var/lib/tmux-msg/messages.db` is the only
  DB present, delegate to `db migrate` (#349 Fix 3); if both legacy
  AND user-home default exist, abort, (3) `discover` to populate the
  agents table from the current tmux state, (4) `systemctl --user
  enable --now` per non-hook-context agent, (5) orphan walk of
  `~/.config/systemd/user/` for `tmux-msg-<adapter>-mailman@<NAME>.service`
  instance units whose `<NAME>` isn't in the freshly-discovered agents
  table — print by default, disable with `--prune-orphans` (composes
  with #338 for the prevention side of the alcatraz-infra#39
  ghost-tenant pattern), (6) `refresh-all-mcps` so chamber MCPs rebind
  to the freshly-installed binary + canonical DB. `install.sh
  --no-bootstrap` opts out of steps 1-6 and prints the historical
  manual next-steps for operators who want full control. `agents
  --format json` gains a `delivery_mode` field so the bootstrap (and
  future filtered consumers) can skip hook-context agents in mailman
  iteration without a second lookup.
- **`tmux-msg-claude db migrate <new-path>` atomic helper — #349 Fix 3.**
  New CLI sub-primitive that wraps the WAL-safe DB move recipe documented in
  v0.16.1 into one command: (1) validate destination, (2) stop per-agent
  mailmen via `systemctl --user`, (3) `PRAGMA wal_checkpoint(TRUNCATE)` on
  the source, (4) `mv` source → destination (with a cross-volume copy
  fallback for EXDEV), (5) clean source `-wal` + `-shm` sidecars, (6)
  restart per-agent mailmen, (7) fire `refresh-all-mcps` against the
  destination so chamber MCPs rebind to the new inode, (8) self-verify
  by counting `messages` rows in the moved DB. `--dry-run` prints the
  plan without touching the filesystem or systemd. Hook-context agents
  are skipped on steps 2 + 6 (no mailman to start/stop). If the
  destination is **not** the user-home default the binary computes on
  its own, steps 6 + 7 are skipped with a warning — systemd-managed
  mailmen + chamber MCPs both resolve only the default path post-#308,
  so a bespoke destination needs foreground `serve` + manual MCP
  restart on the operator side. The command also warns when
  `$CLAUDE_MSG_DB` is set in the calling shell, since it can't rewrite
  rc files itself. Refuses to overwrite an existing destination
  (operator-typo guard) and refuses source-equals-destination.
- **`ping` surfaces a structured reason on UNREACHABLE — #358.** A timed-out /
  failed `ping` previously collapsed mailman-down, blocked-delivery,
  backlog-draining, a dead pane, and a parked (stuck) mailman into one opaque
  UNREACHABLE — but the operator's recovery differs per condition. The `ping`
  response (and the matching `tmux-msg.ping` MCP tool) now carry a closed-set
  `reason` (`pane_dead` / `mailman_down` / `stuck` / `blocked_delivery` /
  `backlog_draining`) plus an `evidence` block (`mailman_active`, `queue_depth`,
  `current_state`, `stuck_reason`) on the UNREACHABLE path; the CLI renders it
  as a short suffix
  (`UNREACHABLE (mailman_down: mailman daemon not running; queue=1, state=unknown)`).
  Reuses existing substrate signals (`mailmanActive`, `RecipientQueueDepth`,
  `agents.stuck_reason`, the agent_state probe) — distinguishability is the
  contribution, no new substrate mechanism. Reason/evidence are omitted on the
  reachable path, so the OK wire shape is unchanged. (`last_delivered_at` /
  `mailman_idle_since` from the issue's example join `evidence` when #348 lands
  the `agents`-listing source for them.)
- **`ping` adds a coarse reachability `class` over the structured reason — #366.**
  #358's flat UNREACHABLE umbrella over-claimed brokenness for the healthy cases
  (a `backlog_draining` agent is plainly reachable — mailman up, pane live, just
  draining a queue with our probe behind in line). `ping` (and the
  `tmux-msg.ping` MCP tool) now carry a closed-set `class` on **every** path —
  `reachable`, `pending`, or `unreachable` — layered over the existing `reason`:
  branch on `class` for coarse reachability-routing, on `reason` for fine
  recovery-routing. The map is single-sourced (`reachabilityClass`): a confirmed
  delivery is `reachable`; `backlog_draining` and `blocked_delivery` are
  `pending` (substrate healthy and making progress — retry or wait); and
  `mailman_down` / `stuck` / `pane_dead` are `unreachable` (broken — operator
  must act). `blocked_delivery` classes `pending` **unconditionally**: a ping
  short-circuits before the observe-gate (it never pastes), so it can only mean
  "mailman alive but busy on a prior delivery," never a paste-incapable
  force-defer or a wedged gate. The CLI headline is the three-way `REACHABLE` /
  `PENDING (…)` / `UNREACHABLE (…)`, each failing line carrying a trailing
  retryability hint
  (`PENDING (backlog_draining: …) — retry or wait, the mailman is working`).
  Reason/evidence remain omitted on the reachable path, so the OK wire shape
  gains only the `class` field.

### Changed

- **`ping` exit code is now keyed on the reachability class — #366.**
  `pingExitCode` previously mapped on the raw probe state (`delivered`→0,
  `timeout`→`EX_TEMPFAIL`, `failed`→`EX_UNAVAILABLE`). It now maps on the #366
  `class`: `reachable`→0, `pending`→`EX_TEMPFAIL` (75, retry may help),
  `unreachable`→`EX_UNAVAILABLE` (69, retry won't help). **Behavioral shift:**
  `mailman_down` and `stuck` (both `state=timeout`, previously `EX_TEMPFAIL`) now
  class `unreachable` → **`EX_UNAVAILABLE`** — a down or parked mailman won't
  self-heal on a retry, so tempfail over-promised recoverability.
  `backlog_draining` / `blocked_delivery` stay `EX_TEMPFAIL` (now via `pending`);
  `pane_dead` stays `EX_UNAVAILABLE`. Scripts branching on `ping`'s exit code for
  the mailman_down/stuck case should expect 69 where they previously saw 75.

- **Diagnostic surface for MCP/DB-binding divergence — #348 (PR 1 of 2).** When a
  deploy moves the DB but doesn't restart the long-lived MCP server processes,
  those processes keep writing to the orphaned inode — invisible to `sqlite3` on
  the canonical path and to fresh mailmen, so a sender's `queued: N` and a
  recipient's `queue_depth: 0` are both "correct" and nothing flags the
  divergence. Three new read-only surfaces make it legible without `/proc`
  archeology:
  - **`tmux-msg.whoami_db`** (MCP tool) — the live server's own binding
    `{pid, binary_path, started_at, db_path, db_inode, db_deleted}`, read from
    `/proc` (the open handle, not a re-resolution that could mask the divergence).
  - **`tmux-msg-claude doctor`** (CLI) — walks every live `tmux-msg-claude`
    process, compares each one's open DB inode against the canonical DB, prints a
    per-process verdict, and exits non-zero on any divergence (orphaned inode,
    different inode, or a since-replaced `(deleted)` binary). Usable as a runbook
    gate.
  - **`tmux-msg-claude track <id> --canonical`** — opens the canonical XDG-default
    DB by name (ignoring `--db` / `$CLAUDE_MSG_DB`), the operator's ground-truth
    "is id X actually in the canonical DB?" query.

  All `/proc`-based and read-only (consistent with the existing `discover`
  walker; Linux substrate). `docs/diagnostic-playbook.md` gains a "post-deploy
  MCP-binding divergence" entry pointing at `doctor` as the triage primitive.
  The `agents`-listing mailman-activity fields (`mailman_last_delivered_at` /
  `mailman_idle_since`, which need a store migration) follow in PR 2.

- **`agents` listing surfaces mailman delivery-recency — #348 (PR 2 of 2, closes #348).**
  `agents` (CLI + the `tmux-msg.agents` MCP tool) now carries
  `mailman_last_delivered_at` — the RFC3339 time of the most recent delivery to
  each agent — so the operator can spot the "queued but mailman silent"
  divergence smell (non-zero `queued` + empty/old last-delivered) in one glance.
  The CLI text view renders it as a compact `MAILMAN` column (`2m ago` / `3h ago`
  / `never`). **Derived from `messages.delivered_at`, not a stored per-agent
  column** (the investigation found the source already exists + is retained
  forever by default) — so there is **no write on the mailman delivery hot path**
  and no second source-of-truth to drift; a read-only covering index on
  `messages(to_agent, state, delivered_at)` keeps the per-agent MAX cheap as the
  table grows. The same `RecipientLastDelivered` derive feeds the #363/#366
  ping-evidence slot (one substrate-property, two consumer surfaces). Closes the
  #348 observability arc opened by PR 1.

## [0.16.1] — 2026-06-12

Fast-follow cluster from the v0.16.0 alcatraz deploy retro (alcatraz-infra#39
box-crash + post-deploy observations). Four substrate-hygiene fixes that
prevent recurring the failure modes that surfaced once the v0.16.0 substrate
hit production: chamber-rename ghost units, mailman restart-loops on missing
agents, silent version mis-stamping at install, and the WAL-strand-on-mv
deploy-procedure gap.

Headlines:

- **`unregister` now soft-fails systemctl errors (#338)** so a user-systemd
  flake can't strand the agents-table row — the row IS authoritative state;
  the systemd unit is a downstream consumer.
- **`serve` exits cleanly when its agent isn't in the DB or has no `pane_id`
  (#340)**. systemd's `Restart=on-failure` treats exit 0 as success-and-done
  rather than restart-looping; the alcatraz-infra#39 SQLite-contention freeze
  is now structurally impossible.
- **`install.sh` actually version-stamps the binary it ships (#342)**. Three
  compounding gaps closed in one cut; `--version` after install now reports
  the real `git describe` value rather than a years-stale hardcoded default.
- **Docs: WAL-safe DB-move recipe (#343)**. The substrate-honest "`messages.db`
  always has invisible siblings; never move it alone" rule + the
  checkpoint-then-`mv` (or `.backup`-dot-command) recipe, with a forward-going
  expectation pinned to future deploy notes.

Composes with the still-pending v0.17.0 cluster (#348 diagnostic surface
gaps, #349 install.sh as substrate-honest hard-cut with `db migrate`
sub-primitive) born from the same investigation. v0.16.1 fixes the
*recurrence*; v0.17.0 will fix the *observability* + the *self-recovery*.

### Fixed

- **`install.sh` now version-stamps the binary it builds (#342).** Three
  compounding gaps closed:
  1. `internal/version/version.go`'s default flipped from `var Version =
     "v0.7.0"` to `"dev"`. Pre-#342 an unstamped binary silently reported
     a three-release-stale version that wasn't its own; now it reports
     `"dev"` (matching the long-standing doc-comment intent).
  2. `install.sh` no longer skips the build step on a stale `bin/$BIN_NAME`
     from an earlier tag — the pre-#342 `if [[ ! -x ... ]]` guard meant a
     `git pull` + `install.sh` installed yesterday's binary.
  3. `install.sh` builds via `make bin/$BIN_NAME` (which applies
     `LDFLAGS=-X internal/version.Version=$(git describe ...)`) instead of
     plain `go build` (which inherits the source-default). After this
     fix, `tmux-msg-claude --version` after a fresh `install.sh` always
     reports the current `git describe` value (release tag for a clean
     tag, `vX.Y.Z-N-gSHA` between tags). Surfaced during the v0.16.0
     alcatraz deploy where the installed binary reported `v0.7.0` until
     manual `make build` + reinstall.
- **`serve` exits cleanly on agent-not-found (#340).** When `tmux-msg-claude
  serve --agent NAME` finds no DB row for `NAME` (or the row exists but
  `pane_id` is empty), the substrate now exits with status `0` instead of
  `69` (UNAVAILABLE). The systemd unit template's `Restart=on-failure`
  treats `0` as success and stops restarting; the pre-#340 `69` was
  restart-looped every 2 seconds, and under enough orphan units the
  restart-flood hammered SQLite into the contention freeze that caused
  [alcatraz-infra#39](https://git.frankenbit.de/frankenbit/alcatraz-infra/issues/39).
  The log line tells the operator how to recover (`register --name … --pane …`
  or `discover`, then restart the unit). Composes with #338 as defense-in-
  depth: #338 prevents the orphan unit from existing in the first place
  (unregister cleans systemd); this issue prevents the flood-cause if one
  exists anyway. Tests `TestServe_ExitsCleanWhenAgentUnregistered` +
  `TestServe_ExitsCleanWhenPaneEmpty` pin the exit shape. New
  `docs/reference.md` §"`serve` exit codes" documents the matrix.
- **`unregister` soft-fails systemctl errors (#338).** A user-systemd flake
  (e.g. "Failed to connect to user bus") no longer hard-fails the unregister
  and leaves the agent row stranded. The DB row removal proceeds; the
  response surfaces `mailman: "warn"` + `mailman_error: "<systemd output>"`
  instead of the happy-path `mailman: "stopped"`. Substrate-honest framing
  per the issue: the agents-table row is authoritative state, and a unit
  that survives the cleanup is now caught by #340's
  serve-exit-on-missing-agent path. Mirrored across the CLI
  (`tmux-msg-claude unregister`) and the MCP surface
  (`tmux-msg.unregister`); both gain test coverage for the systemctl-fail
  path. The pre-#338 hard-fail behavior was the latent gap behind
  [alcatraz-infra#39](https://git.frankenbit.de/frankenbit/alcatraz-infra/issues/39)'s
  stale `tmux-msg-claude-mailman@visitor.service` surviving a chamber rename.

### Documentation

- **WAL-safe DB-move recipe (#343).** New `docs/reference.md` §"Moving the DB
  safely" documents the SQLite WAL invariant — `messages.db` always carries
  invisible `-wal` + `-shm` sidecars; a plain `mv messages.db` strands them
  and discards every commit since the last checkpoint. The recipe spells out
  the substrate-honest deploy procedure (stop mailmen → `PRAGMA
  wal_checkpoint(TRUNCATE)` → move → cleanup → restart), plus the `.backup`
  alternative for single-step atomic copies. Surfaced during the v0.16.0
  deploy where ~14 hours of bus history were stranded in an orphaned WAL at
  `/var/lib/tmux-msg/`. **Release-notes-touching-DB-path-moves** going
  forward must include the checkpoint step or use `.backup`.

- **DB-move recipe amended with `refresh-all-mcps` step (#349 Fix 1).** The
  v0.16.0 deploy retrospective surfaced a second substrate-state gap behind
  the `mv messages.db` footgun: chamber MCP servers are stdio-spawned by
  Claude Code at session start, NOT systemd-managed. The `systemctl --user
  stop/start 'tmux-msg-claude-mailman@*.service'` cycle in the WAL recipe
  cleanly cycles mailmen but leaves long-lived MCP servers running on the
  OLD DB inode (file handle survives `mv` since the dirent rename doesn't
  invalidate the open fd). Result: chamber MCPs write to a ghost inode
  invisible to the new path; mailmen read from the canonical path; substrate
  ends up in two-DB split-brain with no surface flagging the divergence.
  Recipe now adds step 5 (`tmux-msg-claude refresh-all-mcps`) firing the
  `mcp-restart-tmux-msg` macro per registered agent so each chamber's
  Claude Code re-initializes its tmux-msg MCP stdio against the current
  binary + canonical DB. **Release-notes touching DB-path moves must
  mention the `refresh-all-mcps` step explicitly** — it's not optional and
  not implied by "restart mailmen"; the substrate-honest deploy procedure
  has to call it out. Substrate-empirical provenance: 2026-06-12 post-v0.16.0
  investigation found 2+ hours of bus messages from one chamber stranded on
  a ghost inode (no operator-facing symptom until manual `pgrep -af
  'tmux-msg-claude mcp'` — and `pgrep -af 'tmux-msg-codex mcp'` for codex
  chambers — surfaced the long-lived processes). Substrate-vs-adapter note:
  the recipe addresses Claude-chamber MCP refresh via `refresh-all-mcps`;
  codex chambers need manual codex-CLI restart since the substrate's
  `mcp-restart-tmux-msg` macro is claude-only per #248 (B). Same Unix
  file-semantics invariant; different macro-delivery surface.

## [0.16.0] — 2026-06-12

The Foreign decks cluster: substrate-vs-adapter pane-observation work
surfaced by Lookout's (Codex chamber) onboarding 2026-06-11. Four
substrate-witness observations — `agent_state` probe blindness on
non-Claude panes, paste-and-enter clobbering operator input, slow
verify-token round-trips, and MCP-path sender-resolution gap — together
with Codex's sandbox-DB-write motivation, drove eleven merged PRs that
close the substrate-side substrate-vs-adapter pane-observation contract.

Headlines:

- **Per-adapter `PaneProfile` contract (#322).** Substrate-honest
  decoupling of the pane-state classifier (`AgentState` / `ObserveGate`)
  from Claude-specific constants. Three of four Foreign-decks observations
  reduce to per-adapter config; the fourth (verify-token robustness)
  defers to `#336` grounded in 2026-06-12 cross-adapter probe data.
- **Second CLI adapter `tmux-msg-codex` (#248).** Ships alongside
  `tmux-msg-claude`, proving the [ADR-0009](docs/adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
  substrate-vs-adapter boundary. Codex's hook-context delivery composes
  with the #249 helper without substrate change.
- **Default DB moved to user-home (#308).** XDG-honored resolution
  (`$XDG_DATA_HOME/tmux-msg/messages.db` or `~/.local/share/tmux-msg/messages.db`)
  replaces the system-global `/var/lib/tmux-msg/messages.db`. Codex's default
  sandbox can now write the DB without per-write operator escalation. **Hard
  cut** — operators with an existing DB must `mv` it to the new path once
  at deploy time.
- **Three-act adapter-correctness discipline closed.** Codified (#314
  register tool-schema substrate-neutral) → embodied (#280/#315/#326 binary
  Profile threading through usage/help/schema) → enforced (#324 CI guard
  forbids `tmux-msg-claude` literal in `internal/cli` outside `profile.go`).
- **Paste-incapable adapter force-defer (#323 narrow fix).** Mailman
  refuses to paste-and-enter to adapters with `PasteCapable=false`,
  preventing the clobber while `#322`'s `PaneProfile` refactor lands the
  substrate-uniform resolution.

Plus the substrate-hygiene companions: #258a (deferred-delivery `register`
trigger), #289 (`unregister` reciprocal of `register`), #290 (startup
DB-path log on `mcp`/`serve`), #299 (`paneNotFoundBackoff` overflow guard
base-agnostic + counter-reset test), #300 (`mailman_stuck` Prometheus
gauge), #311 + #312 + #320 (diagnostic-playbook + reference + Codex MCP
docs grounded in Lookout's substrate-witness observations).

**Deferred to v0.16.1 and beyond.** Orthogonal observations from the
cluster work (#319 gauge label leak, #327 codex env-propagation
investigation, #328 doc-comment parallels, #332 Claude-pane observe-gate
temporal-delta) go to v0.16.1. Substrate-hygiene follow-ups surfaced by
the 2026-06-12 alcatraz box-crash retro (#338 `register`/`unregister`
systemd-unit cleanup, #339 lazy `pane_id` refresh on drift, #340
exit-cleanly on agent-not-found) go to v0.17.0. The verify-token
robustness arc (#336) lands in the Hull-and-rigging milestone with its
own substrate-witness-grounded design pass.

The Foreign decks milestone closes with v0.16.0.

### Added

- **Per-adapter `PaneProfile` pane-observation contract (#322).** The `internal/tmuxio` pane-state classifier (`AgentState` / `ObserveGate` / `extractInputContent`) now reads its prompt-sentinel + compaction / awaiting-operator / status-line snippets from a process-global `PaneProfile` — installed by `cli.Run` from the adapter's `Profile.Pane` — instead of hardcoded Claude Code constants. `ClaudePaneProfile` (assembled from the canary-pinned constants) is the package default and what the Claude binary supplies, so every existing classification path is behaviour-preserving. The codex binary now supplies `CodexPaneProfile` with its substrate-verified `› ` sentinel (U+203A + a **regular** space, bytes `e2 80 ba 20` — **not** Claude's NBSP). This resolves the codex side of two Foreign-decks observations with **zero per-adapter logic**: `agent_state` now classifies codex panes (no more "prompt sentinel not found → unknown"), and the observe-gate correctly defers paste-and-enter while a codex operator is typing — the substrate-uniform resolution of the #323 interim force-defer. Verified against live `%9` captures (cursor at sentinel → idle; cursor past → awaiting-operator) and mutation-checked (reverting a runtime read to the bare const reproduces the clobber). Codex stays `PasteCapable=false` pending verify-token robustness (#336); its marker fields are intentionally empty pending characterization of codex's compaction / popup / status UIs. The per-adapter **Verifier** seam (#322 observation 2 — the slow/fragile verify-token) is **deferred to #336**: a 2026-06-12 probe session reframed it from a codex quirk into a cross-adapter verify-token fragility (paste-collapse + mid-turn), which wants its own design grounded in that data rather than a speculative seam here.
- **Static check: `TestNoClaudeLiteralInCLISource` forbids hardcoded `tmux-msg-claude` in `internal/cli` (#324).** Converts #280/#315's "route through `active.BinaryName`" convention into a CI-enforced invariant. Walks all non-test `.go` files in the package via `go/ast`, inspects only `*ast.BasicLit` STRING nodes (comments are skipped), and fails with the offending file+line if the literal appears outside `profile.go` (the one allowed source-of-truth for the BinaryName default).
- **Codex MCP path: document `TMUX_AGENT_NAME` env-block requirement (#320).** Codex's
  MCP host does not propagate `$TMUX_PANE` to spawned MCP server processes, so the
  substrate's implicit `$TMUX_PANE → registry` sender-resolution fallback never fires.
  The remedy is a per-server `env` injection in `~/.codex/config.toml`:
  ```toml
  [mcp_servers.tmux-msg]
  command = "tmux-msg-codex"
  args = ["mcp"]
  env = { TMUX_AGENT_NAME = "lookout" }
  ```
  Documented in `cmd/tmux-msg-codex/README.md` §MCP server (new file) and
  `docs/diagnostic-playbook.md` §MCP-path sender-unknown. CLI and hook-context paths are
  unaffected — they run in the operator's shell where the environment is fully propagated.
  ADR-0009 boundary: fix lives in adapter docs, not substrate code. Surfaced by Lookout
  (Codex chamber, onboarding witness 2026-06-11).
- **Second CLI adapter: `tmux-msg-codex` (OpenAI Codex) — #248.** The substrate now ships a
  second adapter binary alongside `tmux-msg-claude`, proving the [ADR-0009](docs/adr/0009-hook-context-delivery-substrate-vs-adapter-boundary.md)
  substrate-vs-adapter boundary: all subcommand dispatch + handlers moved into an
  adapter-agnostic `internal/cli`, and each binary is a thin wrapper supplying its adapter
  `Profile` (binary name → usage/version/mailman-unit chrome + deprecation alias). Codex
  delivers via `hook-context` — its hook output schema (`hookSpecificOutput.hookEventName` +
  `additionalContext`) matches Claude's, so the #249 helper presents messages **with zero
  substrate changes**. Install with `./install.sh --adapter=codex` (coexists with claude;
  each adapter gets its own `tmux-msg-<adapter>-mailman@` unit, both share the bus DB). New
  `hook-context --event-name <Event>` flag pins the echoed `hookEventName` deterministically
  (Codex requires the output event name to match the firing event but doesn't document its
  hook stdin schema). See `docs/reference.md` §Adapter integration (verified codex-cli
  0.130.0, 2026-05-10). The paste-and-enter observe-gate stays Claude-only — deferred until
  a concrete paste-needing adapter surfaces.
- **Deferred-delivery `register` trigger — spawn-die session bridge (#258a).**
  `send --deliver-after=register` (and the MCP `deliver_after:"register"`) stages
  a message addressed to another agent that auto-promotes to queued when that
  agent next (re)registers — "remember this for its next dispatch" (e.g. Pilot's
  dispatch-across-sessions pattern). No explicit `flush_deferred` is needed: the
  register IS the trigger fire, on both the CLI and MCP register paths. The
  register response reports `deferred_promoted` (count, non-zero only). Promoted
  rows deliver immediately rather than being announced as backlog — they bypass
  the #204 floor via #227's `deliver_after` exemption, and the promotion runs
  *after* the backlog-announce policy so register messages don't trigger a
  spurious 📬 nudge. The remaining `#258` triggers — timestamp/duration
  scheduling and `OR`-composition — refile to #295 (build-on-demand). Closes #258.
- **`mailman_stuck{agent,reason}` Prometheus gauge (#300).** When metrics are
  enabled (`--metrics-addr`), the gauge is set to `1` when the mailman parks
  in the `#291` stuck state and drops to `0` when the stuck state is cleared
  (via `register --force` or `ClearStuck`). Initialises from the DB on every
  startup — a mailman restarted against an already-parked agent sets the gauge
  before attempting any delivery, so the park window is fully visible in
  Grafana/alerting even across daemon restarts. Two integration tests pin both
  transitions; the unit test pins nil-safety.
- **`mcp`/`serve` startup DB-path log (#290).** Both `tmux-msg-claude mcp` and
  `tmux-msg-claude serve` now emit an INFO line at startup naming the resolved
  DB and its resolution source:
  ```
  mcp: claude_msg_db=/tmp/crew-demo.db source=env(CLAUDE_MSG_DB)
  serve: claude_msg_db=/var/lib/tmux-msg/messages.db source=default(env unset)
  ```
  The line is visible in `journalctl` on alcatraz and in any stderr-capturing test
  harness. Source is one of `env(CLAUDE_MSG_DB)`, `flag(--db)`, or
  `default(env unset)` — the canonical way to confirm which DB a process is bound
  to without needing a raw SQL probe. Addresses the 2026-06-10 crew-demo rig
  misdelivery where an MCP silently fell back to the production DB with no log
  trail (upstream of #288's pane-id collision).
- **`unregister` CLI + MCP (#289).** Adds `tmux-msg-claude unregister --name <agent>`
  (and `tmux-msg.unregister` MCP tool) as the clean reciprocal of `register`. Stops the
  agent's mailman first (idempotent — not-running is OK), then removes the agent row.
  Flags: `--purge-queue` drops queued messages addressed to the agent (default: preserve
  for forensic value and re-registration); `--force` overrides the queued-message guard
  that otherwise fails loudly with a count. Idempotent: unregistering an absent agent
  returns `removed: false` rather than an error — safe for script cleanup paths. Delivered
  and failed audit rows are never touched by `--purge-queue` — message history is
  preserved regardless. Addresses the stale-row class surfaced 2026-06-10 on alcatraz
  where a retired chamber slot leaked permanently into every `agents` listing.

### Docs

- **Reference: bus host-locality + SSH'd-pane patterns (#312).** New
  `docs/reference.md` §"Bus host-locality" names the substrate's deliberate
  scope-boundary: the bus is host-local (one SQLite DB per host, per user
  per #308), and SSH'd panes are one-way carriers — bus-on-host → SSH-transport
  → remote-input, *not* bus-to-bus communication. Documents three substrate-honest
  patterns for SSH'd panes (unregistered / mailbox-only / paste-and-enter) and
  forward-references the Remote MCP mode opt-in (#310) as the bidirectional path
  that's not default substrate behavior. Closes the framing gap surfaced by the
  2026-06-11 Caymans-Admin observation ("from this side of the wire it doesn't
  feel like 'tmux-msg from Alcatraz' — it feels like an operator pasting through
  a transport I can't see") so future operators don't expect a reply path the
  substrate doesn't promise. Composes with #308 (user-scope DB alignment) and
  security.md §3.2 (the load-bearing identity invariant the host-locality
  rests on).

- **Diagnostic-playbook entry: drift-detection rejection (#311).** New
  `docs/diagnostic-playbook.md` section "Drift-detection refused my send" walks
  the operator through the substrate's `drift_detected_unrecoverable` safety
  event: registered agent name doesn't match the pane's self-declared title, so
  the bus refuses to paste rather than risk delivering to the wrong pane.
  Documents the symptom shape (the exact `WARN drift_detected_unrecoverable`
  log line, the `state: failed` send-response), the root cause (substrate's
  `discover` walker matching the pane's self-declared identity (typically
  `pane_title`, also `cmdline` / `window_name`) against
  the registered name), and two resolution paths — substrate-honest
  match-the-name path first, then `--drift-soft-fail` override for deliberate
  experiments. Closes the diagnostic gap surfaced by the 2026-06-11
  Caymans-Admin observation (registered as `caymans-admin`, pane self-declared
  as `Admin`; re-registering as `admin` made delivery succeed). Composes with
  ADR-0009 framing: drift detection is a substrate-general invariant on
  pane-identity, not adapter-specific.

### Changed

- **Default DB location moved to user-home; install no longer requires shared-space chown (#308).**
  The default DB resolves under the operator's user-home — `$XDG_DATA_HOME/tmux-msg/messages.db`,
  or `~/.local/share/tmux-msg/messages.db` when `$XDG_DATA_HOME` is unset — instead of the former
  system-global `/var/lib/tmux-msg/messages.db`. `install.sh` no longer creates or chowns a
  shared-space data dir (the binary creates the user-home dir lazily on first open), and the
  systemd mailman units drop the `Environment=CLAUDE_MSG_DB=...` directive (the binary's own XDG
  resolution is correct under the operator's UID). This makes the substrate's trust boundary
  exactly congruent with tmux's per-user model and resolves a substrate-vs-adapter mismatch
  surfaced by codex sandbox integration: a sandbox-by-default adapter (codex) can now write the DB
  without per-write operator escalation, since it lives under the user's own home (substrate-witness
  Lookout, 2026-06-11). **Hard cut** — operators with an existing `/var/lib/tmux-msg/messages.db`
  must `mv` it to `~/.local/share/tmux-msg/messages.db` once at deploy time (no auto-migration shim;
  there is exactly one installed deployment). The `--db` / `$CLAUDE_MSG_DB` overrides are unchanged,
  and the #293 start-mailman mismatch guard now compares against the user-home default. `uninstall.sh
  --purge` targets the user-home DB dir.

### Fixed

- **Paste-incapable adapters force-defer instead of clobbering operator input (#323).**
  The `internal/tmuxio` observe-gate is calibrated for Claude Code's `❯` prompt sentinel
  + cursor position to detect "operator is mid-typing" and defer the paste. A Codex pane's
  `›` input area is mis-classified, so the gate could not reliably defer — a paste-and-enter
  delivery to a Codex chamber clobbered the operator's in-progress input (operator-witnessed
  during Lookout's onboarding). The substrate now carries a per-adapter paste-capability flag
  (`Profile.PasteCapable`: `true` for `tmux-msg-claude`, `false` for `tmux-msg-codex`), and the
  mailman **force-defers** at startup when a paste-incapable adapter's `delivery_mode` is
  `paste-and-enter`: it refuses the paste loop, leaves messages queued, and logs the corrective
  migration command (`register --delivery-mode hook-context`, Codex's designed delivery path per
  ADR-0009 / #248 decision (B), or `mailbox-only`). The gate keys on the process-global adapter
  Profile (the Codex mailman runs from the `tmux-msg-codex` binary), so no schema change is needed
  and it fires regardless of how the agent reached paste-and-enter mode — including a Codex chamber
  already registered in paste mode, the exact #323 provenance. This is the **narrow interim fix**;
  teaching the observe-gate to read Codex panes directly (so paste-and-enter could be supported)
  is the per-adapter `PaneProfile` refactor tracked at #322. Mutation-verified: inverting the gate
  predicate lets the message reach `delivered` (the clobbering paste) instead of staying queued.

- **`paneNotFoundBackoff` overflow guard is now base-agnostic (#299).** The
  old guard (`if consecutive >= 7 { return stuckBackoffCap }`) was hard-wired to
  the production `time.Second` base: the 7th shift (64s) happened to exceed the
  60s cap. With the new `stuckBackoffBase` test seam (shrinkable to
  `time.Millisecond`), that constant became wrong. The guard is replaced by a
  value-cap (`shift >= 63 → cap; d < 0 || d > stuckBackoffCap → cap`) that
  correctly terminates the schedule for any base. A new phased integration test
  (`TestServe_CounterResetOnProbeRecovery`) pins the consecutive-counter reset:
  after a non-can't-find-pane probe abort that resets the streak, a full
  `StuckThreshold` consecutive failures are required before parking — the test
  uses `setStuckBackoffBaseForTest(time.Millisecond)` to complete in ~30ms
  instead of the production seconds scale.

- **Adapter-correctness: usage hints + help prose name the running binary — #280.** Per-subcommand
  flag-error usage hints (and `run '… discover'`-class runtime pointers) interpolate the active
  adapter's binary name instead of a hardcoded `tmux-msg-claude`, and the top-level `mcp` /
  `hook-context` help prose names the active adapter via a new `Profile.DisplayLabel`
  ("Claude Code" / "Codex"). Cosmetic-only and behavior-preserving for the claude adapter (the
  default profile reproduces every string verbatim). The `mcp.go` register tool-schema
  descriptions carry deeper Claude framing and are deferred to #314.

- **Adapter-correctness: the `register` surface is substrate-neutral — #314.** The `tmux-msg.register`
  MCP tool's input-schema descriptions named the claude binary in the mailman-unit / `inbox` /
  `hook-context` references and framed delivery as "the recipient's Claude session"; the parallel
  `register --delivery-mode` CLI flag-help carried the same prose. A codex agent consuming either
  surface (its `hook-context` onboarding path) saw the wrong adapter. The binary references now name
  the active adapter, and the delivery-mode prose is adapter-neutral ("the recipient agent's
  session") in both the schema and the CLI help. The neutralization changes the claude prose too —
  intentional, since the register surface describes substrate-general mechanism, so naming Claude
  there was the substrate-vs-adapter leak ADR-0009 governs. The `tmux-msg.control` tool's "Claude
  Code slash-command" framing is deliberately left as-is: the paste-and-enter control surface is
  genuinely Claude-only (#248 decision (B)), so neutralizing it would over-claim a codex control
  surface that does not exist. (Dev-facing doc-comment parallels tracked in #328.)

## [0.15.1] — 2026-06-11

Bugfix release: feature-frozen on top of v0.15.0 to land the five substrate-
hardening fixes surfaced during the 2026-06-10 alcatraz tmux outage forensics
(#287 + #291 + #293 + #296 + #298), plus the durable record for the tool-name
naming decision (ADR-0010 accepting `tmux-tell`). The v0.16.0 substrate-
hardening cluster (#285 / #286 / #288 / #289 / #290 / #299 / #300 plus PR #283
codex adapter) continues against this baseline. Deprecation eligibility: all
four cleared-for-removal surfaces extend through v0.16.0 per ADR-0008
§Discretion clause — the v0.15.1 cut surface is intentionally narrow.

### Docs

- **ADR-0010 (Accepted): tool name is `tmux-tell` (#294).** Durable record of
  the 2026-06-10 blind-vote disposition. Pilot drove a two-phase private
  candidate-collection process under blindness guarantees (Phase 1
  three-favorites, Phase 2 single pick); the 8-participant aggregate produced
  `tmux-post` (4 votes), `tmux-note` (3 votes), `tmux-tell` (1 vote). Operator
  disposed `tmux-tell` on adapter-grammar (the `tmux-tell-claude` imperative
  reads "tmux, tell Claude…"), product-name framing over technical-description,
  `/tell` MUD/IRC async heritage, and ship-framing fit. Rename arc files as a
  v0.17.0 candidate; this release ships only the durable disposition, not the
  binary rename. The ADR also retires the original "spontaneous fall in love"
  bar from the closed PR #218 round, replacing it with a corrected
  five-axis bar (substrate-honesty under architectural-corner commitment,
  adapter grammar, speakability, tonal match, churn cost).

### Fixed

- **`tmux-msg.register` MCP tool now auto-clears the `#224` attention signal,
  matching the CLI register surface (#298).** A chamber re-registering via MCP
  (spawn-die, self-recovery, ad-hoc reset) had its stale `awaiting_operator`
  signal persist on the operator's attention queue — the CLI register path
  cleared it but the MCP path didn't, surfaced as a pre-existing asymmetry
  during `#297` review. The MCP handler now calls `SetAttentionState(idle)`
  best-effort right alongside the existing `#297` stuck-state clear. Test
  pins both paths; mutation-verified.

- **Mailman no longer storms tmux on a persistent `can't find pane` failure
  (#291).** A stale or wrong-server pane registration used to drive the
  pre-paste safety-abort into a tight retry loop (~100 probes/sec), which
  wedged the tmux server (2026-06-10 incident). Consecutive `can't find pane`
  failures now back off exponentially (1s → 2s → … → 60s cap), and after
  `stuck-threshold` consecutive failures (default 10) the mailman parks itself
  (`stuck_reason = 'pane-not-found'`, new `agents.stuck_reason` column) and
  stops probing tmux entirely. Queued messages are retained (no loss). The
  parked state shows in `tmux-msg-claude agents` (new STUCK column) and clears
  on `register --force` (CLI and MCP). New per-agent knobs: `stuck-threshold`,
  `stuck-poll-interval`.

- **Sender-backlog cap is now scoped per-(sender, recipient) (#296).** The
  cap (`capSenderBacklog`, default 2) previously counted a sender's queued
  messages globally across all recipients, so 2 undrained messages to one
  slow-but-healthy recipient blocked the sender's outbound to *every* other
  recipient — one busy consumer silently collapsed a sender's whole
  fleet-wide channel (2026-06-10: a coordination broadcast was dropped this
  way). The cap now counts only the `(from_agent, to_agent)` pair, mirroring
  the recipient-queue cap's scoping. It becomes a per-sender fairness slice
  of one recipient's queue (no sender may hold more than 2 of a mailbox's 5
  slots) rather than a global outbound ceiling; a sender blocked at one
  recipient still reaches all others. The `ErrSenderBacklogFull` message now
  names both ends (`from→to`).

- **`scripts/record-asciinema-demo.sh` no longer shares the operator's tmux
  server (#287).** The recording driver now runs the demo session on a
  private tmux server rooted at `$TMUX_TMPDIR=/tmp/observe-gate-demo-tmux`, so
  a recording-side tmux crash can't take the operator's chamber session with
  it (the alcatraz-infra#31 outage class). Defense-in-depth: even if the
  recording wedges or `tmux` itself segfaults, only the sandbox dies; the
  operator's main tmux is unaffected. TMUX_TMPDIR isolation chosen over `-L
  <socket>` because tmux-msg-claude's mailman + discover currently shell out
  to plain `tmux` without `-L` (see #288); TMUX_TMPDIR is honored
  transparently via env-inheritance.

- **`register --start-mailman=true` now refuses non-default `CLAUDE_MSG_DB`
  (#293).** A systemd-managed mailman launches from the unit-file
  `Environment=` directive, not the caller's env, so a sandbox-DB caller
  requesting `start_mailman=true` would silently misroute (agent row in
  sandbox DB, mailman polling production DB). The CLI now refuses the
  combination with an actionable error pointing at `--start-mailman=false`
  + `<binary> serve --agent <name>` as the foreground-subprocess recovery
  path. The MCP `tmux-msg.register` tool applies the same check:
  registration succeeds, but `mailman` is `skipped` with `mailman_error`
  naming the divergence. Default-DB callers see no change.
  `docs/reference.md` §Caveat documents the unit-file-Environment behavior
  alongside the register surface.

## [0.15.0] — 2026-06-10

### Added

- **Scripted asciinema take driver: `scripts/record-asciinema-demo.sh` (#273).** Turns the `docs/asciinema-capture.md` recipe into an unattended record-and-cleanup script. One command produces `docs/asciinema/observe-gate.cast`; output path overridable via `$CAST`. Sandbox isolation (separate DB at `/tmp/observe-gate-demo.db`, dedicated tmux session `observe-gate-demo`) matches the recipe. Bob's pane runs real `claude` (the observe-gate classifier requires the `❯` sentinel per the F6 finding in the recipe). `tmux send-keys -l` drives bob's prompt character-by-character at human pace (`$TYPING_DELAY`); bus send fires mid-typing on a deterministic clock. Stop-frame: recording ends just after message text lands (before Claude's reply renders). Idempotent: re-runnable from any state via `trap` cleanup. The recipe doc gains a top-of-file pointer at the script.

- **`inbox --watch` reply (`r` key) — #268.** The interactive inbox TUI gains a reply
  action: `r` opens `$EDITOR` (`$VISUAL` → `$EDITOR` → `vi`) on a templated buffer; the
  saved body is sent threaded under the selected message (`reply_to`), addressed to its
  sender, caps enforced in-transaction (reuses the `send` substrate). Empty save =
  abandon. The buffer uses a git-style **scissors** marker — the reply is everything
  above the line, preserved verbatim — so a reply that starts a line with `#NNN` (issue
  ref) isn't eaten by comment-stripping. The `D` mark-failed action from #149's proposal
  was deliberately **not** built (no `queued → failed` substrate path; `failed` is
  sender-facing; a `rejected`/`dismissed` state is a forever-commitment with no current
  consumer — deferred to a forcing-function, full decision-record in #268). Closes #268.

- **Lightweight reply-intent flag: `send --expects-reply`, `inbox --unanswered`, `sent --awaiting-reply` (#270).** Adds a first-class way to signal "I expect a reply" without the blocking `ask`/`wait_for_reply` machinery. `send --expects-reply` stamps the `expects_reply` marker on the outgoing message. Recipients can filter their inbox with `inbox --unanswered` (messages with `expects_reply=1` the recipient hasn't replied to yet). Senders can audit open asks with `sent --awaiting-reply` (messages they marked `expects_reply` where the recipient hasn't replied). Both filters are also wired as MCP parameters on `tmux-msg.inbox` (`unanswered`) and `tmux-msg.send` (`expects_reply`). `Message.ExpectsReply` is now populated on all read paths (`GetMessage`, `ListMessages`, `ClaimNext`, `FindDedupeMatch`, `FindMessagesByPrefix`, `TailRows`, `MessagesByIDs`) and exposed in JSON output.

### Changed

- **CLI refactor: shared subcommand wiring hoisted to `internal/cli` behind an adapter `Profile` (#248 PR1).** Behavior-preserving internal restructure that prepares the second CLI adapter (`tmux-msg-codex`, #248 PR2). The `tmux-msg-claude` binary still hosts every subcommand at the same names with the same flags + exit codes; the shared registration logic now lives in `internal/cli` so a sibling adapter binary can opt into a curated subset via its own `Profile`. No user-visible change; CHANGELOG entry exists so a future bisect through `cmd/` surfaces the refactor explicitly.

- **CONTRIBUTING.md: claim-on-pickup discipline made explicit for both issues and PRs.** The convention was already documented in `docs/chamber-dispatch.md` and added to each chamber's CLAUDE.md during alcatraz-infra#27. This makes the rule discoverable for non-chamber contributors too: when picking up a substantive issue, set the Forgejo `assignees` field before opening the worktree branch; mirror on the PR when filing. Dispatchers read `assignees` before dispatching. Forward-only — historical issues without an assignee aren't backfilled.

- **CONTRIBUTING.md §Release cuts: explicit pre-cut step to fast-forward the shared alcatraz checkout (closes #284).** A reminder that `/srv/tmux-msg/` on alcatraz is a shared working tree that lags `origin/main` until explicitly fast-forwarded; scripts the operator invokes from there read the last-fast-forwarded state, not current `main`. Step 0 of the cut sequence: `cd /srv/tmux-msg/ && git pull --ff-only`. Surfaced 2026-06-09 after a #282 fast-follow merge-vs-disk staleness false-alarm.

### Fixed

- **`scripts/record-asciinema-demo.sh` — 6 defects fixed (#273 fast-follow).**
  1. **Claude trust prompt** — `wait_for_claude_ready` polls the pane and dismisses the "trust this folder?" dialog before the take starts; fresh-directory runs no longer hang on the trust modal.
  2. **Headless pty size** — `asciinema rec` now passes `--cols 120 --rows 30` directly; the cast dimensions are forced regardless of calling terminal size (COLUMNS/LINES env vars are ignored by asciinema when not connected to a real pty).
  3. **Existing cast silent-abort** — `--overwrite` added to `asciinema rec` (and `rm -f "$CAST"` at Phase 1 as belt-and-suspenders) so re-runs don't silently replay the stale file.
  4. **Keystrokes before attach** — `sleep 2` after `asciinema rec &` replaced with a `tmux list-clients` poll; the take phase doesn't start until asciinema's attach has actually connected.
  5. **Visible-from-alice send** — `tmux-msg-claude send` was fired from the script's own shell (invisible); now typed into alice's pane via `send-keys -l` so the viewer sees the send originating from alice's side (Herald's editorial intent from the recipe Step 5).
  6. **Delivery race** — fixed `sleep $((STALE_THRESHOLD + POST_LAND_WAIT))` replaced with `sleep $STALE_THRESHOLD` then a `wait_for_delivery` poll (greps mailman log for `delivered id=`); the recording no longer stops before the paste completes, and no longer leaks keystrokes into the operator's terminal post-recording.
  Dead code: `trap cleanup EXIT` re-arm after Phase 1's manual `cleanup()` call removed. Float-safety: `$POST_LAND_WAIT` now passed directly to `sleep` rather than through `$((...))` arithmetic expansion.

- **`inbox --watch` no longer multiplies its poll timer (#268).** The #149 watch loop
  re-armed the tick on every poll result, so each `space`-ack (which triggers a refresh
  poll) leaked an extra tick chain — compounding the poll rate over a session. The tick
  is now the sole rescheduler; poll results never re-arm, so action-triggered refreshes
  stay one-shot.

## [0.14.0] — 2026-06-09

### Fixed

- **`TestPin_HealthScanLatencyCeiling_Under100ms` no longer flakes under `go test -race` (#254).** Path (c) triage: the production commitment (scan < 100ms) is intact — alcatraz hardware measures ~10ms without the race detector, ~160ms under it (16× overhead). The pin's assertion now skips the wall-clock check when running under `-race` (scan still executes for correctness); the 100ms ceiling remains enforced on every non-race CI run. ADR-0001 amended with the (c) diagnosis, (c.1) counter-test (`TestPin_HealthScanLatencyCeiling_SlowScanCaught` — slow fake reader verifies the ceiling still fires for genuinely slow scans), and (c.2) amendment section.

### Added

- **Hook-context delivery mode (#249, ADR-0009).** A third `delivery-mode`
  alongside `paste-and-enter` and `mailbox-only`: a `hook-context` agent's
  Claude session pulls pending messages and injects them as `additionalContext`
  via a SessionStart / UserPromptSubmit hook, instead of the mailman pasting
  into the pane. The mailman short-circuits for `hook-context` (no paste, like
  mailbox-only); an **adapter-side** hook-helper — the `tmux-msg-claude
  hook-context` subcommand — claims pending messages (honoring the #204 floor +
  #227 deferred staging), renders them, marks them delivered, and emits the
  Claude hook JSON. It's a no-op when nothing is pending, so it's safe to wire
  unconditionally. **ADR-0009** records the load-bearing call: the substrate
  stays delivery-method-agnostic (CLI-specific hook delivery lives in the
  adapter, setting up the second-adapter work #248), and the #169 invariant is
  reframed from "delivered = pasted" to "delivered = presented" (paste OR
  inject), with `delivery_mode` carrying the how. Operator-ruled 2026-06-09;
  Claude-only in v1 (Codex/Gemini hooks ride #248). README + `docs/reference.md`
  §Hook-context delivery document the `settings.json` wiring.

- **Godog / gherkin E2E scenario layer (#264).** Six substrate-boundary scenarios in `features/*.feature` document the contracts the project makes with operators: observe-gate delivery, paste-safety gating, dedupe recovery, operator routing, deferred delivery, and the attention-signal cycle. Step definitions in `features/steps/suite_test.go` exercise the store state machine directly (no real tmux server needed), so `go test ./features/steps/` passes in CI. Run them alongside the full suite with `go test -count=1 ./...`. `godog v0.15.1` promoted from indirect to direct dev dependency. CONTRIBUTING.md updated with the "adding a scenario" recipe.

- **Request-reply — `ask` / `wait_for_reply` / `check_replies` (#250).** A
  synchronous-Q&A surface on top of the existing reply-to chain. `ask --to
  <agent> "question"` is a single-recipient `send` that marks the row
  `expects_reply` and returns the message id as an `ask_id`;
  `wait-for-reply <ask_id> [--timeout]` blocks until a reply addressed to the
  caller with `reply_to = ask_id` arrives (returning `{reply, timed_out}`);
  `check-replies <ask_id> [--since]` is the non-blocking poll. Same three as MCP
  tools (`tmux-msg.ask` / `wait_for_reply` / `check_replies`). New
  `expects_reply` column + reply-query store seams (`ListReplies` / `FindReply`
  / `WaitForReply`). Operator design calls (2026-06-09): `ask` is a **distinct
  tool + marker** (Q1); `wait_for_reply` is a **push-shaped blocking seam**,
  poll-backed at the substrate side since tmux-msg is multi-process and a
  literal sqlite `update_hook` can't bridge processes (Q2); **no auto-ack** on
  consume (Q3); an unverified (`delivered_in_input_box`, #169) reply is
  **returned with an `unverified` flag**, not discarded (Q4). Single-recipient
  in v1 (multi-recipient `ask` is out of scope).

- **`inbox --watch` — interactive TUI consumer for mailbox-only agents (#149).**
  A full-screen, live-updating drain surface for `mailbox-only` queues (the agents the
  mailman never pastes into, so nothing auto-advances their lifecycle). Lists the queued
  mail, refreshes as messages land (rowid-polling, default 2s, `--watch-interval` to
  tune — `update_hook` can't see the mailman's cross-process writes, the #148 lesson),
  and acks under the cursor: `↑`/`↓` navigate, `space` acks the selected message
  (`queued → acknowledged`, composing with #221's `--ack`), `enter` expands the full
  body inline, `q`/`Ctrl-C`/`Esc` exit with a scrollback-preserving summary. Built on
  bubbletea; the `Model`/`Update` logic is unit-tested without a TTY. Interactive-only:
  requires a real terminal and rejects `--format json` / `--ack`. Richer per-message
  triage (reply-via-`$EDITOR`, operator-reject) is deferred to a follow-up.
- **`docs/asciinema-capture.md` — the observe-gate demo recipe (#216, recipe pass).**
  A reproducible recipe for capturing the motion-dependent differentiator (a message
  holds while you type, lands when you pause) as an asciinema cast: sandbox tmux socket
  + sandbox DB, the typist-equals-recipient sequence, and the editorial choices folded
  in (message body, typing content, caption, hosting). The live `.cast` capture is the
  operator-coordinated follow-up (it can't be synthesized by a chamber); this pass lands
  the deterministic recipe so the take is one-shot.

## [0.13.0] — 2026-06-08

### Added

- **Recipient-side delivery deduplication — `dedupe-window` TOML knob (#157 PR2, #157).** The mailman now closes the `delivered_in_input_box` ambiguity loop automatically. Before delivering any message, the mailman checks whether a prior `delivered_in_input_box` row from the same sender with the same body exists within the `dedupe-window` (default `"60s"`). If found, it re-verifies the original's verify-token against the recipient's pane scrollback: if visible it confirms the original and absorbs the replay; if not, the replay delivers normally. The absorb path: original upgraded to `verified=1`, duplicate marked `failed` (reason: `dedupe_absorbed`), `dedupe_notice` inserted back to sender. Configure per-agent or fleet-wide:
  ```toml
  [defaults]
  dedupe-window = "60s"   # default; set to "0s" to disable

  [agent.operator]
  dedupe-window = "0s"    # disable for agents with short scrollback
  ```
  `"0s"` (or absent → default 60 s) preserves zero behavior change. Single-writer invariant preserved. Composes with `resend` (#157 PR1, `v0.12.0`): resend queues the replay; the mailman's dedupe closes the loop automatically if the original surfaces.

- **Operator-presence routing — `send --to operator` (#228).** A new
  reserved sender-facing recipient string `operator` resolves at send
  time to the chamber the operator is currently or was most recently
  attached to, via a two-step substrate observation: (1) `tmux
  list-clients` poll matches an attached client's active pane against
  registered chamber pane_ids; (2) fallback to a single-slot
  "last-seen-in" presence record. The substrate updates the slot on
  every successful step-1 resolution, so subsequent sends route to the
  last-known chamber even with no client currently attached. New
  `presence` table holds the slot (single key today:
  `operator.last_seen_in`); a new `tmuxio.ActiveClientPanes` helper
  wraps `tmux list-clients -aF '#{client_active_pane}'` with the same
  soft-failure semantics as the existing `LivePanes` helper. Substitution
  happens in `runSendWithStore` / `doSendMCP` / `doMultiSendMCP` before
  the registry lookup — `to: ["alice", "operator"]` independently
  substitutes only the `operator` entry. Fails-loud when no observation
  has ever landed AND the slot is unset (or points at an unregistered
  chamber): no silent drop. Composes with #224's chamber → operator
  attention signal: chamber-side declaration + substrate-side routing
  are sibling halves of the operator-attention loop. Substrate addition
  is additive (new table + new send path; no schema changes to messages
  / agents) — K-preserving per ADR-0008 §Amendment A (Reading B).

- **Deferred delivery — `deliver_after` / `flush_deferred` (#227, v1: post-compaction self-handoff).**
  A message can now be **staged** instead of queued. `tmux-msg-claude send
  --deliver-after=resume` (or `tmux-msg.send {deliver_after:"resume"}`) inserts
  it in a new `deferred` state — invisible to inbox / ClaimNext / mailman —
  until the chamber calls `tmux-msg-claude flush --trigger=resume` (or
  `tmux-msg.flush_deferred {trigger:"resume"}`), typically as part of its
  post-`/compact` resume routine, so self-handoff orientation lands in the
  freshly-resumed context instead of being absorbed by the summarizer. New
  `deferred` state + `deliver_after` column (auto-migrates); `sent --deferred`
  lists staged messages. Composes with #204: a promoted-deferred row **bypasses
  the claim-floor** (its `deliver_after` marker exempts it from the id>floor
  test), so a register between defer and flush can't skip the handoff. A
  deferred send is single-recipient and bypasses the queue caps (it isn't in
  the live queue). v1 accepts the `resume` trigger only; `register`-promotion,
  timestamp scheduling, and `OR`-composition are a surfaced follow-up (#258).

- **Configurable message retention policy — `retention` TOML knob + mailman background sweep (#245, #150 PR2).** Each mailman now runs a periodic goroutine that deletes `delivered` and `failed` rows older than the configured window. Default `"infinite"` preserves zero behavior change for existing deploys. Configure per-agent or fleet-wide in `/etc/tmux-msg/config.toml`:
  ```toml
  [defaults]
  retention = "30d"
  retention-sweep-interval = "1h"   # default
  [agent.operator]
  retention = "infinite"            # audit log: never auto-delete
  ```
  Accepted windows: any `parseWindow` spec (`"30d"`, `"7d"`, `"24h"`, etc.). Sweep touches only `delivered` + `failed` rows for the serving agent (single-writer invariant); in-flight rows are never affected. Composes with the existing `reset --older-than` one-off flush (#150 PR1, shipped in v0.12.0).

- **Chamber → operator attention signal (#224).** A new substrate-level
  three-value state on each registered agent surfaces the load-bearing
  "this chamber is awaiting operator input" distinction that
  `tmux-msg-claude agents` couldn't show before. States: `idle` (default;
  no operator action pending), `busy` (reserved for future hook-driven
  mid-tool-call tracking), `awaiting_operator` (chamber has presented a
  choice and is waiting for the operator). New MCP tools
  `tmux-msg.flag_operator(body)` + `tmux-msg.clear_operator_flag()` and
  matching CLI subcommands `tmux-msg-claude flag-operator "<body>"` /
  `clear-operator-flag`. `flag_operator` posts the body to a reserved
  `operator-attention` recipient (mailbox-only; operator registers it
  once at setup) AND flips the chamber's `attention_state` to
  `awaiting_operator` — best-effort across both substrate mutations;
  if the state-flip fails after the message lands, the response carries
  a `state_error` field so the chamber knows to investigate rather than
  treating it as a silent partial success. The flag clears implicitly
  on the chamber's next `register` call (post-/compact, restart,
  spawn-die) or explicitly via `clear-operator-flag`. `tmux-msg-claude
  agents` gains an ATTENTION column for at-a-glance operator visibility;
  `tmux-msg.agents` includes the same field in its JSON output.
  Reserved-recipient enforcement: `flag_operator` fails-loud (#152
  send-to-unregistered semantic) if `operator-attention` is not
  registered, rather than silently swallowing the attention request.
  Substrate addition is additive (new column on agents table) —
  K-preserving per ADR-0008 §Amendment A (Reading B).

### Changed

- **`docs/why.md`: add a §See also referencing `agents-connector` (#251).** A
  substrate-honest acknowledgment of [`Aldenysq/agents-connector`](https://github.com/Aldenysq/agents-connector)
  (Rust, MIT) — a sibling project solving the same local-inter-agent-messaging-in-tmux
  problem from the cross-vendor angle (Claude/Codex/Gemini in one session, delivery via
  each CLI's native hooks). Names the convergence (both independently landed on
  local-only, SQLite-durable, peer-to-peer, tmux-substrate, MIT) and the divergence
  (their cross-vendor-via-hooks vs tmux-msg's persistent-Claude-chambers-via-observe-gate),
  and concedes the use cases each fits better. No feature-matrix; just the honest pointer.

- **Consumer surfaces now read the durable `verified` column (#230).** The
  `verified` column shipped in #169 and #213 reframed the docs to "the column
  exists but consumer X doesn't surface it yet"; this wires the remaining
  consumers to it so those passages read truthfully against the binary. `sent`,
  `inbox`, `track`, `get`, `thread`, and the MCP `message_status` / `inbox`
  tools render a delivered-but-unverified message as `delivered_in_input_box`
  (the `thread` tree marks it `⚠`); `stats` prints a `Delivered split: verified
  / in-input-box / pre-marker` line sourced from the column; `status --today`
  sources its verified counts from the column (failed / crash / cap-exceeded
  counts stay journalctl-sourced, with the journal as defensive backup). A
  pre-#169 row (`verified = NULL`) is reported as a distinct *pre-marker* count,
  never retroactively guessed. New store seam `VerificationCountsByAgent`
  (per-agent companion to `DeliveredVerificationCounts`). Cross-refs: #169
  (column), #213 (docs reconcile).

### Deprecated

- **`resend --force` against a `delivered_in_input_box` (delivered-but-unverified)
  message — no longer needed (#230).**
  Deprecated in v0.13.0; earliest removal v0.15.0.

  The `verified` column (#169) now lets `resend` recognize a delivered-but-
  unverified message directly, so replaying one is the sanctioned recovery and
  no longer requires `--force` (operator decision (C), 2026-06-08). Passing
  `--force` against such a message is still accepted but emits
  `WARN deprecated_surface_used name=resend_force_unverified removal=v0.15.0`
  once per process. `--force` remains required (and un-deprecated) for replaying
  a confirmed delivery (`verified = 1`) or a pre-marker delivery
  (`verified = NULL`), where the substrate can't confirm the message wasn't
  seen. ADR-0008's third real deprecation cycle, after #177's alias arc and
  #140's notify-on-* family.

## [0.12.0] — 2026-06-08

### Added

- **`inbox --ack` / `--ack-all` — announce-skipped backlog drain (#221).** The
  don't-flood-on-restart policy (#204) leaves pre-existing backlog in state `queued`
  indefinitely after a session restart (the mailman skips rows ≤ `backlog_epoch_id`).
  Two new ack paths let operators clear that residue once acknowledged:
  - `tmux-msg-claude inbox --ack <id>` — mark one message acknowledged (idempotent).
  - `tmux-msg-claude inbox --ack-all` — mark all messages ≤ `backlog_epoch_id`
    acknowledged (clears exactly the announce-skipped residue without touching newer
    arrivals). Scope is the per-agent epoch stamped at last register.
  - MCP surface: `tmux-msg.inbox` gains `ack_ids: string[]` and `ack_all: bool`
    parameters with the same semantics.
  - Terminal state: `acknowledged` — substrate-honest (these messages were never pasted,
    so they do not carry `delivered`). Excluded from the default `--state queued` view
    but retrievable via `get` / `tmux-msg.get` (audit-preserving).
  - Store: `MarkAcknowledged` + `MarkAcknowledgedBatch`; auth-scope guard (only
    messages addressed to the calling agent are affected). Cross-ref: #204 (backlog residue origin).
- **Prometheus metrics surface on the mailman daemon (#146, PR1 of the
  observability stack).** `tmux-msg-claude serve --metrics-addr :PORT` (or the
  `metrics-addr` config knob, per-agent) exposes a Prometheus `/metrics`
  endpoint. Off by default — absent flag → no endpoint, no behavior change for
  existing deploys. Six metrics, all `tmux_msg_`-prefixed:
  `messages_total{from,to,state}` (the talk-pair heatmap source; `state` ∈
  `delivered` / `delivered_in_input_box` / `failed`),
  `delivery_latency_seconds{recipient}` (histogram, queued→delivered),
  `delivery_verify_attempt_seconds{recipient}` (histogram, verify-token loop —
  **defined here and shared with #153's budget calibration** so it consumes
  rather than re-instruments), `queue_depth{agent}` (gauge),
  `mailman_loop_iterations_total{agent}`, and
  `paste_unsafe_aborts_total{agent,reason}`. New leaf package
  `internal/metrics` (nil-safe API — a disabled mailman holds a nil handle and
  pays one nil-compare per call); the low-level paste path stays
  metrics-agnostic via a `tmuxio.Deliver` `OnVerify` callback. README gains an
  **§Observability** section. The Alloy scrape job + Grafana dashboard JSON
  (PR2/PR3) land in the alcatraz-infra repo — see the sibling issue.
- **`reset --older-than` — time-bounded audit-history prune (#150 PR1).** New `--older-than
  <duration>` flag on `reset` deletes `delivered` and `failed` messages older than the given
  window, leaving in-flight (`queued`/`delivering`) messages untouched:
  - `tmux-msg-claude reset --confirm --older-than 30d` — prune all delivered+failed older than
    30 days (all agents).
  - Composes with `--agent <name>` (scope to one recipient) and `--state delivered|failed` (AND
    semantics — restrict to one terminal state). Example: `--older-than 7d --state failed`.
  - `--older-than` and `--hard` are mutually exclusive (error on combined use).
  - Store: `DeleteMessagesBefore(toAgent, cutoff, states)` — time-bounded delete with optional
    agent scope and state filter; cutoff compared lexicographically against ISO8601 `created_at`.
- **Configurable verify-token retry budget (#153).** New `verify-retry-budget`
  per-agent TOML knob + matching `--verify-retry-budget` `serve` CLI flag.
  Default `"5s"` preserves today's behavior — the original 100ms / 250ms /
  500ms / 1s / 1.5s / 1.65s schedule across 7 capture attempts sums to the
  default budget. Any duration scales the schedule proportionally (10s
  doubles each delay, 15s triples, etc.); the helper
  `tmuxio.DeriveRetrySchedule(budget)` produces the scaled schedule and
  `tmuxio.SetRetrySchedule` applies it at mailman startup. Each mailman
  is its own process per agent, so the per-agent setting reaches the
  right scope without per-call plumbing through `DeliverParams`. Operators
  monitor verify-attempt latency via #146's
  `tmux_msg_delivery_verify_attempt_seconds` histogram (Prometheus,
  per-mailman `/metrics` endpoint) before tuning. Forensic SPIKE on the
  live alcatraz DB (2026-06-08) found zero `verified=0` events in the
  post-#169 window — the default budget appears adequate for current
  production load; the knob ships as a safety valve for future
  large-payload hubs.

### Changed

- **`docs/why.md`: answer the two pitch-gap questions (#234).** Adds a "But why not
  just…?" section with two subsections — *…raw `tmux send-keys`?* (the observe-gate,
  the single-writer invariant, delivery-state durability, name-not-pane addressing) and
  *…a single session with subagents?* (persistent specialist context, real parallelism,
  the nuanced token economics, role discipline). Both stay substrate-honest — each
  concedes the case where you *don't* need tmux-msg before making its own. The landing
  README's "Where to go next" pointer notes the comparisons. Surfaced by the operator
  during #214 review.

### Deprecated

- **`delivered_unverified` family aliases (CLI flag + TOML key + `--state` value +
  JSON shadow fields) — earliest removal extended from v0.12.0 to v1.0 per ADR-0008
  §Discretion clause (#140 extension).**
  Deprecated in v0.10.0; earliest removal v1.0.0.

  Per the operator's decision 2026-06-08, the alias machinery for the
  `delivered_unverified → delivered_in_input_box` rename arc (#140) is held in
  place through the v1.0 stability boundary instead of removed at the v0.12.0
  two-minor-floor earliest. Same rationale as the v0.11.0 cut's #177 extension:
  maximize migration comfort for existing operator config; alias machinery is
  cheap (TOML-key shim + CLI-flag passthrough + JSON shadow fields + the `--state`
  value normalization); v1.0 is the natural cutover. The binary's WARN logs now
  emit `removal=v1.0` (were `removal=v0.12.0`) across all four surfaces:
  `--notify-on-delivered-unverified` CLI flag (`cmd/tmux-msg-claude/serve.go:185,193`),
  `notify-on-delivered-unverified` TOML key (`cmd/tmux-msg-claude/serve.go:206`),
  `--state delivered_unverified` CLI arg (`cmd/tmux-msg-claude/sent.go:45`), and the
  JSON shadow fields' doc-comments (`internal/config/config.go:89,242,355,502`,
  `internal/healthscan/healthscan.go:44`). K-counter remains preserved by the alias
  machinery per ADR-0008 §Amendment A (Reading B); v0.12.0 increments K to 6.

### Fixed

- **`install.sh` alias-horizon strings (#237).** Five `(removed v0.11.0)` strings
  in `install.sh` contradicted the binary's `removal=v1.0` WARN log and all
  documentation following the v0.11.0 cut. Inline comments updated to
  `removed at v1.0 boundary per ADR-0008 §Discretion clause extension`;
  operator-visible `echo` strings updated to `removed at v1.0 boundary`.

## [0.11.0] — 2026-06-08

### Added

- **Deprecation eligibility derive-script — `scripts/deprecations.sh` (#209).**
  Per [ADR-0008](docs/adr/0008-deprecation-policy.md) §Amendment B, a thin bash
  script walks `CHANGELOG.md` to surface each `### Deprecated` entry's
  `(deprecated-in, earliest-removal)` version pin. Used at release-cut time via
  `--for v<X.Y.Z>` to confirm the cleared-for-removal list before the cut;
  `--all` produces the full table. Permissive parser handles canonical entries
  (Amendment B's structured format) and pre-canonical legacy entries (extracts
  what it can, surfaces a `[legacy format]` tag) without silently dropping
  either. Entries without an extractable version-pin are surfaced as unpinned
  — eyeballed manually at cut time.

### Changed

- **README: split into a lean landing page + `docs/reference.md` operator manual (#214).**
  The 729-line README served an evaluating stranger and a committed operator at once and
  read as a wall of text. Split along that seam: the landing README (232 lines) keeps
  pitch → what-it-is/isn't → install → quickstart (cut at the rendered-output win) → the
  observe-gate differentiator → MCP setup → "where to go next"; the full command
  reference, message-rendering chrome, MCP details, identity/storage/migration, and the
  K-counter mechanics move verbatim to the new `docs/reference.md`. The `tail`
  rowid-polling implementation note moves to `CONTRIBUTING.md`. Restructure only — no
  behavioral or factual content changed; every fact remains reachable.

- **ADR-0008 amended (Amendment B) — structured `### Deprecated` CHANGELOG
  format (#209).** Codifies a machine-parseable shape for `### Deprecated`
  entries: a title line (`- **<surface> — replaced by <replacement>
  (#<issue>).**`) followed by a version-pin line (`Deprecated in v<X.Y.Z>;
  earliest removal v<A.B.C>.`), with free-form prose after. The CHANGELOG
  remains the single source of truth (no separate registry; Option C hybrid
  per operator decision 2026-06-07). Existing v0.9.0 / v0.10.0 entries follow
  the near-equivalent legacy shape — the derive-script handles them
  permissively; new entries should adopt the canonical form.

- **`CONTRIBUTING.md` — release-cut runbook section (#209).** Adds a
  step-by-step §Release cuts section codifying the cut sequence (sync →
  CHANGELOG → README version → `scripts/deprecations.sh --for v<X.Y.Z>` →
  pre-commit → cut PR → tag → deploy). The deprecation-eligibility check
  (step 4) is the operator's surface for "which surfaces did I promise to
  remove?".

- **README: de-insider pass for the public launch (#215).** Dropped the inline
  `(#NNN)` issue-reference breadcrumbs throughout — they resolve to nothing for a
  reader on the public GitHub mirror (the K-counter's `#163` tracker stays, in its
  existing full-URL form). Demoted two insider blockquotes out of the newcomer's
  first screen: the "substrate vs adapter" naming aside is compressed to the one
  line that actually explains the binary name, and the `claude-msg → tmux-msg-claude`
  migration story moved out of the fresh-install path into a new "Migrating from
  `claude-msg`" section near the end (a fresh install has nothing to migrate). No
  behavioral content changed; the provenance still lives in this CHANGELOG and the
  ADRs.

### Deprecated

- **`claude-msg` binary alias + `claude-mailman@` systemd template alias +
  `$CLAUDE_AGENT_NAME` env var fallback — earliest removal extended from v0.11.0
  to v1.0 per ADR-0008 §Discretion clause (#177 extension).**
  Deprecated in v0.9.0; earliest removal v1.0.0.

  Per the operator's decision 2026-06-08, the alias machinery for the v0.9.0
  rename arc (#177) is held in place through the v1.0 stability boundary
  instead of removed at the v0.11.0 two-minor-floor earliest. Rationale:
  maximize migration comfort for existing operator config; the alias machinery
  is cheap to maintain (a symlink + a systemd template alias + an identity-layer
  env-var fallback); v1.0 is the natural stability cutover where removing the
  alias machinery composes with the broader 1.0 surface freeze. The binary's
  WARN log now emits `removal=v1.0` (was `removal=v0.11.0`) — codified in
  `cmd/tmux-msg-claude/main.go:26` (`deprecatedBinaryRemoval`) and
  `internal/identity/identity.go:27` (`envVarRemoval`) — and `docs/reference.md`
  §Migrating from `claude-msg` carries the same updated horizon. K-counter
  remains preserved by the alias machinery per ADR-0008 §Amendment A (Reading B);
  v0.11.0 increments K to 5.

### Fixed

- **`ListFilter.Unverified + State` silent impossible WHERE (#220 Item 1).**
  `ListMessages` now returns an error when `Unverified=true` is combined with a
  `State` value other than empty or `"delivered"`. Previously this emitted
  `WHERE state='queued' AND state='delivered' AND verified=0`, always returning
  zero rows silently. The CLI `sent --state` mutex was already in place; no
  user-visible behaviour change. New tests cover both valid combos and the
  rejected path.

- **`parseMCPToField` branch test coverage (#220 Item 2).** Added direct tests
  for the multi-recipient (array form), single-recipient (scalar form), and
  invalid-shape (number, null) branches of `parseMCPToField` — previously
  exercised only indirectly.

- **`ClaimNext` `NoReplyExpected` scan regression test (#220 Item 2).** The
  `no_reply_expected` column was already scanned correctly; this test pins that
  behaviour so a future scan-list change catches the gap immediately.

- **Quick + `no_reply_expected` + multi-recipient 3-way combined test (#220
  Item 2).** New tests for both the CLI and MCP send paths confirm that all
  three flags (`quick`, `no_reply_expected`, fan-out) survive the round-trip
  through the store.

- **README: reconcile the `verified`-marker docs with the shipped binary (#213).**
  The durable `verified` column (#169) shipped — the migration exists
  (`internal/store/store.go`) and `tmux-msg-claude sent --state delivered_in_input_box`
  queries it — but the README still described it as unbuilt in several places: the
  Storage schema omitted the column, and the `stats` / `resend` / `thread` passages
  said the split was "not DB-queryable / tracked in #169." Corrected to match shipped
  behavior — the column exists and is DB-queryable, while `stats` / `resend` / `thread`
  / `mcp` / `status` don't *consume* it yet (that consumer-plumbing is tracked
  separately as #230). Also fixed an internal contradiction: one passage claimed
  `stats` reports the verified/unverified split — it does not.

## [0.10.0] — 2026-06-08

### Added

- **Backlog don't-flood on (re)register (#204).** When an agent registers (or
  re-registers after a restart) with messages already queued, the mailman no
  longer pastes the whole backlog into the freshly-resumed pane at once. The
  register handler stamps a per-agent **claim-floor** (`backlog_epoch_id`) and
  the mailman skips queued rows at or below it. Two policies, selected by the
  `on-register-backlog` TOML knob (per-`[agent.<name>]` > `[defaults]` >
  hardcoded `"announce"`):
  - **`announce`** (default): leave the entire backlog queued and deliver a
    single synthetic `📬 N queued — run tmux-msg.inbox` nudge.
  - **`auto-deliver`**: paste the newest `on-register-backlog-cap` messages
    (default 3) and announce the older remainder; when the whole backlog fits
    the cap, everything delivers and no nudge is sent.
  An unrecognized policy value falls back to `announce` (the never-floods safe
  default). Mailbox-only agents are a no-op (they never get a paste, so there
  is nothing to flood). The register response gains `backlog_policy`,
  `backlog_skipped`, and `backlog_nudge` fields alongside the existing `queued`
  count (#151). The skipped backlog stays in state `queued` — the operator
  still sees it via `tmux-msg.inbox`; an explicit drain/ack affordance for that
  residue is tracked as a follow-up (#221). The synthetic nudge rides the
  normal single-writer mailman path (the register process never pastes), so the
  delivered-is-pasted invariant (#169) is preserved. Store: `agents` gains a
  `backlog_epoch_id` column (migrates automatically); `ClaimNext` skips rows at
  or below the floor.

- **`sent` — sender's outbox listing (#159).** `tmux-msg-claude sent` lists
  messages the calling agent has sent, newest-first, defaulting to the last 24 h.
  Flags: `--since DUR` (any duration or calendar shortcut accepted by `stats`/`digest`
  — `1h`, `today`, `all`, etc.), `--state STATE`, `--to AGENT`, `--limit N`,
  `--format text|json`. The special state `delivered_in_input_box` filters for
  `state=delivered AND verified=0` rows (soft-fails from #169). Text output: table
  header + one row per message; footer summarises counts of `delivered_in_input_box`
  and `failed` rows with a `tmux-msg-claude resend <id>` recovery hint. JSON output
  adds `display_state` to each row so callers can distinguish verified/unverified
  deliveries without client-side column inspection. Operationalises the
  sender-outbox-first diagnostic playbook in the README: `sent` is now the
  first-class CLI affordance where the playbook previously said "start from the
  SQLite store." Sister to `inbox` (recipient-side) and `resend` (recovery).
  Store: `Message` now carries the `verified` column in all read paths, and
  `ListFilter` gains `SinceCreatedAt`, `Unverified`, and `OrderDesc` fields.

- **`send --to a,b,c` — multi-recipient fan-out (#158).** Pass a comma-separated
  list to `--to` (CLI) or an array to the `to` field (`tmux-msg.send` MCP) to
  deliver the same message body to multiple recipients in a single call. Each
  recipient gets its own message id and independent delivery — there are no
  shared-id semantics. Response shape: `{ok, messages:[{to,id,queued,recipient,...},...]}`;
  scalar single-recipient shape preserved for back-compat. Per-recipient outcomes
  are independent: an unknown or cap-full recipient fails its own row without
  aborting delivery to the remaining recipients (outer `ok` reflects whether ALL
  rows succeeded). Pairs naturally with `--quick` for compact fan-out acks.
  Spam guard: configurable `max-recipients-per-send` TOML knob (default 10) rejects
  sends that exceed the per-call recipient cap before any row is inserted.

- **`send --quick` — compact single-line chrome for routine acks (#154).** Set
  `--quick` (CLI) or `quick=true` (`tmux-msg.send` MCP) to render a message as
  a single compact line in the recipient's pane instead of the full bracket-header
  block. The compact form: `✓ Sender · [re <id> ·] <body>`. Load-bearing fields are
  preserved (sender, thread linkage when `reply_to` is set, body); spatial framing is
  dropped (no timestamp, no message id, no blank line). Reduces typing-overhead-to-
  signal ratio on heavy-coordination days where many necessary acks accumulate. Sister
  to `--no-reply-expected` (#145): `--no-reply-expected` reduces unnecessary acks;
  `--quick` reduces the overhead of necessary acks. `no_reply_expected`, if set, is
  carried as a `🔕` prefix on the body in compact form. The length marker (#160) is not
  applied to quick messages. Stored as `quick INTEGER NOT NULL DEFAULT 0` in the
  messages table; existing databases migrate automatically.

### Changed

- **Rename `delivered_unverified` → `delivered_in_input_box` (#140).** Substrate-honest
  naming: the log token, CLI `--state` value, JSON `display_state`, config key
  (`notify-on-delivered-in-input-box`), and Go identifiers (`MarkDeliveredInInputBox`,
  `NotifyOnDeliveredInInputBox`, `DeliveredInInputBox`) are renamed throughout. The old
  name described what *didn't* happen ("unverified"); the new name describes what *did*
  ("paste landed in the recipient's input box"). Deprecated aliases keep the K-counter
  surfaces live for the two-minor deprecation cycle (see `### Deprecated`). Frozen ADR
  prose and CHANGELOG versioned entries retain the old name per the substrate-rename
  freeze precedent.

- **CI — `gofmt` check added to the required pipeline (#202).** The
  `test / go vet + build + test (pull_request)` workflow now runs `gofmt -l .`
  before `go vet` and fails when ANY file in the tree carries gofmt drift.
  Closes the substrate gap that let the pre-#172 17-file drift accumulate
  undetected — gofmt-cleanliness moves from discipline (Surveyor catches in
  review) to substrate (CI enforces). No new required-status context: the
  step folds into the existing test job per the issue's Option A. Same
  discipline-graduating-to-substrate trajectory as Keep-a-Changelog
  conventions and branch protection rules.

- **ADR-0008 — Reading B (K-counter interaction) codified (#208).** Adds an
  Amendment section to `docs/adr/0008-deprecation-policy.md` recording the
  K-counter interaction settled during the v0.9.0 cut conversation:
  deprecation-with-functioning-alias **preserves** the K-counter (#163),
  removal **resets** it. Operator-impact alignment is the rationale —
  Reading A (any deprecation resets K) would punish responsible policy
  execution; Reading B aligns the counter with what operators feel (does
  existing config still work?). Worked example threads the v0.9.0 → v0.10.0
  → v0.11.0 #177 rename arc: deprecate in v0.9.0 (K preserved, K=3) → still
  under aliases in v0.10.0 (K preserved, K=4) → alias removal earliest
  v0.11.0 (K resets to 0). Out-of-scope consequences cross-ref the
  structured `### Deprecated` derive-script (#209) and the cap-vs-keep-
  raising K decision (#163) explicitly so the addendum doesn't drift into
  unscoped policy territory. Pure docs — no code surface affected. Closes
  #208.

### Deprecated

- **Legacy `delivered_unverified` surfaces (#140, earliest removal v0.12.0).**
  The following surfaces now emit `WARN deprecated_surface_used name=<X> removal=v0.12.0`
  and continue to function until v0.12.0:
  - CLI flag `--notify-on-delivered-unverified` — accepted alongside the new
    `--notify-on-delivered-in-input-box`; when used, the WARN fires once per process
    and the value maps through to the new flag.
  - TOML config key `notify-on-delivered-unverified` — accepted alongside the new
    key; mailman emits the WARN at startup if the old key is in use.
  - CLI `--state delivered_unverified` (`tmux-msg-claude sent`) — accepted and
    normalized to `delivered_in_input_box`; the WARN fires once per invocation.
  - JSON fields `delivered_unverified` (per-agent health payload) and
    `notify_on_delivered_unverified` (config show) — emitted as deprecated shadows
    (same value as their `delivered_in_input_box` / `notify_on_delivered_in_input_box`
    counterparts). Consumers reading the old field still get a value; the shadow fields
    will be removed in v0.12.0 with no further notice.
  Two-minor floor from v0.10.0 per ADR-0008. Locks in alongside #177's v0.11.0 removal.

### Fixed

- **`install.sh` robustness — `bin/` ownership + `getent` exit-2 shadowing (#193).**
  Two latent issues observed during Shipwright's #175 work, swept here:
  1. The fallback `go build` path created `bin/` as root, then ran the build as
     `OPERATOR_USER` — which couldn't write into the root-owned directory.
     `bin/` is now created via `install -d -o "$OPERATOR_USER" -g "$OPERATOR_USER"`,
     idempotently re-applying ownership on an existing dir as well so a stale
     root-owned `bin/` from a prior aborted run gets fixed in place.
  2. `getent passwd "$OPERATOR_USER"` exits 2 when the user is not found;
     under `set -euo pipefail` that propagated and aborted the script before
     the explicit "cannot resolve home dir" error rendered, so an `OPERATOR_USER=<typo>`
     died silently with exit 2 instead of surfacing the friendly message
     from #175. The substitution now ends `|| true` so the empty-result guard
     fires as intended, and the error message names the misspelled user.

## [0.9.0] — 2026-06-07

### Added

- **`register` surfaces the queued-message backlog count (#151).** The `register`
  response (CLI + `tmux-msg.register` MCP) now includes a `queued` field — the number
  of messages already waiting for this agent at register time. Closes the
  inbox-poll-not-push gap for the spawn-per-task / post-restart chamber pattern: a
  fresh session learns it has backlog without a separate `inbox` poll (run
  `tmux-msg.inbox` if `queued > 0`). Reuses the existing `store.RecipientQueueDepth`
  helper. Non-fatal by design — registration already succeeded, so a count read-error
  degrades to a soft `queued_error` field (an honest `0` is never confused with
  "unknown"). The richer announce-paste + auto-deliver-backlog + per-agent TOML-knob
  paths from the original #151 proposal are deferred to the follow-up #204.

- **`docs/chamber-dispatch.md` — assignee-on-claim dispatch convention (#180).**
  Documents the coordination discipline for multi-agent deployments where several
  agents draw work from one issue tracker and more than one party can dispatch it:
  claim an issue by assigning it to yourself before starting, and check `assignees`
  before dispatching. Frames the gap as a substrate boundary — the bus carries
  coordination *conversations*, not the discoverable *persistent state* "this issue
  is mine," which belongs on the tracker. Anchored to the 2026-06-07 cross-dispatch
  collision. CONTRIBUTING gains a pointer under "How we work."

- **K=3 release-stability tracker — v0.8.0 marks Cycle 2 of 3 (#163).** Establishes
  the K-counter that gates the road to `1.0`: three consecutive releases with no
  breaking change across the five public surfaces (MCP tool schemas, CLI args/flags/
  exit codes, `--format json` shapes, DB schema, exported Go API). After the v0.5.0
  substrate rename + v0.6.0 MCP wire-protocol rename (the last deliberate breaks),
  v0.7.0 and v0.8.0 have both been fully additive — **K is now 2 of 3**; one more
  clean cut reaches K=3 and clears that block on the Sea-trials milestone. README
  gains a "Release stability (the K-counter)" subsection; the live per-release record
  is the tracker table in #163. Going forward, each release entry names the K it lands
  on (this entry establishes the discipline; the next clean cut records "K reaches 3").

- **Durable `verified` marker for delivered messages (#169).** A `delivered_unverified`
  soft-failure (paste+Enter landed, but the verify-token never surfaced in budget) was
  previously distinguishable from a confirmed delivery only by a mailman journal line —
  both wrote `state = delivered`, so nothing reading the messages table could count or
  trend the split. A new nullable `verified` column now carries it durably: `1` =
  verified (token observed), `0` = `delivered_unverified` soft-fail, `NULL` = delivered
  before the marker existed (never retroactively guessed). The mailman's verified branch
  writes `1` via `MarkDelivered`; the `ErrUnverifiedDelivery` branch writes `0` via the
  new `MarkDeliveredUnverified` — the `WARN delivered_unverified` journal line is
  preserved, so `healthscan` (`status --today` / `health`) is unaffected. New store seam
  `DeliveredVerificationCounts(window)` splits delivered rows into verified / unverified
  / unknown — the DB-only aggregation #147 (`stats`) re-consumes for the breakdown it
  previously had to leave to journal scraping; #146 (Prometheus) and #153 (verify-token
  forensics) become clean SQL too. The marker is orthogonal to `state` (kept
  `delivered`), so no state-based query changes; the column is intentionally not added
  to the per-row `Message` scans (the bit is consumed via the aggregation seam, not
  rendered).


- **`docs/adr/0008` — deprecation policy ADR (#162).** Records the
  operator-ratified post-1.0 deprecation policy: a **two-minor-cycle floor**
  (deprecate `v1.X`, earliest removal `v1.X+2`) with discretion to extend,
  runtime `WARN deprecated_surface_used` logs, a CHANGELOG `### Deprecated`
  convention, and `deprecated: true` JSON fields where programmatic. Pre-1.0
  keeps semver-explicit looseness. Cadence notes + README §Versioning cross-link
  the policy. Completes a Sea-trials 1.0-trigger criterion. Pure docs.

### Changed

- **Binary renamed `claude-msg` → `tmux-msg-claude` (#177, PR1 of 3).** The
  binary name now encodes the substrate (`tmux-msg`) + the CLI-tool adapter
  (`claude`) per the #174 Option 2 decision — making `tmux-msg-codex` /
  `tmux-msg-copilot` adapters cleanly addable later (the multi-binary shape, not
  the adapters themselves, ships here). Concretely: `cmd/claude-msg/` →
  `cmd/tmux-msg-claude/`; the systemd template `claude-mailman@.service` →
  `tmux-msg-claude-mailman@.service`; a multi-target `Makefile` (`make build`
  builds every `cmd/tmux-msg-*/`, `make build-claude` builds one); and
  `install.sh` gains `--adapter=claude` (default) installing
  `/usr/local/bin/tmux-msg-claude`. The substrate-vs-adapter boundary is
  documented in `cmd/tmux-msg-claude/README.md` (no code physically moved out of
  `cmd/` — the daemon-loop extraction to `internal/` is deferred to whenever a
  second adapter materializes). The Go module path stays
  `git.frankenbit.de/frankenbit/tmux-msg` (already substrate-honest). **Migration
  is seamless during the deprecation cycle** — see Deprecated below; existing
  `claude-msg …` invocations and `claude-mailman@…` units keep working via
  aliases. **This rename is a public-surface change: it resets the #163 K=3
  release-stability counter** — the release carrying it (v0.9.0) starts a fresh
  cycle toward K=3. *PR2 (the `$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME` env-var
  rename) and PR3 (docs + chamber-instructions sweep) follow separately; the
  `claude-msg` mentions still present in `--help` text and docs are swept in PR3
  and remain valid via the alias until then.*

- **Agent-name env var `$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME` (#177, PR2 of 3).**
  The substrate identity layer (`internal/identity`) now reads `$TMUX_AGENT_NAME`
  preferentially and falls back to `$CLAUDE_AGENT_NAME` for the deprecation cycle,
  so existing chambers keep resolving identity unchanged — deploy does not force a
  cutover. When the resolution falls back to the legacy var, it emits
  `WARN deprecated_surface_used name=CLAUDE_AGENT_NAME removal=v0.11.0` once per
  process (mirroring PR1's `claude-msg`-alias WARN). The `$CLAUDE_AGENT_NAME`
  mentions still present in `--help` / error text are swept in PR3; they remain
  accurate (the var still works) until then.

- **Docs + in-binary help-text sweep for the rename (#177, PR3 of 3 — closes #177).**
  README, `docs/diagnostic-playbook.md`, and the operator-facing docs (`why`,
  `observe-gate`, `operator-ux`, `failure-modes`, `security`) now use
  `tmux-msg-claude` / `tmux-msg-claude-mailman@` / `$TMUX_AGENT_NAME` in command
  examples, prose, and error-message references. The in-binary surfaces follow: the
  `usage` text, subcommand `--help` strings, the "cannot resolve identity" errors, and
  the MCP tool-schema descriptions now name `tmux-msg-claude` / `$TMUX_AGENT_NAME`.
  README's Install section gains a v0.9.0 rename callout naming the deprecation aliases
  + the v0.11.0 removal; the substrate-vs-flavor box names the new sibling-adapter
  convention (`tmux-msg-codex` / `tmux-msg-copilot`). ADRs are left as accepted-state
  historical records (not retroactively rewritten); the deprecation-alias detection
  (`claude-msg` name check) and the `name=claude-msg` / `name=CLAUDE_AGENT_NAME` WARN
  strings deliberately keep the old names — they ARE the deprecation surface.
  Completes the #177 rename arc (PR1 structural + PR2 env-var + PR3 docs). *The
  chamber-CLAUDE.md sweep turned out vacuous (chambers reference the bus by its MCP
  tools, not the binary/env-var names); the remaining operator-side reach is the #180
  assignee-on-claim addition, tracked in alcatraz-infra#27 (Bosun action). The
  BookStack runbook (#188) needed no change — it already uses substrate names. Both
  out of this repo.*

- **CONTRIBUTING.md — deprecation-policy surface-scope clarification (#162 follow-up).**
  The post-1.0 stability section now states the policy covers **all five** public
  surfaces (per ADR-0008: MCP / CLI / `--format json` / DB + state vocabulary / Go
  API), distinct from the external-**contract** subset (Go API + DB) a downstream
  module pins — and links the now-landed ADR-0008 (was "forthcoming"). Pure docs.

### Deprecated

- **`claude-msg` binary name + `claude-mailman@` systemd template — replaced by
  `tmux-msg-claude` / `tmux-msg-claude-mailman@` (#177).** Earliest removal
  **v0.11.0** (two minor cycles after the v0.9.0 rename, per ADR-0008's floor;
  this is the policy's inaugural worked example, dogfooded pre-1.0). For the
  cycle, `install.sh` installs a `claude-msg → tmux-msg-claude` binary symlink and
  a `claude-mailman@.service → tmux-msg-claude-mailman@.service` systemd template
  symlink, so nothing breaks at the cutover. Invoking the binary through the
  `claude-msg` name emits `WARN deprecated_surface_used name=claude-msg
  removal=v0.11.0` on stderr. **Migration:** switch scripts/units to
  `tmux-msg-claude` / `tmux-msg-claude-mailman@`; the aliases are removed in
  v0.11.0.

- **`$CLAUDE_AGENT_NAME` env var — replaced by `$TMUX_AGENT_NAME` (#177 PR2).**
  Earliest removal **v0.11.0** (ADR-0008 two-minor floor). The identity layer
  falls back to it for the cycle and emits `WARN deprecated_surface_used
  name=CLAUDE_AGENT_NAME removal=v0.11.0` once per process when it does.
  **Migration:** set `$TMUX_AGENT_NAME` in chamber env / dispatch packages; the
  fallback is removed in v0.11.0.

### Fixed

- **gofmt hygiene sweep — 17 files corrected (#172).** Two pre-existing formatting
  deltas in `cmd/tmux-msg-claude/serve.go` (struct-literal alignment and log-concat
  whitespace) plus 16 further files carrying minor whitespace/alignment drift were
  corrected by running `gofmt -w`. The CI workflow runs `go vet` but not `gofmt -d`;
  adding the check is tracked as #202 (out of scope here per the issue's
  Out-of-scope list).

## [0.8.0] — 2026-06-07

### Added

- **`claude-msg resend <id>` — replay a failed/unverified message (#157, PR1 of 2).**
  The explicit recovery path for a message that landed `failed`, or `delivered`
  but unverified (paste landed, verify-token never returned). `resend` replays the
  original to its recipient as a *new* message whose body is byte-identical to the
  original, carrying a `↻ Replayed: original sent at <ts>` chrome marker (rendered
  in `internal/render`, so it shows on the live delivery, in `log`, and in
  `thread`). The send response gains a `replay` block (`original_id`,
  `original_sent_at`, `original_state`, `forced`) — the 5th additive named-block on
  the #152 `SendResponse` contract, after `recipient`/`delivery`/`thread_freshness`;
  no existing field reshaped. Available over MCP as `tmux-msg.resend`. **Duplicate
  guard:** a `failed` message replays directly; a `delivered` message (which
  silently includes the journal-only `delivered_unverified` case — the DB can't
  distinguish it, #169) or a still-in-flight message is refused without `--force`.
  Recovering an unverified delivery therefore means `resend --force` until #169
  makes the verified/unverified split DB-queryable. Replay linkage rides on two new
  nullable columns (`replay_of`, `replay_of_at`); the byte-identical body is
  deliberate — it lets PR2's planned recipient-side body-hash dedupe match a replay
  against its original. PR2 (recipient-side dedupe in the mailman loop) is a
  separate follow-up.

- **`send --reply-to` carries a crossed-message `thread_freshness` signal (#155).**
  When a send threads under an earlier message, the response (CLI + `tmux-msg.send`
  MCP) adds a `thread_freshness` block — `{stale, newer_in_thread[], you_replied_to,
  latest_in_thread}`. `newer_in_thread` lists messages in the reply chain that are
  **addressed to the sender and newer than the high-water-mark of what they've
  seen** (their last message *or* the message they're replying to, whichever is
  later) — "the thread moved past what you're anchored to." This is the
  substrate-knowable reading of the crossed-reply problem (async replies cross in
  flight; you `reply_to` a state an unread inbound may have superseded). It
  deliberately does *not* claim "messages you haven't *processed*" — the substrate
  tracks `delivered` (paste landed), not attended-to — so that framing from the
  original issue was corrected during refinement. By default `stale` is
  informational and the send still succeeds; the new `--block-on-stale` /
  `block_on_stale` opt-in turns it into a hard refusal (`ok:false`) so the sender
  can re-read first. Additive `ThreadFreshness` field on the #152 `SendResponse`
  struct; reuses the shared `store.GetThread` reply-chain walk (#141) rather than a
  bespoke query.

- **`quartermaster→pilot` `/clear` PeerEdge (#167).** Mirrors the existing
  `bosun→pilot` edge (#60): Quartermaster is now an established dispatcher into
  Pilot's clear-before-each-task lifecycle, so it gets the same narrow per-edge
  exception to invoke the otherwise-globally-denied `/clear`. The edge stays
  exact (`quartermaster→pilot` only) — QM→any-other-recipient `/clear` remains
  denied, per the package's conservative-default-with-explicit-opt-in policy;
  broader sender→pilot edges (Engineer, Shipwright) would each be filed
  separately if those dispatch patterns emerge.

- **`send` reports recipient registration + reachability (#152).** The `send`
  response (CLI + `tmux-msg.send` MCP) gains a `recipient` block —
  `registered` / `alive` / `delivery_mode` / `mailman_running` / `pane_status`
  — queried fresh at send-time from the registry + `tmux` + `systemctl`. New
  opt-ins: `--strict` / `strict` (fail when a *registered* recipient is
  unreachable — pane gone) and `--wait-for-delivered` / `wait_for_delivered`
  + `--timeout` (block for a terminal delivery state, returning a `delivery`
  block with `state` + `verify_ms`). The response is now a **named struct
  schema** (`SendResponse` / `RecipientStatus` / `DeliveryStatus`), the
  contract #155 + #157 inherit, rather than an inline map. Purely additive —
  `ok` / `id` / `queued` keep their meaning. **An unregistered recipient
  stays fail-loud regardless of `--strict`** (preserving the day-one safety
  default — the `default queue for unknown` originally sketched in the issue
  was based on a misread of live code, which has rejected unknown recipients
  since #3/#4/#15; see PR for the surfaced fork).

- **`claude-msg tail` — live diagnostic firehose (#148).** A cross-chamber,
  read-only `tail -f` over bus traffic: new rows print as inserted and
  `queued → delivering → delivered/failed` transitions print on the same id.
  Compositional filters (AND): `--from` / `--to` / `--kind` / `--state` /
  `--since` (reuses #147's `parseWindow`, now also accepting `now` — the
  default, start-live-no-backfill). `--format json` emits one object per line;
  Ctrl-C exits cleanly. The watch mechanism is **rowid-polling**, not SQLite's
  `update_hook`: the mailmen that write rows are separate processes from the
  `tail` CLI, and `update_hook` is per-connection/same-process so it would
  never see their writes — `tail` polls `MAX(id)` since-last-seen
  (`--interval`, default 300ms) and re-reads in-flight ids for transitions,
  WAL-safe alongside mailman writes. New store primitives `TailRows` +
  `MessagesByIDs`; the diagnostic playbook gains a "watching it happen live"
  section. Resolves the #137 walk-back pain (correlating two mailmen's
  journals by hand).

- **Body-byte length marker in the bracket header (#160).** Messages whose body
  exceeds a byte threshold (default 512) gain a trailing `· <size>` marker —
  `[Surveyor → Quartermaster · re abad · id 4825 · 2.3k]` — so a reader scrolling
  history can distinguish a two-line ack from a 3K wall of review text, and a
  sender sees the size cost before sending (Surveyor's review-heavy-chamber
  signal, bus id `a236`). Sizes read `<n>b` under 1000 bytes and `<n.n>k` above
  (decimal ×1000, so `2.3k` == 2300 bytes; the lowercase suffix borrows the
  `du -h`/`ls -h` look but not its 1024 base, so a threshold maps cleanly back to
  a marker). Threshold is configurable via the `render-byte-marker-threshold` TOML
  key (human byte-size string, e.g. `"2k"`; fleet `[defaults]` + per-`[agent.<name>]`
  override). Applies on the full bracket-header render path only — the mailman
  delivery path and `claude-msg log`; the marker is the renderer's, so any future
  consumer of `render.Message` inherits it. **API note:** `render.Message` now
  takes a `byteMarkerThreshold int` second argument (a negative value disables the
  marker); pre-1.0 minor-bump break, internal callers only.

- **`claude-msg digest` — campaign-arc narrative summary (#161).** The
  *qualitative* sibling to `stats`: a by-counterparty table (sent / received /
  threads / closed / in-flight) plus an "in-flight threads (likely need
  follow-up)" section listing reply-chains whose last word still awaits an
  answer — the day's-end "what's still owed?" view. Flags: `--since` with
  calendar shortcuts (`today` / `yesterday` / `week`, alongside `all` / `<N>d`
  / any duration), `--counterparty NAME`, `--format text|json`. A thread is
  **closed** when its latest message carries the `🔕` no-reply-expected marker
  (or the send failed) and **in-flight** otherwise — a documented heuristic,
  not ground truth. Reuses #147's aggregation layer (`StatsPerAgent` for the
  sent/received counts, the shared `parseWindow` helper, now extended with the
  calendar shortcuts) and #141's `buildThreadTree` reply-tree walk; the only
  net-new store primitive is `MessagesInWindow` (full rows over the same
  `whereSince` window seam). System chrome (`delivery_failure_notice`,
  `stranded_draft`, `ping`) is excluded from thread analysis.

- **`claude-msg stranded list|show|prune` — paste-snapshot recovery (#142).**
  Operator-visible recovery for the `stranded_draft` bookmarks the observe-gate
  archives when a delivery would clobber in-flight operator input (#92). Source
  probe (AC1): they're `messages` rows with `kind=stranded_draft`, self-addressed.
  `list` shows id/pane/timestamp/byte-size; `show <id>` prints the recovered
  content (`-o file` for long pastes); `prune --older-than <dur>` (required;
  reuses `parseWindow`) clears old ones. The stranded-draft notification now
  carries a recovery hint. Render/parse share marker constants so they can't
  drift (`ListFilter` gains a `Kind` filter; new `DeleteStrandedDraftsBefore`).
  Best-effort on large bracketed pastes — tmux may have captured only its
  `[Pasted text #N]` placeholder rather than the literal text.

- **`claude-msg thread <id>` — reply-chain tree render (#141).** Renders a
  `reply_to` chain (resolved from any id in it via the existing
  `store.GetThread` seam — walk to root, BFS all descendants) as an ASCII
  parent→child tree: `○` root · `✓` delivered · `✗` failed · `…`
  queued/delivering, with `kind`, `from→to`, `state`, and a body preview per
  node. `--format tree` (default) for humans, `--format json` for tooling
  (nested structure). A read-only sibling to `claude-msg log`: `log` is the
  flat-chronological audit view, `thread` the structural navigation view —
  both over the same store seam, no walk duplication. Chosen over a
  `log --tree` flag because the tree is the new command's *default* format,
  which `log` (flat-text default, script consumers) can't adopt without a
  breaking change. No distinct `delivered_unverified` glyph — the substrate
  stores that soft-failure as `delivered`; DB-queryability tracked in #169.

- **`--no-reply-expected` bus-discipline flag on `send` (#145).** Adds
  `no_reply_expected` column to `messages` (INTEGER NOT NULL DEFAULT 0).
  New `--no-reply-expected` flag on `claude-msg send` and `no_reply_expected`
  boolean parameter on `tmux-msg.send` (MCP). When set, the rendered
  message header includes a `🔕` marker signalling the recipient's Claude
  that no acknowledgment is needed — reduces ack-cascade on FYI / status
  messages. Default false; opt-in per message. Renderer, CLI, MCP, store,
  README, and schema updated.

- **`claude-msg stats` — on-demand bus-traffic aggregates (#147).** A new
  subcommand that computes per-agent counts (sent / received / delivered /
  failed / queued), delivery-latency percentiles (p50/p95, nearest-rank),
  window-wide totals, and top sender→recipient pairs directly from the local
  `messages.db`. Flags: `--window all|<N>d|<duration>` (default `24h`),
  `--agent NAME`, `--pair --top N`, `--format text|json`. The aggregation
  lives in `internal/store` (`StatsPerAgent` / `StatsTopPairs` /
  `StatsTotals` over a single window-bounded scan) as the reusable seam the
  #161 `digest` surface will consume; the shared `parseWindow` helper
  (`all` / `<N>d` / Go-duration) lands alongside. This is the in-terminal
  counterpart to the continuous observability stack (#146), which owns
  dashboard trends. Verified vs unverified deliveries are **not** split —
  both are `state='delivered'` in the DB; making that DB-queryable is
  tracked in #169.

- **`claude-msg ping <agent>` substrate-only reachability probe (#144).**
  A `kind=ping` message the recipient's mailman picks up (proving the
  daemon is alive) and answers via substrate-health checks (agent
  registered, pane live) — transitioning straight to `delivered`/`failed`
  **without** paste-and-Enter, so no pane mutation and no recipient
  context-load. New `claude-msg ping` CLI (`--timeout`, `--format`) and
  `tmux-msg.ping` MCP tool share one probe core. Outcome states:
  `delivered` (reachable, exit 0), `failed` (registered but unreachable,
  exit 69), `timeout` (no answer in time, exit 75). Pinging a
  non-registered agent fails loud. The intended replacement for the
  runbook "send a test bus message" verification step, which polluted the
  recipient's pane. README diagnostic + new-agent-setup sections and the
  diagnostic-playbook updated.

- **`docs/adr/0007` + `CONTRIBUTING.md` — Binnacle coexist external contract (#179, implements #164 Option B).** ADR-0007 records the coexist decision: tmux-msg stays MIT + standalone, Binnacle consumes it as an external Go module, the MIT+GPL-3.0 combination clean per the FSF compatibility list. New `CONTRIBUTING.md` commits the external contract — the exported Go API + DB schema (columns + state vocabulary) as stability surfaces — under the ratified deprecation policy (#162: pre-1.0 semver-explicit; post-1.0 two-minor-cycle floor + discretion clause + runtime warnings). README pointer added. Pure docs.

- **README `### Canonical name mapping` subsection (#143).** Documents
  the three-layer naming (wire-protocol / source / Claude Code slug /
  docs-prose), the Claude Code slug sanitization rule
  (`mcp__<server>__<tool_dot_to_underscore>`), a wire-probe
  operator-debug recipe (`tools/list` JSON-RPC via `claude-msg mcp`),
  and a caveat for pre-v0.6.0 cached sessions still surfacing
  `mcp__semaphore__semaphore_*` slugs. Pure docs; no behavior changes.

- **README `### Delivery modes` subsection (#138).** Documents the
  `paste-and-enter` (default) vs `mailbox-only` modes introduced by
  #116 (PR #129) plus the TOML knob landed by #132 (PR #135).
  Operator-as-bus-participant use case, three configuration surfaces
  (MCP / CLI / TOML) with example invocations, precedence chain, +
  `claude-msg config show` cross-link for resolved-value verification.
  Caught during the 2026-06-06 post-close AC audit on #132 — the
  v0.7.0 substrate landed cleanly but the README mention was missed
  at merge. Pure docs; no behavior changes.

- **`docs/why.md` — "Why tmux-msg?" pitch (public-launch prep).** A
  standalone, deployment-agnostic problem-framing doc for newcomers:
  the you-are-the-message-bus pain, the observe-gate trust-close,
  scope/non-goals, and a two-minute quickstart. First piece of the
  GitHub-launch documentation package (operator-directed 2026-06-06).
  Pure docs; no behavior change.

### Changed

- **`whereSince()` reader-startle comment (#176).** Added a one-line
  comment at the `return "1=1"` site in `internal/store/stats.go`
  clarifying it is a compile-time constant for `--window all` with no
  user input interpolated. No behavior change.

- **README rewritten for the public launch (closes #156; restructures the #143 canonical-name-mapping section added in #166 into the streamlined layout).** Restructured
  landing-page-first: leads with the "Why" pitch (links `docs/why.md`), genericized
  off the alcatraz-specific examples (substrate-first per ADR-0003), condensed the
  observe-gate section into a summary that links `docs/observe-gate.md`, and refreshed
  stale spots — the Message-rendering examples now show the bracket header (#121/#122)
  instead of the retired ─── box, the onboarding flow uses the shipped `register` CLI
  (was a stale SQL fallback), and the `--version` example tracks v0.7.0. Folds in the
  canonical-name-mapping table + wire-probe recipe (#143) and the two-shape header
  conventions + delivery-modes recipient-POV (#156). Pure docs.
- **`tmux-msg.send` MCP tool description (#156).** Names the queued→delivered lifecycle
  (and points at `tmux-msg.message_status` to confirm) and surfaces `reply_to`'s
  threading semantics, so a newcomer reading the schema doesn't read "queued" as
  "delivered" or miss reply-threading. Description string only; no behavior change.

### Fixed

- **`install.sh` fails loud on an unresolvable operator user (#175).** Dropped
  the hardcoded `${USER:-alex}` fallback: the operator account now resolves from
  `OPERATOR_USER` (env override) → `$SUDO_USER` → `$USER`, with **no** last-resort
  guess. If none resolves — or it resolves to `root` — the installer errors with
  exit 1 and a hint to set `OPERATOR_USER=<you>` or use `sudo`, instead of silently
  chowning `$DATADIR` + the systemd template to a wrong/nonexistent account. Closes
  two pre-public-release issues: silent misconfiguration, and shipping a maintainer's
  personal username in a public installer. The README `## Install` section gains a
  "what runs as root, and what runs as you" subsection documenting the privilege
  boundary (root writes the binary + creates the data dir; `go build` + the mailman
  daemons run as the operator).

- **`docs/security.md` ASCII alignment (#192).** Centered the "Bus" label between
  the Sender and Receiver boxes in the §1.3 Agent ↔ Agent diagram. Purely cosmetic;
  no policy change.

## [0.7.0] — 2026-06-06

### Changed

- **AskUserQuestion canary fixture refreshed for post-v0.6.0 validation
  (#133).** Added `golden_quartermaster_askuserquestion_2026-06-06.txt`
  alongside the existing 2026-06-04 capture; the
  `TestAwaitingOperatorMarker_MatchesGoldenCapture` canary now
  verifies both fixtures contain `AwaitingOperatorMarker` as a
  substring. Operator-coordinated capture via Bosun from a live
  AskUserQuestion popup confirms the existing marker `"↑/↓ to navigate
  · Esc to cancel"` still matches canonical popups post-cutover.

  Per `feedback_filed_rootcause_is_hypothesis`: the 2026-06-05
  incident's "marker mismatch" theory was working hypothesis until
  empirical verification. The new capture **disconfirms** the theory
  — the existing marker DOES match the AskUserQuestion popup variant
  the operator was in. The 2026-06-05 incident's failure mode was
  therefore something else (capture-window scroll, non-AskUserQuestion
  popup type, or one-off render-state quirk) rather than marker drift.
  The Half 2 safety net (#105 / PR #134) is the load-bearing
  protection regardless; even if the recognizer misclassifies, the
  pre-paste safety check aborts on `StateUnknown` so the load-bearing
  harm (paste-into-popup destruction) stays closed.

### Added

- **TOML config support for `delivery-mode` per-agent (#132 follow-up
  to #116).** New `delivery-mode` TOML knob in `[agent.<name>]` /
  `[defaults]` blocks. The mailman's startup reads the resolved config
  value (per-agent block > defaults block > DB column); when set, the
  config value overrides the `agents.delivery_mode` column at
  mailman-startup time:

  ```toml
  [agent.operator]
  delivery-mode = "mailbox-only"
  ```

  Lets operators who manage state via config (rather than via the
  `claude-msg register` / MCP `tmux-msg.register` paths) declare the
  mode without writing to the DB. The register-time CLI / MCP path
  still writes to the DB column; the TOML knob is the OVERRIDE at
  mailman-startup time — the DB column is the long-term default;
  config wins per-run.

  Validation: invalid mode values from config log a `WARN
  config_delivery_mode_invalid` and the DB column wins (fail-loud,
  not fail-stop — a typo in `/etc/tmux-msg/config.toml` doesn't
  silently break the mailman).

  New `config.ResolveString` helper centralizes the per-string-field
  precedence chain (sister to `ResolveBool` / `ResolveDuration`); the
  delivery-mode knob is the first string-typed field, designed
  forward-compatibly for additional string knobs.

  Surfaces:
  - **TOML**: `delivery-mode` per-agent or `[defaults]` block
  - **`ResolvedView.DeliveryMode string`** surfaced in
    `claude-msg config show` so operators can verify the resolved
    value without tracing through TOML manually
  - **No CLI flag**: the register-time CLI already covers the operator
    workflow; adding a `--delivery-mode` override to `serve` would
    duplicate the `register` surface without adding a use case

- **Pre-paste safety check against popup-as-Unknown destruction
  (#105 Half 2).** New mailman safety net: immediately before each
  paste-and-Enter delivery, the mailman takes one final `AgentState`
  reading. If the pane is observed in `StateAwaitingOperator` (popup
  open or operator typing) or `StateUnknown` (classifier couldn't
  substantiate), the delivery is aborted: the message reverts from
  `delivering` back to `queued` for the next mailman cycle, and a
  `WARN pre_paste_safety_abort` log line names the per-message ID +
  classified state for triage.

  Why: the load-bearing failure mode #105 surfaced was MaxWait firing
  with `lastState=Unknown` after an `AskUserQuestion` popup that
  didn't match `AwaitingOperatorMarker` consumed the operator's draft
  via paste-as-keystrokes (#105 2026-06-05 incident). The
  observe-gate's classification might miss a popup variant; the
  safety check is belt-and-suspenders that doesn't rely on the
  recognizer being perfect.

  New `tmuxio.IsPasteUnsafe(state) bool` helper centralizes the
  per-state policy (returns true for AwaitingOperator + Unknown; false
  for Idle + Working + AtRestInCompaction — the Compaction case is
  paste-unsafe-for-different-reasons handled at the
  `PostCompactPause` layer).

  Surfaces:
  - **CLI flag**: `--pre-paste-safety-disabled` (default false). Operators
    rarely need to disable; the check is structurally inexpensive (one
    capture-pane probe per delivery).
  - **TOML knob**: `pre-paste-safety-disabled = true` per-agent or in
    `[defaults]`, standard precedence chain.
  - **`ResolvedView.PrePasteSafetyDisabled bool`** surfaced in
    `claude-msg config show`.

  Recognizer-improvement work (Half 1 — empirical capture of the
  failing popup variant, marker expansion) tracked separately at #133
  since it needs operator-coordinated popup capture.

### Changed

- **Doc precision: "read-only-observe-only" overstated the gate's
  discipline (#126).** Multiple doc + code-comment surfaces described
  `ObserveGate` as strictly read-only. Accurate at v0.3.0 (#92's
  introduction) but v0.4.0 (#95) added the `📫` mailbox notification
  — a single character injection into the operator's input row via
  the `OnOperatorTyping` callback. The framing has been corrected to
  `near-read-only (one optional 📫 nudge when you're typing)` at the
  load-bearing surfaces (README top-line + observe-gate section +
  `docs/observe-gate.md` introduction + §"What it is, in one
  paragraph") and to the more verbose
  `observe-only-with-one-named-visibility-side-effect` at the
  code-comment layer (`internal/tmuxio/observe_gate.go`
  `ObserveGateOpts` + `ObserveGate` doc-comments). The `AgentState`
  probe remains strictly read-only and the docs there are unchanged
  — only the gate-itself framing was overstated.

- **Doc precision: stale migration paragraph in README (#124).** The
  README's "Migration from v0.2.x" paragraph described the legacy
  probe-and-watch TOML keys as "no-ops + startup WARN" — accurate at
  v0.3.0 but v0.4.0 (#94) made TOML decoding strict, so unknown keys
  now make the mailman's config load fail with an error naming the
  offending key. Operators following the old advice would see their
  mailman fall back to compile-time defaults rather than the
  WARN-then-continue path described. The paragraph now reflects the
  strict-fail behavior + the deprecated-key-removal recovery path.

### Fixed

- **Flaky timing test: `TestServe_PostCompactPauseDelaysNextDelivery`
  (#127).** The test measured the gap between `/compact` delivery and
  the next message's delivery via `time.Now()` at the test's polling-
  observation time. The 2ms poll cadence introduced double-sided
  jitter (~4ms) on the observed gap, which occasionally dipped below
  the 80ms `PostCompactPause` threshold even though the mailman's
  actual gap was always >= 80ms. Surfaced during PR #125's full-suite
  run.

  Fix: measure the gap via the store's `delivered_at` column (stamped
  inside `MarkDelivered` at the actual state-transition moment)
  rather than `time.Now()` at observation time. The polling now only
  decides when to stop waiting; the gap measurement reflects what the
  mailman actually did, not what the poller managed to observe. 20
  consecutive runs green.

### Added

- **`delivery_mode` for operator-as-bus-participant (#116).** New
  `delivery_mode` column on the `agents` table (default
  `paste-and-enter`, preserving existing behavior for all currently-
  registered agents) plus a new `mailbox-only` value that registers
  a pane as a bus *destination* without expecting the mailman to
  paste into it. The intended use case: an operator's own shell
  becomes a registered bus participant — chambers can `send to=operator`,
  and the operator polls via `claude-msg inbox` when convenient.

  Per ADR-0005 §Decision (1)'s wheel-reinvention check, this is a
  config-difference (one column on the agents table), NOT a
  participant-supertype expansion. The substrate's primitive remains
  `agent`; the configuration space widens by one field.

  Surfaces:
  - **CLI**: `claude-msg register --name operator --delivery-mode mailbox-only`
    (new subcommand mirroring the existing MCP tool — load-bearing for
    operators at a bare shell who can't easily invoke MCP)
  - **MCP**: `tmux-msg.register` gains a `delivery_mode` parameter
    (`paste-and-enter` | `mailbox-only`, default `paste-and-enter`)

  Mailman lifecycle: registering with `delivery_mode=mailbox-only`
  implicitly sets `start_mailman=false` (no daemon needed; messages
  stay in `state=queued` and the operator polls). Explicit
  `start_mailman=true` overrides for operators who want a daemon
  running for monitoring/health purposes.

  Chrome detection: `claude-msg state` and `tmux-msg.agent_state`
  short-circuit to `idle` for mailbox-only agents — a bare-shell pane
  has no Claude TUI to probe, so the chrome-marker heuristics would
  always classify as `unknown`. Zero capture-pane calls.

  Flip-back asymmetry: if you later switch a registered agent from
  `mailbox-only` back to `paste-and-enter`, you need to manually
  restart the mailman unit (`systemctl --user restart
  claude-mailman@<name>`). The mailman doesn't auto-restart on the
  delivery-mode change because the previous startup short-circuited
  to `Result=success` (no resume trigger). The serve-time
  short-circuit log-line names this asymmetry so operators discover
  it when they hit it.

  Scope re-label: original issue labeled `size/S` (1-2h); actual
  implementation is `size/M` (5 surfaces touched: schema migration +
  store accessors + MCP register handler + CLI register subcommand +
  mailman gate + chrome short-circuit). Documented in PR body.

- **`get` subcommand + `tmux-msg.get` MCP tool — fetch processed
  messages by ID (#111).** New recovery surface for the case where a
  delivery landed correctly into the SQLite store but was visually
  swallowed by the recipient pane's state (mid-AskUserQuestion popup,
  mid-compaction, etc.). The bus always preserved the body; this just
  gives both CLI and MCP-aware sessions a direct retrieval path
  instead of requiring manual SQLite lifting.

  Surfaces:
  - **CLI**: `claude-msg get <id> [--from <name>] [--format text|json]`
  - **MCP**: `tmux-msg.get` with `{id: string}` input. Returns full
    message body + metadata (`from`, `to`, `kind`, `state`,
    `created_at`, `delivered_at?`, `reply_to?`).

  Accepts full public_id or short prefix (the 4-char IDs that appear
  in delivery headers work). Short-prefix lookup with disambiguation:
  if multiple authorized matches → error names the matching IDs so the
  operator can re-issue with a longer prefix.

  Access model: sender OR recipient by default. A `privileged-agents`
  TOML knob extends the allowlist:

  ```toml
  privileged-agents = ["bosun", "quartermaster"]
  ```

  No existence leak: not-authorized requests return the same error
  class as not-found, so a requester can't probe for IDs they have no
  business knowing about.

- **`working-deliver-immediately` opt-in for fast-path delivery to
  busy chambers (#106).** New `--working-deliver-immediately` CLI
  flag + `working-deliver-immediately = true` per-agent TOML knob
  (default `false`) that opts the observe-gate's `StateWorking`
  branch out of the safer-default backoff and into the same fast-path
  return as `StateIdle`. When enabled, mid-turn deliveries land in
  the recipient's input row while Claude is still streaming and are
  read as the next operator turn after the current one completes
  (Claude Code's TUI buffers mid-turn keystrokes; the paste is
  structurally safe). For crew-coordination workflows the cadence
  win is real — typical 1s instead of 3-57s under backoff.

  Per-state eligibility (`StateWorking` ONLY):
  - `StateAwaitingOperator` — operator drafting; paste would destroy
    their input. Hard-deferred regardless.
  - `StateAtRestInCompaction` — `/compact` slash-command parser would
    race the paste. Hard-deferred regardless.
  - `StateUnknown` — the popup-as-Unknown failure mode #105 surfaced;
    immediate paste into an unrecognized state is the destructive
    case. Hard-deferred regardless.

  The verify-token retry + `delivered_unverified` notice path is the
  load-bearing safety net for the small race window between
  observing `StateWorking` and the paste landing.

  Operator-side migration: no action required. The flag defaults to
  `false`, preserving the v0.3.0-through-v0.6.0 conservative behavior.
  Flip per-agent in `/etc/tmux-msg/config.toml` when the coordination-
  latency tradeoff favors immediate delivery (e.g., Bosun the
  orchestrator, where coordination cadence matters).

### Changed

- **Delivery template re-grounded on narrow-viewport rendering (#121).**
  The mailman's delivered-message template switched from box-drawing
  rules to a compact ASCII bracket header, and the trailing footer rule
  is dropped:

  Before:
  ```
  ─── Reply from Bosun → Quartermaster ── re: 1d0c ── id 8f54 ──
  body content
  ────────────────────────────────────────────────
  ```

  After:
  ```
  [Bosun → Quartermaster · re 1d0c · id 8f54]

  body content
  ```

  Reason: on narrow viewports (mobile chat clients), the ~48-char
  box-drawing rules wrapped to 2-3 short stacked lines, and some mobile
  fonts lacked U+2500 (BOX DRAWINGS LIGHT HORIZONTAL) and fell back to
  underline-position glyphs. The bracket-and-middle-dot format uses
  characters with near-universal font coverage and stays compact enough
  to fit narrow viewports without wrapping ugliness. The blank line
  between header and body separates the envelope label from content;
  the bracket-open at the start of each new header delimits consecutive
  messages on visual scan.

  Information content preserved: sender, recipient (replies), reply
  thread (replies), message ID, local clock (regular messages). Grep
  workflows that match on `id NNNN` still work — the ID still appears
  in plain text in every header.

  Syntax compressions alongside the chrome swap: `re:` → `re` (colon
  dropped — the bracket boundaries already segment the header), and
  the `──` segment-separators became `·` middle-dots.

## [0.6.0] — 2026-06-05

### Changed

- **MCP wire-surface re-grounded on substrate name (#112, ADR-0004).**
  The MCP server name and tool method prefix flipped from `semaphore`
  to `tmux-msg`:
  - **Server name**: `"semaphore"` → `"tmux-msg"` in MCP registration.
    Every chamber's `.mcp.json` mapping for this MCP needs updating
    (replace `"semaphore"` key with `"tmux-msg"`).
  - **Tool method names**: `semaphore.send` → `tmux-msg.send`,
    `semaphore.inbox` → `tmux-msg.inbox`, and 8 others (full
    rename of all 10 registered tools).
  - **Control-command names** for the `tmux-msg.control` whitelist:
    `mcp-restart-semaphore` → `mcp-restart-tmux-msg`,
    `mcp-enable-semaphore` → `mcp-enable-tmux-msg`,
    `mcp-disable-semaphore` → `mcp-disable-tmux-msg`. Operator-side
    macros that fire these commands need updating in lockstep with
    the binary deploy.

  Hard cutover per ADR-0004 §Decision (4): no alias period. Every
  chamber updates `.mcp.json` + restarts Claude Code in one
  operational window. ~5 minutes of MCP-bus-quiet across all six
  chambers expected.

- **Substrate terminology re-grounded `chamber` → `agent`
  (#107, ADR-0005).** The substrate's per-pane-CLI-tool primitive is
  renamed from project-local `chamber` jargon to substrate-honest
  `agent` (which already lived in the substrate's identifier
  vocabulary — `agents` SQL table, `--agent` flag,
  `claude-mailman@<agent>.service` template):
  - **Go code identifiers**: `ChamberState` → `AgentState`, all
    derived forms (`chamberState`, `chamber_state`, etc.) swept
    across `cmd/` and `internal/`. The `internal/store` schema's
    `agents` table was already named correctly and stays unchanged.
  - **MCP tool method**: `semaphore.chamber_state` →
    `tmux-msg.agent_state`. Bundled into the same restart-cycle
    cutover as the MCP wire-surface rename per ADR-0005
    §Decision (4).
  - **Doc prose**: README, `docs/diagnostic-playbook.md`,
    `docs/operator-ux.md`, `docs/security.md` swept.
  - **Out of scope** (preserved as written per ADR-0004 §Generality
    + ADR-0005 §Decision (2)): ADR-0001 through ADR-0006 prose stays
    frozen as accurate-to-time; chamber-level CLAUDE.md files in
    `frankenbit/alcatraz-infra` are project-local lexicon (covered
    by separate bridge note in `alcatraz-infra#21`); Binnacle's own
    usage is a separate Bosun follow-up.

  Operator migration in one cutover (alongside MCP wire-surface
  rename above):

  ```bash
  # On alcatraz, after merging this PR + cutting v0.6.0:
  sudo systemctl --user stop 'claude-mailman@*'
  # Install v0.6.0 binary
  sudo install -m 0755 -o root -g root claude-msg /usr/local/bin/
  # Update each chamber's .mcp.json: server name semaphore → tmux-msg
  # Update each chamber's Claude Code session: /mcp restart, then
  # re-launch session so the new MCP tool names register.
  systemctl --user daemon-reload
  systemctl --user start 'claude-mailman@*'
  ```

## [0.5.0] — 2026-06-05

### Changed

- **Project re-grounded on its substrate primitive (#97).** Renamed
  from `cli-semaphore` to `tmux-msg`. This is not a cosmetic rename
  but a substrate-class accuracy correction: the substrate IS tmux
  (pane registry + paste-and-Enter delivery + per-pane chrome
  detection); the CLI tool running inside the pane is downstream.
  The old name conflated two layers — `cli` was generic, `semaphore`
  was internally accurate but obscure for external readers. The new
  name names what the substrate actually is, and preserves the
  multi-CLI-flavor binary scheme: `claude-msg` today, sibling
  binaries (`codex-msg`, `copilot-msg`) when there's need for them.

  Surface changes:
  - **Repo**: `frankenbit/cli-semaphore` → `frankenbit/tmux-msg`
    (Forgejo creates URL redirects; old issue/PR links continue to
    resolve)
  - **Go module path**: `git.frankenbit.de/frankenbit/cli-semaphore`
    → `git.frankenbit.de/frankenbit/tmux-msg`; every import statement
    in the codebase updated mechanically
  - **Operational directories**: `/etc/cli-semaphore/` →
    `/etc/tmux-msg/`, `/var/lib/cli-semaphore/` → `/var/lib/tmux-msg/`
  - **Code constants**: `config.DefaultPath`, `defaultDBLocation`,
    help-text strings, and doc-comment cross-references all updated
  - **Unchanged**: the binary stays `claude-msg` (it's CLI-tool-
    flavored, not substrate-flavored), the daemon stays
    `claude-mailman@<agent>.service` (same reason), the MCP server
    name stays `semaphore` (decoupled from the repo name)

  Migration: the v0.5.0 binary reads from the new operational paths.
  On alcatraz, the v0.4.0 → v0.5.0 deploy moved `/etc/cli-semaphore/`
  → `/etc/tmux-msg/` and `/var/lib/cli-semaphore/` → `/var/lib/tmux-msg/`
  atomically during the mailman swap window. Operators with custom
  install paths need to mv their `/etc/cli-semaphore/` and
  `/var/lib/cli-semaphore/` to the new names before starting the
  v0.5.0 binary.

  Out of scope of this rename (tracked separately):
  - `ChamberState` → `SessionState` identifier rename (#107) — same
    substrate-honesty discipline applied to internal identifiers;
    the Binnacle/Nimbus jargon `chamber` is downstream from
    cli-semaphore's perspective and shouldn't leak upstream
  - Binnacle repo's references to `cli-semaphore` (delegated to
    Bosun as a follow-up dispatch)

## [0.4.0] — 2026-06-04

### Fixed

- **Multi-line draft truncation in observe-gate's (c) flush (#96).**
  `extractInputContent` previously returned only the first sentinel-
  prefixed row of the captured pane, so when the observe-gate fired
  the (c) Clear-paste-archive flush on a multi-line operator draft,
  the archived `kind=stranded_draft` row held only line 1 — while
  `Ctrl+U` cleared the entire input buffer. Lines 2+ were silently
  destroyed with no bus-recovery path, which is the exact failure
  mode the (b) Clear-and-discard option was rejected to avoid: (c)
  silently degraded to (b) for multi-line drafts.

  Empirical evidence (the 2026-06-04 post-deploy validation test of
  PR #93): a ~5-min typing session that should have archived several
  paragraphs only archived 123 bytes — the first sentence of the
  operator's submitted message. Documented at the time as a partial-
  archive surprise; #96 traced it back to this single-row extraction
  gap.

  Fix: `extractInputContent` now walks from the sentinel row downward,
  joining continuation rows with `\n` until it hits an
  `isInputAreaBoundary` row. Two boundary recognizers:
  - `⏵⏵` (U+23F5, the status-line marker that bounds the input area
    below in every Claude Code pane)
  - 20+ consecutive `─` (U+2500) characters, the below-input
    separator. Threshold tuned to avoid false-positives on operator-
    typed runs of box-drawing chars (a vanishingly-rare edge case).

  The walk-until-boundary shape matches Claude Code's TUI layout:
  `─── <title> ──` separator above, `❯ ` sentinel + continuation
  rows, below-input separator, status line. The fix preserves the
  archive-then-clear-then-paste ordering — `Ctrl+U` is unchanged;
  it's the archive half that's now honest about what it captures.

  Test coverage: `TestExtractInputContent_MultilineDraftCapturedToStatusBoundary`,
  `TestExtractInputContent_StopsAtBelowInputSeparator`,
  `TestExtractInputContent_StopsAtStatusLine`, plus a function-level
  pin `TestIsInputAreaBoundary_RecognizerCases` (9 sub-cases). The
  existing `TestExtractInputContent_SentinelRowFound` fixture was
  updated to include the boundary chrome that production captures
  always have. Mutation experiment: reverting to the legacy single-
  row extraction makes the new multi-line tests fail with the exact
  truncation signature (only line 1 captured).

### Added

- **Strict-mode TOML config decoding (#94).** `config.LoadFrom`
  now uses `toml.Decode` + a post-decode `MetaData.Undecoded()` check;
  any unknown key in `/etc/cli-semaphore/config.toml` causes the load
  to fail with an error naming the offending key(s). Catches operator
  typos AND configs that still mention the legacy probe-and-watch
  knobs swept below — `prompt-sentinel-gate = true` in an old config
  block now fails with `config: parse /etc/cli-semaphore/config.toml:
  unknown key(s): agent.bosun.prompt-sentinel-gate`. Replaces the
  prior silent-drop behavior + post-hoc startup WARN, matching the
  fail-loud discipline the v0.3.0 substrate shift introduced
  elsewhere.

- **📫 mailbox notification for pending bus messages (#95).** When
  the observe-gate's first iteration observes `StateAwaitingOperator`
  (cursor past sentinel = operator drafting), the mailman injects a
  single `📫` (U+1F4EB) character into the operator's input row as
  a one-shot visibility signal that a bus message is waiting. Closes
  the gap surfaced post-deploy of v0.3.0: the substrate-class shift
  eliminated the legacy `─` probe dashes that had served as an
  unintentional visibility signal, leaving the operator with no
  indication that something was pending while they typed.

  Design properties per the operator's 2026-06-04 framing:
  - **Inject once per delivery cycle**, not on every observe
    iteration. `ObserveGate.OnOperatorTyping` callback tracks a
    `notifiedOfTyping` boolean; subsequent iterations skip the
    re-fire. Qualitatively different from probe-and-watch's
    continuous dash injection.
  - **No cleanup attempted.** The mailman does NOT track or remove
    the `📫`. Operator-deletes-or-it-rides-along is the intentional
    design — sibling to the (b)-rejected discipline that informs
    the (c) flush. Recipients seeing `📫` in a message body know what
    it means ("the sender saw a pending bus message land while they
    were typing").
  - **Vector**: mailman → operator (incoming notification). Distinct
    from the rejected greenlight glyph proposal which was operator →
    mailman (manual override). The greenlight was subsumed by v0.3.0's
    speed; `📫` fills a different (smaller, real) UX gap.

  New surface:
  - `internal/tmuxio.PendingMessageMarker` constant (`"📫"`)
  - `internal/tmuxio.NotifyPendingMessage(ctx, pane)` helper —
    single `tmux send-keys -l 📫`, no Enter follow-up
  - `ObserveGateOpts.OnOperatorTyping` callback field — gate fires
    it ONCE per delivery cycle on first `StateAwaitingOperator`
  - `--notify-emoji-disabled` CLI flag + `notify-emoji-disabled`
    TOML knob (default `false` = notification on)
  - `ResolvedView.NotifyEmojiDisabled` for `claude-msg config show`

  Test coverage: 5 new tests covering one-fire-per-cycle, no-fire on
  idle fast-path, nil-callback safety, send-keys call shape (no
  Enter), and pane-required validation. Mutation experiment: removing
  the `notifiedOfTyping` guard makes the one-fire test fail with the
  expected over-fire count matching iteration depth.

### Removed

- **Dead probe-and-watch primitives + legacy gate knobs (#94).**
  Follow-up sweep to v0.3.0's observe-gate substrate-class shift,
  deferred from PR #93 to keep that diff scoped. The active code path
  hasn't called any of these since 2026-06-04; this PR removes them
  from the codebase entirely.

  Removed from `internal/tmuxio/`:
  - `probe.go` (full file) — `WaitForQuietPane`, `QuickPresenceProbe`,
    `InputRowHasContent`, `analyzeDelta`, `stripTrailingProbes`,
    `classifyInputRow`, `QuietOpts`, `QuickPresenceOpts`, `DeltaKind`
    + `DeltaQuiet` + `DeltaInputActivity` constants, `ErrCapExceeded`
    sentinel, `QuietProbe` constant, `sleepWithPing` helper
  - `probe_test.go` (full file) — analyzeDelta / stripTrailingProbes
    / QuickPresenceProbe / WaitForQuietPane tests
  - `pin_test.go` (full file) — discipline pins for the dead
    `DeltaKind` binary-verdict surface
  - The four marker canary tests (PromptSentinel byte-encoding +
    golden-capture; AwaitingOperatorMarker golden-capture;
    CompactionMarker golden-capture) migrated to the new
    `state_canary_test.go` since they exercise live `state.go`
    constants, not the dead probe-and-watch flow
  - `fakeProbeRunner` (test helper used by ChamberState tests)
    migrated to `state_test.go`

  Survived the sweep into `state.go`:
  - `PromptSentinel` constant (with its full forward-watch doc-
    comment) — used by ChamberState's cursor-aware path and
    `observe_gate.go`'s `extractInputContent`
  - New `isInputRowQuiet` helper (the parse-only sibling that used to
    be `classifyInputRow`, now returns `bool` instead of the dead
    `DeltaKind` enum)

  Removed from `cmd/claude-msg/serve.go`:
  - Legacy CLI flags: `--quiet-disabled`, `--quick-presence-probe`,
    `--prompt-sentinel-gate`, `--quiet-observe-window`,
    `--quiet-input-backoff`, `--quiet-max-wait`
  - The `_ = *quiet…` discards keeping the flag pointers alive after
    parse
  - The startup `WARN config: deprecated knobs ignored …` block —
    replaced by a stricter TOML decoder (see Added below) so a config
    file that still mentions retired keys fails the load loudly with
    an "unknown key(s):" error naming the offending key, rather than
    a silent ignore + post-hoc WARN.

  Removed from `internal/config/config.go`:
  - `Block` fields: `QuietDisabled`, `QuickPresenceProbe`,
    `PromptSentinelGate`, `QuietObserveWindow`, `QuietInputBackoff`,
    `QuietMaxWait`
  - `ResolvedView` legacy fields (used to surface them via
    `claude-msg config show`)
  - The `(*File).DeprecatedKnobs(agent)` helper that drove the
    startup WARN
  - Corresponding cases in `blockBoolField` / `blockDurField` /
    `Resolve`

  Removed from `cmd/claude-msg/config.go`:
  - `claude-msg config show`'s `quiet-disabled` / `quick-presence-probe`
    / `prompt-sentinel-gate` / `quiet-*` output lines (replaced by
    `gate-disabled` / `poll-interval-min` / `poll-interval-max` /
    `input-stale-threshold` in v0.3.0; the legacy lines are gone now)

  Removed from `tools/check-pin-slugs/`:
  - `OperatorInputRowGate` from the in-use-slugs allowlist (the pin
    was retired in v0.3.0 with the asymmetric gate composition it
    guarded; tracker comment preserved at the call site)

  Removed from `internal/config/config_test.go`:
  - `TestLoadFrom_ParsesQuickPresenceProbeAndPromptSentinelGate` and
    `TestResolveBool_PrecedenceChain_QuickPresenceProbeAndPromptSentinelGate`
    (the fields they pinned no longer exist); replaced with sibling-
    shape `TestLoadFrom_ParsesGateDisabled` and
    `TestResolveBool_PrecedenceChain_GateDisabled` that exercise the
    observe-gate's surviving bool knob.

  Operator migration: any `/etc/cli-semaphore/config.toml` that still
  references the removed keys now produces a TOML parse error at
  mailman startup, naming the offending key. Delete the lines or the
  containing `[agent.<name>]` block and restart.

## [0.3.0] — 2026-06-04

### Fixed

- **PromptSentinel NBSP encoding bug — silent since PR #66.**
  PR #66 (prompt-sentinel gate) + PR #77 (cursor-aware ChamberState
  v2) shipped with `PromptSentinel = "❯ "` using a regular space
  (U+0020). Empirical pane capture across all 6 chambers on
  2026-06-04 (post-PR-#77 deploy smoke test) revealed Claude Code
  actually emits `❯` + NBSP (U+00A0, hex `c2 a0`), not a regular
  space. The sentinel constant never matched any real Claude Code
  pane in production — both PR #66's `InputRowHasContent` and PR
  #77's cursor-aware classification silently fell through to their
  fallback branches, making the prompt-sentinel-gate (deployed for
  Bosun + QM since 2026-06-03) operationally invisible (full
  WaitForQuietPane handled all traffic; sentinel never matched).

  The defect was invisible because:
  - Unit-test fixtures used the regular-space variant (spec-derived
    rather than capture-derived); tests passed against a fiction of
    production substrate
  - The safer-default-on-uncertainty contract made the always-falls-
    through behavior operationally indistinguishable from a working
    gate (over-gate is harmless; under-gate would have been caught)
  - Cycle 6 PR #77's smoke test was the first time the algorithm
    was expected to classify chambers as `idle` post-deploy; 4/5
    chambers returning `unknown` surfaced the substrate-defect at
    a layer below the cursor-aware algorithm

  Fix:
  - `internal/tmuxio/probe.go`: `PromptSentinel` constant updated to
    `"❯ "` (explicit NBSP escape) with extensive doc-comment
    naming the empirical-capture verification + the substrate-discovery
    timeline.
  - `internal/tmuxio/testdata/golden_bosun_idle_2026-06-04.txt` (new):
    real `tmux capture-pane` output from Bosun's idle pane, frozen
    as a capture-derived test fixture. Pins the production encoding
    against future drift.
  - Two new canary tests in `internal/tmuxio/probe_test.go`:
    `TestPromptSentinel_BytesMatchNBSP` (asserts the constant's byte
    encoding matches the empirically-captured production bytes) and
    `TestPromptSentinel_MatchesGoldenCapture` (loads the golden file
    + verifies PromptSentinel matches a sentinel row).
  - All inline test fixtures in `probe_test.go`, `state_test.go`,
    and `cmd/claude-msg/state_test.go` updated from `"❯ "` (regular
    space) to `"❯ "` (NBSP escape) — 35 occurrences across
    three files. The escape sequence keeps the NBSP visible in
    source code; using a literal NBSP would silently fool future
    readers into thinking it's a regular space.

  Forward-watch: any future Claude Code TUI version that changes
  the prompt character or separator will surface as a golden-capture
  mismatch. The canary tests catch it loudly.

  Sibling discipline: Surveyor's O28 (integration-config-wiring) had
  the closest existing shape; this is its sibling
  **substrate-constant-byte-encoding** class — verify the byte
  encoding of constants that reference external-tool emissions
  against the actual tool emission, not the spec.

### Added

- **Read-only-observe-only delivery gate (#92).** `internal/tmuxio/
  observe_gate.go` introduces `ObserveGate`, replacing the probe-and-
  watch `WaitForQuietPane` flow in the mailman's pre-delivery path.
  The new gate uses repeated `ChamberState` polls (read-only-observe
  substrate-class, zero pane mutation) + content-hash stale detection
  to decide when to deliver. Typical-case latency drops from 72s
  (legacy single backoff cycle) or 138s (legacy double backoff cycle)
  to ~3–5s. The `─` probe dashes that previously appeared in the
  receiver's input row during gate observation are gone.

  Gate decision matrix:
  - `StateIdle` (cursor at sentinel, empty input row or auto-suggestion
    ghost-text) → deliver immediately.
  - `StateAwaitingOperator` (cursor past sentinel = operator typing) →
    hash the input-row content; if it stays unchanged for at least
    `InputStaleThreshold` (default 2 min), return `Stale=true` so the
    caller can archive + clear + paste.
  - `StateWorking` / `StateAtRestInCompaction` / `StateUnknown` →
    safer-default wait, progressive backoff (×1.5: 3s → 4.5s → 6.75s →
    … → 15s cap).

  Stale-flush mechanics implement the (c) Clear-paste-archive primary
  path per #92's 2026-06-04 design call: the gate returns the captured
  input content; the mailman archives it as `kind=stranded_draft`
  (cap-bypass) before sending Ctrl+U + paste. On archive failure, the
  (a) Append fallback kicks in (paste-and-Enter without clearing —
  compound message, but doesn't strand the delivery). The (b) Clear-
  and-discard option is REJECTED in code + comments because the input
  content might be a half-delivered bus message from a previous
  failed delivery; blind Ctrl+U would destroy bus content not
  operator content.

- **`KindStrandedDraft` message kind** (`internal/store/types.go`).
  Self-addressed snapshot row inserted via `InsertNotice` (cap-bypass)
  whenever the observe-gate decides to flush operator-typed content
  from the input row. The body preserves the cleared content verbatim
  + a reference to the triggering delivery's public_id so the operator
  can recover the draft post-hoc.

- **New TOML/CLI knobs** for tuning the observe-gate: `gate-disabled`
  (default `false`), `poll-interval-min` (default `3s`),
  `poll-interval-max` (default `15s`), `input-stale-threshold` (default
  `2m`). All composable with the existing per-agent precedence chain.

### Changed

- **Default delivery-gate behavior** (#92). The pre-delivery gate is
  now on by default for all chambers (observe-gate, read-only,
  ~3–5s typical). Previously the gate was OFF by default
  (`quiet-disabled = true` since 2026-06-01) with an opt-in
  `prompt-sentinel-gate` for Bosun + Quartermaster that fell back to
  the probe-and-watch gate (60s `quiet-input-backoff` per iteration —
  the load-bearing cost in cli-semaphore #91's investigation). The new
  default is strictly better than both: faster than the legacy gate,
  safer than no gate at all.

  Migration for operators with the old config: blocks like
  `[agent.bosun] prompt-sentinel-gate = true` can be deleted — the new
  gate is on for all chambers without per-agent config. Existing
  `quiet-*` and `prompt-sentinel-gate` / `quick-presence-probe` knobs
  are ignored at runtime; the mailman logs a WARN at startup naming
  any that are set so the operator knows to migrate.

- **Silent-pass gap closed on `AwaitingOperatorMarker` canary (#89,
  retrofit from PR #88).** `TestAwaitingOperatorMarker_MatchesGoldenCapture`
  in `internal/tmuxio/probe_test.go` previously relied solely on a
  `strings.Contains(golden, AwaitingOperatorMarker)` substring check.
  Go's `strings.Contains(g, "")` returns true for any `g` — so a
  regression that reverted `AwaitingOperatorMarker` to the pre-#79
  placeholder `""` would silently pass the canary while disabling the
  `StateAwaitingOperator` branch in `ChamberState`. PR #88 surfaced
  the same gap on `TestCompactionMarker_MatchesGoldenCapture` and
  added an explicit empty-marker guard; this retrofit carries the
  same one-line guard back to PR #87's canary. No production
  behavior change. Mutation experiment verified: emptying the
  marker now fires both the new guard (in `probe_test.go`) and the
  e2e classification pin
  `TestChamberState_AwaitingOperatorOnAskUserQuestionGolden` (in
  `state_test.go`).

### Deprecated

- **Legacy probe-and-watch CLI flags + TOML knobs** (#92): `--quiet-
  disabled` / `quiet-disabled`, `--quick-presence-probe` /
  `quick-presence-probe`, `--prompt-sentinel-gate` /
  `prompt-sentinel-gate`, `--quiet-observe-window` /
  `quiet-observe-window`, `--quiet-input-backoff` /
  `quiet-input-backoff`, `--quiet-max-wait` / `quiet-max-wait`. All
  become no-ops at runtime; the observe-gate subsumes their behaviors
  per the migration plan. Mailman startup logs a WARN naming any that
  are set. Will be removed in a future release.

### Removed

- `cmd/claude-msg/serve_quiet_test.go` (3 tests:
  `TestServe_QuietGate_DeliversAfterInputActivity`,
  `TestServe_UnverifiedDelivery_MarksDeliveredWithWarn`,
  `TestServe_QuietGate_CapExceededLogsAndDelivers`) — all coupled to
  probe-and-watch behavior that no longer exists at the active path.
  The `delivered_unverified` semantics at the deliver layer remain
  covered by `internal/tmuxio.TestDeliver_ReturnsUnverifiedSentinelAfterRetriesExhausted`;
  a serve-layer pin can be re-added as a follow-up once the gate
  migration settles if needed.
- `TestPin_OperatorInputRowGate_QuickProbeSkippedWhenSentinelPromotes`
  (`cmd/claude-msg/pin_test.go`) — the asymmetric gate composition
  this pinned (sentinel-first-cheap promotes, QuickPresenceProbe
  skipped) doesn't exist anymore because there's only one gate. The
  empty PIN_ slot is preserved as a comment for traceability.

### Added

- **AtRestInCompaction detection: `/compact` UI capture (#70).** The
  chamber-state primitive's `CompactionMarker` constant flips from
  placeholder `""` to `"Compacting conversation…"` (with U+2026
  ellipsis), populated from two empirically-captured pane snapshots
  taken during the same `/compact` event — at 8% and 68% progress.
  The two captures show different spinner glyphs (`✻` U+273B vs `✢`
  U+2722), confirming Claude Code cycles the leading glyph across
  spinner frames; the marker intentionally excludes the glyph and
  matches the trailing phrase that survives the animation. The
  marker check at precedence 1 in `ChamberState` fires BEFORE the
  precedence-2 pane-equality "working" check — load-bearing because
  the compaction UI animates (spinner cycles, percentage ticks,
  elapsed time changes) so `capA != capB`, and without the marker
  check firing first a mid-compaction chamber would mis-classify as
  Working. Two capture-derived golden fixtures at
  `internal/tmuxio/testdata/golden_quartermaster_compaction_2026-06-04.txt`
  and `internal/tmuxio/testdata/golden_quartermaster_compaction_advanced_2026-06-04.txt`
  pin the encoding + the spinner-cycling robustness against future
  drift; two new tests in `probe_test.go` + `state_test.go` enforce
  the constant-vs-golden alignment (with an explicit empty-marker
  guard — `strings.Contains(g, "")` is true so a regression to the
  placeholder needed an explicit non-empty assertion to surface) AND
  the end-to-end `ChamberState` classification (`StateAtRestInCompaction`
  with marker surfaced in Evidence, capA=early-golden and capB=
  advanced-golden so the test exercises the precedence-over-working
  property). Mutation experiment verified: reverting the marker to
  placeholder makes both pins fire — the canary on the explicit
  guard, the e2e on the mis-classification as Working. Pre-#70 a
  chamber mid-`/compact` classified as `working` (the spinner-animation
  hit precedence 2); post-#70 it correctly classifies as
  `at-rest-in-compaction`. Closes the second of the two empirical-
  capture lit-ups originally bundled as the parent #69 verdict —
  `AwaitingOperatorMarker` (#79, PR #87) and `CompactionMarker`
  (#70, PR #88) — completing the 5-state vocabulary's detection
  coverage.

- **AwaitingOperator detection: AskUserQuestion popup capture (#79).**
  The chamber-state primitive's `AwaitingOperatorMarker` constant
  flips from placeholder `""` to `"↑/↓ to navigate · Esc to cancel"`,
  populated from an empirically-captured AskUserQuestion popup. The
  popup overlays the input area (no `❯` row visible), so the cursor-
  aware classification falls through to the marker check at
  precedence 5 — the popup footer's keybinding hint combined with
  U+00B7 middle-dot separators is structurally unique to Claude
  Code's popup UI. Capture-derived golden fixture at
  `internal/tmuxio/testdata/golden_quartermaster_askuserquestion_
  2026-06-04.txt` pins the encoding against future drift; two new
  tests in `probe_test.go` + `state_test.go` enforce the constant-
  vs-golden alignment AND the end-to-end `ChamberState` classification
  (`StateAwaitingOperator` with marker surfaced in Evidence). Mutation
  experiment verified: reverting the marker to placeholder makes
  ChamberState return `StateUnknown` and the pin fires with an
  explanatory error pointing at AwaitingOperatorMarker. Pre-#79 the
  Quartermaster pane during an AskUserQuestion popup classified as
  `unknown`; post-#79 it correctly classifies as `awaiting-operator`.

- **Bulk MCP refresh: `claude-msg refresh-all-mcps` (#62).** Replaces
  the per-chamber `/mcp restart semaphore` typing tax after binary
  deploys. Iterates the registered `agents` table and fires the
  existing `mcp-restart-semaphore` macro (#28) per chamber via the
  shared `doControl` path. Reports per-chamber success/cap-rejected
  outcome in text or JSON; exits non-zero if any chamber failed so
  scripts can detect partial fan-out. Operator-only (CLI surface; no
  MCP tool variant — peer-invokable bulk-restart would be a DoS
  amplification class). Sender backlog cap is raised to the exact
  upper bound `2*N + capSenderBacklog` for the duration of the
  fan-out (operation-scoped cap-raising, not cap-exemption; per-
  recipient cap stays at 5 to protect each chamber individually).
  README "New tools require a session restart" section names the
  convenience surface + the size/M follow-up trigger for state-gating
  if mid-tool-call disruption becomes recurring felt-pain
  (post-#69 chamber-state primitive enables `state in [idle,
  awaiting-operator]` as the natural gate).

- **Discipline-pin: cross-process cap-as-ceiling invariant (#33).**
  The existing `TestPin_AtomicCapEnforcement_CeilingUnderConcurrency`
  in `internal/store/pin_test.go` exercises BeginTx atomicity inside
  one `*Store` via N goroutines sharing one `*sql.DB`. Surveyor's #29
  round-3 review flagged the missing axis: SQLite's file-level
  RESERVED lock + `_txlock=immediate` + `busy_timeout` are what
  actually make the cap hold across **distinct OS-level processes**
  (mailman daemons + `claude-msg` CLI invocations + MCP server
  children all hit the same `messages.db` from separate processes).
  Two new sibling pins under the same `AtomicCapEnforcement` slug
  close the gap: a probe binary at
  `internal/store/cmd/concurrency-probe/` opens the store and
  exits 0/2/1 per the cap-rejection contract; the parent test in
  `internal/store/messages_xprocess_test.go` spawns N=20 children for
  single-insert + N=8 for InsertMessagePair and asserts exactly
  cap-many succeed. Mutation experiment confirmed: dropping
  `_txlock=immediate` from the DSN trips both pins with `SQLITE_BUSY`
  on the contending probes. Slug reuse (same architectural
  commitment, different concurrency axis) — no ADR amendment needed.
  `make check-pin-slugs`: 7 slugs registered, 7 in use, aligned.

- **Uninstall path: `uninstall.sh` + README "Removal" section (#80).**
  The M6 install issue (#14) shipped with an un-ticked "Uninstall path
  documented" AC; #80 captured the gap. The new script is idempotent,
  default-safe (leaves the SQLite DB at `/var/lib/cli-semaphore/`
  alone), and supports `--purge` to wipe the data dir after an
  interactive confirmation when stdin is a TTY. Foot-gun guard:
  refuses to run from inside the data directory itself. README's new
  "Removal" section sits between Install and Use from Claude Code,
  naming what the script does NOT touch (`/etc/cli-semaphore/`,
  `~/.claude.json`, `loginctl enable-linger`).

- **Sender-outbox-first diagnostic playbook
  (`docs/diagnostic-playbook.md`, #65).** Captures the triage flow
  for when a chamber reports a missed bus message — three checks in
  order: (1) the SQLite store says whether the sender actually
  reached the bus, (2) the receiver's mailman journal says whether
  delivery was attempted, (3) the external system the message was
  *about* cross-checks the chamber's flow against reality. Surfaced
  by the 2026-06-03 incident where a "bus is broken" hypothesis was
  forwarded as recovered substrate before the DB was checked. README
  cross-links from the existing "Diagnosing a failed or unverified
  message" section. Operational-coordination-layer expression of the
  broader filed-bug-rootcause-is-hypothesis discipline.

- **ADR-0002: Chamber-state carry-forward spec for Binnacle's M6b
  (#74).** Names which parts of the cli-semaphore chamber-state
  primitive (#69) carry forward verbatim to Binnacle's M6b dashboard
  / operator API, and which are bridge-specific. Durable: the
  five-state vocabulary, the result schema (with `evidence` as an
  opaque blob), the `unknown`-as-advisory convention, and both
  per-agent + enumeration API primitives. Bridge-specific: the
  tmux-capture detection mechanism, the ~200ms temporal-delta
  latency floor, and the Evidence struct's inner field shape. Sub-
  issue (5) of the #69 parent tracker.

- **Discipline-pin: perf-skip composition for the asymmetric gate
  (#67).** PR #66's mutation-experiment table called out one un-pinned
  branch: removing the `!runFullGate` guard from the QuickPresenceProbe
  block (`cmd/claude-msg/serve.go:473`) would make both pre-checks run
  unconditionally when both were enabled, wasting ~50ms on the
  expensive probe path whenever the sentinel had already promoted.
  Perf regression, not a correctness break — but invisible to today's
  CI. `TestPin_OperatorInputRowGate_QuickProbeSkippedWhenSentinelPromotes`
  in `cmd/claude-msg/pin_test.go` closes the gap. Slug
  `OperatorInputRowGate` (already in ADR-0001's register — no
  amendment needed). The pin asserts probeCount == 2 (WaitForQuietPane's
  single iteration only); mutation experiment verified that dropping
  the guard yields probeCount = 4 (both pre-checks fire). See #67.

- **Cursor-position-aware ChamberState — v2 algorithm (#69 design
  call 2026-06-04).** PR #76's deploy smoke test surfaced that v1
  classified all idle chambers (with Claude Code's slash-command
  auto-suggestion ghost-text in their input row) as `unknown`. The
  operator's design call resolved the gap: distinguish "cursor at
  prompt sentinel" (auto-suggestion or empty prompt — both idle)
  from "cursor past prompt sentinel" (operator mid-typing —
  awaiting-operator).

  v2 adds a third tmux read-only call (`display-message -p -t pane
  '#{cursor_x}/#{cursor_y}'`) for cursor position. The substrate-class
  property updates from "2 capture-pane + 0 send-keys" to "2
  capture-pane + 1 display-message + 0 send-keys" — all three calls
  are read-only at the tmux layer; the "knock at the door without
  waking" framing is preserved. `TestChamberState_NoPaneMutation` is
  updated to assert the new shape.

  Algorithm precedence:
  1. Captures fail → `unknown` + wrapped error
  2. CompactionMarker matches → `at-rest-in-compaction`
  3. Captures differ across temporal delta → `working`
  4. **Cursor query + input-row classification** (new):
     - Cursor at sentinel position (col == sentinel width) →
       `idle` (clean prompt OR Claude Code auto-suggestion ghost-text)
     - Cursor past sentinel position → `awaiting-operator`
       (operator mid-typing)
  5. AwaitingOperatorMarker matches → `awaiting-operator` (backup
     for non-`❯`-painting UIs)
  6. Cursor-less fallback via `classifyInputRow` → `idle` if sentinel
     found empty; `unknown` otherwise
  7. Otherwise → `unknown` with accurate sub-case reason

  Failure paths gracefully degrade: cursor query failures fall back
  to the v1 cursor-less heuristic; the algorithm still classifies
  using the available substrate. The Unknown branch's evidence
  message is now accurate (split into "sentinel found but cursor
  not at input row" vs "sentinel not found at all" — the v1 message
  said "no prompt sentinel" even when the sentinel WAS in the pane,
  which was misleading).

  Five new tests pin the cursor-aware branches: clean-prompt-idle,
  auto-suggestion-idle (the operational-fixture from Pilot's
  `❯ /nimbus-board` smoke evidence), operator-mid-typing-awaiting-
  operator, cursor-less-fallback, and unknown-with-accurate-reason.

- **Chamber-state consumer surfaces (#72 + #73).** Both the operator
  CLI and the autonomous-agent MCP path now consume `tmuxio.ChamberState`
  via a single shared `resolveChamberState` helper so the JSON schema
  is byte-identical across surfaces — durable shape per
  `cli-semaphore#74`'s carry-forward spec.

  **CLI (#72)**: new `claude-msg state --agent NAME [--format text|json]`
  subcommand. Text format mirrors `claude-msg config show`'s
  AGENT/STATE/EVIDENCE/CAPTURED labeled-columns shape. JSON format
  emits `{agent, state, evidence, captured_at}` matching the MCP
  tool exactly. Non-zero exit on probe failure (agent-not-registered,
  no-pane, capture-pane error) so shell scripts can branch; the result
  is always emitted to stdout regardless, with `evidence.reason`
  describing what happened.

  **MCP (#73)**: new `semaphore.chamber_state` tool. Input
  `{agent: string}`, output `{agent, state, evidence, captured_at}`.
  Tool description names the substrate-class (read-only-observe), the
  five-state vocabulary, the advisory-not-authoritative convention for
  `unknown`, and the v1 detection coverage (idle/working/unknown
  reliable; at-rest-in-compaction + awaiting-operator land when #70
  populates the markers). Brings the MCP tool count to 10; the
  `TestMCP_ToolsListContract` pin is updated to include it.

  Both surfaces honor the safer-default-on-uncertainty contract:
  when `chamber_state` returns `unknown` or errors, the consumer
  surface still emits the structured result with the descriptive
  Reason field so the caller can decide. Bosun can call
  `semaphore.chamber_state` before dispatching to avoid waking an
  idle chamber unnecessarily, or to check whether a target is
  awaiting-operator before queueing a message that would chain into
  the popup-corruption case from #58/#59.

  Tests pin: CLI happy-path JSON + text, agent-not-registered error
  path, agent-no-pane error path, unknown-format validation guard,
  MCP happy-path schema match, MCP missing-agent validation.

- **`tmuxio.ChamberState` — read-only-observe chamber-state probe (#71).**
  Adds a "knock at the door without waking the inhabitant" primitive
  for `cli-semaphore#69`'s chamber-state visibility campaign. Returns
  one of five states — `unknown` / `idle` / `working` /
  `at-rest-in-compaction` / `awaiting-operator` — by inspecting two
  consecutive `capture-pane` snapshots taken 200ms apart.

  Substrate-class: read-only-observe. Exactly two capture-pane calls,
  zero send-keys, zero pane mutation. Pinned by
  `TestChamberState_NoPaneMutation`. Distinct from `QuickPresenceProbe`
  and `WaitForQuietPane` (write+observe via probe-and-watch); the new
  function deliberately avoids any pane disturbance so Bosun (or any
  caller) can poll chamber state without affecting the chamber's
  workflow.

  **v1 load-bearing branches** (Idle / Working / Unknown) are fully
  detected:
  - `working` when the two captures differ across the temporal-delta
    window (streaming output, spinner animations, any pane paint)
  - `idle` when the pane is stable AND the `❯ ` PromptSentinel is
    found with no content past it (reuses PR #66's
    `classifyInputRow` helper, newly extracted as a parse-only
    sibling of `InputRowHasContent`)
  - `unknown` when capture fails, when the pane is stable but no
    sentinel + no marker fires, or when context cancels mid-probe

  **TODO branches** (AtRestInCompaction / AwaitingOperator) are
  structurally wired but currently disabled — `CompactionMarker` and
  `AwaitingOperatorMarker` are empty-string placeholders until
  `cli-semaphore#70` lands the empirical capture of the
  compaction-in-progress + AskUserQuestion-popup UIs. Once #70 ships
  the marker strings + test fixtures, the constants populate in the
  same commit and both branches activate. Same
  Claude-Code-version-dependent forward-watch as `PromptSentinel`.

  **Optional (B) /proc-inspection hybrid** named in #69 is NOT
  implemented in v1 per the issue's "impl-time judgment" framing — if
  pane-capture alone proves insufficient empirically, the hybrid
  lands as a follow-up sub-issue.

  `Evidence` struct ships alongside `State` carrying the observation
  that led to the classification (always-populated `Reason` plus
  per-state fields like `PromptEmpty`, `ChangedLineCount`, `Marker`).
  Consumers — the CLI surface (#72) and the MCP tool (#73), both
  pending — wrap this struct verbatim in their response schema; the
  shape is durable per the Binnacle-carry-forward framing on #74.

  Tests cover: `State.String()` for all 5 values + out-of-range
  default, Idle classification, Working classification (with
  `ChangedLineCount` populated), Unknown classification, pane-required
  validation, the substrate-class no-pane-mutation property, and the
  context-cancellation-mid-temporal-delta contract.

### Fixed

- **TOML knobs `quick-presence-probe` + `prompt-sentinel-gate` now
  actually take effect.** Both knobs were documented in serve.go's
  flag help and ResolveBool calls but silently no-op'd because
  `config.Block`'s struct + `blockBoolField`'s switch had never been
  extended to know about them. Operators setting either field in
  `/etc/cli-semaphore/config.toml` got hardcoded-default behavior with
  no diagnostic. The gap shipped with PR #64 (`quick-presence-probe`,
  v0.3.0) and was inherited by PR #66 (`prompt-sentinel-gate`).

  Adds the missing fields to `Block` + `ResolvedView` + `Resolve()` +
  the `config show` text/JSON output + the `blockBoolField` switch.
  TOML resolution test pin
  (`TestLoadFrom_ParsesQuickPresenceProbeAndPromptSentinelGate`) and
  precedence-chain test pin
  (`TestResolveBool_PrecedenceChain_QuickPresenceProbeAndPromptSentinelGate`)
  catch any future field-vs-switch drift.

### Added

- **Prompt-sentinel gate — completes coverage for #63 (Part 2).**
  Adds `tmuxio.InputRowHasContent` — a read-only-observe variant of
  the asymmetric gate that inspects the receiver's input row via a
  single `capture-pane` call (no probe injection, no paint-wait, no
  pane disturbance). Detects the operator-draft-sitting-in-the-buffer
  case that QuickPresenceProbe structurally cannot catch (a sitting
  draft + two appended probes still look like a clean append to
  `analyzeDelta`'s strip-N machinery).

  New opt-in flag `--prompt-sentinel-gate` (also `prompt-sentinel-gate`
  TOML knob, default false). When `--quiet-disabled=true` (default)
  AND this flag is set, the mailman runs the read-only check before
  each delivery; if the input row shows the Claude Code prompt
  sentinel (`❯ `) followed by ANY non-whitespace content (operator's
  draft, an agent's chosen-text narration, a selection-menu echo),
  falls back to the full `WaitForQuietPane` gate. If the sentinel is
  missing entirely (Claude Code in a non-prompt state — mid-stream
  output, menu overlay, search dialog), also falls back per the
  safer-default contract.

  **Composable with `--quick-presence-probe`.** Sentinel runs FIRST
  (read-only, ~5ms); if sentinel says quiet, QuickPresenceProbe runs
  next (write+observe, ~50ms) to catch active-typing during the brief
  paint window between sentinel-check and delivery. Net cost on the
  fast path is ~5ms (sentinel only), ~55ms (both), or 0ms (both off)
  — identical to pre-#63 when neither flag is set.

  Tests cover the operator-draft case, agent-narration-in-input-area
  (worked example from the cli-semaphore#63 Part 2 design pass with
  Surveyor), no-sentinel-found safer-default, empty input row,
  pane-required validation, and the substrate-class property that
  read-only-observe makes exactly one tmux call with zero send-keys
  (the distinguishing property vs QuickPresenceProbe's
  write+observe). Existing default-off behaviour preserved.

  **Constant `tmuxio.PromptSentinel`** is the Claude-Code-version-
  dependent prompt prefix (U+276F + space). Forward-watch: re-verify
  during major Claude Code version updates; the prompt-sentinel
  tests would surface a paint-format change.

- **Asymmetric quick-presence probe — partial coverage for #63 (Part 1).**
  Adds `tmuxio.QuickPresenceProbe` — a one-shot variant of the existing
  probe-and-watch gate that completes in ~50ms instead of multi-second
  observe windows. New opt-in flag `--quick-presence-probe` (also
  `quick-presence-probe` TOML knob, default false) lights up an
  asymmetric pre-check: when BOTH `--quiet-disabled=true` (the default)
  AND `--quick-presence-probe=true`, the mailman runs the cheap probe
  before each delivery; on `DeltaInputActivity` it falls back to the
  full `WaitForQuietPane` gate; on `DeltaQuiet` it delivers
  immediately. The speed win of the default-off gate is preserved for
  the common idle-pane case while the safety of the full gate is
  restored when the operator is actively typing during the probe
  window.

  **Coverage caveat**: the probe detects *operator-typing-right-now*
  (operator keystrokes interleave with the probe inject). It does NOT
  yet detect *operator-drafts-sitting-in-the-buffer* (a passive
  non-typing operator whose unsent draft would be clobbered by a bus
  delivery's trailing Enter). The latter is the headline case from
  #63's reproduction and requires prompt-sentinel detection (capturing
  the input row and checking for content past the prompt marker).
  That's deferred to #63 Part 2; this Part 1 lands the function + the
  asymmetric gate scaffold + the opt-in flag so Part 2 can plug into
  an established surface rather than redesigning from scratch.

  Tests pin both branches (`TestQuickPresenceProbe_QuietWhenIdle`,
  `TestQuickPresenceProbe_DetectsActiveTyping`,
  `TestQuickPresenceProbe_PaneRequired`). Existing default-off
  behaviour is preserved by the opt-in gate — no behavioural change
  unless an operator explicitly sets the flag.

- **`/clear` whitelist entry + PeerEdges per-edge exception layer (#60).**
  Adds `clear` to `internal/control/control.go`'s `Allowed` map with
  `Self: false, Peer: false` (globally denied), then adds a third
  `PeerEdges` tier that lifts the denial narrowly for specific
  (sender, recipient) pairs.

  The first edge is **Bosun → Pilot** — when Pilot hits token
  exhaustion in a state where `/compact` can't recover, Bosun can
  send `/clear` as a rescue path (loses in-flight work but restores a
  usable session). Any other sender / recipient combination remains
  denied; the same goes for `clear` on self scope.

  **Surface changes**:
  - `control.Resolve(name, scope)` → `control.Resolve(name, scope, sender, recipient)`.
    Required signature change so the edge-rule can match on identities.
  - `control.NamesForScope(scope)` → `control.NamesForScope(scope, sender, recipient)`.
    Edge-allowed commands now appear in the listing when the caller is on a matching edge,
    so the `peer-invokable: [...]` error context stays accurate.
  - New `control.Edge` struct + `control.PeerEdges` map (keyed by command name).

  **Policy expansion noted**: the package doc previously cited
  `/clear another agent's history` as the canonical example of what
  the audit surface protects against. The new edge layer keeps that
  protection in place by default — only a hardcoded, reviewable
  exception flips the denial for a specific pair. New edges and new
  whitelist entries still require a code change.

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

- **Message-header clock is now local time (was UTC).** The rendered
  header timestamp at `internal/render/message.go:formatClock` calls
  `t.Local().Format(...)` instead of formatting UTC directly. The
  stored `CreatedAt` remains ISO 8601 UTC — only the operator-facing
  rendered presentation is local. Tests rewritten to compute the
  expected substring from input so they pass in any timezone (CI =
  UTC, alcatraz host = Europe/Berlin). Operator call 2026-06-01: the
  rendered header is operator-facing convenience and should be wall-
  clock-comparable + correlate with journalctl's local-time prefix.

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

[Unreleased]: https://git.frankenbit.de/frankenbit/tmux-msg/compare/v0.5.0...main
[0.5.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.5.0
[0.4.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.4.0
[0.2.1]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.2.1
[0.2.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.2.0
[0.1.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.1.0

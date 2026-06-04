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

[Unreleased]: https://git.frankenbit.de/frankenbit/cli-semaphore/compare/v0.2.1...main
[0.2.1]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.2.1
[0.2.0]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.2.0
[0.1.0]: https://git.frankenbit.de/frankenbit/cli-semaphore/releases/tag/v0.1.0

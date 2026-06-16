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

### Added

- **Cross-surface docs-coherence gate at release cuts (#495).** A new
  `docs/release-cut-checklist.md` + a compact checklist in the release-prep PR body
  (`release.yml`) + a §Release-cuts step enumerate the operator-facing surfaces a
  cut must keep coherent, grouped by **review-net**: in-repo docs (PR-caught),
  BookStack (API), `/srv/CLAUDE.md` (alcatraz-infra repo), and sister chamber
  `CLAUDE.md` (flag-don't-edit) — the off-PR-net surfaces that drifted during the
  rename. A salience mechanism (visible at cut time), not machine enforcement (an
  audit tool is a deferred follow-up). Codifies the surface-sweep Phase 4 (#440)
  did by hand.

## [0.18.1] — 2026-06-16

Substrate-hygiene fast-follow after v0.18.0's rename. Three small cleanups that
finish the rename's long tail — the control-macro names, the codex-config
migration, and a scope-sharpening source-read — plus an observability fix. The
through-line, again, is **substrate-empirical honesty**: across this campaign the
crew repeatedly caught and corrected its own framing as it built — scope reversals
once the simpler architecture surfaced, a "cross-process" qualifier added when the
comparison made the real differentiator clear, a logging gap pre-empted before it
bit. v0.18.1 is the small, honest tail of that.

Headlines:

- **Control-macro names follow the rename — `mcp-*-tmux-msg` → `mcp-*-tmux-tell`
  (#480).** The MCP restart / enable / disable macros now carry the canonical name;
  the old names keep working as deprecated aliases through v1.0 (each logs a
  `WARN deprecated_control_macro` and carries a `deprecated` field in the response).
  The last cosmetic identifier the v0.18.0 rename left behind.
- **Codex config is migrated in place on upgrade (#486).** A codex chamber with a
  pre-rename `[mcp_servers.tmux-msg]` in `~/.codex/config.toml` gets it rewritten to
  `tmux-tell` on the next `install.sh` — text-surgical (your `approval_mode` / `env`
  / `args` / comments stay byte-identical) and idempotent. It advances all three
  rename points, including the inner per-tool key whose `approval_mode` would
  otherwise silently de-link from the renamed tool — the kind of quiet
  operator-visible breakage worth closing explicitly.
- **Project scope sharpened — ADR-0014 source-reading (#442).** A comparative read
  of AutoGen / CrewAI / the Claude Agent SDK pinned the real differentiator:
  AutoGen-core is broker-flat too, but single-process — tmux-tell's **cross-process
  + TUI-paste** delivery is what's distinct. The "cross-process" qualifier sharpens
  the scope-fence's IS list (the ADR-0014 amendment itself is a follow-up).
- **Observability fix — chamber-mailman logs labeled in Loki (alcatraz-infra#46).**
  An Alloy systemd-unit regex gap since the v0.18.0 deploy left chamber-mailman logs
  unlabeled; fixed, so they land in Loki under the right chamber again.

Deferred: folding the cross-process qualifier into ADR-0014's text is an
operator-discretion follow-up; and the legacy-alias **hard-cut** for every
`tmux-msg` surface — these control macros included — still lands at v1.0 (#440
stays open until then).

The rename's tail is swept and the crew's own corrections are on the record — the
v1.0 baseline stays coherent.

### Changed

- **Control-macro identifiers renamed `mcp-{restart,disable,enable}-tmux-msg` →
  `…-tmux-tell`** to follow the substrate rename (#480). The pre-rename names keep
  working as **deprecated aliases through v1.0** (ADR-0008 §Discretion): an
  invocation `Canonicalize`s to the `…-tmux-tell` form, still triggers the same
  macro, carries a `deprecated` field in the control response, and logs a greppable
  `WARN deprecated_control_macro` to stderr (CLI + MCP surfaces). `refresh-all-mcps`
  now fires the canonical `mcp-restart-tmux-tell`, and the permission/whitelist
  registry advertises the new identifiers.

### Fixed

- **`codex-install` now migrates a pre-rename `[mcp_servers.tmux-msg]` section in
  `~/.codex/config.toml` → `tmux-tell`** instead of appending a second
  `[mcp_servers.tmux-tell]` and orphaning the stale one (#486, the codex-config
  half of the #478 substrate rename). The migration is text-surgical (only the
  lines naming the old server/binary change; `approval_mode`/`env`/`args`/comments
  preserved byte-identical) and idempotent. Within a migrating section it advances
  all three substrate-points — the section path, the inner per-tool key
  (`."tmux-msg.<tool>"`, whose `approval_mode` would otherwise silently de-link
  from the renamed tool), and a `command` naming the `tmux-msg-codex` binary. The
  dup case (both sections present) removes the orphaned `tmux-msg` section. A
  `NOTICE` names each rewrite; the migration table (`legacyMcpRenames`) is
  list-shaped for the next rename. A stale `tmux-msg-*` binary in an
  already-canonical section (no migration in scope) is WARNed, not rewritten.

## [0.18.0] — 2026-06-16

The rename release — **`tmux-msg` is now `tmux-tell`**. This completes the second
half of the naming arc (`cli-semaphore` → `tmux-msg` → `tmux-tell`): the binaries,
Go module, systemd units, environment variables, data/config paths, the MCP server
and its tools, the repository, and every operator-facing doc now use the canonical
`tmux-tell` name. **Nothing breaks on upgrade** — every legacy name keeps working
as a deprecated alias (with a one-time warning) through the v1.0 boundary. This is
the v1.0-anchor release: the name is settled, the project's scope is codified
(ADR-0014), and the through-line is substrate honesty — calling each surface what
it actually is, and saying plainly what the tool is and isn't.

Headlines:

- **`tmux-msg` → `tmux-tell`, end to end (#440).** Binaries `tmux-tell-claude` /
  `tmux-tell-codex`, the Go module + systemd unit templates, the Forgejo repo, the
  `$TMUX_TELL_DB` / `$TMUX_TELL_CONFIG` env vars, the `~/.local/share/tmux-tell` and
  `/etc/tmux-tell` paths, the `tmux-tell` MCP server + its `tmux-tell.*` tools, and
  the docs all carry the canonical name now (a four-phase substrate-then-surface
  arc).
- **Upgrades are seamless — legacy names work through v1.0.** The old `tmux-msg-*` /
  `claude-msg` binaries, the `$CLAUDE_MSG_DB` / `$CLAUDE_MSG_CONFIG` env vars, and
  the old `tmux-msg` data/config paths all keep working as deprecated aliases (each
  emits a `WARN` naming its successor) until the v1.0 hard-cut. Data + config paths
  are lazily auto-detected, so an un-migrated install just keeps running; migrate at
  your leisure with `mv ~/.local/share/tmux-msg ~/.local/share/tmux-tell` +
  `sudo mv /etc/tmux-msg /etc/tmux-tell`. See *Migrating from `tmux-msg`* in
  `docs/reference.md`.
- **Project scope codified — ADR-0014 (#441).** An operator-ratified scope-fence:
  what tmux-tell **is** (a peer-style TUI-paste bus with the observe-gate, SQLite
  persistence, the substrate-vs-adapter boundary, hook-context delivery, an MCP
  surface, host-local trust), what it **is not** (a generic broker, real-time
  streaming, multi-tenant, a web UI, end-to-end encryption, non-LLM consumers), and
  SSH-back-tunnel as the planned cross-host reach. "Out of scope per ADR-0014" is
  now the default answer to scope-creep — the burden of proof flips to the proposer.
- **A leaner, rename-clean docs surface (#440 Phase 4).** The operator-facing docs
  were swept to the canonical name (the name-churn surface dropped 428 → 222
  references, −48%) with a new *Migrating from `tmux-msg`* guide; the in-repo docs
  and the BookStack operator pages (Service Inventory, Release & Deploy) are in
  sync.

Deferred to v1.0 and beyond: the legacy-alias **hard-cut** (every `tmux-msg` /
`claude-msg` surface removed at v1.0 — #440 stays open until then); the
`mcp-*-tmux-msg` control-macro identifier rename (#480); and the public GitHub
mirror, which stays private until the v1.0 launch.

The name is settled, the scope is drawn, and the substrate says what it is — the
road to v1.0 starts from a coherent baseline.

### Documentation

- **Phase 4 docs-prose rebrand `tmux-msg` → `tmux-tell` (#440).** The
  operator-facing documentation is swept to the canonical name across ~15 files
  (README, `docs/reference.md`, the operator + agent manuals,
  why/observe-gate/security/diagnostic-playbook/operator-ux/…, CONTRIBUTING, the
  two `cmd/*/README.md`): bare project prose, binary-command examples, default
  paths, and repo URLs flip to `tmux-tell`. New `docs/reference.md` § *Migrating
  from `tmux-msg`* documents the env-var / path / binary-alias mapping + the `mv`
  recipes + the WARN names (`deprecated_env_var_used` / `legacy_data_path_in_use` /
  `deprecated_surface_used`) + the v1.0 hard-cut. The MCP server + tool-name doc
  refs are flipped to `tmux-tell` to match #481 (Phase 2.5's MCP rename), so a
  fresh-install operator reading the docs hits the live tool names; the
  `mcp-*-tmux-msg` control-macro identifiers are left as-is pending #480. ADRs stay
  frozen as point-in-time records (with a one-line historicity note added to
  `docs/adr/README.md`). The operator-facing complement to Phases 1–3's
  code/systemd/env/path substrate.

- **ADR-0014: tmux-tell scope — IS / IS NOT / SSH-back-tunnel (#441).** Codifies
  the project-scope-fence in operator-ratified form: an 8-item IS list (peer-style
  TUI-paste bus, observe-gate, SQLite persistence, substrate-vs-adapter boundary,
  hook-context delivery, MCP server surface, host-local trust, adapter-axis
  extensibility), a 6-item IS NOT list (generic broker, real-time streaming,
  multi-tenant, web UI, E2E encryption, non-LLM consumers), and SSH-back-tunnel
  as the planned-but-distinct cross-host reach mechanism that composes with
  host-locality rather than replicating the bus. **Decision-by-omission
  discipline lands with the ADR**: "X is out-of-scope per ADR-0014" becomes
  the default answer to "should we add X?" unless the proposer names a
  load-bearing reason X falls within the IS list. Sibling discipline to the
  issue-level scope-fence at the project level — burden of proof flips from
  rejecter to proposer.

### Changed

- **Phase 2 operator-surface rename `tmux-msg` → `tmux-tell` (#440).** Engineer's
  Phase 1 (#474) made the Go binaries self-name `tmux-tell-claude` /
  `tmux-tell-codex` and generate `tmux-tell-<adapter>-mailman@` unit names;
  this lands the matching operator-surface: systemd unit template FILES
  renamed to `init/tmux-tell-<adapter>-mailman@.service`; `install.sh`'s
  binary, unit-template, and `cmd/` references on the new name (with a
  Phase-2 migration block that stops + disables any active legacy
  `tmux-msg-<adapter>-mailman@<agent>.service` instances and re-enables
  the `tmux-tell-<adapter>-mailman@<agent>.service` equivalents during
  install — closing the dual-delivery hazard that would otherwise arise
  from two mailmen polling the same DB, sibling shape to #443 Obs1); and
  `deploy.yml`'s smoke + doctor invocations on the new binary names. The
  `docs/reference.md` operational command examples flip to the new names;
  bare project-prose ("tmux-msg is a substrate", title, MCP wire-surface
  tool names like `tmux-msg.send`) is held for Phase 4 (Herald). The
  Forgejo repo rename `frankenbit/tmux-msg` → `frankenbit/tmux-tell`
  follows this merge as a separate operator-surface step (Forgejo
  auto-308-redirects old URLs); `~/.claude.json` MCP server name update
  follows that. Phase 3 (Engineer — `CLAUDE_MSG_DB` env-var, DB path
  migration, DeprecatedAlias chain entries) and Phase 4 (Herald —
  README + bare-prose sweep + BookStack) remain on the roadmap.

### Fixed

- **`uninstall.sh` MCP-removal hint + path refs are rename-aware (#476).** The
  `claude mcp remove` hint now names `tmux-tell` with the legacy `tmux-msg`
  fallback, and the `--purge` datadir resolves whichever of
  `~/.local/share/{tmux-tell,tmux-msg}` actually exists — so uninstall stays
  correct on both pre- and post-rename chambers (Phase-4 fast-follow to #475's
  review).

## [0.17.2] — 2026-06-15

v0.17.2 closes loose ends from v0.17.1's rapid cycle. Four fixes:

- **`register` no longer silently fences queued messages on delivery_mode flip
  (#390).** If switching an agent from `hook-context` → `paste-and-enter` (or
  back) would leave `N > 0` queued messages stuck below the new mailman's
  backlog floor, `register` now errors and asks you to pick
  `--purge-stale-queue` (ack them) or `--keep-stale-queue` (leave them
  visible). `inbox` marks fenced rows `(backlog-fenced)` so you can see them;
  a new `backlog_fenced` JSON field carries the same signal for scripts. The
  substrate never auto-decides on your behalf — the disposition is your call.
- **`delivered_in_input_box` warning rate dropped 97.6% (#387).** The
  cursor-anchored input-emptied verify-signal that shipped in v0.16.1 (#369)
  reduced the warning count from 84/24h (pre-fix baseline) to 2/24h on
  alcatraz. The single-paste demote in v0.17.1 (#446) likely contributes too,
  but the residuals both predate it, so attribution stays cleanly with
  cursor-anchor as primary.
- **`release.yml` now recognizes `feat!:` / `fix!:` breaking-change shortcuts
  (#407).** Conventional Commits allows two forms — the `BREAKING CHANGE:`
  body footer (already handled) and the `<type>!:` title shortcut (was
  missed). A commit titled `feat!: foo` used to silently fall through to a
  patch bump; it now correctly maps to minor (pre-1.0; will be major
  post-1.0).
- **CHANGELOG entry convention codified in `CONTRIBUTING.md` (#454).** The
  style we've been using for entries — crisp headline + issue/PR links,
  detail belongs in the PR body, every release gets a narrative prelude +
  `Headlines` — is now documented contributor-discipline. The §Release cuts
  section also got modernized for the four-workflow auto-cut chain from
  v0.17.0.

Deploy notes: both adapter binaries (`tmux-msg-claude` and `tmux-msg-codex`)
now roll in one deploy cycle (#436).

### Documentation

- **Post-deploy rate verification: cursor-anchor verify-signal reduces
  `delivered_in_input_box` warnings 97.6% (#387).** Sampled alcatraz mailman
  journal 24h post-deploy (Jun 14 22:18 → Jun 15 23:05): **2 WARN events**
  against the pre-fix baseline of **84 events / 24h** (2026-06-12 probe). Both
  fixes contribute: #369's cursor-anchor input-emptied signal (deployed Jun 14)
  eliminated the dominant mid-turn false-negative class; #446's single-paste
  demote (v0.17.1, deployed Jun 15 22:00) addressed the standalone-Header-submit
  race. The 2 residual events occurred before the #446 deploy (targets: lookout,
  engineer; adapter: claude). Rate floor confirmed; issue closed as verified.

- **CHANGELOG entry convention codified + §Release cuts modernized (#454).** The
  density convention from #391's distillation — per-entry crisp headline + refs
  (detail in the PR body), the per-release narrative prelude + `Headlines:` (the
  `release-draft.yml` extraction surface, #427), and forward-living-comprehensive
  (#426) — is now contributor-discipline in CONTRIBUTING §How we work + a
  §Release-cuts step-2 prelude check; and §Release-cuts steps 7–8 are brought in
  line with #418's four-workflow auto-chain (no more manual `git tag && git push`).
  Part 3 of #391; the routing-principle ADR is tracked in #462.

### Changed

- **`register` requires an explicit disposition when a delivery_mode flip orphans
  queued messages (#390).** Flipping an existing agent's `delivery_mode` (e.g.
  `hook-context` → `paste-and-enter`) left its pre-flip queued messages silently
  fenced below the new mailman's backlog floor — `queue=2`, `mailman_running=true`,
  nothing delivered, reads as a bug; the operator had to find + run
  `inbox --ack-all` by hand (witnessed on the Lookout flip after the #360 deploy).
  Now a flip that would orphan `N > 0` messages **requires** one of two new
  `register` flags: `--purge-stale-queue` (ack them — they were emitted under the
  old delivery semantics) or `--keep-stale-queue` (leave them queued, visible as
  backlog-fenced). Without a flag the flip errors and names the count; a zero-orphan
  flip or a same-mode re-register (a chamber restart) proceeds untouched. `--force`
  stays orthogonal (it authorizes overwriting the registration, not a queue
  disposition). The substrate never unilaterally discards or re-routes
  operator-addressed messages — the operator-explicit flip is the signal, the
  disposition is the operator's call (auto-purge / auto-redeliver deliberately
  rejected). `inbox` now annotates fenced rows as `queued (backlog-fenced)` in text
  and carries a stable `backlog_fenced` boolean on every JSON row for programmatic
  consumers. See `docs/reference.md` § Delivery modes › Flipping delivery_mode.

### Fixed

- **`release.yml` recognizes `<type>!:` breaking-change title shortcut (#407).**
  Conventional Commits 1.0 allows two equivalent forms for breaking-change
  signaling: the `BREAKING CHANGE:` body footer and the `<type>!:` title
  shortcut. The parser already mapped the footer to a minor bump (pre-1.0
  suppression per the CHANGELOG cadence convention); a `feat!: …` titled
  commit **did not** match the existing `^feat(\(.*\))?:` regex (the `!` breaks
  the literal-colon match), so a range with only `<type>!:` titled commits and
  no other `feat:` commits would silently bump to **patch** instead of minor.
  Smoke-witnessed on a synthetic `feat!:` commit against `v0.17.1..HEAD`:
  old parser → `patch` + "no feat: commits since v0.17.1"; new parser →
  `minor` + "BREAKING CHANGE title shortcut (<type>!:) detected (pre-v1.0
  suppressed to minor)". The new check covers any lowercase `<type>!:` (feat,
  fix, chore, etc.) per the spec, sits between the `BREAKING CHANGE:` footer
  check and the `feat:` title check, and shares the pre-1.0-suppressed-to-minor
  mapping. Closes Surveyor's #406 non-blocking follow-up.

## [0.17.1] — 2026-06-15

Substrate-hygiene fast-follow after v0.17.0's cut-chain ship. The first
deploy-lane exposed codex-adapter coverage gaps and delivery-substrate residues
the cut-chain itself didn't carry; this cut closes them in-cluster — and,
fittingly, exercises `release-draft.yml`'s own empty-prelude honest-fail (#427)
as the recovery surface for its own draft.

Headlines:

- **Codex adapter substrate parity (#436 / #438 / #443 Obs1).** Deploy now rolls
  BOTH adapter binaries — a new `restart-mailmen` sub-primitive makes a freshly
  installed codex binary actually take effect on the running mailman (#436);
  install.sh's codex bootstrap branches on the agent's CURRENT `delivery_mode`
  instead of force-writing hook blocks (#438); and `doHookContext` reads
  `delivery_mode` to no-op when the toml is stale (#443 Obs1) — closing Lookout's
  witnessed double-arrival regardless of *why* the toml diverged.
- **Single-paste delivery — demote #336's framed paste (#446, supersedes #336,
  closes #389).** Every message now pastes as one buffer + Enter, like short
  messages always did; the separate-Header paste event — and the #389
  standalone-submit window it opened — is structurally gone, with regression
  coverage for codex multi-block collapsed-paste submit pinned alongside
  (#443 Obs2).
- **`release-draft.yml` empty-prelude honest-fail + `workflow_dispatch` recovery
  (#427).** Distinguishes section-not-found from section-has-no-prelude with a
  substrate-claim-naming error, and adds a retry trigger so the operator can
  re-run the draft after fixing the CHANGELOG without re-merging the release-prep
  PR — the recovery surface this very prelude was authored against.
- **`release.yml` PR-body template tells the real post-merge story (#425).** The
  release-prep PR body + the file-header comment drop the stale "deferred to
  v0.17.1 / operator handles manually" prose for #418's four-workflow auto-chain.
- **CHANGELOG discipline cleanup (#391).** v0.1.0–v0.15.1 distilled to the
  prelude + `Headlines:` + load-bearing-bullets exemplar (~217 KiB → ~130 KiB)
  with release-tag footnotes wired for all 20 versions; v0.16.0 / v0.16.1 /
  v0.17.0 left as the density exemplars + canonical-comprehensive surface
  per #426.

Plus the substrate-hygiene companions spun off while closing the cluster: the
truthful-toml hook-block rewrite-on-flip (a #443 Obs1 cosmetic follow-up),
MCP-env wiring for a fresh paste-served codex chamber (#453), and the
release-prep derive-script's backtick over-escaping (#459).

Deferred to v0.17.2 and beyond: #443's umbrella stays open for Observation 1's
codex-config delivery (#384 / #438 territory), the Part-3 CHANGELOG-convention
codification (#454), and the remaining open v0.17.1-milestone items (#450).

A fast-follow that hardens the codex adapter toward parity with claude and pays
down the cut-chain's own first-run residue — including, recursively, the draft
that ships it.

### Added

- **Regression coverage + characterization for codex multi-block collapsed-paste
  submit (Observation 2 of #443).** No behavior change — the #401
  settle-until-empty-input resubmit loop already handles N staged collapsed
  blocks correctly; this pins *why* via an operator-witnessed pane probe
  (2026-06-15) and two `TestPasteStillInInput` cases. The probe confirmed,
  dual-axis: (pane-side) codex stages all collapsed blocks on ONE logical input
  row after a SINGLE `› ` sentinel (`[Pasted Content N][Pasted Content N] #2…`),
  so the detector's bottom-most-sentinel scope (#402) sees every staged marker —
  no early false-negative on a multi-block composer; and (model-side, confirmed
  by the codex chamber) a single Enter on a ready composer submits the WHOLE
  frame in ONE model turn with no markers dropped. This is codex's two-phase
  *readiness* mechanism (Enter consumed until the composer settles), NOT
  placeholder-count == Enter-count — which is why the loop stops on the
  empty-input signal and never over-sends a blank follow-up turn (a fixed-N
  "count markers, send N Enters" approach would have fired extra Enters into the
  emptied/working composer). Closes Observation 2 of #443; the umbrella #443
  stays open for Observation 1 (codex-config delivery, #384/#438 territory).

### Changed

- **Single-paste delivery for all messages — demoted #336's header-first 3-part
  framed paste (#446, supersedes #336, closes #389).** A large message used to
  paste as three separate `tmux paste-buffer` events (Header / Body / Footer) so
  the short frame stayed inline-readable when a large Body collapsed in the
  recipient TUI. The whole message now pastes as a SINGLE buffer + Enter, like
  every short message always did. Operator's substrate-economy call: the framing
  added moving parts without proportional value (the message expands in the
  transcript on submit anyway, so the upfront-Header benefit is marginal; the
  Footer only repeated the header's id; message bounds are legible without it),
  and the separate-Header paste event opened the #389 standalone-submit window
  (the Header could submit on its own before the Body, forcing a manual
  re-retrieval). A decisive 2026-06-15 codex probe confirmed the framing's #336
  visibility benefit *does* survive on codex (collapse is size-triggered, so a
  short Header stays literal) — but the surviving benefit didn't outweigh the
  cost. **#389 is structurally closed**: with no separate Header paste event there
  is no standalone-submit window, so the trailing-newline escape candidate
  (carried from #430) is no longer needed. Unchanged: the #160 `· 2.3k` header
  byte-marker (only the *framing* trigger was coupled to the threshold — the
  length suffix still rides in the header), the #336 cursor-anchored
  input-emptied verify, and the #401 codex resubmit loop (a large single Body
  still collapses on codex and is handled there).

### Fixed

- **`release.yml` PR-body template describes the four-workflow chain (#425).** The
  release-prep PR body (and the matching file-header comment) still emitted the
  v0.17.0-MVP "post-merge tag/release/deploy deferred to v0.17.1, operator handles
  manually" prose; after #418 landed the chain in v0.17.0 it was stale on every cut
  (witnessed on #424). Now describes the auto-chain: merge → `release-draft.yml`
  draft → **Publish** → `release-publish.yml` → `deploy.yml`.

- **Codex bootstrap is paste-aware (#438).** install.sh's full `--adapter=codex`
  bootstrap unconditionally ran `codex-install`, whose register step force-set
  `delivery_mode=hook-context` — flipping a paste-served codex chamber (Lookout,
  paste-capable since #360) back to hook-context and writing the hook blocks that
  cause the #443 Obs1 duplicate delivery. The bootstrap now branches on the
  agent's CURRENT delivery_mode (read via `whoami`'s new `MODE` line): a
  hook-context chamber still gets `codex-install` (hook config + MCP env), while a
  paste-served chamber gets its systemd mailman enabled + restarted (the claude
  sibling-shape, #410) with its mode preserved and no hook blocks written.
  `enable-linger` now runs on the codex bootstrap path too (a paste-served codex
  mailman needs it to persist at boot, same as claude). `whoami` now reports
  `delivery_mode` (a text `MODE` line + JSON field). Existing delivery_mode is
  preserved across bootstrap (`discover`'s `UpsertAgent` only updates the pane).
  MCP-env wiring for a *fresh* paste-served codex chamber is tracked separately
  (#453).
- **Codex hook-context path no longer double-delivers for a paste-served agent
  (#443 Obs1).** When a codex agent's `delivery_mode` is flipped DB-side to
  `paste-and-enter` but its `~/.codex/config.toml` still carries the
  `UserPromptSubmit`/`SessionStart` hook blocks (stale from a prior hook-context
  mode — #438's bootstrap registers them unconditionally), both delivery paths
  fired for the same queued message: the hook claimed+presented it AND the
  mailman pasted it, double-arriving at the chamber surface (the bus DB shows one
  clean `delivered_at`, so the duplicate was invisible bus-side and visible only
  to the chamber — operator-witnessed on Lookout). `doHookContext` now reads the
  agent's `delivery_mode` and no-ops when it is not `hook-context`: the DB mode is
  the single source of truth and the toml hook block is demoted to a trigger that
  defers to it, so the mailman paste is the single delivery regardless of *why* the
  toml is stale (missed rewrite, manual edit, bootstrap staleness, restart drift).
  The skip is user-silent (the operator deliberately flipped the mode) but emits a
  greppable `WARN hook_context_skipped_paste_mode` so a stale toml is discoverable
  from the journal (same silent-to-chamber / observable-to-substrate shape as
  `WARN control_command_unsupported`, #419). Keeping the toml itself truthful on
  every flip (rewriting the hook blocks) is cosmetic once this guard lands and is
  tracked as a follow-up. Obs2 (multi-collapse Enter semantics) closed separately
  in #444.
- **Deploy chain now rolls BOTH adapter binaries, effectively (#436).** The
  release builds `tmux-msg-claude` and `tmux-msg-codex`, but `deploy.yml`
  hardcoded `--adapter=claude` — so every cut shipped the codex binary a version
  behind (caught at v0.17.0: claude → v0.17.0 while codex sat at the prior day's
  build, silently lagging codex-side fixes like #419). The deploy now adds a
  codex install step (`--adapter=codex --no-bootstrap` — no second
  refresh-all-mcps wave, no re-prune) and the post-deploy smoke asserts BOTH
  adapter versions so a future per-adapter lag is visible in the job log. Crucial
  half: a freshly-installed binary does NOT take effect on an already-running
  mailman (the daemon holds the replaced inode until restarted — the #393
  lesson), and `--no-bootstrap` skips bootstrap's per-agent restart. New
  `tmux-msg-<adapter> restart-mailmen` sub-primitive (enumerates the adapter's
  running mailman units and restarts each via #410's `restartMailman`; idempotent)
  closes that — install.sh's `--no-bootstrap` path now invokes it, so the codex
  binary is *effective*, not just present. Also drops the stale pre-#360
  install.sh comment claiming codex has no systemd mailmen. The codex bootstrap
  branch still registers hook-context (a separate staleness) — tracked in #438.
- **`release-draft.yml` honest-hard-fail on empty narrative prelude +
  `workflow_dispatch` recovery trigger (#427).** Witnessed during v0.17.0
  cut 2026-06-15: the auto-extractor returned the misleading "CHANGELOG
  section not found" error when the section was actually found but had no
  narrative prose before the first `### ` subsection. Distinct now:
  empty-prelude case emits a substrate-claim-naming error
  ("`## [vX.Y.Z]` section has no narrative prelude before the first
  `### ` subsection — release-draft.yml requires a narrative + headlines
  shape per the canonical-substrate-vs-curated-surface contract per
  #426") with pointer to v0.16.0 / v0.16.1 as the convention exemplars.
  Adds a `workflow_dispatch` trigger with a `tag` input so the operator
  can retry release-draft.yml after fixing the underlying issue, without
  re-merging the release-prep PR. Closes #427's forward-watch.

### Documentation

- **CHANGELOG discipline cleanup — brevity pass + version-header link wiring
  (#391).** Distilled the ancient release sections (v0.1.0–v0.15.1) to the exemplar
  shape (narrative prelude + `Headlines:` digest + load-bearing-facts-plus-link
  bullets), wired release-tag footnotes for all 20 release versions, and fixed the
  stale `[Unreleased]` compare-link (~217 KiB → ~130 KiB). v0.16.0 / v0.16.1 /
  v0.17.0 left untouched as the density exemplars + the canonical-comprehensive
  surface per #426. Forward-discipline codification (CONTRIBUTING convention +
  #284 pre-cut check) deferred to #454.

## [0.17.0] — 2026-06-14

The Release-engineering cluster: substrate-empirical first-deploy-lane
work that built the tmux-msg cut + deploy chain end-to-end and
hardened the substrate against the chamber-paste-substrate races that
the v0.16.0 + v0.16.1 codex-adapter work surfaced once Lookout's codex
chamber started carrying production traffic.

Headlines:

- **Four-workflow cut chain — `release-draft.yml` + `release-publish.yml`
  alongside the existing `release.yml` + `deploy.yml` (#418).** Forgejo's
  release UI becomes the canonical-publish-substrate: operator merges
  the release-prep PR, reviews the auto-created draft, clicks **Publish**;
  Forgejo creates the tag, fires `release: published`, and the deploy
  chain auto-fires. No CLI `git tag && git push` — the manual step
  evaporates at the tool-axis when the substrate routes through the
  canonical surface.
- **Phase 1 host-mode deploy runner (#392 / #393).** Dedicated
  `forgejo-runner-alcatraz-host.service` systemd unit + the
  `/srv/scripts/deploy-tmux-tell.sh` wrapper as a single NOPASSWD
  surface (the wrapper validates the runner workspace + the
  `install.sh` integrity before exec — narrowing the privileged-exec
  blast radius vs. authorizing `install.sh` directly). `deploy.yml`
  workflow_dispatch lands the binary + bootstraps in one operator
  trigger; `doctor` runs at the smoke step (soft-fail per #415, the
  first-deploy-lane substrate-empirical discipline pin's
  conservative-then-soften half).
- **Codex `/mcp` control-message session-break closed — `Profile.
  SupportsMCPSlashCommand` (#419).** A `refresh-all-mcps` cascade
  used to paste `/mcp disable tmux-msg` literally into Codex chambers'
  prompts (Codex CLI has no `/mcp` slash command), polluting the input
  + breaking the session. Per-adapter capability flag + sentinel +
  `strings.Fields` first-token matcher gate this cleanly; codex chambers
  log a structured WARN + mark the message delivered without pasting.
  Broader per-(command, adapter) compat tracked in #420 for the v0.18.x
  cycle.
- **First-deploy-lane substrate-empirical learning surfaces, closed
  in-cluster.** `bootstrap` now `enable` + `restart`s mailmen so deploy
  cycles them off the deleted pre-install inode (#410); `doctor`
  softens to advisory while #411 + #414 are open (#415); the
  release-draft body extractor lifts the narrative-prelude from
  CHANGELOG.md as the operator-facing curated surface, leaving per-PR
  detail canonical in CHANGELOG.md @ tag (#426); the release workflows
  swap to a `RELEASE_TOKEN` repo secret because Forgejo Actions' auto-
  token doesn't propagate declared `pull-requests: write` /
  release-create scopes (#422); `install.sh`'s top comment + the
  `--no-bootstrap` next-steps echo align with the post-#410 substrate
  truth (#429); BookStack page 193 "Release & Deploy Procedure"
  captures the full cut + deploy substrate for future operators (#428,
  via Herald).

Plus the substrate-hygiene companions: #407 (`feat!:` / `fix!:`
title-shortcut parser deferred from #394), #411 + #414 (codex
MCP-restart-path + paste-prompt-readiness gates, blocking restoration
of `doctor` hard-fail), #420 (broader per-(command, adapter)
slash-control compat surface), #423 (dedicated least-privilege
release-bot service-account to replace the master PAT in
`RELEASE_TOKEN`), #425 (release.yml PR-body template prose
cleanup), #427 (release-draft empty-narrative-prelude hard-fail,
fast-followed in this cut's [Unreleased] section).

**Deferred to v0.17.1 and beyond.** The framed-paste Header
standalone-submit race operator surfaced during the cut session
(#430 — backslash-escape on Header's trailing newlines) goes to
the next cut alongside the broader codex slash-control compat
work (#420) and the dedicated release-bot user (#423).

The Release-engineering milestone closes with v0.17.0.

### Changed

- **Codex is now `PasteCapable` — paste-and-enter delivery (#360).** The
  `tmux-msg-codex` binary flips `Profile.PasteCapable` `false → true`, so a Codex
  agent registered with the default delivery mode now receives messages pasted
  into its pane like Claude, instead of only via `hook-context`. This lands as
  the derivative of #336: #322 taught the observe-gate to read Codex's `› `
  sentinel + cursor (so it defers while a Codex operator is typing — the #323
  clobber premise is dissolved), and #336 replaced the collapse-fragile
  token-match verify with a cursor-anchored input-emptied signal that confirms
  Codex deliveries even when a >1KB paste collapses to `[Pasted Content]`. The
  serve-time paste-incapable guard is **kept and generalized** (#323 → #360): it
  no longer means "this adapter is Codex" but "this adapter's Profile signals it
  can't be observed for paste-safe delivery" — Codex now passes it; it remains
  the safe-default force-defer for any future paste-incapable adapter. Existing
  Codex agents already registered `hook-context` are unaffected (the mode is a
  per-agent column); `hook-context` stays available as the no-paste alternative.
  **Operator note:** Codex's post-submit *dual-`›` visual* (the submitted prompt
  lingers while a new empty input opens below + the cursor jumps down) is
  cosmetic — the message did submit; see docs/reference.md §Codex.
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
- **Delivery verification keys off an input-emptied signal, not token-match**
  (#336). A paste that submits leaves the recipient's live input row empty —
  Claude clears it in place, codex opens a fresh empty input block below.
  Emptiness is read from the **cursor position** (the cursor sits at the
  sentinel column when the input is empty and moves past it once content is
  present), reusing the cursor-aware discriminator `AgentState` already uses
  (#69) rather than a plain-text scan. Cursor-anchoring is what makes the
  signal robust to placeholder ghost-text: codex paints a dim example prompt
  into an empty composer (the "idle ghost-text" state) that `capture-pane -p`
  renders as literal text — a plain-text emptiness check misreads it as a
  populated input and false-negatives the verify (caught by the #336 live
  probe before merge; the cursor stays at the sentinel regardless of
  ghost-text). The signal is also robust to paste-collapse (codex renders a
  large paste as `[Pasted Content]` even after submit, masking the verify
  token) and honest about mid-turn delivery (a queued Enter leaves the paste
  buffered in the input row with the cursor past the sentinel → correctly
  reported not-yet-delivered), replacing the token-match signal that both
  false-negatived on collapse and false-positived on a pasted-but-unsubmitted
  message whose token was literally visible in the input box. Falls back to
  token-match when the input row can't be anchored (cursor query failed or
  the cursor isn't on a sentinel row — pre-#336 behavior preserved there).
  Targets the dominant `delivered_in_input_box` warning class (~84/24h,
  predominantly mid-turn).

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

- **Release automation: full UI-driven cut loop — `release-draft.yml` +
  `release-publish.yml`.** Closes the v0.17.0 cut automation loop end-to-end
  using Forgejo's release UI as the canonical publish substrate (operator
  reviews + clicks **Publish**; no CLI tag-push). Four-workflow chain:
  - **release.yml** (existing, #394): `workflow_dispatch` → creates
    `release-prep/vX.Y.Z` PR with mechanical CHANGELOG transition + version
    determination.
  - **release-draft.yml** (new): triggers on the release-prep PR merging to
    main; extracts the version from the head branch name + the matching
    CHANGELOG section, creates a **draft** Forgejo release pre-populated with
    that content + the merge-commit as target.
  - **release-publish.yml** (new): triggers on `release: types: [published]`
    when operator clicks Publish; chains `deploy.yml` via `workflow_call`
    passing the release tag.
  - **deploy.yml** (existing, #393): gains a `workflow_call` trigger
    alongside `workflow_dispatch` so release-publish can chain it. Same
    `ref` input on both paths.
  Operator's only manual step in the cut becomes: review the release-prep
  PR + merge it; review the auto-created draft release + click **Publish**.
  Forgejo creates the tag from the draft; deploy fires automatically.
  Originally deferred from #394 to v0.17.1 per #406's MVP shape; folded in
  as a fast-follow 2026-06-14 evening per operator call ("no time pressure
  for the cut; better test surface from running the full automation chain").
  Substantively the evaporation-test pattern firing at the tool-axis: the
  manual `git tag && git push` step evaporates when we route through
  Forgejo's UI publish (the canonical-substrate-surface) instead of
  parallel-tracking with CLI.
- **Plan-first workflow for size/M+ work — [ADR-0013](docs/adr/0013-plan-first-workflow.md).**
  Dispatcher signals plan-first on substantial work; chamber composes the plan
  with a stable metadata header at `/tmp/tmux-tell-plans/<issue-N>-<title>.md`
  (atomic create, fail-loud on existing); reviewers read filesystem-local +
  post verdicts via tmux-tell bus; **APPROVED plan is archived to the work
  issue before implementation starts** (closes the `/tmp` loss window); a
  completion comment lands at PR merge, supersession comments handle plan
  revisions mid-implementation. Default-on for size/M+; default-off for
  size/S and below; both sides may override with explicit announcement.
  Surfaces architectural disagreements before code, at the cheapest revision
  moment. Alcatraz-local for now (all chambers share `/tmp`); multi-machine
  development would migrate to a different substrate.
- **Release-prep workflow — #394.** `.forgejo/workflows/release.yml` adds
  a manual `workflow_dispatch` trigger (`bump_override` + `dry_run` inputs)
  that determines the next version from conventional commits since the
  last tag (`feat:` → minor, `BREAKING CHANGE:` → minor pre-v1.0, else →
  patch; override wins), mechanically transitions `## [Unreleased]` →
  `## [vX.Y.Z] — DATE`, and opens a release-prep PR via the Forgejo API.
  Post-merge tag + Forgejo-release + deploy.yml dispatch automation
  deferred to release-publish.yml (v0.17.1 follow-up); for this cut the
  operator handles tag/release/deploy manually after merging the
  release-prep PR.

- **CI-driven deploy workflow — #393.** `.forgejo/workflows/deploy.yml` adds
  a manual `workflow_dispatch` trigger that runs on a dedicated
  `alcatraz-host` host-mode Forgejo runner (paired with #392's
  alcatraz-infra-side wrapper + runner setup), invokes the deploy wrapper
  (`--adapter=claude --prune-orphans`), then hard-fails on `doctor`
  divergence per #348's exit-code contract. Concurrency-queued at the
  workflow layer + wrapper-side flock for direct invocations. Codex
  adapter stays operator-manual for MVP; first-deploy smoke gated on
  alcatraz-infra Phase 1 runner setup (binary install + visudo +
  registration + `systemctl enable`).

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
  enable` followed by `systemctl --user restart` per non-hook-context
  agent (the restart is needed because `enable --now` is a no-op on an
  already-active unit, leaving the deleted pre-install inode running
  post-deploy — see the #410 ### Fixed entry for the substrate-empirical
  witness), (5) orphan walk of
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
- **Header-first 3-part framed paste for large messages** (#336): a large
  message delivers as a Header / Body / Footer frame, each pasted as its own
  buffer, so the short `[Sender · … · id <id>]` / `[· id <id>]` bounds stay
  visible even when the body collapses in the recipient TUI. Small and quick
  messages are unchanged (single paste, no footer).
- **`-settle-delay` serve flag** plus per-agent `settle-delay` TOML knob
  (#360): operator-tunable pause between paste and the submit Enter, for an
  adapter whose TUI needs longer to ingest a (possibly collapsed) paste.
- **`ClearInput` clear-by-line-count** (#336, refined #360): the mailman's
  stranded-draft clear sends `clearPressesPerLine` (2) Ctrl+U per input line, so
  a multi-line draft on an adapter that clears line-by-line (codex) is fully
  cleared before the replacement paste. The press count was corrected from one
  to two per line under #360: codex clears a multi-line draft in ~2 presses per
  line (text-clear + line-join), so one-per-line under-cleared and left residual
  lines for the paste to compound with (P3, operator-substrate-witnessed in the
  #336 live-probe gate). Claude clears all on the first press, so its extra
  presses are harmless no-ops — adapter-agnostic over-clear, no per-adapter
  branch.

### Documentation

- **`install.sh` top comment + `--no-bootstrap` next-steps describe the
  `enable + restart` mailman shape (#413).** Stale `enable --now`
  references in the install.sh prelude + the `--no-bootstrap` operator
  hint predated #410's substrate change. `enable --now` is a no-op on an
  already-active unit and leaves the deleted-inode binary running
  post-deploy; the substrate-honest shape is `enable` followed by
  `restart`. Updated both surfaces to surface the substrate truth (#410
  reference inline) so future contributors don't re-teach the old
  conflation. Lookout flagged on PR #410 review (comment 67482).

### Fixed

- **`release-draft.yml` extracts the condensed narrative + headlines,
  not the full per-PR detail.** Witnessed during v0.17.0 cut + v0.16.x
  backfill 2026-06-15: a cluster release's CHANGELOG section runs ~25k
  chars (the per-PR detail under `### Added` / `### Changed` / `### Fixed`),
  which overwhelms the Forgejo release listing. The CHANGELOG's own
  convention is "narrative + Headlines bullets + Plus-the-companions +
  Deferred notes" at the top of each version section — that IS the
  operator-facing release-notes shape. The Python extractor now stops at
  the first `### ` subsection and appends a "Full per-PR detail" link to
  CHANGELOG.md at the tag for detail-readers. CHANGELOG.md stays the
  canonical comprehensive substrate; the release body becomes a curated
  surface that fits in one screen.
- **Release workflows use a repo-scoped `RELEASE_TOKEN` PAT for PR /
  release-draft creation.** First v0.17.0 cut attempt 2026-06-15 surfaced
  that Forgejo Actions' auto-issued `GITHUB_TOKEN` honors `contents:
  write` (the release-prep branch push works) but does NOT propagate
  `pull-requests: write` or release-create scope to the runtime token —
  `release.yml`'s PR-create POST and `release-draft.yml`'s draft-release
  POST both 403'd. Switched both to `${{ secrets.RELEASE_TOKEN }}`
  (the master/Bosun PAT stored as a repo secret). Substrate-honest
  reason: declared workflow permissions don't currently map to the
  runtime token's actual API scope on this Forgejo version; an
  explicit-secret path is the workable seam until that upstream gap is
  closed.
- **Codex `/mcp` control commands are skipped, not pasted as literal text
  (#419).** A `/mcp …` control delivery (e.g. the `/mcp disable tmux-msg` rows a
  `refresh-all-mcps` cascade fans out) to a codex agent has no matching slash
  command in the codex CLI, so it landed as literal text in the `›` prompt and
  broke the session — witnessed on Lookout during the first Phase-1 deploy. The
  mailman now skips delivering a `/mcp …` control command to an adapter whose
  `Profile.SupportsMCPSlashCommand` is false (codex): it marks the message
  delivered (consumed, not pasted) and logs a structured
  `WARN control_command_unsupported adapter=… agent=… id=… body=…`. Claude
  (which has `/mcp`) is unchanged. This is the narrow Option-A fix for the
  active breakage; the broader per-(command, adapter) compat surface (codex's
  mixed `/cost` / `/compact` / `/rename` / `/clear` support) is tracked in #420.
- **`bootstrap` restarts mailmen so deployed binaries actually take
  effect.** First-deploy-lane smoke on alcatraz 2026-06-14 (#393's
  deploy.yml against a live cluster) surfaced the substrate gap:
  `install.sh` rebuilds + replaces the `tmux-msg-claude` binary, but
  bootstrap's step-4 `systemctl --user enable --now` is a **no-op on
  an already-active mailman** — so the mailman process kept running
  the now-deleted-inode pre-install binary indefinitely, and `doctor`
  correctly flagged DIVERGENCE on every deploy. Step 4 now runs
  `systemctl --user enable` (without --now) followed by
  `systemctl --user restart` per non-hook-context mailman, which
  unconditionally cycles the process so it picks up the canonical
  on-disk binary. The new `restartMailman` helper in
  internal/cli/systemctl.go mirrors `startMailman` / `stopMailman`
  shape + carries the substrate gap rationale inline. Doctor-clean
  post-deploy state is now structural, not coincidental.

- **`deploy.yml` + `release.yml` aligned with Forgejo Actions schema
  validator (#393 / #394 fast-follow).** First-dispatch smoke surfaced
  4-class schema rejections: `gitea.*` → `github.*` (Forgejo aliases the
  namespace), `timeout-minutes` removed (the runner's instance-level 3h
  job-timeout is now the upper bound — substantive **weakening** from
  the workflow's previous 10-minute cap; deploys exceeding ~10min should
  be flagged for follow-up since they exceed the original design intent),
  `run-name` removed (cosmetic; tripped validator), top-level
  `concurrency:` removed (wrapper-side `flock --nonblock` provides
  fail-fast concurrency gate at the substrate-honest level; release-prep
  branch-already-exists API failure provides natural gate for release.yml).
  Schema-compatibility notes added to both file headers naming the
  constraint for future contributors.

- **Codex paste-and-enter no longer silently drops large messages (#401).**
  A codex paste that collapses to `[Pasted Content N chars]` needs a SECOND
  Enter to submit: the first Enter expands the collapsed block, the second
  submits it (a `tmux paste-buffer` quirk — operator-witnessed + Engineer-tested
  on Lookout). The mailman sent only one Enter, so >~1KB codex messages sat
  unsubmitted in the input — and the #336 cursor-anchor verify *false-positived*
  on them (codex parks the cursor on an empty sub-line while the `[Pasted
  Content]` lingers above), so the bus logged the drops as `delivered`. Both are
  fixed via a codex-supplied `PaneProfile.PasteCollapseMarker` (`[Pasted
  Content`; Claude's is empty → unchanged): while the marker is in the LIVE input
  the paste is definitively not-submitted (overrides the cursor-anchor), and the
  mailman re-sends Enter while it persists (Enter-on-empty is a safe no-op, so a
  resubmit racing an already-submitted paste is harmless) — bounded by the
  existing verify-retry budget. Real-path validated: a 2.5KB collapsed paste
  delivers in ~1.3s. Claude delivery is byte-unchanged. Supersedes #401's
  original settle-default framing — the failure is settle-independent.

### Documentation

- **BookStack: release & deploy procedure page — #428.** New
  [Release & Deploy Procedure](https://docs.saratow.net/books/tmux-tell/page/release-deploy-procedure)
  page in the *tmux-tell* book documents the post-v0.17.0 cut-and-ship path: the
  four-workflow chain (`release.yml` → release-prep PR → `release-draft.yml` →
  draft release → operator **Publish** → `release-publish.yml` → `deploy.yml`),
  the two operator touchpoints, the Phase 1 host-mode runner
  (`forgejo-runner-alcatraz-host.service`, `runs-on: alcatraz-host`), the
  `deploy-tmux-tell.sh` wrapper as the NOPASSWD security boundary, `RELEASE_TOKEN`
  (master-PAT stopgap, #423 tracks the least-privilege follow-up), the `doctor`
  soft-fail (#411/#414), and the canonical-substrate-surface routing discipline
  (#418/#426) — release UI = publish, `CHANGELOG.md`@tag = comprehensive, release
  body = curated narrative. Cross-links to the alcatraz-infra Service Inventory
  (host-mode runner unit family). Doc-only; no code or wire change.

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

Bugfix release: feature-frozen on v0.15.0 to land five substrate-hardening fixes
from the 2026-06-10 alcatraz tmux-outage forensics (#287 / #291 / #293 / #296 /
#298), plus the durable record for the tool-name decision (ADR-0010 accepting
`tmux-tell`). The v0.16.0 hardening cluster continues against this baseline; all
four cleared-for-removal deprecation surfaces extend through v0.16.0 per ADR-0008
§Discretion.

### Docs

- **ADR-0010 (Accepted): tool name is `tmux-tell` (#294).** Durable record of the
  2026-06-10 blind-vote: Pilot's two-phase blind candidate-collection (8
  participants) produced `tmux-post` (4) / `tmux-note` (3) / `tmux-tell` (1); the
  operator disposed `tmux-tell` on adapter grammar (`tmux-tell-claude` reads "tmux,
  tell Claude…"), product-name framing, and `/tell` MUD/IRC heritage. The rename
  arc files as a v0.17.0 candidate; this ships only the disposition. Retires the
  closed-PR-#218 "fall in love" bar for a five-axis one.

### Fixed

- **Mailman no longer storms tmux on a persistent `can't find pane` failure
  (#291).** A stale/wrong-server pane registration drove the pre-paste safety-abort
  into a ~100-probe/sec loop that wedged the tmux server (2026-06-10 incident).
  Consecutive `can't find pane` failures now back off exponentially (1s → 60s cap),
  and after `stuck-threshold` (default 10) the mailman parks
  (`agents.stuck_reason='pane-not-found'`, new STUCK column) and stops probing;
  queued messages retained, clears on `register --force`. New `stuck-threshold` /
  `stuck-poll-interval` knobs.
- **Sender-backlog cap now scoped per-(sender, recipient) (#296).** The cap
  (default 2) counted a sender's queued messages globally, so 2 undrained messages
  to one slow recipient blocked the sender's outbound to *every* other recipient
  (2026-06-10: a coordination broadcast dropped this way). Now counts only the
  `(from, to)` pair — a per-sender fairness slice of one mailbox's 5 slots, not a
  global ceiling; `ErrSenderBacklogFull` names both ends.
- **`tmux-msg.register` MCP auto-clears the #224 attention signal, matching the CLI
  (#298).** An MCP re-register left a stale `awaiting_operator` signal on the
  operator's queue (the CLI path cleared it, MCP didn't); the handler now calls
  `SetAttentionState(idle)` best-effort alongside the #297 stuck-state clear.
- **`register --start-mailman=true` refuses a non-default `CLAUDE_MSG_DB` (#293).**
  A systemd mailman launches from the unit's `Environment=`, not the caller's env,
  so a sandbox-DB caller would misroute (agent row in sandbox, mailman polling
  production). The CLI now errors with the `serve --agent` foreground recovery path;
  MCP registers but reports `mailman: skipped` + `mailman_error`.
- **`scripts/record-asciinema-demo.sh` no longer shares the operator's tmux server
  (#287).** Runs the demo on a private server via `$TMUX_TMPDIR` so a
  recording-side tmux crash can't take the operator's session (the
  alcatraz-infra#31 outage class); `TMUX_TMPDIR` over `-L` because the
  mailman/discover shell out to plain `tmux` (#288).

## [0.15.0] — 2026-06-10

Demo-tooling + reply-ergonomics release: a scripted asciinema take driver, an
interactive inbox reply action, a lightweight reply-intent flag, and the CLI
refactor that prepares the second (`tmux-msg-codex`) adapter.

### Added

- **Scripted asciinema demo driver — `scripts/record-asciinema-demo.sh` (#273).**
  Turns the capture recipe into an unattended record-and-cleanup script producing
  `docs/asciinema/observe-gate.cast`; sandbox-isolated (separate DB + tmux session),
  drives a real `claude` pane character-by-character with a mid-typing bus send,
  stop-framed just after the message lands. Idempotent via `trap`.
- **`inbox --watch` reply action (`r` key) (#268).** Opens `$EDITOR` on a templated
  buffer; the saved body sends threaded under the selected message, caps enforced
  in-transaction. Git-style scissors marker so a reply line starting `#NNN` isn't
  comment-stripped. The `D` mark-failed action was deliberately not built (no
  `queued → failed` path; decision-record in #268).
- **Lightweight reply-intent flag (#270).** `send --expects-reply` stamps an
  `expects_reply` marker; `inbox --unanswered` (recipient) and `sent
  --awaiting-reply` (sender) filter on it — a non-blocking alternative to
  `ask`/`wait_for_reply`. Wired as MCP params too; `Message.ExpectsReply` populated
  on all read paths.

### Changed

- **CLI refactor: shared subcommand wiring hoisted to `internal/cli` behind an
  adapter `Profile` (#248 PR1).** Behavior-preserving; prepares the `tmux-msg-codex`
  adapter (#248 PR2). Same names/flags/exit codes; a sibling adapter opts into a
  curated subset via its `Profile`.
- **CONTRIBUTING: claim-on-pickup made explicit for issues + PRs.** Surfaces the
  `docs/chamber-dispatch.md` convention for non-chamber contributors: set
  `assignees` before opening the branch, mirror on the PR; forward-only.
- **CONTRIBUTING §Release cuts: pre-cut fast-forward of the shared alcatraz checkout
  (closes #284).** Step 0 `cd /srv/tmux-msg/ && git pull --ff-only` —
  `/srv/tmux-msg/` lags `origin/main` until fast-forwarded (surfaced by a #282
  staleness false-alarm).

### Fixed

- **`scripts/record-asciinema-demo.sh` — 6 defects (#273 fast-follow).**
  Trust-prompt dismissal before the take, forced `--cols 120 --rows 30` pty size,
  `--overwrite` for re-runs, an attach-poll before keystrokes, an alice-side visible
  send, and a delivery-poll stop-frame (no pre-paste cutoff, no post-recording
  keystroke leak).
- **`inbox --watch` no longer multiplies its poll timer (#268).** The watch loop
  re-armed the tick on every poll result, compounding the poll rate; the tick is now
  the sole rescheduler.

## [0.14.0] — 2026-06-09

Delivery-modes + request-reply release: `hook-context` becomes a third delivery
mode (messages injected as `additionalContext` via a Claude hook, no paste), a
synchronous `ask` / `wait_for_reply` / `check_replies` Q&A surface lands on the
reply-to chain, and an interactive `inbox --watch` TUI drains mailbox-only queues.
Plus a Godog E2E scenario layer and the observe-gate demo recipe.

Headlines:

- **Hook-context delivery mode (#249, ADR-0009).** A third `delivery-mode`: a
  hook-context agent pulls pending messages and injects them as `additionalContext`
  via a SessionStart/UserPromptSubmit hook instead of a paste. ADR-0009 reframes the
  #169 invariant from "delivered = pasted" to "delivered = presented" (paste OR
  inject); hook delivery is adapter-side (sets up #248).
- **Request-reply — `ask` / `wait_for_reply` / `check_replies` (#250).** A
  synchronous-Q&A surface on the reply-to chain: `ask` marks the row `expects_reply`
  + returns an `ask_id`, `wait-for-reply` blocks (poll-backed — sqlite `update_hook`
  can't bridge processes), `check-replies` polls. Same three as MCP tools.
- **`inbox --watch` interactive TUI (#149).** A full-screen live drain for
  mailbox-only queues: navigate + `space`-ack + `enter`-expand, rowid-polling
  refresh (the #148 cross-process lesson); bubbletea, unit-tested without a TTY.

### Added

- **Hook-context delivery mode (#249, ADR-0009).** A third `delivery-mode` alongside
  `paste-and-enter`/`mailbox-only`: the mailman short-circuits (no paste) and an
  adapter-side `tmux-msg-claude hook-context` subcommand claims pending messages
  (honoring the #204 floor + #227 deferred staging), renders them, marks them
  delivered, and emits Claude hook JSON (no-op when nothing pending, safe to wire
  unconditionally). ADR-0009: the substrate stays delivery-method-agnostic, the #169
  invariant becomes "delivered = presented"; Claude-only in v1 (Codex/Gemini ride
  #248).
- **Request-reply — `ask` / `wait_for_reply` / `check_replies` (#250).** `ask --to
  <agent>` marks `expects_reply` + returns an `ask_id`; `wait-for-reply <ask_id>
  [--timeout]` blocks for a `reply_to=ask_id` reply (`{reply, timed_out}`),
  `check-replies` is the non-blocking poll; same three as MCP tools. New
  `expects_reply` column + `ListReplies`/`FindReply`/`WaitForReply` seams. Operator
  calls: no auto-ack on consume; an unverified reply returns with an `unverified`
  flag, not discarded. Single-recipient in v1.
- **`inbox --watch` — interactive TUI for mailbox-only agents (#149).** A
  full-screen live drain: `↑`/`↓` navigate, `space` acks (`queued → acknowledged`,
  composing with #221), `enter` expands, `q`/`Esc` exit with a summary; rowid-polling
  refresh (default 2s — `update_hook` can't see cross-process writes, #148).
  Bubbletea; `Model`/`Update` unit-tested without a TTY; rejects `--format
  json`/`--ack`.
- **Godog / gherkin E2E scenario layer (#264).** Six substrate-boundary scenarios in
  `features/*.feature` (observe-gate delivery, paste-safety, dedupe, operator
  routing, deferred delivery, attention-signal) with step defs exercising the store
  state machine directly (no real tmux), green in CI. `godog` promoted to a direct
  dev dependency.
- **`docs/asciinema-capture.md` — observe-gate demo recipe (#216).** A reproducible
  recipe for capturing the motion-dependent differentiator (a message holds while
  you type, lands when you pause) as an asciinema cast (sandbox socket + DB,
  typist-equals-recipient sequence, editorial choices). The live `.cast` is the
  operator-coordinated follow-up.

### Fixed

- **`TestPin_HealthScanLatencyCeiling_Under100ms` no longer flakes under `-race`
  (#254).** Path (c) triage: the <100ms commitment holds (~10ms on alcatraz, ~160ms
  under the race detector's 16× overhead); the pin skips the wall-clock check under
  `-race` (scan still runs) and stays enforced on every non-race CI run. ADR-0001
  amended with the diagnosis + a slow-scan counter-test.

## [0.13.0] — 2026-06-08

Operator-loop + delivery-correctness release: `send --to operator` routes by
observed operator presence, chambers can raise an attention flag, messages can be
**staged** for post-compaction self-handoff (`deliver_after`/`flush_deferred`),
recipient-side dedupe closes the replay-ambiguity loop, and a retention sweep ages
out old rows.

Headlines:

- **Operator-presence routing — `send --to operator` (#228).** The reserved
  `operator` recipient resolves at send-time to the operator's currently /
  most-recently attached chamber (via `tmux list-clients` + a last-seen presence
  slot); fails loud when never observed. Composes with the #224 attention signal.
- **Deferred delivery — `deliver_after` / `flush_deferred` (#227).** A message can
  be **staged** (new `deferred` state, invisible to inbox/mailman) until the
  chamber flushes the trigger — typically post-`/compact`, so self-handoff
  orientation lands in the resumed context instead of the summarizer. Bypasses the
  #204 claim-floor so a register between defer and flush can't skip it.
- **Chamber → operator attention signal (#224).** Per-agent `attention_state`
  (`idle`/`busy`/`awaiting_operator`); `flag_operator(body)` posts to a reserved
  `operator-attention` mailbox + flips the state, `clear_operator_flag` (or the
  next `register`) clears it; `agents` gains an ATTENTION column.
- **Recipient-side delivery dedupe — `dedupe-window` (#157 PR2).** The mailman
  re-verifies a prior `delivered_in_input_box` replay against pane scrollback
  within the window (default 60s): absorbs the duplicate (`verified=1` on the
  original, `dedupe_absorbed` on the dup, `dedupe_notice` to sender) or delivers
  it. Closes the loop `resend` (PR1) opens.

### Added

- **Recipient-side delivery dedupe — `dedupe-window` TOML knob (#157 PR2).** Before
  delivery, the mailman checks for a same-sender same-body
  `delivered_in_input_box` row within `dedupe-window` (default `60s`, `0s`
  disables) and re-verifies the original's token against scrollback: if visible,
  absorb the replay (original → `verified=1`, duplicate → `failed dedupe_absorbed`,
  `dedupe_notice` to sender); else deliver. Single-writer invariant preserved;
  composes with `resend` (#157 PR1).
- **Operator-presence routing — `send --to operator` (#228).** The reserved
  `operator` string resolves at send-time: (1) `tmux list-clients` matches an
  attached client's active pane to a registered chamber, (2) fallback to a
  single-slot `operator.last_seen_in` presence record (updated on every step-1
  hit). New `presence` table + `tmuxio.ActiveClientPanes` helper; substitution is
  per-entry (`to:["alice","operator"]`); fails loud when never observed +
  slot-unset. Additive, K-preserving (ADR-0008 Amendment A).
- **Deferred delivery — `deliver_after` / `flush_deferred` (#227).** `send
  --deliver-after=resume` stages a message in a new `deferred` state (invisible to
  inbox/ClaimNext/mailman) until `flush --trigger=resume`; `sent --deferred` lists
  staged. A promoted row bypasses the #204 claim-floor so a register between defer
  and flush can't skip the handoff; single-recipient, queue-cap-exempt. v1 accepts
  the `resume` trigger only (register-promotion / scheduling / OR-composition →
  #258).
- **Configurable retention — `retention` TOML knob + background sweep (#245, #150
  PR2).** A periodic mailman goroutine deletes `delivered`/`failed` rows older than
  the window (default `infinite` = no change); `retention-sweep-interval` default
  `1h`. Touches only the serving agent's terminal rows (single-writer); in-flight
  untouched. Composes with the `reset --older-than` one-off (#150 PR1).
- **Chamber → operator attention signal (#224).** Per-agent `attention_state`
  (`idle` / `busy` (reserved) / `awaiting_operator`); MCP
  `flag_operator(body)`/`clear_operator_flag` + CLI equivalents. `flag_operator`
  posts to the reserved mailbox-only `operator-attention` recipient AND flips the
  state (best-effort — `state_error` field if the flip fails after the post);
  clears on the next `register` or explicitly. `agents` gains an ATTENTION column;
  fails loud (#152) if `operator-attention` is unregistered. Additive,
  K-preserving.

### Changed

- **Consumer surfaces now read the durable `verified` column (#230).** `sent`,
  `inbox`, `track`, `get`, `thread` (marks `⚠`), and the MCP
  `message_status`/`inbox` tools render a delivered-but-unverified message as
  `delivered_in_input_box`; `stats` prints a `Delivered split: verified /
  in-input-box / pre-marker` line and `status --today` sources verified counts from
  the column (failed/crash/cap stay journal-sourced). Pre-#169 rows (`NULL`) report
  as a distinct pre-marker count, never guessed. New `VerificationCountsByAgent`
  seam. (Wires up the #169 column; #213 doc reconcile.)
- **`docs/why.md` §See also — `agents-connector` (#251).** A substrate-honest
  pointer to the sibling Rust/MIT project solving local-inter-agent-tmux-messaging
  from the cross-vendor-via-hooks angle; names the convergence + divergence, no
  feature-matrix.

### Deprecated

- **`resend --force` against a `delivered_in_input_box` message — no longer needed
  (#230, earliest removal v0.15.0).** The `verified` column lets `resend` recognize
  a delivered-but-unverified message directly, so replaying it is the sanctioned
  recovery; `--force` there now emits `WARN deprecated_surface_used
  name=resend_force_unverified removal=v0.15.0`. `--force` stays required for a
  confirmed (`verified=1`) or pre-marker (`NULL`) delivery. ADR-0008's third
  deprecation cycle (after #177 + #140).

## [0.12.0] — 2026-06-08

Observability + retention release: the mailman exposes a Prometheus `/metrics`
endpoint, operators can prune audit history (`reset --older-than`) and drain the
announce-skipped backlog (`inbox --ack`), and the verify-token retry budget
becomes tunable.

Headlines:

- **Prometheus metrics on the mailman (#146).** `serve --metrics-addr :PORT`
  exposes six `tmux_msg_`-prefixed metrics (talk-pair heatmap, delivery +
  verify-attempt latency histograms, queue depth, loop iterations, paste-unsafe
  aborts); off by default. Alloy scrape + Grafana dashboard land in alcatraz-infra.
- **`inbox --ack` / `--ack-all` — backlog drain (#221).** Clear the residue #204's
  don't-flood policy leaves `queued` after a restart; new terminal state
  `acknowledged` (audit-preserving, retrievable via `get`).
- **`reset --older-than` — audit-history prune (#150 PR1).** Time-bounded delete of
  `delivered`/`failed` rows (in-flight untouched); composes with `--agent` +
  `--state`.

### Added

- **`inbox --ack` / `--ack-all` — announce-skipped backlog drain (#221).** `--ack
  <id>` (idempotent) and `--ack-all` (all rows ≤ the per-agent `backlog_epoch_id`,
  leaving newer arrivals) clear the residue #204 leaves `queued`; MCP
  `tmux-msg.inbox` gains `ack_ids[]` / `ack_all`. New terminal state `acknowledged`
  (never pasted, so not `delivered`; excluded from the default `queued` view,
  retrievable via `get`). Store `MarkAcknowledged`/`…Batch`, auth-scoped to the
  caller.
- **Prometheus metrics on the mailman (#146, PR1 of the observability stack).**
  `serve --metrics-addr :PORT` (or `metrics-addr` knob) exposes `/metrics`, off by
  default. Six `tmux_msg_`-prefixed metrics: `messages_total{from,to,state}`,
  `delivery_latency_seconds`, `delivery_verify_attempt_seconds` (shared with #153's
  budget calibration), `queue_depth`, `mailman_loop_iterations_total`,
  `paste_unsafe_aborts_total`. New nil-safe `internal/metrics`; the paste path stays
  metrics-agnostic via an `OnVerify` callback. Alloy scrape + Grafana JSON
  (PR2/PR3) land in alcatraz-infra.
- **`reset --older-than` — time-bounded audit-history prune (#150 PR1).** Deletes
  `delivered`/`failed` older than the window, leaving in-flight; composes with
  `--agent` + `--state` (AND); mutually exclusive with `--hard`. Store
  `DeleteMessagesBefore(toAgent, cutoff, states)`.
- **Configurable verify-token retry budget (#153).** `verify-retry-budget`
  per-agent TOML knob + `--verify-retry-budget` flag; default `5s` reproduces the
  current 7-attempt schedule, any duration scales it proportionally
  (`DeriveRetrySchedule` / `SetRetrySchedule`). A 2026-06-08 SPIKE found zero
  `verified=0` events post-#169 — the knob ships as a safety valve for
  large-payload hubs. Monitor via #146's verify-attempt histogram.

### Changed

- **`docs/why.md` — answer the two pitch-gap questions (#234).** Adds a "But why
  not just…?" section: *…raw `tmux send-keys`?* (observe-gate, single-writer,
  delivery-state durability, name-not-pane addressing) and *…a single session with
  subagents?* (persistent specialist context, real parallelism, token economics,
  role discipline) — each concedes the case where you don't need tmux-msg first.

### Deprecated

- **`delivered_unverified` family aliases — earliest removal extended v0.12.0 →
  v1.0 (#140 extension, ADR-0008 §Discretion).** Per the operator's 2026-06-08
  decision, the rename-arc alias machinery (CLI flag + TOML key + `--state` value +
  JSON shadows) holds through the v1.0 boundary (same rationale as #177's v0.11.0
  extension — cheap shim, maximize migration comfort). WARN logs now emit
  `removal=v1.0`; the K-counter stays preserved (Reading B), v0.12.0 increments K
  to 6.

### Fixed

- **`install.sh` alias-horizon strings (#237).** Five `(removed v0.11.0)` strings
  contradicted the binary's `removal=v1.0` WARN; updated to the v1.0-boundary
  wording per ADR-0008 §Discretion.

## [0.11.0] — 2026-06-08

Release-cut-discipline + public-launch-docs release: a deprecation-eligibility
derive-script + a machine-parseable `### Deprecated` format + a release-cut runbook
operationalize ADR-0008, and the README splits into a lean landing page + a full
operator manual for the GitHub launch.

Headlines:

- **Deprecation tooling (#209).** `scripts/deprecations.sh --for v<X.Y.Z>` walks
  the CHANGELOG to confirm the cleared-for-removal list at cut time (ADR-0008
  Amendment B); a structured `### Deprecated` entry format + a CONTRIBUTING
  release-cut runbook codify the surrounding discipline.
- **README split for the public launch (#214, #215).** A lean 232-line landing page
  (pitch → what-it-is → install → quickstart → observe-gate → MCP) +
  `docs/reference.md` operator manual; de-insidered (`(#NNN)` breadcrumbs dropped,
  migration story moved out of the fresh-install path).

### Added

- **Deprecation-eligibility derive-script — `scripts/deprecations.sh` (#209).** Per
  ADR-0008 §Amendment B, a thin bash script walks `CHANGELOG.md` surfacing each
  `### Deprecated` entry's `(deprecated-in, earliest-removal)` pin; `--for v<X.Y.Z>`
  confirms the removal list at cut time, `--all` prints the table. Permissive parser
  handles canonical + legacy entries (tags the latter), surfaces unpinned entries
  rather than dropping them.

### Changed

- **README split into a lean landing page + `docs/reference.md` (#214).** The
  729-line README served evaluator + operator at once; split along that seam — the
  232-line landing keeps pitch → what-it-is/isn't → install → quickstart →
  observe-gate → MCP → "where to go next"; the full command reference, chrome,
  identity/storage/migration, and K-counter mechanics move verbatim to
  `docs/reference.md` (the `tail` rowid-polling note to CONTRIBUTING). Restructure
  only.
- **ADR-0008 Amendment B — structured `### Deprecated` format (#209).** A
  machine-parseable shape: title line + a `Deprecated in vX; earliest removal vY.`
  pin line + free prose. The CHANGELOG stays the single source of truth (no separate
  registry; Option C hybrid); the derive-script reads legacy entries permissively.
- **`CONTRIBUTING.md` — release-cut runbook (#209).** A §Release cuts section
  codifying the sequence (sync → CHANGELOG → README version → `deprecations.sh
  --for` → pre-commit → cut PR → tag → deploy); the deprecation check is the
  operator's "which surfaces did I promise to remove?" surface.
- **README de-insider pass for the public launch (#215).** Dropped inline `(#NNN)`
  breadcrumbs (they resolve to nothing on the public mirror; the `#163` K-counter
  tracker stays as a full URL) and demoted the substrate-vs-adapter aside + the
  `claude-msg` migration story out of the newcomer's first screen.

### Deprecated

- **`claude-msg` / `claude-mailman@` aliases + `$CLAUDE_AGENT_NAME` fallback —
  earliest removal extended v0.11.0 → v1.0 (#177 extension, ADR-0008 §Discretion).**
  Per the operator's 2026-06-08 decision, the v0.9.0 rename-arc alias machinery
  (symlink + systemd template alias + identity env-var fallback) holds through the
  v1.0 boundary rather than the v0.11.0 floor (cheap machinery; v1.0 is the natural
  surface-freeze cutover). WARN logs now emit `removal=v1.0`; the K-counter stays
  preserved (Reading B), v0.11.0 increments K to 5.

### Fixed

- **`ListFilter.Unverified + State` silent impossible WHERE (#220 Item 1).**
  `ListMessages` now errors when `Unverified=true` is combined with a `State` other
  than empty/`delivered` (previously emitted `state='queued' AND state='delivered'`,
  silently returning zero rows). No user-visible change.
- **Test-coverage hardening (#220 Item 2).** Direct branch tests for
  `parseMCPToField` (array/scalar/invalid), a `ClaimNext` `no_reply_expected` scan
  regression pin, and a 3-way `quick` + `no_reply_expected` + fan-out round-trip
  test across the CLI + MCP send paths.
- **README: reconcile the `verified`-marker docs with the shipped binary (#213).**
  The `verified` column (#169) shipped but the README still described it as unbuilt;
  corrected to "the column exists + is DB-queryable, but `stats` / `resend` /
  `thread` / `mcp` / `status` don't *consume* it yet (#230)," and fixed a false
  claim that `stats` reports the split.

## [0.10.0] — 2026-06-08

Bus-discipline + backlog-hygiene release: a freshly-resumed chamber no longer
gets its whole queued backlog pasted at once, the `sent` outbox makes the
sender-outbox-first playbook a first-class CLI affordance, and `send` gains
multi-recipient fan-out + compact `--quick` acks. Plus the substrate-honest
`delivered_unverified` → `delivered_in_input_box` rename.

Headlines:

- **Backlog don't-flood on (re)register (#204).** A register stamps a
  `backlog_epoch_id` claim-floor; the mailman skips rows at/below it. Default
  `announce` policy delivers a single `📬 N queued — run tmux-msg.inbox` nudge;
  `auto-deliver` pastes the newest N (cap, default 3) and announces the rest.
- **`sent` — sender's outbox listing (#159).** Newest-first outbox with
  `--since/--state/--to/--limit/--format`; the `delivered_in_input_box` state
  filters soft-fails (#169) + a `resend` recovery hint. The first-class affordance
  for the sender-outbox-first playbook.
- **`send --to a,b,c` fan-out (#158).** One call, one id + independent delivery per
  recipient; a per-recipient failure doesn't abort the rest;
  `max-recipients-per-send` cap (default 10).
- **`send --quick` compact acks (#154).** `✓ Sender · [re <id> ·] <body>`
  single-line chrome for routine acks; sister to `--no-reply-expected` (#145) — one
  cuts unnecessary acks, the other the overhead of necessary ones.

### Added

- **Backlog don't-flood on (re)register (#204).** Register stamps a per-agent
  `backlog_epoch_id`; `ClaimNext` skips rows at/below it. `on-register-backlog`
  TOML knob: `announce` (default — leave queued + one `📬` nudge) or `auto-deliver`
  (paste newest `on-register-backlog-cap`, default 3, announce the rest); unknown
  value → `announce`; mailbox-only is a no-op. Register response gains
  `backlog_policy`/`backlog_skipped`/`backlog_nudge` (+ the #151 `queued`); the
  nudge rides the single-writer mailman path so the delivered-is-pasted invariant
  (#169) holds. Drain/ack of the skipped residue tracked at #221.
- **`sent` — sender's outbox listing (#159).** `tmux-msg-claude sent`, newest
  first, default 24h; `--since` (durations + calendar shortcuts), `--state`,
  `--to`, `--limit`, `--format`. The `delivered_in_input_box` state filters
  `delivered AND verified=0` soft-fails; JSON adds `display_state`. Store: `Message`
  carries `verified` in all read paths; `ListFilter` gains
  `SinceCreatedAt`/`Unverified`/`OrderDesc`.
- **`send --to a,b,c` multi-recipient fan-out (#158).** Comma-list (CLI) / array
  (MCP) delivers one body to many; each gets its own id + independent delivery;
  response `{ok, messages:[…]}` (scalar shape preserved); a failing recipient
  doesn't abort the rest; `max-recipients-per-send` cap (default 10).
- **`send --quick` — compact single-line acks (#154).** `✓ Sender · [re <id> ·]
  <body>`; preserves sender/thread/body, drops spatial framing; `no_reply_expected`
  rides as a `🔕` body prefix; the #160 length marker doesn't apply. New `quick`
  column (auto-migrates).

### Changed

- **Rename `delivered_unverified` → `delivered_in_input_box` (#140).** The old name
  described what *didn't* happen; the new one what *did* (paste landed in the input
  box). Log token, CLI `--state`, JSON `display_state`, config key, and Go
  identifiers renamed; deprecated aliases for the two-minor cycle (see Deprecated);
  frozen ADR/CHANGELOG prose keeps the old name per the rename-freeze precedent.
- **CI `gofmt` check added to the required pipeline (#202).** `gofmt -l .` runs
  before `go vet` and fails on any drift — graduating gofmt-cleanliness from
  review-discipline to substrate (closes the gap behind the pre-#172 17-file drift).
- **ADR-0008 Reading B — K-counter interaction codified (#208).** Deprecation
  *with* a functioning alias **preserves** the K-counter (#163); removal **resets**
  it (aligns K with "does existing config still work?"). Worked example: the #177
  rename — deprecate v0.9.0 (K=3) → aliased v0.10.0 (K=4) → removal v0.11.0 (K=0).

### Deprecated

- **Legacy `delivered_unverified` surfaces (#140, earliest removal v0.12.0).** The
  `--notify-on-delivered-unverified` flag, the `notify-on-delivered-unverified` TOML
  key, `--state delivered_unverified`, and the JSON shadow fields all keep working
  but emit `WARN deprecated_surface_used … removal=v0.12.0`. Two-minor floor from
  v0.10.0 per ADR-0008.

### Fixed

- **`install.sh` robustness — `bin/` ownership + `getent` exit-2 shadowing (#193).**
  `bin/` is created via `install -d -o "$OPERATOR_USER"` (idempotent, fixes a stale
  root-owned dir) so the fallback `go build` can write it; `getent passwd` gains
  `|| true` so an `OPERATOR_USER=<typo>` surfaces the friendly #175 error instead of
  dying with exit 2 under `set -euo pipefail`.

## [0.9.0] — 2026-06-07

The substrate-vs-adapter rename release: the binary becomes `tmux-msg-claude`
(substrate + adapter), making sibling adapters (`tmux-msg-codex` etc.) cleanly
addable, and the rename is dogfooded as the inaugural worked example of the new
ADR-0008 deprecation policy (aliases + removal-WARNs, earliest removal v0.11.0).
Also lands the K=3 release-stability counter, a durable `verified` delivery
marker, and the assignee-on-claim dispatch convention.

Headlines:

- **Binary `claude-msg` → `tmux-msg-claude` (#177, 3-PR arc).** Encodes substrate
  + CLI-adapter per #174 Option 2; `cmd/`, the systemd template, a multi-target
  Makefile, and `install.sh --adapter=claude` all follow. Seamless via aliases for
  the deprecation cycle; resets the #163 K-counter.
- **ADR-0008 deprecation policy (#162) + its inaugural dogfood.** Two-minor-cycle
  removal floor, runtime `WARN deprecated_surface_used`, `### Deprecated`
  convention; the #177 rename is the first worked example (removal v0.11.0).
- **Durable `verified` delivery marker (#169).** A nullable `verified` column
  splits `delivered` rows into verified / `delivered_unverified` / unknown so the
  soft-failure is DB-queryable, not journal-only — `stats` (#147) + Prometheus
  (#146) + verify-token forensics (#153) become clean SQL.
- **K=3 release-stability counter (#163)** — three consecutive break-free releases
  across the five public surfaces gates the road to 1.0; the rename resets it, so
  v0.9.0 restarts the cycle.

### Added

- **`register` surfaces the queued backlog count (#151).** The `register` response
  gains a `queued` field (messages already waiting) so a spawn-per-task /
  post-restart chamber learns it has backlog without a separate `inbox` poll; soft
  `queued_error` on read-failure (honest `0` never confused with unknown). Richer
  announce/auto-deliver paths deferred to #204.
- **Durable `verified` marker for delivered messages (#169).** New nullable
  `verified` column (`1` verified / `0` unverified soft-fail / `NULL` pre-marker),
  written by `MarkDelivered` / `MarkDeliveredUnverified`; the `WARN
  delivered_unverified` journal line stays. New `DeliveredVerificationCounts`
  aggregation seam; orthogonal to `state` (still `delivered`), not added to per-row
  scans.
- **ADR-0008 — deprecation-policy ADR (#162).** Operator-ratified post-1.0 policy:
  two-minor-cycle removal floor (deprecate `v1.X`, earliest `v1.X+2`) with
  discretion, runtime WARNs, `### Deprecated` + `deprecated: true` JSON
  conventions; pre-1.0 keeps semver-explicit looseness.
- **K=3 release-stability tracker (#163).** Counts consecutive break-free releases
  across MCP schemas / CLI / `--format json` / DB schema / Go API; v0.7.0 + v0.8.0
  were additive (K=2), then the #177 rename resets it. README gains a "Release
  stability (the K-counter)" subsection.
- **`docs/chamber-dispatch.md` — assignee-on-claim convention (#180).** Claim an
  issue (assign yourself) before starting + check `assignees` before dispatching;
  the bus carries coordination *conversations*, the tracker carries the *persistent*
  "this is mine" state. Anchored to the 2026-06-07 cross-dispatch collision;
  CONTRIBUTING pointer added.

### Changed

- **Binary renamed `claude-msg` → `tmux-msg-claude` (#177, PR1).**
  `cmd/claude-msg/` → `cmd/tmux-msg-claude/`, systemd template →
  `tmux-msg-claude-mailman@`, multi-target Makefile, `install.sh --adapter=claude`
  (default). Module path unchanged (already substrate-honest). Public-surface
  change — resets the #163 K-counter.
- **Env var `$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME` (#177, PR2).**
  `internal/identity` prefers the new var, falls back to the legacy one for the
  cycle with a once-per-process `WARN deprecated_surface_used … removal=v0.11.0`.
- **Docs + in-binary help-text rename sweep (#177, PR3 — closes #177).** README,
  docs, usage/`--help`, identity errors, and MCP tool-schema descriptions adopt
  `tmux-msg-claude` / `$TMUX_AGENT_NAME`; ADRs left as historical records; the
  deprecation-alias detection strings keep the old names deliberately (they ARE the
  deprecation surface).
- **CONTRIBUTING deprecation-scope clarification (#162 follow-up).** Names that the
  policy covers all five public surfaces, distinct from the external-contract subset
  (Go API + DB) a downstream module pins; links the landed ADR-0008.

### Deprecated

- **`claude-msg` binary name + `claude-mailman@` systemd template → `tmux-msg-claude`
  / `tmux-msg-claude-mailman@` (#177).** Earliest removal **v0.11.0** (ADR-0008's
  inaugural worked example). `install.sh` installs binary + unit symlinks for the
  cycle; the `claude-msg` name emits `WARN deprecated_surface_used name=claude-msg
  removal=v0.11.0`.
- **`$CLAUDE_AGENT_NAME` → `$TMUX_AGENT_NAME` (#177 PR2).** Earliest removal
  **v0.11.0**; identity falls back with a once-per-process removal WARN.

### Fixed

- **gofmt hygiene sweep — 17 files (#172).** `gofmt -w` over pre-existing
  whitespace/alignment drift; adding a CI `gofmt -d` check is tracked at #202.

## [0.8.0] — 2026-06-07

The diagnostic-and-recovery-tooling release: a suite of read-only
bus-introspection commands (`stats`, `digest`, `tail`, `thread`), message
recovery (`resend`, `stranded`), a substrate-only reachability probe (`ping`),
and bus-discipline chrome (`🔕` no-reply, body-byte marker, crossed-message
`thread_freshness`), all on a newly-named `SendResponse` struct contract (#152).
Plus the public-launch docs groundwork: the Binnacle-coexist external contract
and the README rewrite.

Headlines:

- **`SendResponse` struct contract + recipient reachability (#152).** `send`
  returns a named-struct schema with a `recipient` block
  (`registered`/`alive`/`delivery_mode`/`mailman_running`/`pane_status`) queried
  fresh at send-time; `--strict` + `--wait-for-delivered`. The contract #155 +
  #157 extend.
- **Recovery surfaces — `resend` (#157), `stranded list|show|prune` (#142).**
  Replay a failed/unverified message byte-identically; recover the operator
  drafts the observe-gate archives.
- **Diagnostic suite — `stats` (#147), `digest` (#161), `tail` (#148), `thread`
  (#141), `ping` (#144).** On-demand aggregates, campaign-arc narrative, a live
  firehose, a reply-chain tree, and a substrate-only reachability probe — all
  read-only over the local DB.
- **Bus-discipline chrome — `🔕` no-reply-expected (#145), body-byte marker
  (#160), crossed-message `thread_freshness` (#155).**
- **Binnacle-coexist external contract — ADR-0007 + `CONTRIBUTING.md` (#179,
  implements #164 Option B).** tmux-msg stays MIT + standalone; Binnacle consumes
  it as an external Go module; the exported API + DB schema become stability
  surfaces under #162's deprecation policy.

### Added

- **`SendResponse` struct contract + recipient reachability (#152).** `send`
  (CLI + `tmux-msg.send`) returns a named-struct schema with a `recipient` block,
  queried fresh from registry + `tmux` + `systemctl`; `--strict` (fail when a
  registered recipient is unreachable) + `--wait-for-delivered`/`--timeout`.
  Additive — `ok`/`id`/`queued` unchanged; an unregistered recipient stays
  fail-loud regardless of `--strict`.
- **`resend <id>` — replay a failed/unverified message (#157).** Replays the
  original byte-identically as a new message with a `↻ Replayed` marker + a
  `replay` response block; `failed` replays directly, `delivered`/in-flight needs
  `--force` (the `delivered_unverified` case isn't DB-distinguishable until #169).
  New `replay_of`/`replay_of_at` columns; MCP `tmux-msg.resend`.
- **`send --reply-to` crossed-message `thread_freshness` signal (#155).** Threaded
  sends return `{stale, newer_in_thread[], you_replied_to, latest_in_thread}` —
  messages addressed to you and newer than your thread high-water-mark;
  `--block-on-stale` turns it into a hard refusal. Reuses `store.GetThread` (#141).
- **`claude-msg stats` — on-demand bus-traffic aggregates (#147).** Per-agent
  counts, delivery-latency p50/p95, window totals, top sender→recipient pairs from
  the local DB; `--window`/`--agent`/`--pair`/`--format`. The aggregation seam
  (`StatsPerAgent`/`StatsTopPairs`/`StatsTotals` + shared `parseWindow`) is reused
  by `digest`.
- **`claude-msg digest` — campaign-arc narrative summary (#161).** The qualitative
  sibling to `stats`: a by-counterparty table + an "in-flight threads (likely need
  follow-up)" view; a thread is closed when its latest message carries `🔕`.
  Calendar shortcuts (`today`/`yesterday`/`week`); reuses the #147/#141 seams.
- **`claude-msg tail` — live diagnostic firehose (#148).** Read-only `tail -f`
  over bus traffic (inserts + state transitions) with AND-composable
  `--from/--to/--kind/--state/--since` filters; rowid-polling (not `update_hook` —
  the mailmen are separate processes), WAL-safe. New `TailRows`/`MessagesByIDs`;
  resolves #137's two-mailman-journal correlation pain.
- **`claude-msg thread <id>` — reply-chain tree (#141).** Renders a `reply_to`
  chain as an ASCII parent→child tree (`○` root / `✓` delivered / `✗` failed /
  `…` in-flight); `--format tree|json`. Read-only sibling to `log` over the shared
  `store.GetThread` seam.
- **`claude-msg ping <agent>` — substrate-only reachability probe (#144).** A
  `kind=ping` the recipient's mailman answers via health checks (registered +
  pane-live), transitioning straight to `delivered`/`failed` without paste — no
  pane mutation. States: delivered (exit 0) / failed (69) / timeout (75); MCP
  `tmux-msg.ping`. Replaces the pane-polluting "send a test message" runbook step.
- **`claude-msg stranded list|show|prune` — paste-snapshot recovery (#142).**
  Operator-visible recovery for the `stranded_draft` rows the observe-gate
  archives when a delivery would clobber operator input (#92); `prune
  --older-than` required. Best-effort on large bracketed pastes (tmux may capture
  only the `[Pasted text #N]` placeholder).
- **`--no-reply-expected` bus-discipline flag (#145).** New `no_reply_expected`
  column + flag/MCP param; renders a `🔕` marker telling the recipient no ack is
  needed (reduces ack-cascade on FYIs).
- **Body-byte length marker in the bracket header (#160).** Bodies over a
  threshold (default 512) gain a trailing `· <size>` marker (`… · id 4825 ·
  2.3k`); `<n>b` under 1000, `<n.n>k` above (decimal ×1000). Configurable via the
  `render-byte-marker-threshold` TOML knob. `render.Message` gains a
  `byteMarkerThreshold` arg (pre-1.0 minor break, internal callers only).
- **`quartermaster→pilot` `/clear` PeerEdge (#167).** Mirrors the `bosun→pilot`
  edge (#60) now QM is an established dispatcher into Pilot's clear-before-task
  lifecycle; the edge stays exact (QM→any-other still denied).
- **ADR-0007 + `CONTRIBUTING.md` — Binnacle-coexist external contract (#179,
  implements #164 Option B).** tmux-msg stays MIT + standalone; Binnacle consumes
  it as an external Go module (MIT+GPL-3.0 clean); the exported Go API + DB schema
  become stability surfaces under #162's deprecation policy.
- **Docs for the v0.7.0 surfaces + public-launch prep.** README `### Canonical
  name mapping` (#143), README `### Delivery modes` (#138, caught in a post-close
  AC audit), and `docs/why.md` — the deployment-agnostic "Why tmux-msg?" pitch
  (first piece of the GitHub-launch doc package).

### Changed

- **README rewritten for the public launch (#156; restructures #143's
  canonical-name-mapping section from #166).** Landing-page-first, leads with the
  `docs/why.md` pitch, genericized off alcatraz-specific examples (substrate-first
  per ADR-0003), refreshed stale spots (bracket header #121/#122, shipped
  `register` CLI, v0.7.0 `--version`).
- **`tmux-msg.send` MCP description names the queued→delivered lifecycle (#156)**
  (+ points at `tmux-msg.message_status`) and `reply_to` threading, so a newcomer
  doesn't read "queued" as "delivered." Description string only.
- **`whereSince()` reader-startle comment (#176).** One-line comment noting
  `return "1=1"` is a compile-time constant for `--window all`, no user input
  interpolated. No behavior change.

### Fixed

- **`install.sh` fails loud on an unresolvable operator user (#175).** Drops the
  `${USER:-alex}` fallback; resolves `OPERATOR_USER` → `$SUDO_USER` → `$USER` with
  no last-resort guess, erroring (exit 1) if none resolves or it's `root` — closes
  silent-misconfiguration + shipping a maintainer's username in a public installer.
  README `## Install` gains a "what runs as root vs as you" subsection.
- **`docs/security.md` ASCII alignment (#192).** Centered the "Bus" label in the
  §1.3 diagram. Cosmetic.

## [0.7.0] — 2026-06-06

The operator-as-bus-participant release: `delivery_mode=mailbox-only` lets the
operator's bare shell register as a bus destination (chambers `send to=operator`,
operator polls `inbox`), a pre-paste safety net closes the popup-as-Unknown
draft-destruction failure mode from #105, and message recovery (`get`) + a
busy-chamber fast-path round out the delivery surface. The delivered-message
template is re-grounded on the compact bracket header.

Headlines:

- **`delivery_mode` + `mailbox-only` — operator joins the bus (#116, TOML knob
  #132).** New `agents.delivery_mode` column (default `paste-and-enter`); a
  `mailbox-only` pane is a destination the mailman never pastes into. Per ADR-0005
  it's a config-difference (one column), not a participant-supertype expansion.
- **Pre-paste safety check (#105 Half 2).** A final `AgentState` read immediately
  before each paste aborts delivery (reverts `delivering` → `queued` + `WARN
  pre_paste_safety_abort`) when the pane is `AwaitingOperator` or `Unknown` —
  belt-and-suspenders against the popup-destruction case that doesn't rely on the
  recognizer being perfect.
- **`get` / `tmux-msg.get` — fetch processed messages by ID (#111).** Recovery
  path for a delivery that landed in the store but was visually swallowed by the
  recipient's pane state; sender-or-recipient access (+ `privileged-agents`
  allowlist), no existence leak.
- **`working-deliver-immediately` opt-in (#106).** Opts the `StateWorking` branch
  out of backoff into the idle fast-path (~1s vs 3–57s) for
  coordination-latency-sensitive chambers; `AwaitingOperator` / `Compaction` /
  `Unknown` stay hard-deferred.
- **Bracket-header delivery template (#121).** Box-drawing rules → compact
  `[Bosun → Quartermaster · re 1d0c · id 8f54]` (narrow-viewport + mobile font
  coverage; `re:` → `re`, `──` → `·`).

### Added

- **`delivery_mode` for operator-as-bus-participant (#116).** `mailbox-only`
  registers a pane as a destination without a paste-expecting mailman; CLI
  `register --delivery-mode mailbox-only` + MCP `tmux-msg.register` param.
  `mailbox-only` implies `start_mailman=false` and short-circuits `state` to
  `idle` (no TUI to probe). Flip-back to `paste-and-enter` needs a manual mailman
  restart (the short-circuited startup left no resume trigger).
- **TOML `delivery-mode` per-agent override (#132).** `[agent.<name>]` /
  `[defaults]` `delivery-mode` overrides the DB column at mailman startup; invalid
  values `WARN config_delivery_mode_invalid` and the DB column wins. New
  `config.ResolveString` helper (sister to `ResolveBool` / `ResolveDuration`).
- **Pre-paste safety check (#105 Half 2).** `tmuxio.IsPasteUnsafe(state)`
  centralizes the policy (unsafe for `AwaitingOperator` + `Unknown`);
  `--pre-paste-safety-disabled` / TOML knob, default on. Recognizer-improvement
  (Half 1) tracked at #133.
- **`get` subcommand + `tmux-msg.get` MCP tool (#111).** Fetch a processed
  message by full or short (4-char) public_id; disambiguation on prefix
  collision; `privileged-agents` extends the sender/recipient access model;
  not-authorized and not-found return the same error class.
- **`working-deliver-immediately` (#106).** Per-agent opt-in fast-path for
  `StateWorking` deliveries (Claude Code buffers mid-turn keystrokes, so the paste
  is structurally safe); verify-token retry + `delivered_unverified` is the safety
  net for the observe→paste race.

### Changed

- **Delivery template re-grounded on the compact bracket header (#121).**
  `[Sender → Recipient · re <id> · id <id>]` + blank line + body, replacing the
  U+2500 box-drawing rules that wrapped on mobile viewports and lacked font
  coverage. Information content (sender / recipient / thread / id / clock) and
  `id NNNN` grep workflows preserved.
- **AskUserQuestion canary fixture refreshed post-v0.6.0 (#133).** Added a
  2026-06-06 capture; the canary now checks both fixtures. Empirically
  **disconfirms** the 2026-06-05 marker-drift theory — the existing marker matches
  the popup the operator was in, so that incident's cause was elsewhere; the #105
  pre-paste safety net is the load-bearing protection regardless.
- **Doc precision: `ObserveGate` is "near-read-only", not strictly read-only
  (#126).** v0.4.0's `📫` mailbox nudge (#95) is a one-character input-row
  injection, so the strictly-read-only framing was overstated; corrected at the
  README + observe-gate doc surfaces. The `AgentState` probe stays strictly
  read-only.
- **Doc precision: stale v0.2.x migration paragraph (#124).** v0.4.0's strict TOML
  decode (#94) makes unknown keys fail the config load (not WARN-and-continue);
  the README migration note now reflects strict-fail + the deprecated-key-removal
  recovery.

### Fixed

- **Flaky `TestServe_PostCompactPauseDelaysNextDelivery` (#127).** Measure the
  post-compact gap via the store's `delivered_at` (stamped in `MarkDelivered`)
  rather than `time.Now()` at poll-observation time, removing ~4ms poll jitter
  that occasionally dipped below the 80ms threshold. (Surfaced in PR #125.)

## [0.6.0] — 2026-06-05

The substrate-naming-honesty release: two hard-cutover renames re-ground the MCP
wire surface and the core terminology on the substrate's own vocabulary — no alias
period, one restart window.

### Changed

- **MCP wire-surface re-grounded on the substrate name (#112, ADR-0004).** The MCP
  server name + tool-method prefix flip `semaphore` → `tmux-msg` (all 10 tools, e.g.
  `tmux-msg.send`), as do the control-command names (`mcp-restart-semaphore` →
  `mcp-restart-tmux-msg`, + enable/disable). Hard cutover per ADR-0004 §Decision (4)
  — no alias period; every chamber updates `.mcp.json` + restarts Claude Code in one
  ~5-min window.
- **Terminology re-grounded `chamber` → `agent` (#107, ADR-0005).** The per-pane
  primitive is renamed from project-local `chamber` jargon to the substrate-honest
  `agent` already in its identifier vocabulary (`agents` table, `--agent`): Go
  identifiers (`ChamberState` → `AgentState`) swept across `cmd/` + `internal/`, MCP
  `semaphore.chamber_state` → `tmux-msg.agent_state` (bundled into the same cutover),
  doc prose swept. Out of scope per ADR-0005 §Decision (2): ADR prose stays frozen
  accurate-to-time; chamber-level CLAUDE.md files are project-local lexicon (bridge
  note in alcatraz-infra#21).

## [0.5.0] — 2026-06-05

Project re-grounded on its substrate primitive (#97): renamed `cli-semaphore` →
`tmux-msg`.

### Changed

- **Project renamed `cli-semaphore` → `tmux-msg` (#97).** A substrate-class
  accuracy correction, not cosmetic: the substrate IS tmux (pane registry +
  paste-and-Enter delivery + per-pane chrome detection); the CLI tool in the pane is
  downstream. Surfaces: repo + Go module path (Forgejo keeps URL redirects),
  operational dirs (`/etc/cli-semaphore/` → `/etc/tmux-msg/`, `/var/lib/…`
  likewise), config constants + help text. Unchanged (CLI-flavored, not
  substrate-flavored): the `claude-msg` binary, the `claude-mailman@` unit, and the
  `semaphore` MCP server name. Migration: the v0.5.0 binary reads the new paths;
  operators with custom paths `mv` the old dirs before starting it. (The
  `ChamberState` identifier rename + Binnacle's references are tracked separately as
  #107.)

## [0.4.0] — 2026-06-04

Hardening pass on the v0.3.0 observe-gate substrate-shift: strict TOML config
decoding, the `📫` mailbox visibility nudge, a multi-line draft-archive fix, and
the dead-probe-and-watch-code sweep.

### Fixed

- **Multi-line draft truncation in the observe-gate (c) flush (#96).**
  `extractInputContent` returned only the first sentinel row, so a multi-line
  operator draft archived as `stranded_draft` kept only line 1 while `Ctrl+U`
  cleared the whole buffer — (c) Clear-paste-archive silently degraded to the
  rejected (b) Clear-and-discard. Now walks from the sentinel row down to an
  input-area boundary (`⏵⏵` status marker or 20+ `─`), joining continuation rows.
  (Surfaced by a v0.3.0 post-deploy session that archived 123 bytes of a
  multi-paragraph draft.)

### Added

- **Strict-mode TOML config decoding (#94).** `config.LoadFrom` fails the load on
  any unknown key (naming it) via `MetaData.Undecoded()`, replacing the
  silent-drop + post-hoc WARN — so a typo or a lingering legacy probe-and-watch
  knob fails loud, matching the v0.3.0 fail-loud discipline.
- **📫 mailbox notification for pending bus messages (#95).** When the
  observe-gate first sees `StateAwaitingOperator` (operator drafting), the mailman
  injects a single `📫` into the input row as a one-shot "a message is waiting"
  signal — once per delivery cycle (`OnOperatorTyping` callback), no cleanup
  (operator-deletes-or-it-rides-along, sibling to the (b)-rejected discipline).
  Restores the visibility the retired `─` probe dashes used to give.
  `--notify-emoji-disabled` knob; `PendingMessageMarker` + `NotifyPendingMessage`
  surface.

### Removed

- **Dead probe-and-watch primitives + legacy gate knobs (#94).** Follow-up sweep
  to v0.3.0's observe-gate substrate-class shift (deferred from PR #93 to keep
  that diff scoped): removes `internal/tmuxio/probe.go` + `probe_test.go` +
  `pin_test.go` wholesale (`WaitForQuietPane`, `QuickPresenceProbe`,
  `InputRowHasContent`, `analyzeDelta`, the `DeltaKind` verdict surface, the
  `OperatorInputRowGate` pin), the six legacy `--quiet-*` / `--quick-presence-probe`
  / `--prompt-sentinel-gate` CLI flags + their `config.Block` / `ResolvedView`
  fields + `config show` lines. `PromptSentinel` + a parse-only `isInputRowQuiet`
  survive into `state.go`; the marker canary tests migrate to
  `state_canary_test.go`. A config still referencing a removed key now fails the
  load loudly (per the strict decoder above).

## [0.3.0] — 2026-06-04

The chamber-state-visibility release: the `ChamberState` read-only-observe
primitive (#69 campaign) lands the five-state vocabulary — `idle` / `working` /
`unknown` / `at-rest-in-compaction` / `awaiting-operator` — and the mailman's
pre-delivery gate is rebuilt on top of it, replacing the multi-second
probe-and-watch flow with a zero-pane-mutation observe-gate (~72s/138s → ~3–5s
typical). Ships alongside host-level config, a monitoring stack, and the
`/clear` peer-edge exception layer.

Headlines:

- **`ChamberState` read-only-observe probe (#69 / #71).** "Knock at the door
  without waking the inhabitant" — two `capture-pane` snapshots + a cursor query
  classify a chamber into five states with zero pane mutation. Consumed by
  `claude-msg state` (#72) and the `semaphore.chamber_state` MCP tool (#73);
  detection markers populated by #70 (compaction) + #79 (AskUserQuestion popup).
- **Observe-gate replaces probe-and-watch (#92).** On by default for all
  chambers; read-only, ~3–5s typical vs the legacy gate's 72s/138s. Legacy
  `quiet-*` / `prompt-sentinel-gate` / `quick-presence-probe` knobs deprecated
  to runtime no-ops with a startup WARN.
- **Host-level config file (#54).** `/etc/cli-semaphore/config.toml`
  (`CLAUDE_MSG_CONFIG`-overridable), CLI-flag > per-agent > defaults >
  compile-time precedence chain + a `config show` resolver.
- **Monitoring stack (#42 / #45 / #39 / #41).** `claude-msg health` +
  `status --today` source operational state from journalctl + systemd
  (deliver-time percentiles, crash counts) so tooling stays decoupled from the
  mailmen.

### Added

- **`tmuxio.ChamberState` read-only-observe probe (#71).** Five-state
  classification from two 200ms-apart `capture-pane` reads, zero send-keys; an
  `Evidence` struct carries the classification reason. v1 detects
  idle/working/unknown; compaction + awaiting-operator markers wired but inert
  until #70/#79.
- **Cursor-position-aware ChamberState v2 (#69).** Adds a read-only
  `display-message` cursor query to split cursor-at-sentinel (idle, incl. Claude
  Code ghost-text) from cursor-past-sentinel (operator mid-typing →
  awaiting-operator). Graceful degrade to the v1 cursor-less heuristic on query
  failure.
- **Compaction detection — `/compact` UI capture (#70).** `CompactionMarker` =
  `"Compacting conversation…"`, matched ahead of the working-check so an
  animating compaction UI classifies as `at-rest-in-compaction`, not `working`.
- **AwaitingOperator detection — AskUserQuestion popup capture (#79).**
  `AwaitingOperatorMarker` populated from the popup footer; a chamber showing the
  question popup classifies as `awaiting-operator`.
- **Chamber-state consumer surfaces (#72, #73).** `claude-msg state --agent NAME
  [--format text|json]` (#72) and the `semaphore.chamber_state` MCP tool (#73)
  share one `resolveChamberState` helper so the JSON schema is identical across
  surfaces (durable per the ADR-0002 carry-forward spec).
- **Observe-gate + `KindStrandedDraft` (#92).** `ObserveGate` polls
  `ChamberState` + a content-hash to decide delivery; on a stale operator draft
  it archives the input as a self-addressed `stranded_draft` (cap-bypass) before
  clear+paste. New `gate-disabled` / `poll-interval-min|max` /
  `input-stale-threshold` knobs.
- **Prompt-sentinel + quick-presence pre-gates (#63 Parts 1+2).**
  `QuickPresenceProbe` (~50ms write+observe, detects operator-typing-now) and
  `InputRowHasContent` (read-only, detects a draft sitting in the buffer);
  composable, sentinel-first. Subsumed by the #92 observe-gate later in the same
  release.
- **Host-level config file (#54).** Precedence chain + `config show` resolver;
  silent fallback on a missing file, WARN-and-continue on a malformed one.
- **Monitoring stack (#42, #45, #39, #41).** `internal/healthscan` +
  `claude-msg health [--since DUR] [AGENT...]` + `status --today`: counters,
  deliver-time p50/p95/p99, systemd crash counts, all sourced from journalctl +
  systemd.
- **Bulk MCP refresh — `claude-msg refresh-all-mcps` (#62).** Fires the
  `mcp-restart-semaphore` macro per registered agent after a binary deploy;
  operator-only CLI (no peer-invokable MCP variant — DoS-amplification class).
- **`claude-msg track --watch` (#49).** Polls message state and re-renders on
  each transition until terminal/timeout; replaces the "ping me when consumed"
  wrapper script.
- **`claude-msg discover --apply-aliases` (#46).** Folds a long `--resume` value
  containing an existing canonical short-name into an alias instead of creating a
  duplicate registry row; ambiguous matches still create rows for manual
  disambiguation.
- **`/clear` whitelist + `PeerEdges` per-edge exceptions (#60).** Adds `clear` to
  the control allowlist as globally-denied, plus a narrow (sender, recipient)
  edge layer; the first edge is Bosun → Pilot as a token-exhaustion rescue path.
- **Uninstall path — `uninstall.sh` + README "Removal" (#80).** Idempotent,
  default-safe (leaves the SQLite DB), `--purge` with TTY confirmation; refuses
  to run from inside the data dir.
- **Delivery-failure notification (#53).** The mailman auto-inserts a
  `delivery_failure_notice` back to the sender on terminal failure (cap-exempt
  via new `Store.InsertNotice`), loop-guarded so a failed notice spawns no
  notice. Independent `--notify-on-failed` / `--notify-on-delivered-unverified`
  toggles.
- **Diagnostic playbook (`docs/diagnostic-playbook.md`, #65).**
  Sender-outbox-first triage flow (store → mailman journal → external system)
  for a reported missed message.
- **Discipline-pins + CI lint.** `check-pin-slugs` lint enforcing ADR-0001's slug
  register (#51); ADR-0001 amended with the `OperatorInputRowGate` +
  `CapExemption` slugs (#55); a cross-process cap-as-ceiling pin (#33); a
  perf-skip-composition pin (#67).
- **ADR-0002 — chamber-state carry-forward spec for Binnacle M6b (#74).** Names
  what of the `ChamberState` primitive carries to Binnacle verbatim vs what's
  bridge-specific.

### Changed

- **Probe-and-watch quiet-gate flipped to opt-in (default OFF), then subsumed by
  the observe-gate (#92).** `--quiet-disabled` default → `true` after the
  Binnacle M2.11 exchange showed up to 5 min worst-case latency without
  preventing the collisions it targeted; the observe-gate (#92, on by default)
  is the successor.
- **Probe-and-watch gate redesigned to an operator-only two-dash check (#52)** —
  *behavioral break (pre-1.0 minor).* Four-way verdict → two-way
  (`DeltaQuiet`/`DeltaInputActivity`); conversation-area streaming no longer
  blocks delivery; `--quiet-tui-backoff` removed.
- **Message-header clock now renders local time** (stored `CreatedAt` stays UTC)
  so headers correlate with journalctl's local-time prefix.
- **`AwaitingOperatorMarker` canary hardened (#89).** Adds an explicit
  empty-marker guard so a regression to the `""` placeholder can't pass via
  `strings.Contains(g, "")`.
- **README "Diagnosing a failed or unverified message" section (#48).** Walks the
  `track` → journalctl → fix flow with common cause patterns.

### Fixed

- **PromptSentinel NBSP encoding bug — silent since PR #66.** The sentinel
  constant used a regular space (U+0020) but Claude Code emits `❯` + NBSP
  (U+00A0), so the gate never matched a real pane; fixed with the NBSP escape + a
  capture-derived golden fixture + byte-encoding canary tests. (PR #66 / #77)
- **TOML knobs `quick-presence-probe` + `prompt-sentinel-gate` now take effect.**
  Both were wired into flag-help + `ResolveBool` but never added to
  `config.Block`'s struct/switch, so they silently no-op'd to defaults (shipped
  with PR #64 / #66).
- **CLI flag-ordering trap closed (#44).** `control alice --command compact`
  (recipient-first) no longer drops `--command`; a `reorderFlagsFirst` helper
  reorders flags ahead of positionals across all positional-taking subcommands,
  and `control` auto-binds a trailing positional to `--to`.
- **`WARN drift_check_ambiguous` carries the fix recipe inline (#47).**

### Deprecated

- **Legacy probe-and-watch flags + TOML knobs (#92):** `quiet-disabled`,
  `quick-presence-probe`, `prompt-sentinel-gate`, `quiet-observe-window`,
  `quiet-input-backoff`, `quiet-max-wait` — all runtime no-ops, subsumed by the
  observe-gate; the mailman startup logs a WARN naming any that are set.

### Removed

- **Probe-and-watch-coupled tests** — `serve_quiet_test.go` (3 tests) +
  `TestPin_OperatorInputRowGate_QuickProbeSkippedWhenSentinelPromotes`, all bound
  to behavior the observe-gate replaced.

### Known limitations

- **`store.AddAlias` / `SetAliases` cross-canonical collision check has a TOCTOU
  window** — narrowed to microseconds by `_txlock=immediate` (#29) and
  alex-as-sole-registrar in practice; tighten (check inside the UPDATE txn) if
  concurrent register ever becomes real.

### Notes for 1.0 trigger (Surveyor review of v0.2.0)

Before 1.0 we want **K=3 release stability** across all public surfaces (MCP
schemas, CLI args/flags/exit codes, `--format json` shapes, DB schema, the
public Go API for `discover`/`store`/`tmuxio`) + a committed deprecation policy +
the Binnacle-absorb-or-coexist decision settled.

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

[Unreleased]: https://git.frankenbit.de/frankenbit/tmux-msg/compare/v0.17.0...main
[0.17.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.17.0
[0.16.1]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.16.1
[0.16.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.16.0
[0.15.1]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.15.1
[0.15.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.15.0
[0.14.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.14.0
[0.13.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.13.0
[0.12.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.12.0
[0.11.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.11.0
[0.10.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.10.0
[0.9.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.9.0
[0.8.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.8.0
[0.7.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.7.0
[0.6.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.6.0
[0.5.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.5.0
[0.4.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.4.0
[0.3.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.3.0
[0.2.1]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.2.1
[0.2.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.2.0
[0.1.0]: https://git.frankenbit.de/frankenbit/tmux-msg/releases/tag/v0.1.0

# ADR-0003: Substrate-vs-flavor naming

> **Status**: Proposed
> **Date**: 2026-06-05 (proposed)
> **Authors**: Quartermaster (author), operator (driving the rename
> decision and Option C scope), Surveyor (pre-review notes on PR #108
> that shaped the framing of "substrate-class accuracy correction";
> ADR-0003 framing-review tightening on Q1 role-vs-implementation,
> Q2 structurally-distinct qualifier, and the §Decision (2) scope
> binding), Infra Admin (original `cli-semaphore` author, 2026-05-29
> — the substrate this ADR re-grounds)

## Context

The project was originally named **`cli-semaphore`** when Infra Admin
built it on 2026-05-29 as a temporary cross-chamber synchronization
mechanism between Bosun, Surveyor, Pilot, and Admin. The name
foregrounded *what it was used for at the time* (semaphores between
CLI agents) rather than *what it actually is* (a message-bus substrate
that uses tmux for delivery).

Six days of operational use surfaced a tension the name made hard to
see:

- The substrate — pane registry, paste-and-Enter delivery, per-pane
  chrome detection (idle / busy / popup-open / mid-compaction /
  awaiting-operator), SQLite mailbox store, mailman daemons — is
  built **on tmux**. tmux is what carries messages from sender to
  recipient; the substrate's primitives all bottom out in tmux
  operations.
- The CLI tool running inside each pane — today exclusively
  `claude-msg`, built for Claude Code — is the **consumer** of the
  substrate. It calls the substrate's primitives; it doesn't
  implement them.
- The "semaphore" framing conflated these two layers. The substrate
  is not specifically about Claude Code, nor about CLI agents in
  general — it's about delivering messages to/from arbitrary
  programs running inside tmux panes. The CLI-tool-flavor is a
  per-consumer concern.

The conflation has real consequences:

1. **Sibling CLI tools are architecturally invisible.** A future
   `codex-msg` (for OpenAI Codex CLI) or `copilot-msg` (for GitHub
   Copilot CLI) would have to be either (a) a fork of the entire
   repo with a different binary name, or (b) a feature flag on
   `claude-msg` that toggles consumer-specific behavior. Neither is
   right: sibling binaries are first-class scope, not afterthoughts
   to be retrofitted.
2. **Scope decisions about what belongs where lack a frame.** When a
   contributor adds a feature, "is this substrate-level or
   flavor-level?" is the question that decides which package it
   lives in, whether it should be parameterized, and whether per-CLI
   variation is expected. Without a named distinction, the question
   gets answered ad hoc per PR.
3. **The original name leaked into doc + ADR voice.** Substrate code
   referring to itself as "cli-semaphore" reinforces the
   consumer-specific framing every time a reader encounters it. The
   name is what the substrate is *called*, not what it *is*.

The operator surfaced this on 2026-06-05 in the same conversation
that produced issue #97 (the rename trigger). Two questions had to
be resolved: **what's the right name**, and **what's the scope of
the rename**.

## Decision

**(1) Rename the project from `cli-semaphore` to `tmux-msg`.**

`tmux-msg` names the substrate at the right level of abstraction:
**tmux** is the load-bearing technical substrate, and **msg**
(messages) is what the substrate carries. Neither half over-claims
nor under-claims: the substrate is not about agents (too narrow), not
about IPC in general (too broad), not about a specific consumer.

**(2) Codify the substrate-vs-flavor architectural commitment.**

The boundary:

- **Substrate** (lives in `tmux-msg`): pane registry; identity
  resolution (`$TMUX_PANE` → agent name); paste-and-Enter delivery;
  chrome detection (idle / busy / popup-open / mid-compaction /
  awaiting-operator); SQLite mailbox store with per-agent queues;
  the **mailman daemon role pattern** (watchdog-loop,
  `RecoverDelivering` reset-on-restart, sd_notify integration,
  systemd template shape, journal-tag convention); the control
  surface (`semaphore.control` whitelisted operations); health-scan
  and observability primitives.
- **Flavor** (lives in per-consumer binaries; today only
  `claude-msg`, future siblings `codex-msg` / `copilot-msg`):
  binary name; command-line surface tailored to the host CLI tool's
  conventions; consumer-specific message rendering (Claude-specific
  markup, Codex-specific transcript references, etc.); any wire
  protocol adaptation for the host CLI tool's input/output shape;
  the per-flavor systemd unit file that wires the substrate's
  daemon-role pattern to a specific flavor binary
  (`init/claude-mailman@.service` today is conceptually
  flavor-side; it currently ships in the substrate repo as a
  pragmatic single-flavor accommodation pending sibling-binary
  surface).

**The substrate must remain consumer-agnostic.** When substrate code
needs to vary by consumer (e.g., a chrome heuristic that only
matches Claude Code's pane layout), the variation lives behind a
substrate-level abstraction (interface / strategy / config), not
behind a `if consumer == "claude"` branch.

**Scope of the "no branching" commitment**: this constraint applies
to **Go source under `internal/` and `cmd/claude-msg/`** — the
substrate's runtime code. Per-flavor *shipping artifacts* (the
unit-file name `claude-mailman@.service`, the binary name
`claude-msg`, the CLI command set) live in the flavor space by
construction; their per-flavor naming is a flavor identity, not a
substrate branch on consumer identity. Naming this scope explicitly
keeps the commitment actionable — the pin candidate (when promoted)
greps Go source, not deployment artifacts.

**(3) Adopt Option C for the rename scope.**

Three scope options were considered (see Alternatives). Option C =
full rename: Forgejo repo + Go module path + operational paths
(`/etc/tmux-msg/`, `/var/lib/tmux-msg/`) + chamber-level docs in
`/srv/claude/*/CLAUDE.md` + repo-internal docs and ADR-index voice.

The scope explicitly **does not** include:

- **Existing ADRs (0001, 0002).** ADRs are immutable historical
  records of decisions made at a point in time. They reflect the
  state when written; rewriting them to use the new name would
  erase the temporal context.
- **CHANGELOG entries for prior releases.** Same reasoning: the
  CHANGELOG documents what was true at the time of each release,
  including the project's then-name.
- **Golden testdata fixtures** (`internal/tmuxio/testdata/golden_*.txt`).
  These are real pane captures used as fixtures for the parser /
  state-detection tests. The captured content includes operator
  and chamber narrative referencing the project by its then-name;
  editing the fixtures falsifies their provenance.
- **Downstream consumers** (Binnacle's references, alcatraz-infra's
  references beyond the chamber-level docs). Those are tracked
  separately as deferred follow-ups.

## Alternatives considered

### Scope options

- **Option A — Forgejo only.** Rename the Forgejo repo; leave the Go
  module path, operational paths, and all internal references
  unchanged. Rejected: leaves the Go module path
  (`git.frankenbit.de/frankenbit/cli-semaphore`) as a permanent
  reminder of the old name in every import statement. The Forgejo
  rename creates URL redirects so URLs would resolve, but the
  imports would visibly contradict the repo name.
- **Option B — Operator-facing surfaces only.** Rename the Forgejo
  repo + the README + the human-facing docs. Leave Go module path,
  operational paths, systemd unit name, env-vars, internal code
  unchanged. Rejected: produces a permanent divide between
  "operator-visible name" and "code-internal name." Any contributor
  reading source code would be re-confronted with the old name on
  every grep.
- **Option C — Full rename (chosen).** Rename Forgejo + Go module
  path + operational paths + chamber-level docs + repo-internal
  references. The substrate is fully re-grounded.

### Name candidates

- **`claude-msg`** (the current binary name). Rejected: same
  conflation in a different direction. Naming the substrate after
  one of its consumers makes sibling binaries (`codex-msg`,
  `copilot-msg`) syntactically odd — they'd be alternative entry
  points to a project named after their sibling. The substrate is
  not Claude-specific; the binary is.
- **`tmux-mailbox`**. Rejected: "mailbox" is one of the substrate's
  primitives (per-agent SQLite queue), not the substrate itself.
  The substrate includes the pane registry, chrome detection,
  delivery mechanics, and observability — all of which are
  upstream of "mailbox."
- **`tmux-mailman`**. Rejected: "mailman" names the **daemon role**
  that delivers from the mailbox to a pane. The mailman is a
  process pattern *atop* the substrate, not the substrate itself.
  Naming the substrate after one of its operational roles repeats
  the original `cli-semaphore` conflation at a different layer.
- **`tmux-msg`** (chosen). Names the substrate (tmux) and what it
  carries (messages) at the right level of abstraction. Sibling
  binaries (`claude-msg`, `codex-msg`, `copilot-msg`) compose
  naturally: each is a `<flavor>-msg` consuming the `tmux-msg`
  substrate.

### Doing nothing

- **Keep the `cli-semaphore` name.** Rejected: the conflation
  has measurable cost (sibling-binary invisibility, ad-hoc scope
  decisions, doc voice that reinforces the wrong framing) and the
  cost grows with project maturity. Renaming early (~one week of
  operational use) is cheaper than renaming late.

## Consequences

### Cleaner

- **Sibling CLI tools become architecturally first-class.** Adding a
  `codex-msg` binary to the project is now a straightforward act of
  composition — a new entry point that consumes the substrate's
  primitives, with whatever Codex-specific surface its host CLI
  tool wants. No fork. No feature flag. No retrofitting.
- **Scope decisions get a frame.** "Is this substrate-level or
  flavor-level?" is the standing test for new features and
  refactors. The answer dictates package location, whether
  per-consumer parameterization is needed, and whether the change
  is even in this repo's scope (vs a downstream consumer's).
- **Doc voice aligns with the framing — leverage for (1) and (2).**
  README §Substrate vs CLI-tool-flavor names the boundary
  explicitly; future contributors read it before writing. This is
  what converts (1) sibling-first-class-ness and (2) substrate-vs-
  flavor scope decisions from aspirational into mechanically
  actionable: the boundary lives in code reviewers' working memory,
  not just in this ADR.
- **Substrate code names what it is.** Reading any file under
  `internal/`, the package paths and identifiers refer to the
  substrate by its actual primitive, not by a consumer-specific name.

### Harder

- **One-time rename ceremony.** PR #108 implements the mechanical
  rename across ~75 files + Go module path + operational paths +
  systemd template + chamber-level docs. A worked example for
  future rename PRs: README §rename-ceremony, CHANGELOG
  `[Unreleased]` framing, operator-chamber live substrate swap as
  part of merge.
- **Downstream rename debt.** Binnacle references (ADR-0022 §primitives,
  ADR-0026, schema seams) and alcatraz-infra references (chamber
  CLAUDE.mds — already swept in PR #108) carry the historical name
  in places. The downstream sweep is a deferred follow-up; the
  substrate doesn't wait on it.
- **The old name persists in immutable contexts.** Existing ADRs
  (0001 §13, 0002 throughout), CHANGELOG entries for prior releases,
  golden testdata fixtures, and Forgejo PR/issue URLs in
  cross-references all retain `cli-semaphore` as accurate-to-time.
  Forgejo URL redirects handle the link rot; the prose remains
  historically accurate. Future readers should treat any
  `cli-semaphore` reference dated before 2026-06-05 as the
  pre-rename voice.

### Followed-on

- **Discipline-pin candidate.** "Substrate code does not branch on
  consumer identity" is a candidate for a future commitment slug
  under ADR-0001's register, if the substrate-vs-flavor boundary
  is breached during a feature addition. Not pinned now (no
  violation to guard against yet); flagged for the slug-register
  when the first sibling binary surfaces a real case.

## What would change the decision

Reasons to retract or supersede ADR-0003:

- **Sibling CLI tools prove architecturally incompatible with the
  substrate.** If `codex-msg` or `copilot-msg` actually surfaces
  needs (delivery shape, identity model, chrome detection) that
  can't share a substrate with `claude-msg` without contortion,
  the substrate-vs-flavor separation is the wrong cut. The right
  retraction depends on the failure mode: a finer cut
  (substrate-vs-protocol-vs-flavor) would supersede; per-flavor
  substrates (separate repos per CLI tool) would retract.
- **Chrome-detection or delivery heuristics turn out to be
  inherently per-CLI-tool with no substrate-level commonality.**
  If the per-pane chrome states (`idle` / `busy` / `popup-open` /
  ...) carry no shared semantics across CLI tools, the line
  between substrate and flavor migrates toward the flavor side
  until "tmux-msg" reduces to a SQLite store + pane registry. At
  that point a supersession with a narrower substrate scope is
  warranted.
- **A future CLI-tool ecosystem ships native equivalents.** If
  Claude Code, Codex, Copilot, etc. converge on built-in
  inter-agent IPC (akin to MCP-over-bus), the substrate-on-tmux
  approach retires in favor of the native mechanism. The
  substrate's value is in the absence of a standard; standards
  obsolete substrates.
- **The substrate's scope inverts.** If most of what `tmux-msg`
  does turns out to be Claude-Code-specific in practice (despite
  the architectural intent), the substrate-vs-flavor separation
  is aspirational rather than load-bearing. Retract via
  superseding ADR; rename back or rename again to reflect the
  actual scope.

The watch: track every PR that adds a feature gating on consumer
identity. The first **structurally-distinct case** is acceptable
(one heuristic doesn't break the boundary); the second
**structurally-distinct case** triggers ADR-0003 amendment to either
codify the per-consumer-strategy pattern or retract the boundary.

The "structurally-distinct" qualifier is load-bearing: adding more
consumers to an existing per-consumer switch (e.g., extending a
chrome heuristic from `claude` to also handle `codex` with the
same branch shape) is the *same* structural case repeated, not a
second one. The trigger fires when a *new shape* of consumer-aware
branching surfaces — e.g., chrome detection is the first case;
per-consumer paste-format adaptation would be the second. This
mirrors the symmetric promotion-threshold framework (n=2+
structurally-distinct cases warrant pattern extraction); applied in
reverse for a violation-detection threshold.

## References

- Issue #97 (rename trigger — "substrate-class accuracy correction")
- Issue #109 (this ADR's tracking issue)
- PR #108 (implementation of the rename — the mechanical change
  this ADR justifies)
- `README.md` §Substrate vs CLI-tool-flavor (current voice of the
  substrate-vs-flavor framing — codified in the same operator
  conversation that produced this ADR)
- `CHANGELOG.md` `[Unreleased]` entry on the rename (worked example
  of the rename ceremony for any future project-name change)
- ADR-0001 (similar shape: framing-level decision recording a
  multi-alternative choice, not a code-level one)
- The operator's 2026-06-05 conversation on naming candidates
  (`claude-msg` / `tmux-mailbox` / `tmux-mailman` / `tmux-msg`) and
  scope options (A/B/C) — preserved in the quartermaster session
  transcript and Surveyor's pre-review notes on PR #108

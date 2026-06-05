# ADR-0004: MCP wire-surface naming (application of ADR-0003)

> **Status**: Proposed
> **Date**: 2026-06-05 (proposed)
> **Authors**: Quartermaster (author), operator (design calls on
> server name / tool prefix / control commands / migration shape),
> ADR-0003 (the principle this ADR applies)

## Context

ADR-0003 established the substrate-vs-flavor architectural
commitment: tmux is the substrate; `claude-msg` is today's
per-CLI-tool flavor; future siblings (`codex-msg`, `copilot-msg`)
are first-class scope.

PR #108 implemented the mechanical rename of the substrate
(`cli-semaphore` → `tmux-msg`) across the Go module path,
operational paths, code constants, README, install scripts, and
chamber-level docs. It did NOT touch the **MCP wire surface**
— the operator-visible interface that consumers (today: Claude
Code; future: Codex, Copilot) use to invoke substrate primitives
via the Model Context Protocol.

ADR-0003 §Decision (3) Option C enumerated the scope of the
mechanical rename: Forgejo + Go module path + operational paths +
chamber-level docs + repo-internal docs. The MCP wire surface was
neither explicitly included nor explicitly excluded — implicitly
deferred. ADR-0004 picks up that deferral and applies ADR-0003's
substrate-vs-flavor cut to the MCP wire layer with its own
sub-decisions.

The lingering `semaphore` symbols on the wire (operator-visible,
breaking-change footprint):

| Surface | Current | Count |
|---|---|---|
| MCP server name | `"semaphore"` | 1 — every chamber's `.mcp.json` references it |
| MCP tool method names | `semaphore.send`, `.agents`, `.whoami`, `.inbox`, `.message_status`, `.status`, `.register`, `.control`, `.unregister`, `.chamber_state` | 10 — every chamber's tool-call namespace |
| Control-command names | `mcp-restart-semaphore`, `mcp-enable-semaphore`, `mcp-disable-semaphore` | 3 — the whitelisted slash commands `semaphore.control` queues |
| `main.go` help text | "mirrors semaphore.control"; "Bulk-fire mcp-restart-semaphore" | — |

Plus internal Go symbols (~40 non-test mentions, ~110 total
including tests; not wire-visible): variable names, comments,
identifiers across `cmd/claude-msg/` and `internal/`. Same rename
pass, no wire impact; covered as
substrate-side cleanup hygiene in the implementation PR.

Under ADR-0003's framing, the current naming is wrong on two axes:
the server name and the tool prefix both perpetuate the original
`cli-semaphore` conflation. The wire surface needs to align with
the substrate's actual name (`tmux-msg`) — and the alignment has
its own design decisions worth ADR-grade record, distinct from the
principle ADR-0003 codified.

This ADR is **the first application of ADR-0003 to a specific
surface**. If future ADRs apply ADR-0003 to other surfaces (env-var
prefixes, file-naming conventions, log-source identifiers), the
"ADR-0003 application" shape becomes a pattern candidate. Today is
instance-1; anchor-before-pin discipline says wait for instance-2
before naming the pattern.

## Decision

Four sub-decisions, each alternative-bearing:

### (1) MCP server name: `tmux-msg`

The MCP server is registered by each per-flavor binary
(`claude-msg`, future `codex-msg`/`copilot-msg`). Under ADR-0003,
the server is registering **substrate primitives** for consumption
by a per-flavor binary — so the server name should be
**substrate-named**, not per-flavor-named.

Sibling binaries running in other chambers' panes register the same
server name (`"tmux-msg"`) in their own chamber's `.mcp.json`. MCP
server identity is one-per-name-per-`.mcp.json`, and `.mcp.json` is
per-chamber (per Claude Code session config), so the substrate-named
server presumes **one flavor binary per chamber config** — which is
the alcatraz operational reality. A hypothetical chamber wanting to
register two flavor binaries' substrate primitives in the same
`.mcp.json` would collide on the `"tmux-msg"` server name; that
configuration is out of scope here and would warrant its own
sub-decision if it ever surfaces.

**Per-flavor flavor-specific tools.** A future flavor binary that
needs flavor-specific MCP tools (e.g., a `codex-msg` exposing
Codex-only transcript-handling primitives) registers a **second**
MCP server in the same `.mcp.json`, named per-flavor (`"codex-msg"`,
`"copilot-msg"`, etc.). The substrate-named `"tmux-msg"` server
carries substrate primitives; the per-flavor server carries
flavor-specific tools. The two servers co-exist in one `.mcp.json`
without conflict because they have distinct names. This preserves
the substrate-vs-flavor cut at the wire layer: same server name
across all flavors for substrate primitives; per-flavor server name
for whatever each flavor needs on top.

### (2) Tool method prefix: `tmux-msg.<verb>`

Tool method names use the full substrate prefix:

- `tmux-msg.send`, `tmux-msg.inbox`, `tmux-msg.whoami`,
  `tmux-msg.agents`, `tmux-msg.message_status`, `tmux-msg.status`,
  `tmux-msg.register`, `tmux-msg.unregister`, `tmux-msg.control`,
  `tmux-msg.chamber_state` (one-to-one rename from the existing
  `semaphore.*` set).

The prefix is intentionally redundant with the server-name segment
in Claude Code's MCP namespace (`mcp__tmux-msg__tmux-msg_send`).
The redundancy buys:

- **Self-describing names outside the MCP context.** TOML config
  references, control commands, scripted shell invocations, doc
  prose, log entries, and grep targets all see the prefixed name
  in isolation (no server-name segment to disambiguate). `tmux-msg.send`
  reads correctly anywhere; bare `send` would be ambiguous.
- **Symmetry with the existing pattern.** Today's `semaphore.X`
  shape becomes `tmux-msg.X`; consumers, docs, and tooling
  pre-MCP-rendering see the same shape they already understand.
- **Grep specificity.** `grep tmux-msg\.` finds substrate method
  references unambiguously; `grep send` would have catastrophic
  false-positive rate.

### (3) Control-command names: `mcp-<verb>-tmux-msg`

The whitelisted slash commands that `semaphore.control` queues are
renamed to mirror the server name:

- `mcp-restart-semaphore` → `mcp-restart-tmux-msg`
- `mcp-enable-semaphore` → `mcp-enable-tmux-msg`
- `mcp-disable-semaphore` → `mcp-disable-tmux-msg`

The control commands cross the MCP server-name boundary (they're
Claude Code slash commands that target a specific MCP server
identity), so they must match whatever the server is named.

### (4) Migration: hard cutover

Every chamber's `.mcp.json` updates in one operational window;
every Claude Code session restarts to pick up the new server +
tool names. No alias period, no dual registration, no feature flag.

The scope is small: a single tmux instance on alcatraz with six
chambers. The carry cost of alias-period machinery (dual
registration logic, deprecation messaging, eventual cleanup PR)
outweighs the convenience of incremental migration when the
operational window is ~5 minutes of bus-quiet.

**Restart sequencing.** The restart is **operator-side per chamber**
(manual `/mcp restart` or Claude Code session restart), not via the
renamed `mcp-restart-tmux-msg` macro. The control-command rename
under decision (3) means the restart macro itself is being renamed
in the same cutover; it cannot drive its own deprecation. The
operator coordinates the restart sequence chamber-by-chamber after
the new binary lands and `.mcp.json` files are updated.

## Alternatives considered

### Server name

- **Per-flavor (`claude-msg`, `codex-msg`).** Rejected: same
  conflation in a different direction. The MCP server exposes
  **substrate primitives**, not flavor-specific operations.
  Per-flavor server names would imply substrate identity is
  per-binary, violating ADR-0003's substrate-vs-flavor cut.
  Operationally, multi-flavor chambers in the same tmux instance
  would each register a separate server, fragmenting consumers'
  awareness of substrate availability.
- **Verbose alternatives** (`tmux-msg-bus`, `tmux-message-bus`,
  `tmux-msg-server`). Rejected: longer without clarity gain. The
  bare substrate name is the right level of abstraction.
- **No rename (keep `semaphore`).** Rejected: perpetuates the
  original conflation that ADR-0003 explicitly diagnosed.

### Tool method prefix

- **`msg.<verb>`** (short, generic). Rejected per operator: too
  generic, collision-prone with other MCP tools (a future
  `claude-mail` or `slack-msg` MCP server could plausibly use
  the same prefix). The redundancy of `tmux-msg.send` in the
  rendered tool name is a small cost; the collision risk of a
  bare-`msg` prefix is real.
- **`tm.<verb>`** (short, substrate-flavored abbreviation).
  Rejected: same shape as `msg.<verb>` — saves characters at the
  cost of self-describing-ness. `tm.send` is opaque in non-MCP
  contexts in a way `tmux-msg.send` is not. Both short-prefix
  alternatives are rejected at decision time for the same
  reasoning; if the prefix-friction watch ever triggers (§What
  would change the decision), either could become worth
  reconsidering.
- **No prefix (`send`, `inbox`, etc.)** Rejected: while MCP
  already namespaces via server name, leaving methods unprefixed
  makes them ambiguous in non-MCP contexts (TOML, shell, docs).
  Self-describing tool names matter at the cost of slight
  redundancy in the rendered MCP namespace.
- **Mixed prefix (some prefixed, some not).** Rejected: inconsistent
  shape is worse than either fully-prefixed or unprefixed. Choose
  one rule and stick to it.

### Control-command names

- **Binary-named (`mcp-restart-claude-msg`).** Rejected: the
  controls target the MCP server, which is substrate-named per
  decision (1). Binary-named controls would suggest there's a
  per-flavor MCP server (which there isn't).
- **Keep `mcp-restart-semaphore`.** Rejected for the same conflation
  reasons as the server name.

### Migration

- **Alias period** (both names registered for one minor version,
  with deprecation log on the old prefix). Rejected: the alias
  machinery's carry cost (dual registration, deprecation logic,
  follow-up cleanup PR) outweighs the convenience at a
  single-tmux-instance / six-chamber scale.
- **Feature-detection flag** (`legacy_names: true` in config to
  re-enable old names). Rejected: adds permanent surface area
  for a transient need. The flag would either be on forever
  (defeating the purpose) or removed in a follow-up PR (same
  carry cost as alias period).
- **No migration** (leave the rename for some future v1.0). Rejected:
  the conflation cost grows with project maturity; every chamber
  reading its own MCP tool calls is re-confronted with the old
  name. Cheaper to rename now (six chambers' worth of `.mcp.json`
  + Claude Code restart) than later (more chambers + more
  embedded references + harder operator coordination).

## Consequences

### Cleaner

- **Wire surface aligns with substrate identity.** Server name =
  repo name = wire prefix. Reading any of them, the reader knows
  what they're talking to (the substrate, not a specific consumer).
- **Sibling binaries inherit naming for free.** A future `codex-msg`
  registers the same MCP server name (`"tmux-msg"`) in its own
  chamber's `.mcp.json`; same tool methods (`tmux-msg.send`, etc.);
  same control commands. No per-flavor MCP namespace divergence.
- **Out-of-MCP-namespace contexts self-describe.** TOML config,
  shell scripts, control commands, doc prose, log entries, grep
  targets all see `tmux-msg.*` in isolation. No "wait, which
  `send`?" ambiguity.
- **Future ADR-0003-application ADRs have a precedent.** The shape
  of this ADR (per-decision alternatives, hard-cutover rationale,
  what-would-change-the-decision watch) is reusable for the next
  surface that needs the framing applied.

### Harder

- **Operational migration window.** Every chamber updates
  `.mcp.json` + restarts Claude Code in one window. Estimated ~5
  minutes of MCP-bus-quiet across all six chambers. Operator-side
  coordination ask: pause non-essential cross-chamber work during
  the cutover.
- **Recorded session transcripts and memory entries become
  historically-out-of-sync.** Existing memory entries citing
  `semaphore.send` etc. remain accurate-to-time but won't be
  replayable as live tool calls. Forward-tracking: memory entries
  written post-cutover use the new names; pre-cutover entries are
  preserved as-is.
- **Bridge period for external docs / mental models.** Operators
  with `semaphore.X` in muscle memory need a beat to update.
  Acceptable cost; muscle memory updates fast under daily use.
- **One-time PR overhead.** A dedicated implementation PR sweeps
  the server name, 10 tool methods, 3 control commands, ~40
  non-test Go symbols, and the help text. Scoped enough to be
  bounded; large enough to warrant its own review window.

## Precedent on ADR-0003 amendments by application ADRs

ADR-0003 §Substrate enumerates substrate primitives by their
current wire names at the time of writing — e.g., the control
surface is named as `semaphore.control` (line 87). After ADR-0004
implementation, that primitive is wire-named `tmux-msg.control`,
making ADR-0003's enumeration accurate-to-time rather than current.

**ADR-0004 sets the precedent: ADR-0003 stays frozen.** Application
ADRs (this one, and any future ADR that applies ADR-0003's framing
to a specific surface) do NOT amend ADR-0003. Future readers of
ADR-0003 should treat in-text references to specific wire names as
accurate-to-time; the current voice for any specific surface lives
in the application ADR that addresses that surface (this one for
the MCP wire layer).

**Why frozen:** ADR-0003 §Decision (3) explicitly named historical
ADRs 0001 and 0002 as immutable records, citing temporal context
preservation. Applying the same discipline to ADR-0003 itself
means application ADRs don't recursively touch their parent —
keeping the amendment cost bounded as more application ADRs land
over time. The alternative (application ADRs do amend their
parent) creates a recursive amendment pattern where any new
substrate-surface coverage touches every prior ADR mentioning that
surface; cost grows quadratically with ADR count.

**Cost:** ADR-0003 becomes incrementally less surface-accurate
over time. A reader following ADR-0003's references finds the
current voice in successor ADRs; the cost is the one extra hop.
Acceptable in exchange for bounded amendment scope.

**Scope of "frozen":** the discipline applies to substantive ADR
content — §Context, §Decision, §Alternatives, §Consequences. Index
metadata (ADR-0003's row in `docs/adr/README.md`'s index table)
remains live state and can update for status flips, supersession
markers, etc. without violating the frozen-substance discipline.

## What would change the decision

Reasons to retract or supersede ADR-0004:

- **Sibling CLI tools surface fundamentally different MCP wire
  shapes.** If `codex-msg` or `copilot-msg` need a different
  substrate cut (e.g., a fundamentally different tool method
  surface), the server-name-equals-substrate decision retracts
  — the surface would be per-flavor by necessity. Today's
  expectation is that the substrate primitives translate; if
  that turns out wrong, this ADR retracts in favor of per-flavor
  MCP server names.
- **MCP gains native server-aliasing.** If a future MCP spec lets
  a server register N names with one canonical and N-1 aliases,
  the hard-cutover constraint relaxes — future renames can ship
  with an alias period at no carry cost. This ADR's migration
  shape (decision 4) retracts in favor of aliased rename whenever
  it next happens.
- **`tmux-msg.<verb>` causes real friction at scale.** If the
  prefix proves to clutter tool-call autocomplete, copy-paste,
  or transcript-readability badly enough that operators are
  routinely abbreviating it, one of the §Alternatives' rejected
  short-prefix shapes (`msg.<verb>`, `tm.<verb>`) or the no-prefix
  alternative becomes worth reconsidering. The watch threshold:
  instance-2 of prefix-friction as a **structurally-distinct**
  complaint shape (different friction mode, not the same complaint
  repeated), mirroring ADR-0003 §What would change the decision's
  amendment trigger. Hold the structurally-distinct qualifier to
  the same bar as in ADR-0003.
- **The MCP wire surface stops being load-bearing.** If chambers
  migrate to a different inter-process mechanism (e.g., native
  CLI-tool IPC obsoletes MCP-over-stdio for this use case), the
  wire surface this ADR governs retires; ADR-0004 retracts when
  there's nothing left to name.

The watch: the prefix-friction trigger is the most likely
amendment cause. Track operator + chamber feedback on tool-call
shape for the first ~one month post-cutover. If no
structurally-distinct complaints surface in that window, the
prefix shape is durable.

## References

- ADR-0003 (`docs/adr/0003-substrate-vs-flavor-naming.md`) — the
  principle this ADR applies
- #97 — original rename trigger
- #108 (`c36192b`) — mechanical rename PR (substrate-side; left
  the MCP wire surface for this ADR)
- #112 — this ADR's tracking issue (operator's design calls
  captured verbatim in the issue body)
- README.md §Substrate vs CLI-tool-flavor (current voice of the
  boundary that this ADR applies to a specific surface)
- Operator's 2026-06-05 conversation on the four sub-decisions
  (server name / tool prefix / control commands / migration shape),
  preserved in the quartermaster session transcript

# Trust boundaries + threat model

> **Status: DRAFT.** Structural review by Surveyor complete
> (issue #35, comment 58666); this version applies the S1-S5 reshape
> proposals + the two sweep findings. Promoted out of DRAFT when
> #35 closes.
>
> Surveyor's three cross-project review rounds (the original
> #28/#29/#30/#31 series on the message bus, plus the v0.2.1
> Q(a)/Q(b) pass) carry most of the structural framing this doc
> makes explicit; the response commits — `20d7c33`, `5178a81`,
> `3e16ba2`, `4c6171f` — are the durable artifacts of those rounds.

## TL;DR — the trust model in one paragraph

tmux-tell is designed for a **single-operator homelab** trust
model: one human (the operator) has shell access to one host
(alcatraz). Anything that has shell access is fully trusted. The
bus enforces caps, scope rules, and atomicity for the operator's
benefit (catching their own bugs, preventing prompt-injected agents
from cascading damage); it does NOT authenticate identity, defend
against external attackers, or attempt cryptographic integrity.
Deploying outside this model requires reopening every design
decision marked **load-bearing** in Section 3.

## 1. Trust boundaries

Mapped against the code as of v0.2.1.

### 1.1 Operator ↔ Bus

```
┌───────────┐  shell access (sudo, file I/O, systemd)  ┌──────────────┐
│ Operator  │ ───────────────────────────────────────► │     Bus      │
│           │                                          │ (mailmen +   │
│           │ ◄─────────────────────────────────────── │  store +     │
└───────────┘  journalctl, sqlite3, tmux-tell-claude CLI     │  MCP server) │
                                                       └──────────────┘
```

- **What's trusted**: anything the operator can do via shell. This
  includes direct INSERT into `messages.db`, killing/restarting
  mailmen, editing whitelist source code, rewriting agent
  registrations.
- **What's enforced**: nothing on this boundary. The bus has no
  authority over the operator.
- **What would break the model**: operator account compromise (SSH
  key theft, malware on the operator's workstation). Out of scope —
  see §2.

### 1.2 Bus ↔ Agents (Claude Code sessions in tmux panes)

```
┌──────────────┐  MCP stdio / tmux-tell-claude CLI  ┌────────────────────┐
│   Bus        │ ───────────────────────────► │  Agent (Claude     │
│              │                              │  Code session)     │
│              │ ◄─────────────────────────── │  in tmux pane      │
└──────────────┘  send-keys / paste-buffer    └────────────────────┘
```

- **What's trusted**: the agent's MCP child process inherits
  `$TMUX_PANE` from tmux, which the bus uses for identity
  resolution.
- **What's enforced**:
  - Identity precedence: explicit override (`--from`,
    `$TMUX_AGENT_NAME`) → `$TMUX_PANE` → agents registry. See
    `internal/identity/identity.go`.
  - Per-recipient queue cap (default 5), per-(sender, recipient)
    backlog cap (default 2 — scoped per recipient since #296, so one
    slow consumer can't block a sender's traffic to others), body
    size cap (16 KB). Atomic in `InsertMessage` since #29.
- **What's trusted but acknowledged**: `$TMUX_PANE` is shell-
  settable. Any process the operator has shell access for can fake
  any agent identity. This is fine under the trust model — shell
  access IS trust — but is the load-bearing assumption that breaks
  hardest when the model widens.

### 1.3 Agent ↔ Agent (peer messages + peer control)

```
┌──────────┐                 Bus                  ┌──────────┐
│ Sender   │ ───► tmux-tell.send ─► mailman ─────► │ Receiver │
│ Agent    │                                      │ Agent    │
│          │ ───► tmux-tell.control (peer-scope) ─►│          │
└──────────┘                                      └──────────┘
```

- **What's enforced**:
  - Whitelist: only the commands in `internal/control/control.go`'s
    `Allowed` map can be peer-sent. Each command's `Peer` flag
    gates whether it's reachable as a peer command at all (compact,
    cost, mcp-disable are self-only; rename, help, mcp-enable,
    mcp-restart are peer-allowed).
  - Sentinel-based macro: `mcp-restart-tmux-msg` peer-invokable
    macro synthesises a disable+enable pair atomically (#28). Raw
    `mcp-disable-tmux-msg` is self-only specifically to prevent a
    prompt-injected peer from denying-of-service another agent's
    bus connection.
  - Silent-drift detection (#37): before delivery, the mailman
    confirms the registered pane is still running the expected
    agent's `--resume` value. Mismatch → reroute (recoverable) OR
    `MarkFailed` (default since v0.2.1 per the autonomous-receiver
    threat model articulated in Surveyor's Q(b) review).
- **What's trusted**: the recipient agent's prompt-injection
  resistance. The bus passes message bodies through unchanged; if
  the body contains an instruction the recipient acts on, that's a
  recipient-side problem.

### 1.4 MCP handler ↔ Store

```
┌──────────────────┐    SQL    ┌──────────────────┐
│  MCP handler /   │ ────────► │  store           │
│  CLI subcommand  │           │  (BEGIN          │
│  (policy layer)  │ ◄──────── │   IMMEDIATE      │
└──────────────────┘  rows /   │   transactions)  │
                      errors   └──────────────────┘
```

- **What's trusted**: the handler's policy enforcement. The store
  trusts whatever the handler inserts as long as the
  schema-invariants hold (FK references resolve, non-empty
  required fields, etc.). This is the trust boundary Surveyor
  named in the #28 Q1 review: the `mcp-restart-tmux-msg` macro
  bypasses the per-row whitelist scope check on the inner inserts
  because the handler has already authorized the macro.
- **What's enforced**:
  - Schema invariants (non-empty body, FK on `reply_to`, etc.)
  - Atomic cap enforcement via `_txlock=immediate` since #29.
  - Cross-canonical alias collision detection since v0.2.1 (#38 + Q(a)).
- **Don't be tempted to**: revalidate handler output in the
  store. Doing so would break the macro pattern and put the trust
  boundary in the wrong place.

## 2. Threat model

Explicit framing of the assumed adversary surface for the
single-operator homelab deployment on alcatraz.

### 2.1 Not in scope

- **External network attackers.** The bus binds to localhost +
  unix sockets; tmux is host-local; SQLite is a file. Network
  reach requires shell access first.
- **Curious siblings / household members.** Single operator;
  alcatraz is not multi-tenant.
- **Supply-chain attacks on Go modules.** Out of scope at this
  scale; mitigated by `go.sum` pinning but not actively defended.
- **Side-channel attacks** (timing, cache, etc.). Not relevant for
  a homelab message bus.

### 2.2 In scope

- **A compromised agent** (prompt injection turning Claude against
  its peers). The whitelist scope rules and per-message caps bound
  the damage; cap-bounded peer noise is annoying, not catastrophic.
- **A misconfigured tool** (flooding the queue with messages or
  control commands). Caps catch this; v0.2.1's fail-loud default
  on drift-ambiguous reduces the silent-cascade risk.
- **A buggy mailman** crashing the others. Each mailman runs in a
  separate systemd-user service; systemd's `Restart=on-failure`
  with `WatchdogSec=30s` recovers from crashes without operator
  intervention.

### 2.3 Acknowledged but accepted

- **`$TMUX_PANE` spoofing.** Anyone with shell access can fake any
  agent identity by setting `$TMUX_PANE` to a registered pane's id
  before calling `tmux-tell-claude send`. Accepted under the trust
  model; this is the **single largest item** a future operator
  considering wider deployment must revisit.
- **No body content scanning.** A compromised agent CAN send
  arbitrary message bodies. The recipient's prompt-injection
  resistance is the boundary, not the bus.
- **No rate limiting beyond the 5/2 caps.** A compromised agent
  could repeatedly send-then-wait-for-delivery as a slow trickle.
  Caps prevent burst; nothing prevents sustained low-rate noise.
  Recorded as a known limitation in Q1B of the #28 review.
- **No cryptographic integrity on message bodies.** A
  shell-access process could `UPDATE messages SET body = ?` to
  rewrite an in-flight message before delivery. Not defended
  against; not a meaningful threat under the single-operator
  trust model.

## 3. Load-bearing assumptions

For each design choice that's safe ONLY under the single-operator
homelab trust model, name it explicitly. Surveyor's cross-project
review threads (on the message bus) plus Admin's response commits
cited in §6 are the audit trail; this section makes them load-bearing
in one place.

> **Closed register as of v0.2.1.** The five entries below are the
> complete set of load-bearing assumptions, not a sampling. Adding a
> sixth requires a new §3.N subsection AND a cross-reference from §1's
> relevant boundary diagram (in the same PR). PRs that touch
> trust-boundary code must update §3 if they introduce a new
> load-bearing assumption.

### 3.1 Whitelist source-code edits as the only control-command boundary (#24, #25, #28)

- **Code**: `internal/control/control.go` — `Allowed` map literal.
- **Trusted**: only the operator can edit + push + deploy. Nobody
  but the operator can add commands.
- **What would break it**: multi-developer commit access; per-host
  binary distribution outside the operator's control; in-process
  hot-reload of the whitelist from a config file.
- **What would change for non-homelab**: the whitelist would need
  to come from a signed policy file with operator-explicit grant
  per command, or from a per-agent capability ACL. Not a small
  change.

### 3.2 `$TMUX_PANE` → registry as identity (#27)

> **This is the load-bearing assumption that the rest of §3 implicitly
> composes on top of.** Removing the homelab trust model means
> replacing this assumption; the other four §3 entries either depend
> on it (3.1, 3.4) or coexist independently (3.3, 3.5) but become
> qualitatively different in the new identity model. §1.2, §2.3, and
> §4 all single this out — §3.2 is where the dependency tree is named.

- **Code**: `internal/identity/identity.go` — `Resolve` function.
- **Trusted**: tmux + the operator-managed registry. Spoofable by
  anyone with shell access.
- **What would break it**: multi-user shell access on the same
  host; agent processes running as a different UID than the
  operator.
- **What would change for non-homelab**: replace with an actual
  authentication mechanism — per-agent secret, mTLS, or token
  binding. The identity helper's API is small enough that a
  pluggable backend would fit cleanly, but the semantics of
  "who's the sender" change qualitatively.
- **Storage trust-boundary congruence (#308)**: the DB now lives
  under the operator's user-home (`$XDG_DATA_HOME/tmux-tell` or
  `~/.local/share/tmux-tell/messages.db`), not the former system-global
  `/var/lib/tmux-msg/`. This makes the substrate's *storage* trust
  boundary exactly congruent with the *identity* model above: both are
  scoped to the operator's UID. tmux is per-user by design, and the bus
  now matches — the DB is readable/writable only by the operator's UID
  by virtue of its location, with no install-time chown to align
  ownership and no shared-space path for a second UID to reach. A
  sandbox-by-default adapter (codex) can write it without per-write
  escalation precisely because it's under the user's own home. The
  per-user assumption is now structural, not maintained by a deploy step.

### 3.3 `_txlock=immediate` + `busy_timeout(5000)` for cap enforcement (#29)

- **Code**: `internal/store/store.go` — DSN PRAGMA setup.
- **Trusted**: SQLite's `RESERVED`-lock contract, not application-
  level discipline.
- **What would break it**: switching to a different SQL backend
  without `BEGIN IMMEDIATE` semantics; lifting `SetMaxOpenConns(1)`
  for some kind of "performance" without tracing where atomicity
  is assumed.
- **What would change for non-homelab**: probably moves to a
  server-side database; `BEGIN IMMEDIATE` semantics are common to
  PostgreSQL / MySQL but the cap-check pattern needs re-pinning
  (no longer relying on connection-pool serialisation as a
  fallback).

### 3.4 No body-content scanning

- **Trusted**: agents are internally trusted (Claude Code instances
  under the operator's account); a compromised agent CAN send
  arbitrary content but the recipient's prompt-injection resistance
  is the boundary.
- **What would break it**: agents running adversarial models;
  external senders without operator vetting.
- **What would change for non-homelab**: a content-policy layer
  (allowlist of safe MIME types, body-size policies per
  recipient, etc.). Sized as its own subsystem, not a hook.

### 3.5 Mailman-as-single-writer-per-recipient

- **Code**: systemd template `tmux-tell-claude-mailman@.service`; one
  instance per agent.
- **Trusted**: systemd ensures exactly-one-instance; the bus
  doesn't enforce single-writer at the store level.
- **What would break it**: running two mailmen for the same agent
  manually; a buggy systemd unit that allows multiple instances.
- **What would change for non-homelab**: probably moves to a
  store-level mutex or leader-election; not a small change.

## 4. What would change for non-homelab deployment

Concrete checklist if someone wanted to deploy beyond single-
operator. NOT a roadmap — a scoping aid so the cost of widening
the trust model is honest.

**Sizing scale**: **Small** ≈ single PR; **Moderate** ≈ one
milestone; **Substantial** ≈ one phase / major version; **Large** ≈
multi-version effort. Sizes are intentionally cadence-relative so
adopters can map them to their own project's release rhythm.

The split below tells the architecture of widening, not just the
cost. §4.1 entries gate the others — without authentication and the
identity surface around it, nothing in §4.2 is meaningful. §4.2
entries can land in any order, independently.

### 4.1 Foundational (gate the others)

These three compose into "agent identity is real". They need to
land together or in close sequence; nothing in §4.2 is meaningful
without them.

| Domain                     | Today (homelab)                                                       | For non-homelab                                                                    | Sizing      |
|----------------------------|-----------------------------------------------------------------------|------------------------------------------------------------------------------------|-------------|
| Authentication             | `$TMUX_PANE` → registry                                               | Per-agent identity token (secret, mTLS, or signed JWT)                             | Substantial |
| Sender identity guarantee  | `$TMUX_PANE` (trivially spoofable with shell)                         | Cryptographic binding between Claude session and identity token                    | Large       |
| Authorization              | Binary self/peer scope on whitelist                                   | Per-tool capability ACLs ("agent X may call tmux-tell.control on agent Y")         | Substantial |

### 4.2 Independent (can land separately)

These don't gate one another and don't gate §4.1. Any subset can
land before, after, or alongside the foundational pass.

| Domain                     | Today (homelab)                                                       | For non-homelab                                                                    | Sizing      |
|----------------------------|-----------------------------------------------------------------------|------------------------------------------------------------------------------------|-------------|
| Auditing                   | journalctl                                                            | Structured audit log with tamper-evidence (append-only + signed)                   | Moderate    |
| Rate limiting              | Per-call slot caps (5/2)                                              | Per-sender frequency caps + sliding-window limits                                  | Small       |
| MCP tool scope             | "If you can call MCP, you can call any tool"                          | Per-tool ACLs at the MCP boundary                                                  | Moderate    |
| Body content policy        | None                                                                  | MIME / pattern allowlist; per-recipient size limits                                | Moderate    |
| Whitelist provenance       | Source-code edits                                                     | Signed policy file with operator-grant ceremony                                    | Moderate    |
| Cross-host deployment      | Single tmux server, local SQLite                                      | Networked store + per-host mailmen + multi-host pane addressing                    | Large       |

Authentication is the gate within §4.1; sender-identity guarantee
and authorization are sized to land alongside it but lack meaning
without it. Cross-host deployment in §4.2 technically composes on
top of authentication but doesn't gate anything else, so it sits in
the independent table to keep §4.1 minimal.

## 5. Open questions for future review

These are **watched but not actively tracked** — file a Forgejo
issue if any becomes operationally relevant. Recording them here
keeps the implicit-TODO ambiguity out of §1-§4 (which IS load-bearing
and gets updated in lockstep with code).

- **Control whitelist text-vs-sentinel ambiguity.** The
  `mcp-restart-tmux-msg` macro resolves to text
  `"/mcp restart tmux-msg"` that's never actually typed — it's a
  sentinel for the handler to dispatch the macro. If a future
  whitelist edit accidentally makes it typeable (e.g. adds a
  `IsTyped bool` field default-true to `Command`), the recipient
  gets gibberish input. Should the macro path use a dedicated
  `Kind: Macro` field rather than a sentinel string?
  Surveyor's #28 review touched this briefly but didn't push for a
  change; worth revisiting if a second macro lands.
- **Cross-process race protection.** The store's cap enforcement
  is correct under SQLite's `BEGIN IMMEDIATE` semantics, but the
  in-process test (`messages_concurrent_test.go`) is honest about
  not exercising real cross-connection contention. Filed as #33.
- **`store.AddAlias` TOCTOU on cross-canonical collision check.**
  Documented in `CHANGELOG.md`'s `[Unreleased]` Known Limitations
  section. The check reads outside the UPDATE transaction;
  `_txlock=immediate` shrinks the window to microseconds and
  single-operator reality makes it theoretical, but worth
  tightening if concurrent register becomes real.
- **No deprecation policy yet.** Pre-1.0 we can rename / remove at
  will (within the patch/minor cadence rules documented in
  `CHANGELOG.md`). Post-1.0 a deprecation policy is the prerequisite
  for the 1.0 commitment to mean anything. Tracked in the 1.0
  trigger criteria.

## 6. References

- Surveyor's three review rounds:
  `20d7c33` (#28 Q1-Q4), `5178a81` (#30/#31 Q(a)-(d) + omitempty
  pin), `3e16ba2` (#29 linkP2ToP1 precondition), `4c6171f`
  (v0.2.1 Q(a) alias collision + Q(b) fail-loud).
- Companion docs: `failure-modes.md` (incident audit trail with
  what-would-catch-it-earlier).
- Configuration: `internal/store/store.go` (DSN +
  `SetMaxOpenConns(1)`), `internal/control/control.go` (whitelist),
  `internal/identity/identity.go` (precedence).

## Glossary

| Term                     | Meaning                                                                            |
|--------------------------|------------------------------------------------------------------------------------|
| Trust model              | The set of assumptions the design relies on (single operator, shell-access trust). |
| Trust boundary           | An interface across which trust changes (operator vs bus, handler vs store, etc.). |
| Load-bearing assumption  | A design decision that's only correct under the trust model. Naming them makes the cost of trust-model change explicit. |
| Sentinel                 | A value that signals a special code path rather than being interpreted literally. The `mcp-restart-tmux-msg` Text field is one. |

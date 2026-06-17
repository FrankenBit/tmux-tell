---
arc42-section: 10
revisit-triggers:
  - a quality goal gains or loses a substrate mechanism that realizes it
  - a new scenario is added (operator-discovered or empirical)
  - operator pivot on what "good" means for the project
---
<!-- last-reviewed: 2026-06-18 (Phase-1 PR-C canonical — #386 working session) -->

# §10 Quality Requirements

The five qualities tmux-tell is built to protect — ratified by the operator in
the #386 collaborative working session (2026-06-17). These are deliberately
substrate-honest goals, not enterprise-QAR: they fit tmux-tell's single-host,
paste-delivery shape (#386 §What-this-does-NOT-do).

The section follows the Arc42 three-part shape: **§10.1** names the goals,
**§10.2** decomposes each goal into measurable sub-attributes (the quality tree),
**§10.3** grounds them in concrete scenarios.

The register here is deliberately plain — the goals and scenarios are written
for any reader picking up the architecture spine, not only contributors sharing
the project's internal vocabulary.

## §10.1 Quality goals

1. **Truthfulness.** What the system reports about itself matches reality. If it
   says a message was delivered, it was. If it says an agent is paused, it is.

2. **Reliable, lossless message delivery.** Messages sent reach their recipient,
   unchanged. They don't vanish silently between send and delivery. When
   delivery fails, it fails loudly, not silently.

3. **Inspectability.** Operators can see what the system is doing and why, using
   normal CLI commands — without poking at the database directly.

4. **Approachability.** The system is easy to learn, easy to use, and easy to
   recover from when something goes wrong. Vocabulary is consistent. There are
   no hidden controls.

5. **Continuity across versions, maintained as a discipline.** Upgrading doesn't
   break what already works, and that continuity is held as a deliberate
   practice — deprecated features keep working through a documented runway with
   warnings before they're actually removed
   ([ADR-0008](../adr/0008-deprecation-policy.md)).

## §10.2 Quality tree

For each goal, the sub-attributes that decompose it into checkable dimensions.

### 1. Truthfulness

- **Reported state can be checked.** Every status the system claims —
  `delivered`, `queued`, `paused`, `working` — can be independently verified
  against actual behavior by a separate reader.
- **How a status was reached can be traced.** When the system reports
  something, an inspector can follow the trail of how it got there end-to-end,
  not just take it on faith.
- **Independent verification happens routinely.** When one observer makes a
  claim about the system, a second observer can independently confirm or refute
  it — and the system is built so this verification is a habitual practice, not
  an audit reserved for incidents.

### 2. Reliable, lossless message delivery

- **Messages get there.** Under normal conditions, a sent message reaches its
  recipient.
- **Messages don't vanish.** No silent loss between send and delivery. Messages
  persist durably through every stage (queue, deliver, deferred).
- **Message content stays intact.** What arrives matches what was sent
  byte-for-byte. No encoding or escaping corruption.
- **Failures are loud.** When delivery fails, the failure shows up as a named
  state with a log line — never silently. The honest `delivered_in_input_box`
  classification ([§8.2](08-cross-cutting-concepts.md)) is one instance.

### 3. Inspectability

- **State is queryable.** Agent state, message state, queue contents, and
  mailman activity are all visible through CLI commands.
- **Decisions are traced.** Every routing, scheduling, or gating decision
  leaves a searchable log line explaining why it happened.
- **Peers can see each other.** Within the trust model, an agent can check
  another agent's relevant state — whether its message was delivered, whether
  the recipient is registered — without operator help.

### 4. Approachability

- **Every control is discoverable.**
  `--help` and [docs/operator-manual.md](../operator-manual.md) cover every
  command. Nothing hidden requires tribal knowledge.
- **Vocabulary stays deliberately small and consistent — every new term has to
  earn its keep.** Terms like *sleep* and *pause* each mean exactly one thing.
  Deprecated terms warn when used (see Goal 5).
- **There's always a path back.** When the system's state diverges from what
  the operator expected, there's a clear way to inspect, diagnose, and fix —
  [docs/diagnostic-playbook.md](../diagnostic-playbook.md) is the index.

### 5. Continuity across versions

- **Deprecated features keep working through their runway.** Removed features
  get a warning period before they actually disappear, per
  [ADR-0008](../adr/0008-deprecation-policy.md). The runway currently extends
  through v1.0.
- **Identifiers survive renames.** Agent names and message public IDs that
  existed in one version still mean the same thing in the next; consumers'
  references don't break silently.
- **Upgrades don't lose in-flight work.** Schema changes preserve queued
  messages, paused flags, and deferred-trigger states through migration (#349).
- **Operators learn why a feature was deprecated.** Every deprecation warning
  points at the issue or ADR explaining the change (#509 set the pattern).

## §10.3 Quality scenarios

Concrete situations that exercise the goals. Each scenario has a *situation*
(what happens), a *response* (how the system handles it), and a *check* (how
we know the response was correct).

### Scenario 1 — Reported state matches reality *(Goal 1)*

- *Situation*: An operator queries the status of a message sent earlier. The
  system reports `delivered`.
- *Response*: The recipient can independently confirm receipt — what the
  substrate says happened actually happened.
- *Check*: For every state the system reports (`delivered`, `queued`,
  `paused`, `working`), a separate command verifies the same state from the
  recipient's perspective. 100% agreement between reported state and observable
  behavior.

### Scenario 2 — A second observer catches drift *(Goal 1)*

- *Situation*: One agent or one log line claims something about the system —
  say, that a peer is registered and accepting messages.
- *Response*: Another agent or another command can check the same claim
  independently, and either confirms it or surfaces the discrepancy with a
  named reason.
- *Check*: Every state claim has at least one independent verification path.
  Zero claims that can only be taken on faith.

### Scenario 3 — Recipient comes online after the message was sent *(Goal 2)*

- *Situation*: A sender queues a message for a recipient who isn't registered
  yet, or who is paused at send time.
- *Response*: The message stays in the queue and delivers automatically when
  the recipient becomes available. No operator intervention needed.
- *Check*: Zero messages dropped because the recipient wasn't ready at send
  time. Delivery happens within one mailman cycle of the recipient becoming
  available.

### Scenario 4 — Message content arrives intact *(Goal 2)*

- *Situation*: A sender queues a message with unusual content — large body,
  special characters, embedded markup, multiple newlines, non-ASCII text.
- *Response*: The message arrives at the recipient byte-for-byte identical to
  what was sent.
- *Check*: SHA-256 of received body equals SHA-256 of sent body, across 100%
  of messages including stress cases.

### Scenario 5 — Crash doesn't lose messages *(Goal 2)*

- *Situation*: The system is restarted (intentionally or due to a crash) while
  messages are queued and agents are paused.
- *Response*: After restart, the queue still contains every undelivered
  message that was in it at the moment of restart. Paused flags,
  deferred-trigger states, and other substrate state survive.
- *Check*: Cross-restart count check: queued-message count and paused-agent
  count match exactly before and after a clean restart. Mid-flight messages
  re-deliver under at-least-once semantics (WAL + `BEGIN IMMEDIATE`, #29).

### Scenario 6 — Stuck message diagnosis *(Goal 3, partly Goal 4)*

- *Situation*: An operator notices a message hasn't been delivered after
  several minutes and wants to know why.
- *Response*: A CLI command surfaces the gating reason in plain language —
  recipient is paused, mailman is down, rate-cap is engaged, deferral trigger
  hasn't fired, etc.
- *Check*: From "I notice something's off" to "I know why" takes at most two
  CLI commands. No need to read the database directly.

### Scenario 7 — Routing and scheduling decisions are traceable *(Goal 3)*

- *Situation*: A decision affects which message gets delivered when —
  priority-scheduled (#449), cap-gated by provider concurrency (#448), held by
  a deferral trigger, etc.
- *Response*: Each such decision leaves a searchable log line that names the
  decision type, the reason, and the affected message or agent.
- *Check*: For every category of decision the system makes, a `grep` on the
  logs surfaces the decision history. No silent decisions.

### Scenario 8 — Deprecated command lands gracefully *(Goal 4, Goal 5)*

- *Situation*: An operator or agent uses a deprecated command — say, `compact`
  after it has been renamed to `sleep` (#509).
- *Response*: The command still works (deprecation runway). The system emits a
  `WARN deprecated_*` log line naming the new command name and pointing at the
  issue or ADR that explains the change.
- *Check*: 100% of deprecated commands continue working through the documented
  runway. 100% of deprecation WARNs name both the new term and a rationale
  link.

### Scenario 9 — Upgrade preserves in-flight state *(Goal 5)*

- *Situation*: A new release introduces a schema change. At upgrade time there
  are queued messages, paused agents, and active deferral triggers.
- *Response*: The upgrade migration preserves all in-flight state. After
  restart, queued messages still deliver to the right recipients, paused
  agents stay paused, and deferral triggers still fire as scheduled.
- *Check*: Every row in the queued, paused, and deferred tables that existed
  before the upgrade is present and functional after. Zero state loss
  attributable to the migration.

## Coverage at a glance

| Goal | Scenarios |
|---|---|
| 1. Truthfulness | 1, 2 |
| 2. Reliable, lossless delivery | 3, 4, 5 |
| 3. Inspectability | 6 (recovery angle), 7 |
| 4. Approachability | 6, 8 (vocabulary angle) |
| 5. Continuity | 8 (deprecation), 9 (migration) |

Each goal has at least one core scenario; the higher-mechanism goals (delivery,
inspectability) get proportional coverage. Scenarios 6 and 8 deliberately cross
goals — they exercise the way two qualities interact at a real operator-facing
touchpoint.

## Provenance

The five goals, the quality tree, and the scenarios were ratified by the
operator in the #386 collaborative working session (2026-06-17). The seed that
preceded this content — the substrate-empirical starting list of issues,
mechanisms, and prior commitments that fed the session — is preserved in the
#386 issue history. The seed was the starting list, not the answer; the session
produced the canonical set above.

The register is plain-language by deliberate choice; the underlying
technical-register equivalents and the discipline-pin vocabulary they came from
live in working notes outside the architecture spine, so this section reads
cold for any reader picking up the Arc42 doc.

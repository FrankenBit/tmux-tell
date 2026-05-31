# Failure modes — what we got wrong in the first 48h post-MVP

> **Status: DRAFT.** Co-authored by Admin (audit trail) and Surveyor
> (discipline-pins framing) pending the lead/verify split agreed in
> bus message id 8117. Sections marked `_(Surveyor pass)_` await
> structural review.

## Why this doc exists

cli-semaphore shipped end-of-MVP on 2026-05-29 and within 48h
produced six production incidents. Each fix was small. The
cumulative pattern was "production discovered the assumption, we
patched." This doc distills the pattern — assumptions that failed,
what would have caught each earlier, what to invest in so the next
class of incident is observable instead of surprising.

The audit window: **2026-05-29 cf72ed (MVP cut) through 2026-05-31
c521515 (v0.2.1 + AddAlias TOCTOU note)**.

## 1. Incidents

Six incidents, in landing order. Each row's "Assumption that failed"
is the one-sentence version of the design decision that production
later overturned.

| # | Incident                                                                    | Assumption that failed                                                                              | Fix commit             | How we'd catch it earlier                                                                              |
|---|-----------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------|------------------------|--------------------------------------------------------------------------------------------------------|
| 1 | Watchdog SIGABRT on surveyor mailman (2026-05-30)                           | "Sleeps inside `tmuxio` are short enough that the systemd watchdog won't notice"                    | `5a0f0ee`              | systemd-integration test or an explicit ping-interval contract on the long-running call sites          |
| 2 | Probe accumulation #1 — every delivery hitting MaxWait cap (2026-05-30)     | "Bottom-N-rows capture isolates the input row from non-operator changes"                            | `b5e50d4` (=#32)       | Surveyor's structural review (Q(a)-2) caught it before this incident — closed loop                     |
| 3 | Unverified delivery dropping messages with 250ms retry budget (2026-05-30)  | "Claude Code surfaces a pasted message within 250ms"                                                | `510e74c`              | Observability: deliver-time histogram would have flagged the 99th percentile in pre-prod testing       |
| 4 | Enter-not-firing #1 (2026-05-30)                                            | "`send-keys Enter` immediately after `paste-buffer` reliably submits"                               | `f01c370`              | Empirical end-to-end test against a real Claude session, not a fake tmux runner                        |
| 5 | Watchdog SIGABRT on bosun (×4) (2026-05-31)                                 | "`sleepWithPing`'s short-sleep no-chunk path is too fast to need a watchdog ping"                   | `a7a0f25`              | Proportional thinking: bound max-no-ping window by analysis, not by intuition; defensive ping at end   |
| 6 | Probe accumulation #2 (bosun, 2026-05-31)                                   | "`cursor_y` points at the input box"                                                                | `a7a0f25`              | Adversarial test: simulate the rendering cursor parked outside the input row mid-tool-call             |
| 7 | Silent misdelivery to wrong agent (2026-05-31)                              | "If a pane exists at the registered id, it still belongs to the registered agent"                   | `fc89b22` (=#37)       | Pre-delivery cmdline check; shipped as the fix itself                                                  |
| 8 | Discover creates duplicate agent rows on long --resume names (2026-05-31)   | "discover's `--resume`-extracted names match the canonical registration names"                      | `f3c5d70` (=#38)       | Realistic seed data in discover tests: include long `--resume` names alongside short canonicals        |
| 9 | Alias collision silent-pick (2026-05-31, caught in review)                  | "Iterating canonicals in slice order and returning the first exact-match hit is fine"               | `4c6171f` (=v0.2.1 Qa) | Surveyor's v0.2.0 cross-project review (Q(a))                                                          |
| 10 | Drift-ambiguous + drift-unrecoverable both silently deliver (caught in review) | "WARN-and-deliver is safer than MarkFailed for ambiguous drift"                                 | `4c6171f` (=v0.2.1 Qb) | Surveyor's v0.2.0 cross-project review (Q(b))                                                          |

Notes:
- Incidents 1, 5 are the same class (watchdog timing); the v0.2.0
  fix for 5 generalized the v0.1.0 fix for 1 with a defensive
  always-ping-on-exit guard.
- Incidents 2, 6 are the same class (input-row identification); #32
  was a structural fix that didn't fully close the gap, and the
  Bosun rendering-cursor case (incident 6) forced the simpler
  probe-position-based identification we should have had from the
  start.
- Incidents 9, 10 are the only ones that hit the audit trail *via
  Surveyor's review* rather than production observation — meaningful
  data point on review-vs-production coverage.

## 2. Pattern observations

### 2.1 Caught-by-review vs caught-in-production

Of the 10 issues above, **eight** were discovered by operator
observation in production and **two** by Surveyor's cross-project
review (the v0.2.0 Q(a) and Q(b) findings). Counter to my prior
intuition that "structural review is the cheap fix", review actually
caught proportionally fewer issues than production — but the two it
caught were the ones with the highest blast radius (silent-bad-
delivery to autonomous receivers). Review's role isn't volume; it's
*reach*.

### 2.2 What signal would the journalctl WARN baseline have given us?

Each fix added a structured log line (`quiet_check_err`,
`delivered_unverified`, `drift_detected`, `drift_check_ambiguous`).
None of them were instrumented as *metrics* at the time of the
incident. A rate-of-WARN-per-mailman counter, exposed via a new
`claude-msg health` or in `status`, would have surfaced incidents
3 and 7 within minutes rather than hours.

### 2.3 Obvious in retrospect vs structurally non-obvious

- **Obvious in retrospect** (would have been caught by a careful
  pre-MVP review): 1, 4, 5. These were timing assumptions that don't
  survive contact with real Claude Code's TUI rendering pipeline.
- **Structurally non-obvious** (a clever reviewer might catch, but
  most wouldn't): 2, 3, 6, 7. Input-row identification, cursor-y
  semantics, post-Enter visibility timing — these required the
  specific Claude Code TUI model to be in head.
- **Discoverable only via the review process** (genuinely subtle):
  9, 10. Alias collision required a specific cross-canonical seed
  scenario; fail-loud-vs-soft policy required articulating the
  autonomous-receiver framing.

The non-obvious + review-only categories together suggest that the
project's actual robustness ceiling is not "no incidents" but
"incidents caught early enough that the WARN trail tells the story."
Investing in **observability** (metrics, log-rate dashboards) pays
better than investing in pre-prod hardening at this maturity level.

## 3. Monitoring / observability recommendations

_(Surveyor pass)_

Filed as separate issues, ordered by expected operator pain
reduction:

- [ ] **Deliver-time histogram per recipient** (95th / 99th
  percentile). Exposed via `claude-msg status` or a new
  `claude-msg metrics`. Would have flagged incident 3 immediately.
  Cost: small; the timestamps are already on the `messages` table.
- [ ] **Per-verdict count from `WaitForQuietPane`** (DeltaQuiet /
  InputActivity / TUINoise / ProbeMissing). High ProbeMissing rate
  is a strong signal of input-row identification failures (would
  have caught incident 6 within an hour, not a day).
- [ ] **Crash counter per mailman.** systemd already tracks restart
  count; surface in `status`. Would have flagged incidents 1 and 5
  the moment they happened.
- [ ] **`claude-msg health` subcommand.** Scans the last N hours of
  journalctl and reports rates of `delivered_unverified`,
  `quiet_cap_exceeded`, `drift_detected`, `drift_check_ambiguous`.
  One command for the operator's morning-coffee health check.

To file as separate Forgejo issues so they're tracked individually.

## 4. Test-architecture observations

_(Surveyor pass)_

### 4.1 Where the existing test suite missed these

- Incident 6 (cursor_y outside input box) had a test that passed
  because the fake runner returned whatever cursor_y we asked for —
  the test didn't exercise the "cursor in the wrong place"
  scenario. Adversarial-input testing should be a category, not an
  afterthought.
- Incident 9 (alias collision) had three tests for canonical
  matching but none for shared-alias. Discipline-pin tests should
  prove the *architectural commitment* ("we never silently guess
  between two exact matches"), not just the implementation's
  current behaviour.
- Incidents 1, 5 (watchdog) had no tests for the systemd-watchdog
  contract at all. The Ping callback pattern shipped in `5a0f0ee`
  added one; before that, the watchdog was operator-trust.

### 4.2 Discipline-pin pattern: time to name it?

The pattern surfaced in Surveyor's #29 review and has been applied
three times since: `TestTrackResult_OmitemptyContract`,
`TestInsertMessage_CapEnforcedUnderConcurrency`, and the
v0.2.1-Q(a) `TestLookupByNameWithCanonicals_ExactMatchAmbiguous_*`
family. Plus the `linkP2ToP1` precondition guard from the #29
follow-up. **Four instances of the same pattern; time to write the
ADR.**

Proposed scope for the ADR (titled e.g. *Discipline pins as a test
category*):

- Definition: a discipline pin asserts an **architectural
  commitment** (a design rule), not a behaviour. Failing means the
  design rule was violated.
- Distinction from regression tests: regression tests assert
  specific past behaviour; discipline pins assert a contract that
  prevents a *class* of bugs.
- Authoring discipline: each discipline pin's docstring should
  state the architectural commitment in one sentence and the bug
  class it prevents.
- Where they live: alongside regression tests in the same package,
  but conventionally named (e.g. `_pin_test.go` or a docstring
  marker). _(Surveyor pass on convention)_

This ADR is worth writing because the pattern has shown up
unprompted in four distinct reviews now; that's the threshold I'd
want to see before naming a pattern as a discipline.

### 4.3 What's still untested

- **Cross-process race for cap enforcement.** Issue #33 captured
  this honestly; not yet implemented. The single-process concurrent
  test (`messages_concurrent_test.go`) pins the in-tx atomicity
  but doesn't exercise file-level RESERVED locking across separate
  `Store` instances.
- **Realistic Claude Code TUI rendering scenarios.** Most of the
  probe / verify / settle-delay logic is tested against synthetic
  capture-pane outputs. A few end-to-end tests against a real
  Claude Code session under load would surface the next class of
  timing issues before they reach production.

## 5. What this doc does NOT cover (out of scope)

- Per-incident root-cause analysis. Each fix commit has its own
  message with context.
- Recommendations for whether to keep the probe-and-watch gate
  versus switching to a different model. The gate's design has
  earned its keep across three reviews; this doc records its
  failure modes, not its architecture.
- Comparing cli-semaphore's incident rate to industry benchmarks.
  Single-operator homelab; the only meaningful comparison is
  pre-MVP intuition vs post-MVP empirical data.

## Glossary

| Term | Meaning |
|---|---|
| Discipline pin | Test that asserts an architectural commitment (not behaviour). |
| Audit trail | The sequence of commits + reviews + journal entries that traces an incident's full history. |
| Soft-fail vs fail-loud | Soft-fail: log a WARN and continue. Fail-loud: MarkFailed and surface to the sender. |
| Silent-bad-delivery | A message that the bus marks delivered but reaches the wrong recipient. The 2026-05-31 incident class. |

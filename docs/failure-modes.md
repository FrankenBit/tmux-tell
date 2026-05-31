# Failure modes — what we got wrong in the first 48h post-MVP

> **Status: DRAFT (incorporates Surveyor structural review per
> comment 58662 on issue #34, 2026-05-31).** Author: Admin per
> posture (b)-with-substance agreed on bus id 8d2f. Structural-
> reshape proposals S1-S5 from the review are merged into this
> revision; sweep finding on §1 row 5/6 fix-commit-sharing is
> clarified in the table notes.

## Why this doc exists

cli-semaphore shipped end-of-MVP on 2026-05-29 and within 48h
produced ten issues — **eight in production, two via the v0.2.0
review pass**. Each fix was small. The cumulative pattern was
"production (or review) discovered the assumption, we patched."
This doc distills the pattern — assumptions that failed, what
would have caught each earlier, what to invest in so the next
class of incident is observable instead of surprising.

The audit window: **2026-05-29 cf72ed (MVP cut) through 2026-05-31
c521515 (v0.2.1 + AddAlias TOCTOU note)**.

## 1. Incidents

Ten issues, in landing order — eight production incidents (rows
1-8) plus two caught in the v0.2.0 review pass (rows 9-10). Each
row's "Assumption that failed" is the one-sentence version of the
design decision that production (or review) later overturned.

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
- **Rows 5 and 6 share fix commit `a7a0f25`**: that single commit
  closed both bug classes in one merge ("mailman: find input row
  by probe position + watchdog ping on short sleeps"). Not a paste
  artifact; the commit's title carries both.
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

**Three tools, three failure-class fits**:

- **Observability** (metrics, log-rate dashboards) catches the
  everyday-WARN-trail class — the steady-stream incidents that
  produce repeated symptoms before the diagnosis (deliver-time
  spikes, ProbeMissing rates, restart counts).
- **Structural review** catches the blast-radius class — the
  one-shot incidents where production discovery is too late
  because the first occurrence is the catastrophic one. Incidents
  9 and 10 (silent-bad-delivery to autonomous receivers) are the
  worked examples; both were caught in the v0.2.0 cross-project
  review, neither would have shown up in a WARN trail before
  hitting prod.
- **Pre-prod hardening** (in the absence of either) yields least
  at this maturity level — the test-suite gaps documented in §4.1
  show that pre-prod tests missed every incident that observability
  or review did catch.

Investing in observability + structural review **compounds** at
this stage; investing in pre-prod hardening alone does not.

## 3. Monitoring / observability recommendations

Filed as separate Forgejo issues so each is tracked, sized, and
resolvable on its own cadence. Ordered by expected operator pain
reduction (highest-leverage first):

- [x] [#42 — `claude-msg health` subcommand](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/42)
  (priority/high, size/M). One-command scan-and-report of WARN
  rates + crash counts + stale registry entries; replaces the
  morning-coffee `journalctl ... | grep` ritual. The single
  biggest blind-spot collapse.
- [x] [#39 — deliver-time histogram per recipient (95th/99th)](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/39)
  (priority/medium, size/S). Would have flagged incidents 3
  (unverified delivery) and 4 (Enter-not-firing) immediately
  via 99th-percentile spike. Data already on the `messages`
  table; no schema change.
- [x] [#40 — per-verdict count from `WaitForQuietPane`](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/40)
  (priority/medium, size/S). DeltaQuiet / InputActivity /
  TUINoise / ProbeMissing aggregated per recipient. High
  ProbeMissing rate is the smoking gun for input-row
  identification failures (incident 6).
- [x] [#41 — per-mailman crash counter in status](https://git.frankenbit.de/frankenbit/cli-semaphore/issues/41)
  (priority/low, size/S). Reads `systemctl --user show
  ... NRestarts`. Would have flagged incidents 1 and 5 at the
  moment of each crash, not after operator-reported "panes
  acting weird".

## 4. Test-architecture observations

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

### 4.2 Discipline-pin pattern: by architectural commitment, not by test

The pattern surfaced in Surveyor's #29 review and a codebase grep
at v0.2.1 shows **eight pinning tests across four architectural
commitments**. The right ADR unit is **the commitment**, not the
test — eight tests is implementation, four commitments is design.

| Architectural commitment | Pinning tests | Source review |
|---|---|---|
| **Wire shape is single SoT** (JSON-tag-driven; no manual map construction) | `TestTrackResult_OmitemptyContract`; `TestTrack_WireShape_CLIAndMCPMatch` | Surveyor #28 / #29 reviews |
| **Atomic cap enforcement under concurrency** (caps are ceilings, never floors) | `TestInsertMessage_CapEnforcedUnderConcurrency` | Surveyor #29 round-3 review |
| **Thread-structure precondition** (`linkP2ToP1` callers don't pass explicit `reply_to`) | `TestInsertMessagePair_LinkP2ToP1_RejectsExplicitReplyTo` | Surveyor #29 follow-up |
| **Never silently guess between canonical-or-alias exact matches** | `TestLookupByNameWithCanonicals_ExactMatchAmbiguous_AliasCollision`; `_AliasIsAnotherCanonical`; `TestPaneAgentNameWithCanonicals_ExactMatchAmbiguous`; plus `TestLookupByNameWithCanonicals_Ambiguous` (v0.2.0 substring variant) | Surveyor v0.2.0 Q(a) + v0.2.1 |

Proposed ADR scope (titled e.g. *Discipline pins as a test
category*):

- **Definition**: a discipline pin is a test pattern that asserts
  a **single architectural commitment**. The commitment is the
  load-bearing thing; multiple tests can implement the same
  commitment (and frequently do — see the eight-tests-four-
  commitments table above).
- **Distinction from regression tests**: regression tests assert
  specific past behaviour; discipline pins assert a contract that
  prevents a *class* of bugs (every test in the table above
  prevents an entire class).
- **Authoring discipline**: every pin's docstring opens with
  `PIN: <architectural commitment>` so the commitment-to-test
  traceability is mechanical via grep.
- **Forward-compatibility**: the commitment-count tells you when
  a fifth commitment surfaces (rather than "yet another test").
  This is what makes the discipline durable across years vs. just
  a hindsight label.

The ADR is worth writing because the pattern has shown up
unprompted across four distinct architectural commitments in two
months. That's the threshold for naming a pattern as a discipline.
**This ADR lives in cli-semaphore** (not Binnacle — different test
discipline contexts).

### 4.3 Discipline-pin conventions — naming + location

The ADR should pin three orthogonal grep handles:

- **Location**: in-package, in dedicated `pin_test.go` files. One
  file per package surfaces the pinning surface at the package
  level; grep `pin_test.go` lists the entire discipline surface
  across the codebase. Where a commitment spans multiple files
  (e.g. `internal/discover/canonical_ambiguity_test.go` today),
  consolidating into `internal/discover/pin_test.go` is the
  refactor for the ADR's first follow-up commit.
- **Naming**: function names carry the commitment slug as
  `TestPin_<commitment-slug>_<assertion>`. Example renames:
  - `TestTrackResult_OmitemptyContract` →
    `TestPin_WireShape_OmitemptyContract`
  - `TestLookupByNameWithCanonicals_ExactMatchAmbiguous_AliasCollision` →
    `TestPin_AliasResolution_NeverSilentlyGuess_OnCollision`
  The `TestPin_` prefix makes mixed test files self-documenting
  when a pin sits next to regression tests.
- **Docstring**: opens with the line `// PIN: <one-sentence
  architectural commitment>`. Three handles total — `pin_test.go`
  file pattern + `TestPin_` function prefix + `// PIN:` docstring
  prefix — orthogonal so any single one finds the discipline.

### 4.4 What's still untested

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

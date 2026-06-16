# Contributing to tmux-tell

Thanks for your interest. tmux-tell is a small, MIT-licensed message bus for CLI
agents running in tmux — see the [README](README.md) for what it is and the
[`docs/`](docs/) guides for how it works. Issues and pull requests are welcome.

## Ways to contribute

- **Found a rough edge or a bug?** Open an issue — a short repro or the surprising
  behavior is plenty.
- **Sending a PR?** Keep it focused (one change, honest to its title), include a test
  where it makes sense, and add a `CHANGELOG.md` entry (see below). A maintainer
  reviews before merge.

## Development

You need tmux, sqlite3, and Go (≥ 1.24). The pre-commit recipe:

```bash
go vet ./...
go build ./...
go test -race -count=1 ./...
```

CI runs `go vet`, `go build`, and `go test` **without** `-race` (the runner image
lacks a C compiler / cgo, which the race detector needs) — so run `-race` locally and
push it clean.

## Scenario layer (godog / gherkin)

`features/*.feature` holds the six substrate-boundary scenarios. Each scenario
documents one contract the project makes with operators (observe-gate delivery,
dedupe recovery, operator routing, deferred delivery, attention signal). The
step definitions live in `features/steps/suite_test.go`.

Run the scenarios:

```bash
go test -count=1 ./features/steps/
# or as part of the full suite
go test -count=1 ./...
```

**Adding a new scenario:**

1. Write a `.feature` file (or extend an existing one) under `features/`.
2. Add matching step definitions in `features/steps/suite_test.go` — wire them to
   store / tmuxio primitives so they pass without a real tmux server.
3. Include the scenario in the `CHANGELOG.md` entry for the PR.

The scenario tier documents the *substrate contract*, not the mailman IO. Delivery
timing, tmux paste mechanics, and mailman loop behaviour are tested in
`cmd/tmux-tell-claude/serve*_test.go`; the gherkin layer sits above and focuses on
the store state-machine transitions each documented loop produces.

**`tail` watch mechanism — rowid-polling, not `update_hook`.** The mailmen that write
rows are *separate processes* from the `tail` CLI, and SQLite's `update_hook` only fires
for the connection that registered it (per-connection, same-process), so it would never
see their writes. `tail` polls `MAX(id)` since-last-seen (configurable `--interval`,
default 300ms) and re-reads in-flight ids for state transitions; WAL mode keeps these
reads safe concurrent with mailman writes.

## How we work

- **CHANGELOG.** Every change-carrying PR adds an entry under `## [Unreleased]` in
  `CHANGELOG.md`, in the [Keep a Changelog](https://keepachangelog.com/) style. Docs
  PRs included — a doc change is still a change. Keep entries crisp (headline + refs,
  detail in the PR body) — see [CHANGELOG entries](#changelog-entries) for the
  density convention + the per-release prelude shape.
- **ADRs.** Architectural decisions are recorded in [`docs/adr/`](docs/adr/) (see its
  `README.md` for the convention and `template.md` for the shape). File an ADR when a
  decision constrains future work, touches an architectural commitment, or has real
  alternatives worth recording. Every discipline-pin commitment (ADR-0001) must have a
  backing ADR.
- **Reviews.** PRs get a review before merge; substrate-accuracy (claims grounded
  against the actual code, not the issue body) is the bar reviewers hold.
- **Schema-affecting changes touch the docs schema block.** Any change to the DB
  schema — a new `CREATE TABLE`, a new column, a column-semantics shift, or a
  state-vocab addition — must update the storage schema block in
  [`docs/reference.md`](docs/reference.md) §Storage schema in the same PR.
  Recurring gap surfaced across PRs (#229, #255, #257, #259); making it
  implementer-side discipline keeps the schema block honest against the shipped
  binary without a per-PR reviewer sweep.
- **Claiming an issue (multi-agent deployments).** If more than one party hands out
  work from this tracker, assign the issue to yourself before starting, so the claim
  is discoverable on the issue itself and not just in side-channel chatter — see
  [`docs/chamber-dispatch.md`](docs/chamber-dispatch.md). Concretely: before opening
  the worktree branch, set the Forgejo `assignees` field on the issue
  (`mcp__forgejo__edit_issue index=N assignees=["<your-name>"]`, or the equivalent
  API call). Mirror the same convention on the PR: when you open a PR that closes an
  issue, set the PR's `assignees` field to yourself too. Dispatchers (Bosun /
  Quartermaster) read the `assignees` field on the target issue *before* dispatching
  — if non-empty, route through the current assignee on the bus first rather than
  re-dispatching. The convention is **forward-only**: historical issues without an
  assignee aren't backfilled; new work claims as it picks up.

### CHANGELOG entries

Two layers, one convention: the entry you write per PR, and the prelude the release
cut adds per version. This is the as-applied codification of the #391 distillation
pass (merged in #456) — it documents what held in practice; the worked exemplars
are the `[0.16.1]` / `[0.17.0]` sections.

**Per-entry (every change-carrying PR).** One crisp bolded headline naming the
surface change with the issue/PR refs in brackets, then at most a line or two for a
substrate-honest constraint or composition note. Operator-facing impact is the
lens, not the engineering narrative — *detail belongs in the PR body* (the review
surface); the entry announces the change and links the depth. One nuance: a recipe
that is itself the doc-of-record stays (e.g. a deploy procedure the entry
introduces), while a recipe that mirrors the PR body's step-by-step gets distilled
to headline + link.

**Per-release prelude (at cut time).** Each `## [X.Y.Z]` section opens with a short
narrative paragraph naming the cluster, then a `Headlines:` digest of bolded bullets
where the release has enough substance to digest (3+); a small cut can skip the
digest but not the paragraph. This is not decoration: `release-draft.yml` extracts
everything before the first `### ` subsection as the curated release body, so an
empty prelude **hard-fails the draft by design** (#427).

**Forward-living-comprehensive.** The `CHANGELOG.md` at a tag is the *comprehensive*
record — the canonical surface a reader consults for "what exactly changed" — while
the release body is the curated narrative. So recent release sections stay
comprehensive; only long-ago sections that have aged into archaeology get distilled
(the boundary #391 drew was v0.16.0). This routing principle (release UI = publish,
`CHANGELOG.md`@tag = comprehensive, release body = curated) is recorded in #426 /
#427 and tracked for a full ADR in #462.

A one-time cleanup like #391 is distinct from ongoing drift: if later releases let
PR-body-mirror prose creep back, file a sibling tracker rather than folding the fix
into an unrelated change.

## Release cuts

**Pre-flight.** If the cut driver works from a shared host checkout (on
alcatraz, `/srv/tmux-msg/` is shared across chambers and read directly by
host scripts), fast-forward it first so on-disk state matches `origin/main`
before the cut branch is created:

```bash
cd /srv/tmux-msg/ && git pull --ff-only
```

Operator scripts that invoke files from this tree (recording rig drivers,
ad-hoc smoke tests) read the *last-fast-forwarded* state, not the current
`origin/main` — so a stale shared checkout produces hard-to-diagnose
"merged on origin but missing on disk" surprises (closes #284). Per-chamber
worktrees handle their own state and don't need this step.

The cut sequence (run from a clean main on the cut branch):

1. **Sync state.** `git fetch origin && git checkout -b i/v<X.Y.Z>-release-cut
   origin/main`
2. **CHANGELOG header.** Move `[Unreleased]` content under
   `## [<X.Y.Z>] — <YYYY-MM-DD>`; leave `## [Unreleased]` as the empty shell.
   Confirm the new `[<X.Y.Z>]` section opens with a narrative prelude + `Headlines:`
   per [CHANGELOG entries](#changelog-entries) — `release-draft.yml` extracts
   everything before the first `### ` as the curated release body, so an empty
   prelude **hard-fails the draft by design** (#427).
3. **README version pin.** Update the `--version` example to v<X.Y.Z>.
4. **Deprecation eligibility check.** Run `./scripts/deprecations.sh --for
   v<X.Y.Z>` and confirm the cleared-for-removal list matches intent. If a
   listed surface is NOT being removed this cut, document the extension reason
   in the cut PR — the two-minor floor is a guarantee, not a ceiling
   ([ADR-0008](docs/adr/0008-deprecation-policy.md) §Discretion clause).
5. **Pre-commit checks.** `gofmt -l .` clean; `go vet ./...` clean; `go test
   -race -count=1 ./...` green.
6. **Cut PR.** Open the cut PR; reviewer approves; merge on green.
7. **Publish the auto-draft.** Merging the cut PR fires `release-draft.yml`,
   which creates a **draft** Forgejo release whose body is the `[<X.Y.Z>]`
   section's narrative prelude + `Headlines:` (the curated surface per #426), with
   the merge-commit SHA pinned as `target_commitish`. Review the draft in the
   releases UI and click **Publish** — Forgejo creates the `v<X.Y.Z>` tag from the
   draft. No manual `git tag && git push`; the Publish click is the act of
   shipping (#418).
8. **Deploy fires automatically.** `release: published` triggers
   `release-publish.yml`, which re-validates the tag and chains `deploy.yml` (via
   `workflow_call`) to run `install.sh` + bootstrap on the alcatraz-host runner.
   Watch the deploy job's smoke step; for a manual redeploy or rollback, dispatch
   `deploy.yml` with `ref=<tag>`.

Deprecation-policy hygiene per ADR-0008 (the two-minor floor + the structured
`### Deprecated` format from §Amendment B) is enforced at step 4 — the
derive-script is the operator's surface for "which surfaces did I promise
two cycles ago, and is this the cut where they come off?".

## The external contract (downstream consumers)

tmux-tell is consumed as a standalone module by downstream projects — notably
**Binnacle** (GPL-3.0-only), which composes with tmux-tell as an external Go module
rather than absorbing it ([ADR-0007](docs/adr/0007-binnacle-coexist-external-contract.md)).
That makes two surfaces a **stability contract**, not just internal detail:

1. **The exported Go API** — the boundary downstream code imports.
2. **The DB schema** — the `messages` / `agents` columns and the message + agent
   **state vocabulary** (`queued` / `delivering` / `delivered` / `failed`; the agent
   states). Downstream readers and tools depend on these names.

Changes that touch either surface are contract changes — treat them as such.

### Stability commitments

Governed by the project's deprecation policy
([ADR-0008](docs/adr/0008-deprecation-policy.md)):

- **Pre-1.0 (current).** Semver-explicit looseness: minor bumps (`0.x` → `0.(x+1)`)
  may carry breaking changes, always called out in the `CHANGELOG.md` entry. Pin a
  specific minor if you need stability today.
- **Post-1.0.** A deprecated **public surface** stays functional for **at least two
  minor release cycles** after its deprecation is announced, before removal.
  Maintainers may **extend** the window at their discretion for high-impact changes,
  and deprecated surfaces emit a **runtime warning** when an observed call hits them.

  The policy's grace covers **all five public surfaces** (per ADR-0008): MCP tool
  schemas; CLI subcommand args / flags / exit codes; `--format json` shapes; the DB
  schema + state vocabulary; and the exported Go API. The two surfaces named above
  (the Go API + DB schema) are the **external-contract subset** a downstream module
  like Binnacle pins; the deprecation grace applies to the broader public-surface set.

If you're building on tmux-tell, pin a version and watch the CHANGELOG; the contract
above is what you can rely on between pins.

### License

tmux-tell is **MIT** and stays MIT. Combining it into a copyleft downstream is clean:
a GPL-3.0-only consumer (Binnacle) may link and redistribute it — the *combined*
binary distributes under GPL-3.0-only, while the tmux-tell module itself remains MIT
for every other consumer (per the FSF compatibility list; see ADR-0007). By
contributing, you agree your contributions are released under the MIT license.

## Code of conduct

Be decent. Assume good faith, keep critique about the work, and make this a place
people want to contribute to. Maintainers may moderate or remove contributions that
don't meet that bar.

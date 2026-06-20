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

- **CHANGELOG.** Every change-carrying PR adds a **fragment** in `changelog.d/` —
  **not** a direct edit of `CHANGELOG.md` (#494). The fragment file is named
  `changelog.d/<issue>.<type>.md` (e.g. `changelog.d/480.changed.md`), and its content
  is the [Keep a Changelog](https://keepachangelog.com/)-style bullet the entry would
  have added. `<type>` ∈ `added changed deprecated removed fixed security documentation`;
  for two entries of the same type on one issue use `changelog.d/<issue>.<n>.<type>.md`.
  Docs PRs included — a doc change is still a change. Keep the bullet crisp (headline +
  refs, detail in the PR body). Fragments are assembled into `[Unreleased]` at
  release-prep (`tools/changelog-assemble`), so **parallel PRs never collide on
  `CHANGELOG.md`** — that collision tax (the structural reason for this convention) is
  what the fragment pattern removes. See [CHANGELOG entries](#changelog-entries) for the
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
  assignee aren't backfilled; new work claims as it picks up. Assignee-on-claim
  can't cover the other shape — two chambers filing the *same follow-up tracker*
  within seconds mid-review, before either issue exists to check — so when a
  crossing happens anyway, resolve it cheaply (verify substrate-state → surface the
  divergence → defer to merged-reality → don't re-litigate) per
  [`docs/chamber-dispatch.md`](docs/chamber-dispatch.md) §When a crossing happens anyway.

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

**Fragments (per-PR mechanics, #494).** The per-entry convention above applies to the
fragment body you write in `changelog.d/<issue>.<type>.md` — same crisp-headline density,
same one-bullet-per-change shape. At release-prep, `tools/changelog-assemble` gathers
the fragments into `[Unreleased]`, grouping by `### Type` in the canonical order
(Added → Changed → Deprecated → Removed → Fixed → Security → Documentation) and deleting
the consumed fragments. The prelude convention is **unchanged and orthogonal** — the
assembler only fills the `### Type` bullet blocks; the narrative prelude is still added
by hand at cut, so the #427 extract-before-first-`###` boundary keeps working. Locally,
`go run ./tools/changelog-assemble -check` validates fragment names/types (CI runs this);
`go run ./tools/changelog-assemble` assembles into `[Unreleased]` for a manual preview.

**Forward-living-comprehensive.** The `CHANGELOG.md` at a tag is the *comprehensive*
record — the canonical surface a reader consults for "what exactly changed" — while
the release body is the curated narrative. So recent release sections stay
comprehensive; only long-ago sections that have aged into archaeology get distilled
(the boundary #391 drew was v0.16.0). This routing principle (release UI = publish,
`CHANGELOG.md`@tag = comprehensive, release body = curated) is codified in
[ADR-0016](docs/adr/0016-canonical-substrate-vs-curated-surface-routing.md);
the originating discussion lives in #426 / #427.

A one-time cleanup like #391 is distinct from ongoing drift: if later releases let
PR-body-mirror prose creep back, file a sibling tracker rather than folding the fix
into an unrelated change.

### CI workflow context renames

When you rename a CI workflow job (the `name:` field of a `jobs.<id>` block in
`.forgejo/workflows/*.yml`), the **branch-protection required-status-check name
for `main` must be updated to match the new context** in the same change. Forgejo
matches required status checks by **exact name**; the old name will no longer
appear among completed checks and every future PR will trip `HTTP 405: not all
required status checks successful` even when CI is substantively green — clearable
only via `force_merge: true`, which bypasses the matcher rather than fixing it
(substrate-honest but not the steady-state we want).

Update the rule with one PATCH:

```bash
curl -s -X PATCH -H "Authorization: token $FORGEJO_TOKEN" -H "Content-Type: application/json" \
  -d '{"status_check_contexts": ["<new context name>"]}' \
  "http://127.0.0.1:3000/api/v1/repos/frankenbit/tmux-tell/branch_protections/main"
```

(Token needs `write:repository` scope; chamber-side `FORGEJO_TOKEN_QUARTERMASTER`
works. Idempotent — safe to re-apply.)

Worked instance: #544 (golangci-lint adoption renamed `test / go vet + build + test`
→ `test / lint + build + test`); branch-protection drift caught at merge-gate and
cleared via #546.

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
2. **CHANGELOG header.** First **assemble fragments**: `go run
   ./tools/changelog-assemble -prune` gathers `changelog.d/` into `[Unreleased]` and
   deletes the consumed fragments (#494). The automated `release.yml` does this for you;
   run it by hand only for a manual cut. *Order matters:* assemble **before** the
   deprecation check in step 4 — a deprecation that lives only as a
   `changelog.d/*.deprecated.md` fragment is invisible to `deprecations.sh` until it's
   assembled into `CHANGELOG.md`. Then move `[Unreleased]` content under
   `## [<X.Y.Z>] — <YYYY-MM-DD>`; leave `## [Unreleased]` as the empty shell.
   Confirm the new `[<X.Y.Z>]` section opens with a narrative prelude + `Headlines:`
   per [CHANGELOG entries](#changelog-entries) — `release-draft.yml` extracts
   everything before the first `### ` as the curated release body, so an empty
   prelude **hard-fails the draft by design** (#427).
3. **README version pin — automated.** `release.yml`'s transition step now pins the
   README `--version` example to v<X.Y.Z> as part of the mechanical transition (#514);
   no manual action. It **hard-fails the cut** if the `tmux-tell-claude vX.Y.Z` example
   line is missing — if you restructure that README block, update the regex in
   `release.yml`. For a fully-manual cut, do the bump by hand.
4. **Deprecation eligibility check.** Run `./scripts/deprecations.sh --for
   v<X.Y.Z>` and confirm the cleared-for-removal list matches intent. If a
   listed surface is NOT being removed this cut, document the extension reason
   in the cut PR — the two-minor floor is a guarantee, not a ceiling
   ([ADR-0008](docs/adr/0008-deprecation-policy.md) §Discretion clause).
5. **Pre-commit checks.** `gofmt -l .` clean; `go vet ./...` clean; `go test
   -race -count=1 ./...` green.
6. **Docs-coherence check.** Verify the operator-facing surfaces *beyond* this
   diff per [docs/release-cut-checklist.md](docs/release-cut-checklist.md):
   BookStack (Service Inventory p88, Release & Deploy p193, the *tmux-tell* book),
   `/srv/CLAUDE.md` (alcatraz-infra — a separate repo/commit), and any sister
   chamber `CLAUDE.md` (flag the chamber; don't edit). The release-prep PR body
   carries the compact checkbox version. Items are N/A-able — a no-doc-impact cut
   ticks them fast — but don't skip it for "small" cuts; that's the drift vector.
   Salience, not machine-enforcement (#495).
7. **Arc42 section staleness.** Scan this cut's changes against the
   `revisit-triggers` frontmatter of the [Arc42 sections](docs/arc42/): did any
   section's named trigger fire (a component
   added/removed, the `PaneProfile` shape moved, a new ADR to anchor, a new
   risk/term)? If so, pull the section update into this cut. Most cuts are no-op.
   Sibling to step 6 at the per-cut review-gate axis — salience, not
   machine-enforcement (#386).
8. **Cut PR.** Open the cut PR; reviewer approves; merge on green.
9. **Publish the auto-draft.** Merging the cut PR fires `release-draft.yml`,
   which creates a **draft** Forgejo release whose body is the `[<X.Y.Z>]`
   section's narrative prelude + `Headlines:` (the curated surface per #426), with
   the merge-commit SHA pinned as `target_commitish`. Review the draft in the
   releases UI and click **Publish** — Forgejo creates the `v<X.Y.Z>` tag from the
   draft. No manual `git tag && git push`; the Publish click is the act of
   shipping (#418).
10. **Deploy fires automatically.** `release: published` triggers
   `release-publish.yml`, which re-validates the tag and chains `deploy.yml` (via
   `workflow_call`) to run `install.sh` + bootstrap on the alcatraz-host runner.
   Watch the deploy job's smoke step; for a manual redeploy or rollback, dispatch
   `deploy.yml` with `ref=<tag>`.

Deprecation-policy hygiene per ADR-0008 (the two-minor floor + the structured
`### Deprecated` format from §Amendment B) is enforced at step 4 — the
derive-script is the operator's surface for "which surfaces did I promise
two cycles ago, and is this the cut where they come off?".

### Amending a release after Publish

Clicking **Publish** creates the tag and makes the release public, so the clean
fix for a bad prelude is to **catch it before Publish** — the draft sits in the
releases UI precisely so it can be reviewed, and the prelude gate (a reviewer
reading the draft body) is where a register or accuracy problem should be caught.
Treat post-Publish amendment as **rare**.

Worked precedent: the **v0.17.2** cut (2026-06-15), where a published prelude was
flagged as cryptic and rewritten in place — see
[#472](https://git.frankenbit.de/frankenbit/tmux-tell/issues/472) for the full
incident and the ad-hoc path it took. Contrast the **v0.21.0** cut, where the same
class of problem (an over-claiming prelude) was caught at the draft gate *before*
Publish ([#560](https://git.frankenbit.de/frankenbit/tmux-tell/pulls/560)) — the
outcome you want.

**First, decide what kind of change it is.** Readability rewrites, factual
corrections, and broken-link fixes are in scope. **New content is not** — if you
want to *add* something that shipped, file a follow-up PR and reference it from the
next release's CHANGELOG; don't retro-stuff a published release.

**Then amend, least-destructive surface first:**

1. **The release body — always, and always safe.** Edit the release body in the
   Forgejo releases UI (or via the API). The body is just text attached to the
   release; editing it touches nothing in git history and is the operator's
   hand-edit surface. This alone makes the **public release page** correct.
2. **`CHANGELOG.md` on `main` — always.** Open a PR that brings the `[<X.Y.Z>]`
   section in line with the corrected body, so HEAD and every future reader see the
   right text. Normal review applies; for an operator-authorized consistency fix the
   operator may waive the PR ceremony and have an admin (Bosun) force-merge, with
   the reviewer's approval landing post-merge as a correctness confirmation —
   reserve that for genuine consistency fixes, not as a routine shortcut.
3. **The tag — the operator's call.** After steps 1–2 the body and `main` are
   correct, but `CHANGELOG.md` **at the tagged commit** still shows the old text (a
   tag is a frozen snapshot). Two honest options:
   - **Leave the tag frozen** (default, non-destructive). The published body and
     `main` carry the correction; the tag's snapshot stays as it was cut. Nothing
     anyone already fetched changes.
   - **Force-move the tag** to the corrected commit so `CHANGELOG@<tag>` matches the
     body — and update the release's `target_commitish` to match. This is **mildly
     destructive**: anyone who already fetched the tag sees it move on their next
     fetch. Only do it when (a) the operator authorizes it, (b) the deploy has
     already happened (so the move doesn't disturb a release in flight), and (c)
     nothing downstream relies on stable tag SHAs (today, nothing does). Otherwise
     prefer leaving it frozen.

The bias is the least-destructive surface that makes the public release honest: the
body edit is free and always correct; the tag-move is the rare, operator-authorized
step you take only when `CHANGELOG@tag` consistency is worth the force-move.

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

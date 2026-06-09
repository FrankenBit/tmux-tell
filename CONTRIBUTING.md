# Contributing to tmux-msg

Thanks for your interest. tmux-msg is a small, MIT-licensed message bus for CLI
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
`cmd/tmux-msg-claude/serve*_test.go`; the gherkin layer sits above and focuses on
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
  PRs included — a doc change is still a change.
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
  [`docs/chamber-dispatch.md`](docs/chamber-dispatch.md).

## Release cuts

The cut sequence (run from a clean main on the cut branch):

1. **Sync state.** `git fetch origin && git checkout -b i/v<X.Y.Z>-release-cut
   origin/main`
2. **CHANGELOG header.** Move `[Unreleased]` content under
   `## [<X.Y.Z>] — <YYYY-MM-DD>`; leave `## [Unreleased]` as the empty shell.
3. **README version pin.** Update the `--version` example to v<X.Y.Z>.
4. **Deprecation eligibility check.** Run `./scripts/deprecations.sh --for
   v<X.Y.Z>` and confirm the cleared-for-removal list matches intent. If a
   listed surface is NOT being removed this cut, document the extension reason
   in the cut PR — the two-minor floor is a guarantee, not a ceiling
   ([ADR-0008](docs/adr/0008-deprecation-policy.md) §Discretion clause).
5. **Pre-commit checks.** `gofmt -l .` clean; `go vet ./...` clean; `go test
   -race -count=1 ./...` green.
6. **Cut PR.** Open the cut PR; reviewer approves; merge on green.
7. **Tag + Forgejo release.** From the merged main: `git tag v<X.Y.Z> && git
   push origin v<X.Y.Z>`. Forgejo release notes come from the `[<X.Y.Z>]`
   CHANGELOG block.
8. **Deploy.** `./install.sh` against the freshly-built binary; verify the
   mailman lifecycle + a smoke round-trip on the target host.

Deprecation-policy hygiene per ADR-0008 (the two-minor floor + the structured
`### Deprecated` format from §Amendment B) is enforced at step 4 — the
derive-script is the operator's surface for "which surfaces did I promise
two cycles ago, and is this the cut where they come off?".

## The external contract (downstream consumers)

tmux-msg is consumed as a standalone module by downstream projects — notably
**Binnacle** (GPL-3.0-only), which composes with tmux-msg as an external Go module
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

If you're building on tmux-msg, pin a version and watch the CHANGELOG; the contract
above is what you can rely on between pins.

### License

tmux-msg is **MIT** and stays MIT. Combining it into a copyleft downstream is clean:
a GPL-3.0-only consumer (Binnacle) may link and redistribute it — the *combined*
binary distributes under GPL-3.0-only, while the tmux-msg module itself remains MIT
for every other consumer (per the FSF compatibility list; see ADR-0007). By
contributing, you agree your contributions are released under the MIT license.

## Code of conduct

Be decent. Assume good faith, keep critique about the work, and make this a place
people want to contribute to. Maintainers may moderate or remove contributions that
don't meet that bar.

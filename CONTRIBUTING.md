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

Governed by the project's deprecation policy (ratified via #162; its ADR is a
Sea-trials follow-up):

- **Pre-1.0 (current).** Semver-explicit looseness: minor bumps (`0.x` → `0.(x+1)`)
  may carry breaking changes, always called out in the `CHANGELOG.md` entry. Pin a
  specific minor if you need stability today.
- **Post-1.0.** A deprecated API surface (Go exports, DB schema columns, or state
  vocabulary) stays functional for **at least two minor release cycles** after its
  deprecation is announced, before removal. Maintainers may **extend** the deprecation
  window at their discretion for high-impact changes, and deprecated surfaces emit a
  **runtime warning** when an observed call hits them.

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

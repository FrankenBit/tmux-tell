#!/usr/bin/env bash
# update-readme-version.sh - release-toolkit post_bump_hook (tmux-tell#617).
#
# Ports the hand-rolled #514 README `--version`-example pin into the toolkit's
# post_bump_hooks mechanism. On each cut, pin the single
# `tmux-tell-claude vX.Y.Z` example-output line in README.md to the freshly-cut
# version.
#
# Hard-fail on miss (loud-not-silent): a README restructure that moves the line
# breaks the cut LOUDLY (update the regex) rather than silently re-introducing
# the multi-cut hand-pin drift (v0.17.0 -> v0.18.1 each missed the manual step)
# that motivated #514.
#
# Env (exported by release-prep.sh before the hook runs, CWD = repo root):
#   RELEASE_TOOLKIT_NEW_VERSION   required, bare version e.g. "0.23.0"
#
# Staging: release-prep.sh commits only explicitly-staged files, so this hook
# `git add`s its own change (release-toolkit#209 convention; release-toolkit#236
# would auto-stage hook-modified tracked files and retire this requirement).
set -euo pipefail

VERSION="${RELEASE_TOOLKIT_NEW_VERSION:?RELEASE_TOOLKIT_NEW_VERSION required}"

# Cuts always carry a stable vX.Y.Z; reject anything else (an rc/pre-release
# reaching the README example pin would be a bug).
if [[ ! "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'update-readme-version: malformed RELEASE_TOOLKIT_NEW_VERSION (X.Y.Z required): %s\n' "$VERSION" >&2
    exit 1
fi

README="README.md"
if [[ ! -f "$README" ]]; then
    printf 'update-readme-version: README.md not found\n' >&2
    exit 1
fi

# Anchored to ONLY the `--version` example-output line. Require EXACTLY one
# match; zero or many -> fail loud (the README moved, removed, or duplicated it).
matches=$(grep -cE '^tmux-tell-claude v[0-9]+\.[0-9]+\.[0-9]+$' "$README" || true)
if [[ "$matches" -ne 1 ]]; then
    printf 'update-readme-version: expected exactly 1 tmux-tell-claude vX.Y.Z line in README.md, found %s (if the README moved it, update this hook regex)\n' "$matches" >&2
    exit 1
fi

sed -i -E "s|^(tmux-tell-claude v)[0-9]+\.[0-9]+\.[0-9]+\$|\1${VERSION}|" "$README"

# Self-stage: release-prep.sh's release commit is explicitly-staged-only.
git add "$README"

// Package version exposes the build version of tmux-msg. The
// value is overridden at build time via -ldflags so the binary
// reports something like `v0.2.0` (release) or `v0.2.0-3-g4f5e6d7`
// (between releases, via `git describe --tags --always --dirty`).
//
// At `go run` or untagged-checkout build time, the default below
// applies: "dev". A release script (release.sh, future) bumps the
// constant in the same commit it tags.
package version

// Version is the running build's version string. Override via
// `-ldflags "-X .../internal/version.Version=..."` at link time.
//
// Default "dev" matches the package doc above: an unstamped binary
// reports an obviously-unstamped value rather than masquerading as a
// specific past release. Pre-#342 this read `v0.7.0` — three releases
// stale — so an unstamped binary installed via plain `go build` (e.g.
// the pre-#342 install.sh path that skipped ldflags) silently reported
// a recognizable version that wasn't its own.
var Version = "dev"

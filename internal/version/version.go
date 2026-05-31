// Package version exposes the build version of cli-semaphore. The
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
var Version = "dev"

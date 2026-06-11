package cli

// Profile carries the per-adapter identity that distinguishes one CLI adapter
// binary (tmux-msg-claude, tmux-msg-codex, …) from another while the shared
// dispatch + handlers in this package stay adapter-agnostic. Per ADR-0009 the
// substrate is delivery-method-agnostic; this struct is the thin adapter seam
// #248 introduces so a second binary is a wrapper, not a fork.
//
// Fields are deliberately minimal — only what genuinely differs between
// adapters today:
//   - BinaryName: the installed binary name. Also the systemd mailman unit
//     prefix (<BinaryName>-mailman@<agent>.service) and the name printed in
//     usage / version / unknown-subcommand chrome.
//   - DisplayLabel: the human-readable adapter name used in usage prose where
//     the underlying CLI tool is named in a sentence (e.g. "Claude Code" /
//     "Codex"). Distinct from BinaryName: BinaryName is the literal command a
//     reader would type; DisplayLabel is the product it adapts. Used by the
//     run.go usageText "mcp" / "hook-context" descriptions (#280).
//   - DeprecatedAlias / DeprecatedRemoval: the legacy binary name an adapter
//     carries through a deprecation cycle (Claude: claude-msg → removed v1.0,
//     ADR-0008). DeprecatedAlias is empty when the adapter never had a prior
//     name (e.g. a brand-new Codex adapter) — warnIfDeprecatedName then no-ops.
//
// The deeper Claude-coupling (the internal/tmuxio observe-gate paste sentinels)
// is deliberately NOT in this struct: per #248's (B) decision the Codex adapter
// delivers via hook-context, so the paste layer stays Claude-only until a
// paste-needing adapter lands and shapes that seam with its specifics in hand.
type Profile struct {
	BinaryName        string
	DisplayLabel      string
	DeprecatedAlias   string
	DeprecatedRemoval string
}

// active is the process-global adapter profile. A CLI binary serves exactly one
// adapter for the life of the process, so a package-global set once at Run entry
// is the pragmatic seam — versus threading Profile through ~90 handler
// signatures. It defaults to the Claude adapter so in-package tests, which
// exercise handlers directly without going through Run, observe the historical
// behavior unchanged.
var active = Profile{
	BinaryName:        "tmux-msg-claude",
	DisplayLabel:      "Claude Code",
	DeprecatedAlias:   "claude-msg",
	DeprecatedRemoval: "v1.0",
}

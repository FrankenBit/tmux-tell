package cli

import "git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"

// Profile carries the per-adapter identity that distinguishes one CLI adapter
// binary (tmux-tell-claude, tmux-tell-codex, …) from another while the shared
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
//   - PasteCapable: whether the mailman may deliver to this adapter's pane via
//     the internal/tmuxio paste-and-enter path. True for Claude (the observe-gate
//     reads its ❯ prompt sentinel + cursor position to defer during operator-
//     typing); FALSE for Codex, whose `›` input area the observe-gate cannot yet
//     classify — a paste into it clobbers in-progress operator input (#323). A
//     paste-incapable adapter delivers via hook-context (#248 decision (B),
//     ADR-0009) and the mailman force-defers any paste-and-enter delivery to it.
//
// The per-adapter pane-observation snippets (PromptSentinel + markers) live in
// the Pane field as a tmuxio.PaneProfile — #322's refactor, which #323's
// PasteCapable doc-comment anticipated ("that per-adapter PromptSentinel +
// Markers structure is #322's PaneProfile refactor"). PasteCapable was the
// narrow interim seam (a single boolean kept the paste layer off panes it
// couldn't read); Pane is the deeper seam that teaches the observe-gate HOW to
// read a non-Claude pane, so a paste-capable non-Claude adapter becomes possible
// by supplying its own sentinel set rather than being force-deferred.
type Profile struct {
	BinaryName        string
	DisplayLabel      string
	DeprecatedAlias   string
	DeprecatedRemoval string
	// PasteCapable gates the internal/tmuxio paste-and-enter delivery path.
	// Zero value (false) is the safe default: an adapter that doesn't
	// explicitly assert paste-capability is force-deferred rather than risking
	// a clobber on a pane the observe-gate can't read (#323).
	PasteCapable bool
	// SupportsMCPSlashCommand reports whether this adapter's CLI has the `/mcp`
	// slash command (disable/enable/restart of MCP servers). Claude Code does;
	// codex does NOT — it has only a `--verbose` flag, and an MCP restart needs
	// a full session restart (#411). When false, the mailman SKIPS delivering a
	// `/mcp …` control command (marks it delivered + logs a WARN) rather than
	// letting it land as literal text in the recipient's prompt and break the
	// session (#419, witnessed on Lookout after a refresh-all-mcps cascade).
	//
	// Zero value (false) is the safe default — an adapter that doesn't assert
	// `/mcp` support has the command skipped rather than pasted blind. Claude
	// asserts true explicitly. This is the narrow Option-A seam; the broader
	// per-(command, adapter) compat map is #420, into which this bool folds.
	SupportsMCPSlashCommand bool
	// Pane carries the adapter's pane-observation snippets (prompt sentinel,
	// compaction / awaiting-operator / status-line markers) that the
	// internal/tmuxio classifier reads via its process-global activeProfile.
	// cli.Run installs this into tmuxio at process start (#322). The zero value
	// disables cursor-aware classification — adapters should supply a complete
	// profile (e.g. tmuxio.ClaudePaneProfile() for the Claude adapter).
	Pane tmuxio.PaneProfile
}

// active is the process-global adapter profile. A CLI binary serves exactly one
// adapter for the life of the process, so a package-global set once at Run entry
// is the pragmatic seam — versus threading Profile through ~90 handler
// signatures. It defaults to the Claude adapter so in-package tests, which
// exercise handlers directly without going through Run, observe the historical
// behavior unchanged.
var active = Profile{
	BinaryName:              "tmux-tell-claude",
	DisplayLabel:            "Claude Code",
	DeprecatedAlias:         "claude-msg",
	DeprecatedRemoval:       "v1.0",
	PasteCapable:            true,
	SupportsMCPSlashCommand: true,
	Pane:                    tmuxio.ClaudePaneProfile(),
}

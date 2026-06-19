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
//   - DeprecatedAliases / DeprecatedRemoval: the legacy binary names an adapter
//     carries through a deprecation cycle (Claude: claude-msg AND tmux-msg-claude
//     → removed v1.0, ADR-0008; #440 Phase 3 added the tmux-msg-* rename leg).
//     Empty when the adapter never had a prior name — warnIfDeprecatedName then
//     no-ops. A list so successive renames append without re-shaping the seam.
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
	DeprecatedAliases []string
	DeprecatedRemoval string
	// PasteCapable gates the internal/tmuxio paste-and-enter delivery path.
	// Zero value (false) is the safe default: an adapter that doesn't
	// explicitly assert paste-capability is force-deferred rather than risking
	// a clobber on a pane the observe-gate can't read (#323).
	PasteCapable bool
	// SupportedControlCommands is the per-adapter allowlist of control-command
	// leading tokens (the first whitespace-delimited field of the command body —
	// "/mcp" for "/mcp disable tmux-tell", "/compact" for "/compact") this
	// adapter's CLI actually implements. A KindControl delivery whose leading
	// token is NOT in the set is SKIPPED: the mailman marks it delivered + logs a
	// WARN rather than typing it as literal text that would pollute the prompt and
	// break the session (the breakage witnessed on Lookout after a refresh-all-mcps
	// cascade, #419). This is #420 generalizing #419's narrow `/mcp`-only bool into
	// the per-(command, adapter) compat surface.
	//
	// nil means "supports all control commands" — the reference-adapter
	// convention. Claude Code implements the full slash surface (everything in
	// internal/control.Allowed), so its profile leaves this nil; that also keeps
	// the default `active` profile (Claude) + in-package tests historical-behavior-
	// unchanged. A non-nil set is an explicit allowlist: codex, whose CLI lacks
	// `/mcp …` (only a `--verbose` flag; an MCP restart needs a full session
	// restart, #411) AND `/cost` (`/status` is the closest equivalent), declares
	// the four commands it does honor — `/compact`, `/rename`, `/clear`, `/help`.
	SupportedControlCommands map[string]bool
	// Pane carries the adapter's pane-observation snippets (prompt sentinel,
	// compaction / awaiting-operator / status-line markers) that the
	// internal/tmuxio classifier reads via its process-global activeProfile.
	// cli.Run installs this into tmuxio at process start (#322). The zero value
	// disables cursor-aware classification — adapters should supply a complete
	// profile (e.g. tmuxio.ClaudePaneProfile() for the Claude adapter).
	Pane tmuxio.PaneProfile
	// Provider names the upstream LLM provider this adapter consumes —
	// "anthropic" for claude, "openai" for codex (#448). The mailman writes it
	// to agents.provider at serve start so the cross-mailman per-provider
	// concurrency cap can count how many same-provider chambers are working.
	// Empty (the zero value) opts the agent out of provider-cap accounting — an
	// adapter that doesn't declare a provider is never gated and never counted,
	// so the feature is purely additive on the uncoordinated path.
	Provider string
}

// active is the process-global adapter profile. A CLI binary serves exactly one
// adapter for the life of the process, so a package-global set once at Run entry
// is the pragmatic seam — versus threading Profile through ~90 handler
// signatures. It defaults to the Claude adapter so in-package tests, which
// exercise handlers directly without going through Run, observe the historical
// behavior unchanged.
var active = Profile{
	BinaryName:        "tmux-tell-claude",
	DisplayLabel:      "Claude Code",
	DeprecatedAliases: []string{"claude-msg", "tmux-msg-claude"},
	DeprecatedRemoval: "v1.0",
	PasteCapable:      true,
	// SupportedControlCommands left nil: Claude is the reference adapter and
	// implements the full slash surface, so nil ("supports all", #420) is correct
	// and future-command-proof.
	Pane:     tmuxio.ClaudePaneProfile(),
	Provider: "anthropic",
}

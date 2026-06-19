// Package main is the tmux-tell-claude binary — the Claude Code adapter for the
// tmux-msg substrate. All subcommand dispatch + handlers live in internal/cli
// (the adapter-agnostic shared CLI); this wrapper only supplies the Claude
// adapter Profile and hands off to cli.Run. The substrate-vs-adapter boundary is
// ADR-0009; the second adapter (tmux-tell-codex) is a sibling wrapper over the
// same internal/cli (#248).
//
// The binary renamed claude-msg → tmux-tell-claude per #174 Option 2 / #177;
// claude-msg survives as a deprecated alias through v1.0 (operator decision
// 2026-06-08, ADR-0008 §Discretion clause). install.sh keeps the alias symlink;
// cli.Run emits the ADR-0008 deprecation WARN when invoked through it. Each
// subcommand handler lives in its own file under internal/cli, split into
// runFooCLI (flag parsing + store open) and runFooWithStore (pure logic,
// testable). See the README for the project shape.
package main

import (
	"os"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/cli"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
)

func main() {
	os.Exit(cli.Run(claudeProfile(), os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func claudeProfile() cli.Profile {
	return cli.Profile{
		BinaryName:        "tmux-tell-claude",
		DisplayLabel:      "Claude Code",
		DeprecatedAliases: []string{"claude-msg", "tmux-msg-claude"},
		DeprecatedRemoval: "v1.0",
		// Claude's TUI paints the ❯ prompt sentinel the observe-gate reads to
		// defer paste-and-enter during operator-typing (internal/tmuxio), so
		// this adapter is paste-capable.
		PasteCapable: true,
		// SupportedControlCommands left nil (#420): Claude is the reference
		// adapter — its CLI implements the full slash surface (everything in
		// internal/control.Allowed, including `/mcp …` and `/cost`), so nil
		// ("supports all") is correct and future-command-proof. The explicit
		// allowlist exists for the narrower codex surface; see tmux-tell-codex.
		// Pane-observation snippets the tmuxio classifier reads (#322): the ❯
		// prompt sentinel + compaction / awaiting-operator / status-line
		// markers, empirically pinned by the canary tests in
		// internal/tmuxio/state_canary_test.go.
		Pane: tmuxio.ClaudePaneProfile(),
		// Provider for the #448 per-provider concurrency cap — Claude consumes
		// Anthropic's API.
		Provider: "anthropic",
	}
}

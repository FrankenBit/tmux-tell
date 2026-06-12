// Package main is the tmux-msg-codex binary — the OpenAI Codex CLI adapter for
// the tmux-msg substrate, and the second adapter that proves the ADR-0009
// substrate-vs-adapter boundary (#248). Like tmux-msg-claude it is a thin
// wrapper: all subcommand dispatch + handlers live in the adapter-agnostic
// internal/cli; this binary only supplies the Codex adapter Profile and hands
// off to cli.Run.
//
// Codex never had a prior binary name, so it carries no deprecation alias (the
// claude-msg → tmux-msg-claude cycle is Claude-only). DeliveryMode for a Codex
// agent is hook-context (#248 decision (B), ADR-0009): the substrate's
// hook-context helper (internal/cli/hook.go) is already adapter-agnostic —
// Codex's hook output schema (hookSpecificOutput.hookEventName +
// additionalContext) matches Claude's, so `tmux-msg-codex hook-context` presents
// pending messages with zero substrate changes. The deep paste-and-enter coupling
// (internal/tmuxio observe-gate sentinels) is Claude-only and not exercised here.
//
// See docs/reference.md §Adapter integration for wiring a Codex agent's hook.
package main

import (
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/cli"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

func main() {
	p := cli.Profile{
		BinaryName:   "tmux-msg-codex",
		DisplayLabel: "Codex",
		// No DeprecatedAlias: Codex is a new adapter with no legacy name.
		// PasteCapable stays false: #322's PaneProfile now teaches the observe-
		// gate to READ Codex's `› ` input area (agent_state classifies idle /
		// working / awaiting-operator correctly), but the verify-token mechanism
		// is not yet robust to Codex's paste-collapse + slow render (the
		// cross-adapter verify-token-robustness work). Until that lands, Codex
		// stays non-paste: delivery is hook-context (#248 decision (B), ADR-0009)
		// and the mailman force-defers any paste-and-enter delivery to it (#323).
		PasteCapable: false,
		// Pane-observation snippets the tmuxio classifier reads (#322). Codex's
		// `› ` sentinel (U+203A + space) is substrate-verified; marker fields are
		// empty pending characterization of Codex's compaction / popup / status
		// UIs — see tmuxio.CodexPaneProfile for the named gaps.
		Pane: tmuxio.CodexPaneProfile(),
	}
	os.Exit(cli.Run(p, os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

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
)

func main() {
	p := cli.Profile{
		BinaryName:   "tmux-msg-codex",
		DisplayLabel: "Codex",
		// No DeprecatedAlias: Codex is a new adapter with no legacy name.
		// PasteCapable stays false (explicit for the reader): the observe-gate
		// can't yet classify Codex's `›` input area, so paste-and-enter would
		// clobber operator input (#323). Codex delivers via hook-context
		// (#248 decision (B), ADR-0009); the mailman force-defers any
		// paste-and-enter delivery to a Codex agent until #322's PaneProfile
		// refactor teaches the observe-gate to read Codex panes.
		PasteCapable: false,
	}
	os.Exit(cli.Run(p, os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

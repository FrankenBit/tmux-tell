// Package main is the tmux-msg-codex binary — the OpenAI Codex CLI adapter for
// the tmux-msg substrate, and the second adapter that proves the ADR-0009
// substrate-vs-adapter boundary (#248). Like tmux-msg-claude it is a thin
// wrapper: all subcommand dispatch + handlers live in the adapter-agnostic
// internal/cli; this binary only supplies the Codex adapter Profile and hands
// off to cli.Run.
//
// Codex never had a prior binary name, so it carries no deprecation alias (the
// claude-msg → tmux-msg-claude cycle is Claude-only). A Codex agent can be served
// either way: hook-context (#248 decision (B), ADR-0009 — the hook helper is
// adapter-agnostic; Codex's hook output schema matches Claude's) OR paste-and-enter
// (#360 — once #322 taught the observe-gate to read Codex's `› ` sentinel, the
// observe-gate sentinels are no longer Claude-only; Codex panes classify + defer
// correctly). Codex is PasteCapable (see the Profile below) so the
// register-time default (paste-and-enter) now works for it.
//
// See docs/reference.md §Adapter integration for wiring a Codex agent.
package main

import (
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/cli"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

func main() {
	os.Exit(cli.Run(codexProfile(), os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// codexProfile is the Codex adapter Profile cli.Run consumes. Extracted from
// main so main_test.go can pin its load-bearing fields (notably the #360
// PasteCapable=true flip) against accidental regression — the flip is the
// headline behavior of #360, so it earns a pin.
func codexProfile() cli.Profile {
	return cli.Profile{
		BinaryName:   "tmux-msg-codex",
		DisplayLabel: "Codex",
		// No DeprecatedAlias: Codex is a new adapter with no legacy name.
		// PasteCapable=true (#360): #322 taught the observe-gate to READ Codex's
		// `› ` input area, so it classifies idle / working / awaiting-operator and
		// defers paste-and-enter while a Codex operator is typing — the #323
		// clobber premise is dissolved. Operator-witnessed + Engineer-confirmed
		// that Codex paste-and-enter works: the apparent "delay" was Codex's
		// submit visual (the submitted prompt lingers while a new input opens
		// below + the cursor jumps down), NOT a delivery failure. The remaining
		// verify-token fragility on paste-collapse + mid-turn (#336) is
		// CROSS-ADAPTER — Claude collapses multi-line too and is paste-capable —
		// so it is inherited, not a Codex-specific gate.
		PasteCapable: true,
		// Pane-observation snippets the tmuxio classifier reads (#322). Codex's
		// `› ` sentinel (U+203A + space) is substrate-verified; marker fields are
		// empty pending characterization of Codex's compaction / popup / status
		// UIs — see tmuxio.CodexPaneProfile for the named gaps.
		Pane: tmuxio.CodexPaneProfile(),
	}
}

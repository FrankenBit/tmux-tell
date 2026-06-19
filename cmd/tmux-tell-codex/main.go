// Package main is the tmux-tell-codex binary — the OpenAI Codex CLI adapter for
// the tmux-msg substrate, and the second adapter that proves the ADR-0009
// substrate-vs-adapter boundary (#248). Like tmux-tell-claude it is a thin
// wrapper: all subcommand dispatch + handlers live in the adapter-agnostic
// internal/cli; this binary only supplies the Codex adapter Profile and hands
// off to cli.Run.
//
// Codex never had a prior binary name, so it carries no deprecation alias (the
// claude-msg → tmux-tell-claude cycle is Claude-only). A Codex agent can be served
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

	"git.frankenbit.de/frankenbit/tmux-tell/internal/cli"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/tmuxio"
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
		BinaryName:   "tmux-tell-codex",
		DisplayLabel: "Codex",
		// Codex never had the claude-msg leg, but the #440 substrate rename gave
		// it a tmux-msg-codex → tmux-tell-codex deprecation alias (Phase 3, removed
		// v1.0 per ADR-0008) so any script / muscle-memory on the old name survives.
		DeprecatedAliases: []string{"tmux-msg-codex"},
		DeprecatedRemoval: "v1.0",
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
		// Per-(command, adapter) control-command compat allowlist (#420,
		// generalizing #419's narrow `/mcp`-only bool). Codex's CLI implements
		// only these four slash commands; a control delivery for anything else
		// (notably `/mcp …` — codex has only a `--verbose` flag, an MCP restart
		// needs a full session restart, #411 — and `/cost`, for which `/status` is
		// the closest equivalent) is SKIPPED (logged + marked delivered) rather
		// than pasted as literal text that pollutes the prompt and breaks the
		// session (the breakage witnessed on Lookout after a refresh-all-mcps
		// cascade, #419). Keyed on the leading command token; the explicit set
		// (vs Claude's nil = supports-all) makes codex's narrower surface visible.
		//
		// Provenance of the four-command set: the operator-clarified codex
		// slash-command surface captured in #420 (2026-06-14) — `/compact`,
		// `/rename`, `/clear`, `/help` EXIST; `/cost` (use `/status`) and `/mcp …`
		// are MISSING. `/compact` is additionally test-load-bearing (codex chambers
		// sleep via the bus `sleep` verb → `/compact`). Per the ship-no-guessed-
		// literals discipline (#504): an over-broad inclusion would re-open the
		// #419 false-paste pollution, so the set is grounded in that operator
		// characterization rather than assumed.
		SupportedControlCommands: map[string]bool{
			"/compact": true,
			"/rename":  true,
			"/clear":   true,
			"/help":    true,
		},
		// Pane-observation snippets the tmuxio classifier reads (#322). Codex's
		// `› ` sentinel (U+203A + space) is substrate-verified; marker fields are
		// empty pending characterization of Codex's compaction / popup / status
		// UIs — see tmuxio.CodexPaneProfile for the named gaps.
		Pane: tmuxio.CodexPaneProfile(),
		// Provider for the #448 per-provider concurrency cap — Codex consumes
		// OpenAI's API, a separate provider pool from Claude's Anthropic, so the
		// two adapters' caps are accounted independently.
		Provider: "openai",
	}
}

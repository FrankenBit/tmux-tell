// Package main is the tmux-msg-claude binary — the Claude Code adapter for the
// tmux-msg substrate. All subcommand dispatch + handlers live in internal/cli
// (the adapter-agnostic shared CLI); this wrapper only supplies the Claude
// adapter Profile and hands off to cli.Run. The substrate-vs-adapter boundary is
// ADR-0009; the second adapter (tmux-msg-codex) is a sibling wrapper over the
// same internal/cli (#248).
//
// The binary renamed claude-msg → tmux-msg-claude per #174 Option 2 / #177;
// claude-msg survives as a deprecated alias through v1.0 (operator decision
// 2026-06-08, ADR-0008 §Discretion clause). install.sh keeps the alias symlink;
// cli.Run emits the ADR-0008 deprecation WARN when invoked through it. Each
// subcommand handler lives in its own file under internal/cli, split into
// runFooCLI (flag parsing + store open) and runFooWithStore (pure logic,
// testable). See the README for the project shape.
package main

import (
	"os"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/cli"
)

func main() {
	p := cli.Profile{
		BinaryName:        "tmux-msg-claude",
		DeprecatedAlias:   "claude-msg",
		DeprecatedRemoval: "v1.0",
	}
	os.Exit(cli.Run(p, os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

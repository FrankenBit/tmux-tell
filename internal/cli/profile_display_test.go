package cli

import (
	"bytes"
	"strings"
	"testing"
)

// withProfile swaps the process-global active profile for the duration of a
// test and restores it after, so adapter-correctness assertions can exercise a
// non-default (Codex) profile without leaking into the other in-package tests
// that rely on the historical Claude default.
func withProfile(t *testing.T, p Profile) {
	t.Helper()
	prev := active
	active = p
	t.Cleanup(func() { active = prev })
}

var codexProfile = Profile{BinaryName: "tmux-tell-codex", DisplayLabel: "Codex"}

// TestUsageText_ThreadsProfile proves the top-level usage prose names the active
// adapter — both the binary name and the display-label-driven "mcp"/"hook-context"
// descriptions (#280). Under the Codex profile no "Claude" literal survives; the
// Claude default reproduces the historical strings verbatim (behavior-preserving).
func TestUsageText_ThreadsProfile(t *testing.T) {
	t.Run("codex profile", func(t *testing.T) {
		withProfile(t, codexProfile)
		got := usageText()
		for _, want := range []string{
			"usage: tmux-tell-codex <subcommand>",
			"Speak MCP over stdio (Codex tools)",
			"invoked by a Codex SessionStart/UserPromptSubmit hook",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("usageText() missing %q under codex profile;\ngot:\n%s", want, got)
			}
		}
		if strings.Contains(got, "Claude") {
			t.Errorf("usageText() still names Claude under codex profile;\ngot:\n%s", got)
		}
	})

	t.Run("claude default unchanged", func(t *testing.T) {
		got := usageText()
		for _, want := range []string{
			"usage: tmux-tell-claude <subcommand>",
			"Speak MCP over stdio (Claude Code tools)",
			"invoked by a Claude Code SessionStart/UserPromptSubmit hook",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("usageText() missing historical claude string %q;\ngot:\n%s", want, got)
			}
		}
	})
}

// TestSubcommandUsageHint_ThreadsProfile proves a per-handler flag-error usage
// hint names the active adapter binary (#280). runConfigCLI with no args prints
// its hint before any store open, so it exercises the hint path in isolation.
func TestSubcommandUsageHint_ThreadsProfile(t *testing.T) {
	t.Run("codex profile", func(t *testing.T) {
		withProfile(t, codexProfile)
		var stderr bytes.Buffer
		runConfigCLI(nil, &bytes.Buffer{}, &stderr)
		got := stderr.String()
		if !strings.Contains(got, "usage: tmux-tell-codex config") {
			t.Errorf("config usage hint not threaded with codex binary; got %q", got)
		}
		if strings.Contains(got, "tmux-tell-claude") {
			t.Errorf("config usage hint still names tmux-tell-claude under codex profile; got %q", got)
		}
	})

	t.Run("claude default unchanged", func(t *testing.T) {
		var stderr bytes.Buffer
		runConfigCLI(nil, &bytes.Buffer{}, &stderr)
		if got := stderr.String(); !strings.Contains(got, "usage: tmux-tell-claude config") {
			t.Errorf("config usage hint changed for claude default; got %q", got)
		}
	})
}

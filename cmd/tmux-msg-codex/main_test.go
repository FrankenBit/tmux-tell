package main

import (
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// TestCodexProfile_PasteCapable pins the #360 headline flip: the Codex adapter
// declares PasteCapable=true so a Codex agent on the register-time default
// (paste-and-enter) is served by the mailman paste path rather than force-
// deferred by the serve-time safe-default guard. A regression to false would
// silently send every default-registered Codex agent back to "messages stay
// queued" — so the flip earns a direct pin, not just incidental coverage.
func TestCodexProfile_PasteCapable(t *testing.T) {
	p := codexProfile()
	if !p.PasteCapable {
		t.Errorf("codex Profile.PasteCapable = false, want true (#360 flip regressed)")
	}
	if p.BinaryName != "tmux-msg-codex" {
		t.Errorf("BinaryName = %q, want tmux-msg-codex", p.BinaryName)
	}
	if p.DeprecatedAlias != "" {
		t.Errorf("DeprecatedAlias = %q, want empty (Codex has no legacy name)", p.DeprecatedAlias)
	}
}

// TestCodexProfile_PaneSentinel pins that the Profile carries Codex's substrate-
// verified `› ` prompt sentinel — the read-side prerequisite the PasteCapable
// flip depends on (the observe-gate must be able to anchor the input row to
// defer during operator-typing and to read the cursor-anchored verify signal).
// The exact bytes are canary-pinned in internal/tmuxio; here we pin that the
// codex binary actually wires that profile rather than an empty one.
func TestCodexProfile_PaneSentinel(t *testing.T) {
	p := codexProfile()
	if p.Pane.PromptSentinel != tmuxio.CodexPromptSentinel {
		t.Errorf("Pane.PromptSentinel = %q, want CodexPromptSentinel %q",
			p.Pane.PromptSentinel, tmuxio.CodexPromptSentinel)
	}
}

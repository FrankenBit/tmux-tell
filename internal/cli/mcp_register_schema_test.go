package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRegisterToolSchema_AdapterNamedAndNeutral proves the tmux-msg.register MCP
// tool schema (#314): under a codex profile it names the codex binary in the
// mailman-unit / inbox / hook-context references and carries no claude literal,
// and the delivery-mode prose is adapter-neutral ("the recipient agent's
// session") for BOTH adapters. The codex-profile binary-name threading is the
// fix; the "Claude session" → "agent's session" neutralization is an
// intentional prose change that applies to the claude adapter too (the register
// tool describes substrate-general mechanism, so naming Claude there was the
// substrate-vs-adapter leak ADR-0009 governs). reuses withProfile/codexProfile
// from profile_display_test.go.
func TestRegisterToolSchema_AdapterNamedAndNeutral(t *testing.T) {
	t.Run("valid JSON under both profiles", func(t *testing.T) {
		if !json.Valid(registerToolSchema()) {
			t.Error("register schema is not valid JSON under the claude default")
		}
		withProfile(t, codexProfile)
		if !json.Valid(registerToolSchema()) {
			t.Error("register schema is not valid JSON under the codex profile")
		}
	})

	t.Run("codex profile names codex, no claude leak", func(t *testing.T) {
		withProfile(t, codexProfile)
		got := string(registerToolSchema())
		for _, want := range []string{
			"tmux-msg-codex-mailman@NAME",
			"tmux-msg-codex inbox",
			"tmux-msg-codex hook-context",
			"the recipient agent's session", // adapter-neutral framing
		} {
			if !strings.Contains(got, want) {
				t.Errorf("register schema missing %q under codex profile", want)
			}
		}
		if strings.Contains(got, "tmux-msg-claude") {
			t.Errorf("register schema still carries a tmux-msg-claude literal under codex profile:\n%s", got)
		}
		if strings.Contains(got, "Claude session") {
			t.Errorf("register schema still carries 'Claude session' framing under codex profile")
		}
	})

	t.Run("claude default: binary literals preserved, prose neutralized", func(t *testing.T) {
		got := string(registerToolSchema())
		// Behavior-preserving on the binary name for the claude adapter.
		for _, want := range []string{
			"tmux-msg-claude-mailman@NAME",
			"tmux-msg-claude inbox",
			"tmux-msg-claude hook-context",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("register schema dropped historical claude binary literal %q", want)
			}
		}
		// Intentional neutralization — applies to claude too.
		if !strings.Contains(got, "the recipient agent's session") {
			t.Error("register schema missing neutral 'the recipient agent's session' framing")
		}
		if strings.Contains(got, "recipient's Claude session") {
			t.Error("register schema still carries the old 'recipient's Claude session' framing")
		}
	})
}

// TestRegisterCLIHelp_DeliveryModeNeutral proves the `register --delivery-mode`
// CLI flag-help — the parallel consumer surface to the MCP schema, shown by
// `<binary> register --help` — carries the same adapter-neutral framing (#314,
// folded in per Surveyor review of #326). Same under-claim failure mode as the
// schema: a substrate-general surface must not name Claude. Profile-independent
// (the neutralization is static prose, not binary-name threading).
func TestRegisterCLIHelp_DeliveryModeNeutral(t *testing.T) {
	var stderr bytes.Buffer
	// -h makes the FlagSet print its defaults (incl. the delivery-mode help)
	// to stderr, then runRegisterCLI returns without opening a store.
	runRegisterCLI([]string{"-h"}, &bytes.Buffer{}, &stderr)
	got := stderr.String()
	if !strings.Contains(got, "the recipient agent's session") {
		t.Errorf("register --delivery-mode help missing neutral framing; got:\n%s", got)
	}
	if strings.Contains(got, "Claude session") {
		t.Errorf("register --delivery-mode help still names a Claude session; got:\n%s", got)
	}
}

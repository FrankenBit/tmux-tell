package cli

import (
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// release-toolkit#923: mailmanActive must resolve the unit from the RECIPIENT's
// adapter, not the caller's.
//
// 🔴 THE ARM THAT MATTERS IS "codex" — and it is the one that could not fail
// before the fix for a reason worth stating: mailmanUnit() built the name from
// active.BinaryName, the binary that is RUNNING. Under `go test` that is the
// test binary, so a naive arm asserting "the claude unit was probed" would have
// passed both before and after. The assertion has to be that a CODEX recipient
// yields the CODEX unit, which is false under the old code for any caller.
//
// The control arm is not decoration: without it, a resolver that returned the
// codex unit unconditionally would satisfy the first assertion.
func TestMailmanActiveResolvesRecipientAdapter(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		want     string
	}{
		{"codex recipient", "openai", "tmux-tell-codex-mailman@subject.service"},
		{"claude recipient", "anthropic", "tmux-tell-claude-mailman@subject.service"},
		{"no provider persisted", "", "tmux-tell-claude-mailman@subject.service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, err := store.Open(":memory:")
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			defer func() { _ = s.Close() }()
			// The agent row must exist before a provider can attach to it —
			// without this, SetProvider is a no-op and every arm silently
			// exercises the claude fallback. Measured: the codex arm failed
			// for that reason and not for the one it is written to catch.
			if err := s.UpsertAgent(ctx, "subject", "%1"); err != nil {
				t.Fatalf("UpsertAgent: %v", err)
			}
			if tc.provider != "" {
				if err := s.SetProvider(ctx, "subject", tc.provider); err != nil {
					t.Fatalf("SetProvider: %v", err)
				}
			}

			var probed string
			restore := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
				probed = args[len(args)-1]
				return []byte("active\n"), nil
			})
			defer setSystemctlRunner(restore)

			if got := mailmanActive(ctx, s, "subject"); !got {
				t.Fatalf("mailmanActive returned false on a stub reporting %q", "active")
			}
			if probed != tc.want {
				t.Fatalf("probed the wrong unit\n  got:  %s\n  want: %s\n"+
					"a %s recipient must resolve to its OWN adapter's template, "+
					"regardless of which binary is running", probed, tc.want, tc.provider)
			}
			if strings.Contains(probed, "subject.service") == false {
				t.Fatalf("unit does not name the agent: %s", probed)
			}
		})
	}
}

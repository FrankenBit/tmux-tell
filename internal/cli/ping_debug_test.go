package cli

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/debug"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestGatherPingEvidence_DebugGated pins the #681 convention at its first
// consumer (#365 nit2): a dropped best-effort probe error under
// gatherPingEvidence emits a debug line ONLY when TMUX_TELL_DEBUG is set, and
// stays silent otherwise. Driven via a ghost agent — GetAgent errors on a name
// that was never registered — so at least one probe reliably drops an error.
func TestGatherPingEvidence_DebugGated(t *testing.T) {
	s, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // best-effort close in test
	ctx := context.Background()

	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	// Disabled: no debug output even though the ghost agent's GetAgent errors.
	t.Setenv(debug.EnvVar, "")
	buf.Reset()
	_ = gatherPingEvidence(ctx, s, "ghost-never-registered")
	if buf.Len() != 0 {
		t.Errorf("%s unset: expected no debug output, got %q", debug.EnvVar, buf.String())
	}

	// Enabled: the dropped errors surface debug lines. A never-registered agent
	// errors BOTH the GetAgent lookup and the state-resolve probe (resolveAgentState
	// returns ErrNotFound), so this exercises both structurally-distinct guard
	// shapes on the emit side — the `} else if debug.Enabled()` lookup guard AND
	// the `if err != nil && debug.Enabled()` state-resolve guard — not just one.
	t.Setenv(debug.EnvVar, "1")
	buf.Reset()
	_ = gatherPingEvidence(ctx, s, "ghost-never-registered")
	out := buf.String()
	for _, want := range []string{"lookup err=", "state-resolve err="} {
		if !strings.Contains(out, want) {
			t.Errorf("%s=1: expected a debug line containing %q, got %q", debug.EnvVar, want, out)
		}
	}
}

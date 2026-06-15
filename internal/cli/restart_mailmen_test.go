package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// errExitStub is a generic non-nil error for faking a systemctl command failure.
var errExitStub = errors.New("systemctl: exit status 1")

// TestMailmanUnitAgent pins extraction of the agent name from an active-adapter
// mailman unit, and that a non-matching unit (wrong adapter / not a mailman)
// yields "".
func TestMailmanUnitAgent(t *testing.T) {
	// active defaults to the claude profile.
	if got := mailmanUnitAgent("tmux-tell-claude-mailman@bob.service"); got != "bob" {
		t.Errorf("claude unit → %q, want bob", got)
	}
	if got := mailmanUnitAgent("tmux-tell-codex-mailman@lookout.service"); got != "" {
		t.Errorf("codex unit under claude profile → %q, want \"\" (wrong adapter)", got)
	}
	if got := mailmanUnitAgent("some-other.service"); got != "" {
		t.Errorf("non-mailman unit → %q, want \"\"", got)
	}

	withActiveProfile(t, codexPasteCapableProfile)
	if got := mailmanUnitAgent("tmux-tell-codex-mailman@lookout.service"); got != "lookout" {
		t.Errorf("codex unit under codex profile → %q, want lookout", got)
	}
}

// TestRunningMailmanAgents pins that the list-units output is parsed to agent
// names and that the systemctl call is adapter-scoped (the glob carries the
// active BinaryName).
func TestRunningMailmanAgents(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	var listArgs []string
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		listArgs = args
		// realistic --plain --no-legend list-units output (UNIT LOAD ACTIVE SUB DESC)
		return []byte(
			"tmux-tell-codex-mailman@lookout.service loaded active running tmux-msg mailman for lookout\n" +
				"tmux-tell-codex-mailman@scout.service   loaded active running tmux-msg mailman for scout\n"), nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	agents, err := runningMailmanAgents(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(agents) != 2 || agents[0] != "lookout" || agents[1] != "scout" {
		t.Errorf("agents = %v, want [lookout scout]", agents)
	}
	// adapter-scoped glob
	joined := strings.Join(listArgs, " ")
	if !strings.Contains(joined, "tmux-tell-codex-mailman@*.service") {
		t.Errorf("list-units not scoped to codex glob; args=%v", listArgs)
	}
	if !strings.Contains(joined, "--state=active") {
		t.Errorf("list-units should filter --state=active; args=%v", listArgs)
	}
}

// TestRunRestartMailmen_RestartsEach: every running mailman is restarted and the
// command reports ok with exitOK.
func TestRunRestartMailmen_RestartsEach(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	var restarted []string
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-units":
			return []byte("tmux-tell-codex-mailman@lookout.service loaded active running x\n" +
				"tmux-tell-codex-mailman@scout.service loaded active running x\n"), nil
		case "restart":
			restarted = append(restarted, args[1])
		}
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	var out, errb bytes.Buffer
	code := runRestartMailmen(context.Background(), "text", &out, &errb)
	if code != exitOK {
		t.Fatalf("exit = %d, want exitOK; out=%s err=%s", code, out.String(), errb.String())
	}
	if len(restarted) != 2 ||
		restarted[0] != "tmux-tell-codex-mailman@lookout.service" ||
		restarted[1] != "tmux-tell-codex-mailman@scout.service" {
		t.Errorf("restarted units = %v, want both codex mailman units", restarted)
	}
	if !strings.Contains(out.String(), "2 restarted") {
		t.Errorf("report should say 2 restarted; got %s", out.String())
	}
}

// TestRunRestartMailmen_ZeroIsSuccess: no running mailmen → nothing to do →
// exitOK (a deploy onto an idle host shouldn't fail).
func TestRunRestartMailmen_ZeroIsSuccess(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] == "list-units" {
			return []byte(""), nil // no units
		}
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	var out, errb bytes.Buffer
	if code := runRestartMailmen(context.Background(), "text", &out, &errb); code != exitOK {
		t.Fatalf("zero mailmen should be exitOK; got %d", code)
	}
}

// TestRunRestartMailmen_RestartFailureSurfaces: a failing restart → exitInternal
// and the failed agent is named.
func TestRunRestartMailmen_RestartFailureSurfaces(t *testing.T) {
	withActiveProfile(t, codexPasteCapableProfile)
	prev := setSystemctlRunner(func(_ context.Context, args ...string) ([]byte, error) {
		switch args[0] {
		case "list-units":
			return []byte("tmux-tell-codex-mailman@lookout.service loaded active running x\n"), nil
		case "restart":
			return []byte("Job for ... failed"), errExitStub
		}
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	var out, errb bytes.Buffer
	if code := runRestartMailmen(context.Background(), "text", &out, &errb); code != exitInternal {
		t.Fatalf("restart failure should be exitInternal; got %d", code)
	}
	if !strings.Contains(out.String(), "lookout") || !strings.Contains(out.String(), "FAILED") {
		t.Errorf("report should name the failed agent; got %s", out.String())
	}
}

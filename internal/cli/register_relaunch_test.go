package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// register --relaunch-cmd / --auto-restart persist the #285/#730 fields, and — the
// load-bearing part — a subsequent BARE re-register (no relaunch flags) must NOT
// wipe them. The chamber wrappers auto-register on every launch, so a flag that
// silently reset to its zero-value on re-register would disarm a chamber the
// operator opted in. Mutation anchor: dropping the fs.Visit-gated conditional (and
// writing the flags unconditionally) flips the "preserved on bare re-register"
// asserts.
func TestRegister_CLI_RelaunchFlags(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "messages.db")
	ctx := context.Background()

	seed, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.UpsertAgent(ctx, "pilot", "%6"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	_ = seed.Close()

	t.Setenv("CLAUDE_MSG_DB", dbPath)

	// 1. Register WITH the relaunch flags → both persist.
	var stdout, stderr bytes.Buffer
	const cmd = "chamber-claude.sh Pilot"
	exit := runRegisterCLI([]string{
		"--name", "pilot", "--pane", "%6", "--force", "--start-mailman=false",
		"--relaunch-cmd", cmd, "--auto-restart",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("register exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	check, _ := store.Open(dbPath)
	a, _ := check.GetAgent(ctx, "pilot")
	if a.RelaunchCmd != cmd || !a.AutoRestart {
		t.Fatalf("after register-with-flags: relaunch_cmd=%q auto_restart=%v, want %q / true", a.RelaunchCmd, a.AutoRestart, cmd)
	}
	_ = check.Close()

	// 2. Bare re-register (no relaunch flags) → both PRESERVED.
	stdout.Reset()
	stderr.Reset()
	exit = runRegisterCLI([]string{
		"--name", "pilot", "--pane", "%6", "--force", "--start-mailman=false",
	}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("bare re-register exit = %d, want exitOK; stderr=%s", exit, stderr.String())
	}
	check2, _ := store.Open(dbPath)
	a2, _ := check2.GetAgent(ctx, "pilot")
	if a2.RelaunchCmd != cmd {
		t.Errorf("relaunch_cmd = %q after bare re-register, want preserved %q", a2.RelaunchCmd, cmd)
	}
	if !a2.AutoRestart {
		t.Errorf("auto_restart = %v after bare re-register, want preserved true", a2.AutoRestart)
	}
	_ = check2.Close()
}

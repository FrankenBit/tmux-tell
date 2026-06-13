package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestDBMigrate_DryRun verifies --dry-run prints the plan and exits without
// touching the filesystem or systemd. The systemctl runner is left
// untouched (would panic if called); a side-effect would tickle it.
func TestDBMigrate_DryRun(t *testing.T) {

	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		t.Fatalf("dry-run must not call systemctl: %v", args)
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	source := filepath.Join(tmp, "src", "messages.db")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "dst", "messages.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	seedAgents(t, source, []string{"alpha", "beta"})

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", source, "--dry-run", "--format", "json", dest},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}

	var got dbMigrateResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, stdout.String())
	}
	if !got.OK || !got.DryRun {
		t.Fatalf("want OK=true DryRun=true, got %+v", got)
	}
	if got.Agents != 2 {
		t.Errorf("agents=%d want 2", got.Agents)
	}
	if got.Source != source || got.Dest != dest {
		t.Errorf("source/dest mismatch: %+v", got)
	}

	if _, err := os.Stat(source); err != nil {
		t.Errorf("source should still exist after dry-run: %v", err)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("dest should not exist after dry-run")
	}
}

// TestDBMigrate_MovesFile_NonDefaultDest verifies the happy-path FS work
// (steps 1-5) when destination is a bespoke (non-default) path. Mailman
// restart + refresh-all-mcps are skipped per the substrate constraint;
// the warning surfaces in the result.
func TestDBMigrate_MovesFile_NonDefaultDest(t *testing.T) {

	var systemctlCalls []string
	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		systemctlCalls = append(systemctlCalls, strings.Join(args, " "))
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	source := filepath.Join(tmp, "src", "messages.db")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, "dst", "messages.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	seedAgents(t, source, []string{"alpha", "beta"})

	// Force defaultDBLocation() to a third path (not dest) so dest is
	// recognized as non-default and steps 6+7 skip.
	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_MSG_DB", "")

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", source, "--format", "json", dest},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s stdout=%s", rc, stderr.String(), stdout.String())
	}

	var got dbMigrateResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, stdout.String())
	}
	if !got.OK || got.DryRun {
		t.Fatalf("want OK=true DryRun=false, got %+v", got)
	}
	if got.AgentsStopped != 2 {
		t.Errorf("stopped=%d want 2", got.AgentsStopped)
	}
	if got.AgentsStarted != 0 {
		t.Errorf("started=%d want 0 (non-default skip)", got.AgentsStarted)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "does not match default") {
		t.Errorf("expected non-default warning, got %v", got.Warnings)
	}

	if _, err := os.Stat(source); err == nil {
		t.Errorf("source should be gone after move")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("dest should exist after move: %v", err)
	}
	// Sidecars must be cleaned at source.
	for _, suf := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(source + suf); err == nil {
			t.Errorf("source%s should be cleaned, still exists", suf)
		}
	}

	// systemctl should have been called twice (disable per agent), zero
	// start calls because dest is non-default.
	disables := 0
	starts := 0
	for _, c := range systemctlCalls {
		if strings.HasPrefix(c, "disable") {
			disables++
		}
		if strings.HasPrefix(c, "enable") {
			starts++
		}
	}
	if disables != 2 {
		t.Errorf("disable calls=%d want 2; all=%v", disables, systemctlCalls)
	}
	if starts != 0 {
		t.Errorf("enable calls=%d want 0; all=%v", starts, systemctlCalls)
	}
}

// TestDBMigrate_RejectsMissingPositional verifies the positional argument
// is required.
func TestDBMigrate_RejectsMissingPositional(t *testing.T) {

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI([]string{"--format", "json"}, &stdout, &stderr)
	if rc != exitUsage {
		t.Fatalf("rc=%d want exitUsage (%d); stderr=%s", rc, exitUsage, stderr.String())
	}
}

// TestDBMigrate_RejectsSourceEqualsDest verifies source == dest is refused.
func TestDBMigrate_RejectsSourceEqualsDest(t *testing.T) {

	tmp := t.TempDir()
	same := filepath.Join(tmp, "messages.db")
	seedAgents(t, same, nil)

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", same, "--format", "json", same},
		&stdout, &stderr,
	)
	if rc != exitUsage {
		t.Fatalf("rc=%d want exitUsage; stdout=%s", rc, stdout.String())
	}
	if !strings.Contains(stdout.String(), "same path") {
		t.Errorf("expected 'same path' error in stdout, got %s", stdout.String())
	}
}

// TestDBMigrate_RejectsExistingDest verifies an existing destination is
// not overwritten.
func TestDBMigrate_RejectsExistingDest(t *testing.T) {

	tmp := t.TempDir()
	source := filepath.Join(tmp, "src.db")
	dest := filepath.Join(tmp, "dst.db")
	seedAgents(t, source, nil)
	if err := os.WriteFile(dest, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", source, "--format", "json", dest},
		&stdout, &stderr,
	)
	if rc != exitDataErr {
		t.Fatalf("rc=%d want exitDataErr; stdout=%s", rc, stdout.String())
	}
	if !strings.Contains(stdout.String(), "already exists") {
		t.Errorf("expected overwrite refusal, got %s", stdout.String())
	}
}

// TestDBMigrate_RejectsMissingParent verifies a destination whose parent
// dir doesn't exist surfaces a clear error (no silent MkdirAll).
func TestDBMigrate_RejectsMissingParent(t *testing.T) {

	tmp := t.TempDir()
	source := filepath.Join(tmp, "src.db")
	seedAgents(t, source, nil)
	dest := filepath.Join(tmp, "nope", "missing", "messages.db")

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", source, "--format", "json", dest},
		&stdout, &stderr,
	)
	if rc != exitDataErr {
		t.Fatalf("rc=%d want exitDataErr; stdout=%s", rc, stdout.String())
	}
	if !strings.Contains(stdout.String(), "parent dir does not exist") {
		t.Errorf("expected parent-missing error, got %s", stdout.String())
	}
}

// TestDBMigrate_SkipsHookContextAgents verifies hook-context agents are
// not iterated for mailman start/stop (their mailman unit isn't enabled
// by design).
func TestDBMigrate_SkipsHookContextAgents(t *testing.T) {

	var stops int
	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "disable" {
			stops++
		}
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	source := filepath.Join(tmp, "src.db")
	dest := filepath.Join(tmp, "dst", "messages.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	seedAgentsWithModes(t, source, map[string]string{
		"paste1": "paste-and-enter",
		"hook1":  store.DeliveryModeHookContext,
		"paste2": "paste-and-enter",
	})

	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_MSG_DB", "")

	var stdout, stderr bytes.Buffer
	rc := runDBMigrateCLI(
		[]string{"--db", source, "--format", "json", dest},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}
	if stops != 2 {
		t.Errorf("stop calls=%d want 2 (hook-context skipped); stdout=%s", stops, stdout.String())
	}
}

func TestCheckpointTruncate_OK(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "messages.db")
	seedAgents(t, path, []string{"alpha"})

	if err := checkpointTruncate(context.Background(), path); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	// File should still be openable + queryable after the checkpoint.
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("re-open after checkpoint: %v", err)
	}
	defer s.Close()
	agents, err := s.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("list agents post-checkpoint: %v", err)
	}
	if len(agents) != 1 {
		t.Errorf("agents=%d want 1", len(agents))
	}
}

func TestMoveFile_SameVolume(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "src.bin")
	dst := filepath.Join(tmp, "sub", "dst.bin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatalf("move: %v", err)
	}
	if _, err := os.Stat(src); err == nil {
		t.Errorf("source still exists")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("content drift: %q", got)
	}
}

// seedAgents opens a fresh DB at path + registers the given agents in
// paste-and-enter mode (the default). Closes the store before returning.
func seedAgents(t *testing.T, path string, names []string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, n := range names {
		if err := s.UpsertAgent(ctx, n, "%99"); err != nil {
			t.Fatalf("seed upsert %s: %v", n, err)
		}
	}
}

// seedAgentsWithModes is the variant that pins each agent's delivery_mode.
func seedAgentsWithModes(t *testing.T, path string, modes map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	for name, mode := range modes {
		if err := s.UpsertAgent(ctx, name, "%99"); err != nil {
			t.Fatalf("seed upsert %s: %v", name, err)
		}
		if err := s.SetDeliveryMode(ctx, name, mode); err != nil {
			t.Fatalf("seed set mode %s=%s: %v", name, mode, err)
		}
	}
}

var _ = io.Discard // keep io imported if the file later needs it

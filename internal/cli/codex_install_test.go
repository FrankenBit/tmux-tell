package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// seedCodexAgent registers agentName in the store with hook-context delivery
// mode. Helper shared across codex-install tests.
func seedCodexAgent(t *testing.T, s *store.Store, agentName string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertAgent(ctx, agentName, "%9"); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
	if err := s.SetDeliveryMode(ctx, agentName, store.DeliveryModeHookContext); err != nil {
		t.Fatalf("seed delivery mode: %v", err)
	}
}

// runCodexInstall is a test helper that calls runCodexInstallCLI with
// --skip-discover and --db pointed at a temp store. Returns stdout, stderr,
// and exit code.
func runCodexInstall(t *testing.T, dbPath, configPath, agentName string, extraArgs ...string) (stdout, stderr string, code int) {
	t.Helper()
	args := []string{
		"--db", dbPath,
		"--agent", agentName,
		"--codex-config", configPath,
		"--skip-discover",
	}
	args = append(args, extraArgs...)
	var so, se bytes.Buffer
	code = runCodexInstallCLI(args, &so, &se)
	return so.String(), se.String(), code
}

// TestCodexInstall_FreshInstall verifies a fresh install (no existing config)
// writes all three blocks and reports hooksWritten + envWritten.
func TestCodexInstall_FreshInstall(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, ".codex", "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	so, se, code := runCodexInstall(t, db, cfg, "lookout", "--format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s stderr=%s", code, so, se)
	}

	var result codexInstallResult
	if err := json.Unmarshal([]byte(so), &result); err != nil {
		t.Fatalf("parse result: %v (stdout=%s)", err, so)
	}
	if !result.OK {
		t.Errorf("result.OK=false")
	}
	if !result.HooksWritten {
		t.Errorf("HooksWritten=false, want true")
	}
	if !result.EnvWritten {
		t.Errorf("EnvWritten=false, want true")
	}
	if result.AlreadyOK {
		t.Errorf("AlreadyOK=true on fresh install")
	}
	if len(result.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", result.Warnings)
	}

	// Verify the written file contains all three blocks.
	contents, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body := string(contents)
	for _, want := range []string{
		"[hooks.UserPromptSubmit]",
		"[hooks.SessionStart]",
		"command = \"tmux-tell-codex hook-context\"",
		"[mcp_servers.tmux-msg.env]",
		"TMUX_AGENT_NAME = \"lookout\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config missing %q\nfull content:\n%s", want, body)
		}
	}
}

// TestCodexInstall_Idempotent verifies re-running on an already-wired config
// is a no-op (AlreadyOK=true, no file changes).
func TestCodexInstall_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	// Pre-populate with the exact expected content.
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "lookout"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}
	modtime := func() int64 {
		fi, _ := os.Stat(cfg)
		return fi.ModTime().UnixNano()
	}
	before := modtime()

	so, se, code := runCodexInstall(t, db, cfg, "lookout", "--format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s stderr=%s", code, so, se)
	}

	var result codexInstallResult
	if err := json.Unmarshal([]byte(so), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if !result.AlreadyOK {
		t.Errorf("AlreadyOK=false on fully-wired config; HooksWritten=%v EnvWritten=%v",
			result.HooksWritten, result.EnvWritten)
	}

	// File must not have been touched.
	after := modtime()
	if before != after {
		t.Errorf("config file was modified on idempotent re-run (mtime changed)")
	}
}

// TestCodexInstall_PartialConfig verifies that when hooks are already present
// but the env block is missing, only the env block is appended.
func TestCodexInstall_PartialConfig(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	partial := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"
`
	if err := os.WriteFile(cfg, []byte(partial), 0o600); err != nil {
		t.Fatalf("write partial config: %v", err)
	}

	so, se, code := runCodexInstall(t, db, cfg, "lookout", "--format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s stderr=%s", code, so, se)
	}

	var result codexInstallResult
	if err := json.Unmarshal([]byte(so), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if result.HooksWritten {
		t.Errorf("HooksWritten=true, want false (hooks already present)")
	}
	if !result.EnvWritten {
		t.Errorf("EnvWritten=false, want true (env block was missing)")
	}

	contents, _ := os.ReadFile(cfg)
	if !strings.Contains(string(contents), "TMUX_AGENT_NAME = \"lookout\"") {
		t.Errorf("env block not written: %s", contents)
	}
}

// TestCodexInstall_WrongAgentName verifies that a pre-existing
// TMUX_AGENT_NAME with a different value produces a warning and is not
// overwritten.
func TestCodexInstall_WrongAgentName(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout-new")
	s.Close()

	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "old-lookout"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}

	so, se, code := runCodexInstall(t, db, cfg, "lookout-new", "--format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s stderr=%s", code, so, se)
	}

	var result codexInstallResult
	if err := json.Unmarshal([]byte(so), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if len(result.Warnings) == 0 {
		t.Errorf("expected warning about wrong TMUX_AGENT_NAME, got none")
	}
	foundWarn := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "old-lookout") && strings.Contains(w, "lookout-new") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("warning doesn't mention old/new names: %v", result.Warnings)
	}
	// File must still contain the OLD name (no overwrite).
	contents, _ := os.ReadFile(cfg)
	if !strings.Contains(string(contents), "old-lookout") {
		t.Errorf("existing TMUX_AGENT_NAME was modified without consent")
	}
	if strings.Contains(string(contents), "lookout-new") {
		t.Errorf("new agent name was incorrectly written over existing env block")
	}
}

// TestCodexInstall_CreatesMissingConfigDir verifies that a missing
// ~/.codex/ directory is created rather than failing.
func TestCodexInstall_CreatesMissingConfigDir(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	// Point to a nested dir that doesn't exist yet.
	cfg := filepath.Join(tmp, "nested", "deep", "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	so, se, code := runCodexInstall(t, db, cfg, "lookout")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s stderr=%s", code, so, se)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("config not created: %v", err)
	}
}

// TestCodexInstall_DryRun verifies --dry-run reports what would be written
// without modifying the filesystem.
func TestCodexInstall_DryRun(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	so, _, code := runCodexInstall(t, db, cfg, "lookout", "--dry-run", "--format", "json")
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s", code, so)
	}

	// File must NOT exist (dry-run skips all writes).
	if _, err := os.Stat(cfg); err == nil {
		t.Errorf("config file created during --dry-run; should not exist")
	}

	// Result should still report what would be written.
	var result codexInstallResult
	if err := json.Unmarshal([]byte(so), &result); err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if !result.HooksWritten {
		t.Errorf("HooksWritten=false in dry-run; want true (would have written)")
	}
	if !result.EnvWritten {
		t.Errorf("EnvWritten=false in dry-run; want true (would have written)")
	}
}

// TestCodexInstall_PostInstallMessageInTextOutput verifies the text output
// includes the hook-trust prompt instruction (AC 5).
func TestCodexInstall_PostInstallMessageInTextOutput(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	s.Close()

	so, _, code := runCodexInstall(t, db, cfg, "lookout") // text format (default)
	if code != exitOK {
		t.Fatalf("exit %d: stdout=%s", code, so)
	}
	for _, want := range []string{
		"hook approval",
		"UserPromptSubmit",
		"SessionStart",
		codexHookCommand,
	} {
		if !strings.Contains(so, want) {
			t.Errorf("post-install message missing %q\nfull stdout:\n%s", want, so)
		}
	}
}

// TestCodexInstall_AgentNotFound verifies a clear error when the agent
// isn't in the DB after discover (skip-discover + no pre-seeded agent).
func TestCodexInstall_AgentNotFound(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	// Open store but do NOT seed an agent.
	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	s.Close()

	var so, se bytes.Buffer
	code := runCodexInstallCLI([]string{
		"--db", db,
		"--agent", "ghost",
		"--codex-config", cfg,
		"--skip-discover",
	}, &so, &se)

	if code == exitOK {
		t.Errorf("expected non-zero exit for unregistered agent, got exitOK")
	}
	if !strings.Contains(so.String(), "ghost") || !strings.Contains(so.String(), "not found") {
		t.Errorf("error output missing agent name or 'not found': stdout=%s", so.String())
	}
}

// TestMergeCodexConfig_ExistingContentPreserved verifies that appending new
// blocks preserves pre-existing config content (e.g., other mcp_servers).
func TestMergeCodexConfig_ExistingContentPreserved(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.toml")

	existing := `[mcp_servers.other-tool]
command = "other-tool mcp"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	_, _, _, warnings, err := mergeCodexConfig(cfg, "lookout", false)
	if err != nil {
		t.Fatalf("mergeCodexConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}

	contents, _ := os.ReadFile(cfg)
	body := string(contents)
	// Original entry preserved.
	if !strings.Contains(body, "[mcp_servers.other-tool]") {
		t.Errorf("existing [mcp_servers.other-tool] lost after merge:\n%s", body)
	}
	// New entries present.
	if !strings.Contains(body, "[hooks.UserPromptSubmit]") {
		t.Errorf("hooks.UserPromptSubmit not written:\n%s", body)
	}
	if !strings.Contains(body, "TMUX_AGENT_NAME = \"lookout\"") {
		t.Errorf("TMUX_AGENT_NAME not written:\n%s", body)
	}
}

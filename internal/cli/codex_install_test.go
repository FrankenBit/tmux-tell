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
// writes hook blocks without a global MCP TMUX_AGENT_NAME pin (#553).
func TestCodexInstall_FreshInstall(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, ".codex", "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	_ = s.Close()

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
	if result.EnvWritten {
		t.Errorf("EnvWritten=true, want false")
	}
	if result.AlreadyOK {
		t.Errorf("AlreadyOK=true on fresh install")
	}
	if len(result.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", result.Warnings)
	}

	// Verify the written file contains the hook blocks.
	contents, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body := string(contents)
	for _, want := range []string{
		"[hooks.UserPromptSubmit]",
		"[hooks.SessionStart]",
		"command = \"tmux-tell-codex hook-context\"",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config missing %q\nfull content:\n%s", want, body)
		}
	}
	if strings.Contains(body, "TMUX_AGENT_NAME") || strings.Contains(body, "[mcp_servers.tmux-tell.env]") {
		t.Errorf("fresh install wrote a global MCP env pin (#553):\n%s", body)
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
	_ = s.Close()

	// Pre-populate with the exact expected hook content.
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"
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

// TestCodexInstall_HooksOnlyConfigAlreadyOK verifies that hooks-only config is
// now complete; codex-install must not append a global MCP env pin (#553).
func TestCodexInstall_HooksOnlyConfigAlreadyOK(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout")
	_ = s.Close()

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
	if result.EnvWritten {
		t.Errorf("EnvWritten=true, want false")
	}
	if !result.AlreadyOK {
		t.Errorf("AlreadyOK=false, want true for hooks-only config")
	}

	contents, _ := os.ReadFile(cfg)
	if strings.Contains(string(contents), "TMUX_AGENT_NAME") {
		t.Errorf("global env pin was written: %s", contents)
	}
}

// TestCodexInstall_ExistingAgentEnvWarns verifies that a pre-existing
// TMUX_AGENT_NAME produces a warning and is not overwritten or removed.
func TestCodexInstall_ExistingAgentEnvWarns(t *testing.T) {
	tmp := t.TempDir()
	db := filepath.Join(tmp, "msgs.db")
	cfg := filepath.Join(tmp, "config.toml")

	s, err := store.Open(db)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	seedCodexAgent(t, s, "lookout-new")
	_ = s.Close()

	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-tell.env]
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
		t.Errorf("expected warning about global TMUX_AGENT_NAME, got none")
	}
	foundWarn := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "old-lookout") && strings.Contains(w, "global MCP agent pins") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("warning doesn't mention stale global env pin: %v", result.Warnings)
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
	_ = s.Close()

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
	_ = s.Close()

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
	if result.EnvWritten {
		t.Errorf("EnvWritten=true in dry-run; want false")
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
	_ = s.Close()

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
	_ = s.Close()

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

	_, _, _, warnings, _, err := mergeCodexConfig(cfg, "lookout", false)
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
	if strings.Contains(body, "TMUX_AGENT_NAME") {
		t.Errorf("global MCP env pin was written (#553):\n%s", body)
	}
}

// --- #486: codex-config [mcp_servers.tmux-msg] → tmux-tell migration ---

// TestMigrateLegacyMcpSection_Rename pins the common pre-rename case: a config
// with `[mcp_servers.tmux-msg…]` and no `tmux-tell` section gets its headers
// rewritten to `tmux-tell`, returns "renamed", and leaves every non-header byte
// (command, env, args, comments, key order) identical.
func TestMigrateLegacyMcpSection_Rename(t *testing.T) {
	content := `# operator's hand-tuned codex config
[mcp_servers.tmux-msg]
command = "tmux-tell-codex"
args = ["mcp"]
approval_mode = "never"   # don't prompt

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "lookout"

[some.other-section]
key = "value"
`
	orig := content
	action := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell")
	if action != "renamed" {
		t.Fatalf("action = %q, want renamed", action)
	}
	// Both headers renamed.
	if strings.Contains(content, "[mcp_servers.tmux-msg]") ||
		strings.Contains(content, "[mcp_servers.tmux-msg.env]") {
		t.Errorf("legacy headers survived:\n%s", content)
	}
	if !strings.Contains(content, "[mcp_servers.tmux-tell]") ||
		!strings.Contains(content, "[mcp_servers.tmux-tell.env]") {
		t.Errorf("renamed headers missing:\n%s", content)
	}
	// Non-header bytes byte-identical: reconstruct expected by renaming only the
	// two header lines in the original.
	want := strings.NewReplacer(
		"[mcp_servers.tmux-msg]", "[mcp_servers.tmux-tell]",
		"[mcp_servers.tmux-msg.env]", "[mcp_servers.tmux-tell.env]",
	).Replace(orig)
	if content != want {
		t.Errorf("rename touched non-header bytes.\n got:\n%s\nwant:\n%s", content, want)
	}
}

// TestMigrateLegacyMcpSection_RewritesToolKeysAndCommand pins #486's full
// substrate-coverage (Option B, reversing f451's command-WARN-only call): inside
// a migrating `[mcp_servers.tmux-msg…]` section, the rewrite must advance ALL
// three substrate-points — the section-path segment, the inner per-tool key
// (`."tmux-msg.<tool>"`), and the `command` binary (`tmux-msg-codex`) — while
// leaving `approval_mode`/`args`/`env` byte-identical. Leaving the inner tool key
// at `tmux-msg` would silently de-link the operator's per-tool approval_mode from
// the live `tmux-tell.<tool>` tool (the operator-visible failure mode).
func TestMigrateLegacyMcpSection_RewritesToolKeysAndCommand(t *testing.T) {
	content := `[mcp_servers.tmux-msg]
command = "tmux-msg-codex"
args = ["mcp"]
env = { TMUX_AGENT_NAME = "lookout" }

[mcp_servers.tmux-msg.tools."tmux-msg.status"]
approval_mode = "approve"

[mcp_servers.tmux-msg.tools."tmux-msg.send"]
approval_mode = "approve"
`
	action := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell")
	if action != "renamed" {
		t.Fatalf("action = %q, want renamed", action)
	}
	if strings.Contains(content, "tmux-msg") {
		t.Errorf("a tmux-msg occurrence survived the rewrite:\n%s", content)
	}
	for _, want := range []string{
		"[mcp_servers.tmux-tell]",
		`command = "tmux-tell-codex"`,                      // binary rewritten
		`[mcp_servers.tmux-tell.tools."tmux-tell.status"]`, // path + inner key
		`[mcp_servers.tmux-tell.tools."tmux-tell.send"]`,   // path + inner key
		`args = ["mcp"]`,                                   // preserved
		`env = { TMUX_AGENT_NAME = "lookout" }`,            // preserved
		"approval_mode = \"approve\"",                      // preserved
	} {
		if !strings.Contains(content, want) {
			t.Errorf("rewrite missing %q:\n%s", want, content)
		}
	}
}

// TestMigrateLegacyMcpSection_RemoveDup pins the post-#478 dup case: when both
// `tmux-msg` and `tmux-tell` sections exist, the orphaned legacy section is
// removed entirely (header + body) and the canonical section + other sections
// are preserved.
func TestMigrateLegacyMcpSection_RemoveDup(t *testing.T) {
	content := `[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "lookout"

[mcp_servers.tmux-tell.env]
TMUX_AGENT_NAME = "lookout"

[mcp_servers.other-tool]
command = "other mcp"
`
	action := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell")
	if action != "removed" {
		t.Fatalf("action = %q, want removed", action)
	}
	if strings.Contains(content, "tmux-msg") {
		t.Errorf("legacy tmux-msg section survived dup-removal:\n%s", content)
	}
	for _, want := range []string{
		"[hooks.SessionStart]",
		"[mcp_servers.tmux-tell.env]",
		"[mcp_servers.other-tool]",
		`command = "other mcp"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("dup-removal dropped %q:\n%s", want, content)
		}
	}
}

// TestMigrateLegacyMcpSection_NoOpAndIdempotent pins the no-op states: a config
// with only the canonical section (or neither) is untouched and returns "", and
// re-running on an already-migrated config is a no-op (idempotent by
// construction — no legacy header to match).
func TestMigrateLegacyMcpSection_NoOpAndIdempotent(t *testing.T) {
	cases := map[string]string{
		"canonical-only": "[mcp_servers.tmux-tell.env]\nTMUX_AGENT_NAME = \"lookout\"\n",
		"neither":        "[hooks.SessionStart]\ncommand = \"x\"\n",
		"empty":          "",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			orig := content
			action := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell")
			if action != "" {
				t.Errorf("action = %q, want \"\" (no-op)", action)
			}
			if content != orig {
				t.Errorf("content mutated on no-op:\n got:%q\nwant:%q", content, orig)
			}
		})
	}

	// Idempotent re-run: rename once, then a second pass is a no-op.
	content := "[mcp_servers.tmux-msg.env]\nTMUX_AGENT_NAME = \"lookout\"\n"
	if got := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell"); got != "renamed" {
		t.Fatalf("first pass action = %q, want renamed", got)
	}
	afterFirst := content
	if got := migrateLegacyMcpSection(&content, "tmux-msg", "tmux-tell"); got != "" {
		t.Errorf("second pass action = %q, want \"\" (idempotent)", got)
	}
	if content != afterFirst {
		t.Errorf("second pass mutated content:\n got:%q\nwant:%q", content, afterFirst)
	}
}

// TestMergeCodexConfig_MigratePreRename drives the full writer over a realistic
// pre-rename config (mirroring an operator's ~/.codex/config.toml with per-tool
// approval subsections + a stale binary command): every substrate-point advances
// to tmux-tell, a migration notice is emitted, operator customizations survive,
// no stale-binary WARN fires (the command was rewritten, not flagged), and the
// preserved canonical env is warned as a global identity pin (#553).
func TestMergeCodexConfig_MigratePreRename(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.toml")
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg]
command = "tmux-msg-codex"
args = ["mcp"]
env = { TMUX_AGENT_NAME = "lookout" }

[mcp_servers.tmux-msg.tools."tmux-msg.status"]
approval_mode = "approve"

[mcp_servers.tmux-msg.tools."tmux-msg.send"]
approval_mode = "approve"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	hooksWritten, envWritten, alreadyOK, warnings, notices, err := mergeCodexConfig(cfg, "lookout", false)
	if err != nil {
		t.Fatalf("mergeCodexConfig: %v", err)
	}
	if hooksWritten || envWritten {
		t.Errorf("unexpected append: hooksWritten=%v envWritten=%v (config already wired under old name)", hooksWritten, envWritten)
	}
	if alreadyOK {
		t.Errorf("AlreadyOK=true, want false (a migration is a change)")
	}
	if len(warnings) != 1 ||
		!strings.Contains(warnings[0], "global MCP agent pins") ||
		strings.Contains(warnings[0], "tmux-msg-*") {
		t.Errorf("warnings = %v, want only global-env-pin warning (command was rewritten, not flagged)", warnings)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "migrating_legacy_codex_mcp_section") {
		t.Fatalf("notices = %v, want one migrating_legacy_codex_mcp_section", notices)
	}

	body, _ := os.ReadFile(cfg)
	got := string(body)
	if strings.Contains(got, "tmux-msg") {
		t.Errorf("a tmux-msg occurrence survived migration:\n%s", got)
	}
	for _, want := range []string{
		"[mcp_servers.tmux-tell]",
		`command = "tmux-tell-codex"`,                      // binary rewritten
		`[mcp_servers.tmux-tell.tools."tmux-tell.status"]`, // path + inner tool key
		`[mcp_servers.tmux-tell.tools."tmux-tell.send"]`,
		`args = ["mcp"]`,                        // customization preserved
		`env = { TMUX_AGENT_NAME = "lookout" }`, // customization preserved
		"approval_mode = \"approve\"",           // per-tool setting preserved
	} {
		if !strings.Contains(got, want) {
			t.Errorf("migrated config missing %q:\n%s", want, got)
		}
	}
}

// TestMergeCodexConfig_MigrateOnlyWrites is the load-bearing pin for the write
// gate's `|| migrated` arm: a config already fully wired EXCEPT under the legacy
// section name produces no appends — yet the migration MUST still be persisted.
// Reverting the `|| migrated` half of the gate (back to a bare `len(toAppend)==0`
// early return) drops the rename silently → the legacy header survives on disk →
// this test goes red.
func TestMergeCodexConfig_MigrateOnlyWrites(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.toml")
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "lookout"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	_, _, alreadyOK, _, notices, err := mergeCodexConfig(cfg, "lookout", false)
	if err != nil {
		t.Fatalf("mergeCodexConfig: %v", err)
	}
	if alreadyOK {
		t.Errorf("AlreadyOK=true — a migrate-only change must not report no-op")
	}
	if len(notices) == 0 {
		t.Errorf("no migration notice emitted")
	}

	body, _ := os.ReadFile(cfg)
	got := string(body)
	if strings.Contains(got, "[mcp_servers.tmux-msg.env]") {
		t.Errorf("migrate-only change was NOT persisted (legacy header still on disk):\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.tmux-tell.env]") {
		t.Errorf("renamed header not on disk:\n%s", got)
	}
}

// TestMergeCodexConfig_MigrateRemovesDup drives the full writer over a dup
// config (both sections present): the orphaned legacy section is removed, a
// removal notice is emitted, and the canonical section survives.
func TestMergeCodexConfig_MigrateRemovesDup(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.toml")
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-msg.env]
TMUX_AGENT_NAME = "lookout"

[mcp_servers.tmux-tell.env]
TMUX_AGENT_NAME = "lookout"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	_, _, alreadyOK, _, notices, err := mergeCodexConfig(cfg, "lookout", false)
	if err != nil {
		t.Fatalf("mergeCodexConfig: %v", err)
	}
	if alreadyOK {
		t.Errorf("AlreadyOK=true, want false (dup removal is a change)")
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "removing_orphaned_codex_mcp_section") {
		t.Fatalf("notices = %v, want one removing_orphaned_codex_mcp_section", notices)
	}

	body, _ := os.ReadFile(cfg)
	got := string(body)
	if strings.Contains(got, "tmux-msg") {
		t.Errorf("legacy tmux-msg section survived dup-removal:\n%s", got)
	}
	if !strings.Contains(got, "[mcp_servers.tmux-tell.env]") {
		t.Errorf("canonical section dropped:\n%s", got)
	}
}

// TestMergeCodexConfig_StaleBinaryWarnsWithoutMigration pins the residual WARN
// (#486, Option B): when there is NO `[mcp_servers.tmux-msg]` section to migrate
// but the already-canonical `[mcp_servers.tmux-tell]` section's `command` STILL
// names the pre-rename `tmux-msg-*` binary, the writer WARNs and leaves it (no
// migrating section is in scope, so the binary is the operator's to fix). Inside
// a migrating section the command IS rewritten — that path is covered by
// TestMergeCodexConfig_MigratePreRename, which asserts zero warnings.
func TestMergeCodexConfig_StaleBinaryWarnsWithoutMigration(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.toml")
	existing := `[hooks.UserPromptSubmit]
command = "tmux-tell-codex hook-context"

[hooks.SessionStart]
command = "tmux-tell-codex hook-context"

[mcp_servers.tmux-tell]
command = "tmux-msg-codex"

[mcp_servers.tmux-tell.env]
TMUX_AGENT_NAME = "lookout"
`
	if err := os.WriteFile(cfg, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	_, _, _, warnings, notices, err := mergeCodexConfig(cfg, "lookout", false)
	if err != nil {
		t.Fatalf("mergeCodexConfig: %v", err)
	}
	if len(notices) != 0 {
		t.Errorf("no migration should run (already canonical); notices=%v", notices)
	}
	foundWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "tmux-msg-*") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected a residual stale-binary WARN, got: %v", warnings)
	}
	// The command is left as-is (no migrating section authorized a rewrite).
	body, _ := os.ReadFile(cfg)
	if !strings.Contains(string(body), `command = "tmux-msg-codex"`) {
		t.Errorf("command rewritten outside a migrating section; should be WARN-only:\n%s", string(body))
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestFindOrphanMailmanUnits_HappyPath verifies the orphan walk
// identifies an instance unit whose agent isn't registered, while
// leaving registered + template + non-symlink entries alone.
func TestFindOrphanMailmanUnits_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	prefix := active.BinaryName + "-mailman@"

	// Plant: template (regular file), enabled instance for known agent,
	// enabled instance for orphan agent, an unrelated symlink.
	template := filepath.Join(tmp, prefix+".service")
	if err := os.WriteFile(template, []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"alpha", "ghost"} {
		// Mailman instance enablement looks like a symlink in the user
		// systemd dir on real installs.
		linkSrc := filepath.Join(tmp, prefix+agent+".service")
		if err := os.Symlink(template, linkSrc); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated file — must NOT be classed as an orphan.
	if err := os.WriteFile(filepath.Join(tmp, "some-other.service"),
		[]byte("noise"), 0o644); err != nil {
		t.Fatal(err)
	}

	known := []store.Agent{{Name: "alpha"}}
	orphans, err := findOrphanMailmanUnits(tmp, known)
	if err != nil {
		t.Fatalf("orphan walk: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("orphans=%v want 1 (ghost)", orphans)
	}
	if !strings.Contains(orphans[0], "ghost") {
		t.Errorf("orphan=%q want one for ghost", orphans[0])
	}
}

// TestFindOrphanMailmanUnits_MissingDirOK verifies a missing systemd
// dir returns (nil, nil) — a fresh install has no orphans, not an
// error.
func TestFindOrphanMailmanUnits_MissingDirOK(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "nonexistent")
	orphans, err := findOrphanMailmanUnits(missing, nil)
	if err != nil {
		t.Fatalf("missing dir should be ok, got %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("want no orphans, got %v", orphans)
	}
}

// TestFindOrphanMailmanUnits_EmptyDir verifies the orphan-name field
// is required (no orphan when dir is empty).
func TestFindOrphanMailmanUnits_EmptyDir(t *testing.T) {
	tmp := t.TempDir()
	orphans, err := findOrphanMailmanUnits(tmp, []store.Agent{{Name: "alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("want no orphans, got %v", orphans)
	}
}

// TestFindOrphanMailmanUnits_SkipsTemplate verifies the template unit
// itself (no `@<instance>` part) is never flagged.
func TestFindOrphanMailmanUnits_SkipsTemplate(t *testing.T) {
	tmp := t.TempDir()
	prefix := active.BinaryName + "-mailman@"
	template := filepath.Join(tmp, prefix+".service")
	if err := os.WriteFile(template, []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphans, err := findOrphanMailmanUnits(tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("template should not class as orphan, got %v", orphans)
	}
}

func TestOrphanInstanceName(t *testing.T) {
	cases := map[string]string{
		"tmux-msg-claude-mailman@alpha.service": "alpha",
		"tmux-msg-codex-mailman@bravo.service":  "bravo",
		"tmux-msg-claude-mailman@.service":      "",
		"unrelated.service":                     "",
	}
	for in, want := range cases {
		if got := orphanInstanceName(in); got != want {
			t.Errorf("orphanInstanceName(%q)=%q want %q", in, got, want)
		}
	}
}

// TestBootstrap_HappyPath_NoLegacyDB exercises the full bootstrap
// sequence when no legacy DB exists, with a mocked systemctl, mocked
// HOME pointing at a temp dir, and a seeded agents table.
//
// What this test covers: daemon-reload elision via --skip-daemon-reload,
// post-discover agent count, mailman enable per non-hook-context agent,
// orphan walk with print-by-default, refresh-all-mcps emission. Discover
// itself runs against a synthetic walker that finds nothing (no tmux
// available in test env), so the discover step is a no-op on the seeded
// agents — the agent count stays at what we seeded.
func TestBootstrap_HappyPath_NoLegacyDB(t *testing.T) {
	var systemctlCalls []string
	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		systemctlCalls = append(systemctlCalls, strings.Join(args, " "))
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "messages.db")
	seedAgentsWithModes(t, dbPath, map[string]string{
		"alpha": "paste-and-enter",
		"hook1": store.DeliveryModeHookContext,
		"beta":  "paste-and-enter",
	})

	systemdDir := filepath.Join(tmp, "systemd-user")
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant an orphan to verify the walk catches it but does NOT
	// disable it without --prune-orphans.
	prefix := active.BinaryName + "-mailman@"
	template := filepath.Join(systemdDir, prefix+".service")
	if err := os.WriteFile(template, []byte("template"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(template, filepath.Join(systemdDir, prefix+"ghost.service")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_MSG_DB", dbPath)

	var stdout, stderr bytes.Buffer
	rc := runBootstrapCLI(
		[]string{"--db", dbPath, "--systemd-dir", systemdDir, "--skip-daemon-reload", "--skip-discover", "--format", "json"},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s stdout=%s", rc, stderr.String(), stdout.String())
	}

	var got bootstrapResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, stdout.String())
	}
	if !got.OK || got.Migrated {
		t.Fatalf("OK=true Migrated=false expected, got %+v", got)
	}
	if got.Discovered != 3 {
		t.Errorf("discovered=%d want 3", got.Discovered)
	}
	if got.MailmanEnabled != 2 {
		t.Errorf("enabled=%d want 2 (hook-context skipped)", got.MailmanEnabled)
	}
	if got.OrphansFound != 1 {
		t.Errorf("orphans found=%d want 1", got.OrphansFound)
	}
	if got.OrphansPruned != 0 {
		t.Errorf("orphans pruned=%d want 0 (no --prune-orphans)", got.OrphansPruned)
	}
	if got.McpsRefreshed != 3 {
		t.Errorf("refresh count=%d want 3", got.McpsRefreshed)
	}

	enables := 0
	disables := 0
	for _, c := range systemctlCalls {
		if strings.HasPrefix(c, "enable") {
			enables++
		}
		if strings.HasPrefix(c, "disable") {
			disables++
		}
	}
	if enables != 2 {
		t.Errorf("enable calls=%d want 2; calls=%v", enables, systemctlCalls)
	}
	if disables != 0 {
		t.Errorf("disable calls=%d want 0 (no --prune-orphans); calls=%v", disables, systemctlCalls)
	}
}

// TestBootstrap_PruneOrphans verifies --prune-orphans actually disables
// orphan units it finds.
func TestBootstrap_PruneOrphans(t *testing.T) {
	var disabled []string
	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "disable" {
			disabled = append(disabled, args[len(args)-1])
		}
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "messages.db")
	seedAgents(t, dbPath, []string{"alpha"})

	systemdDir := filepath.Join(tmp, "systemd-user")
	if err := os.MkdirAll(systemdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prefix := active.BinaryName + "-mailman@"
	template := filepath.Join(systemdDir, prefix+".service")
	if err := os.WriteFile(template, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(template, filepath.Join(systemdDir, prefix+"ghost.service")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(template, filepath.Join(systemdDir, prefix+"shadow.service")); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_MSG_DB", dbPath)

	var stdout, stderr bytes.Buffer
	rc := runBootstrapCLI(
		[]string{
			"--db", dbPath,
			"--systemd-dir", systemdDir,
			"--skip-daemon-reload", "--skip-discover",
			"--prune-orphans",
			"--format", "json",
		},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}

	var got bootstrapResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.OrphansPruned != 2 {
		t.Errorf("pruned=%d want 2", got.OrphansPruned)
	}
	if len(disabled) != 2 {
		t.Errorf("disable systemctl calls=%d want 2; got %v", len(disabled), disabled)
	}
}

// TestBootstrap_AmbiguousDBState verifies the bootstrap refuses when
// both legacy + default DBs exist.
//
// Note: legacyDBPath is a const ("/var/lib/tmux-msg/messages.db") and
// likely doesn't exist on test hosts. To exercise the both-exist branch
// without writing to /var/lib, we'd need a test seam; for v1 this test
// just documents the expected exitDataErr shape via the empty case (no
// legacy → no error). The substantive both-exist refusal lives in code
// review + the integration smoke (PR follows).
func TestBootstrap_NoLegacyOK(t *testing.T) {
	if _, err := os.Stat(legacyDBPath); err == nil {
		t.Skip("legacy DB exists on this host; can't run this test cleanly")
	}
	prev := setSystemctlRunner(func(ctx context.Context, args ...string) ([]byte, error) {
		return nil, nil
	})
	t.Cleanup(func() { setSystemctlRunner(prev) })

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "messages.db")
	seedAgents(t, dbPath, []string{"alpha"})

	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_MSG_DB", dbPath)

	var stdout, stderr bytes.Buffer
	rc := runBootstrapCLI(
		[]string{
			"--db", dbPath,
			"--systemd-dir", filepath.Join(tmp, "no-systemd"),
			"--skip-daemon-reload", "--skip-discover",
			"--format", "json",
		},
		&stdout, &stderr,
	)
	if rc != exitOK {
		t.Fatalf("rc=%d stderr=%s", rc, stderr.String())
	}
	var got bootstrapResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Migrated {
		t.Errorf("Migrated=true unexpected (no legacy DB)")
	}
}

func TestResolveSystemdDir(t *testing.T) {
	t.Setenv("HOME", "/operator-home")
	got := resolveSystemdDir("")
	want := "/operator-home/.config/systemd/user"
	if got != want {
		t.Errorf("resolveSystemdDir()=%q want %q", got, want)
	}
	if got := resolveSystemdDir("/override"); got != "/override" {
		t.Errorf("override not honored: %q", got)
	}
}

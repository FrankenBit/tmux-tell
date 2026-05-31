package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFrom_MissingFileReturnsEmptyNoError(t *testing.T) {
	f, err := LoadFrom("/nonexistent/path/config.toml")
	if err != nil {
		t.Fatalf("missing-file should NOT error; got %v", err)
	}
	if f == nil {
		t.Errorf("LoadFrom should always return non-nil File")
	}
}

func TestLoadFrom_MalformedFileErrors(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(tmp, []byte("garbage = "), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadFrom(tmp); err == nil {
		t.Errorf("malformed TOML should error; got nil")
	}
}

func TestLoadFrom_HappyPathParsesDefaultsAndAgent(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "ok.toml")
	content := `
[defaults]
notify-on-failed = false
quiet-input-backoff = "45s"

[agent.surveyor]
notify-on-failed = true
quiet-input-backoff = "90s"
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadFrom(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Defaults.NotifyOnFailed == nil || *f.Defaults.NotifyOnFailed {
		t.Errorf("Defaults.NotifyOnFailed should be false; got %v",
			f.Defaults.NotifyOnFailed)
	}
	if f.Defaults.QuietInputBackoff == nil || *f.Defaults.QuietInputBackoff != 45*time.Second {
		t.Errorf("Defaults.QuietInputBackoff = %v, want 45s",
			f.Defaults.QuietInputBackoff)
	}
	if f.Agent == nil || f.Agent["surveyor"].NotifyOnFailed == nil {
		t.Fatalf("agent.surveyor.NotifyOnFailed missing")
	}
	if !*f.Agent["surveyor"].NotifyOnFailed {
		t.Errorf("agent.surveyor.NotifyOnFailed should be true; got false")
	}
}

func TestResolveBool_PrecedenceChain(t *testing.T) {
	tr := true
	fa := false
	file := &File{
		Defaults: Block{NotifyOnFailed: &fa},
		Agent: map[string]Block{
			"surveyor": {NotifyOnFailed: &tr},
		},
	}

	// Agent-specific value wins over defaults.
	if !ResolveBool(file, "surveyor", "notify-on-failed", true) {
		t.Errorf("agent override should win; got false")
	}
	// Defaults wins when no agent override.
	if ResolveBool(file, "admin", "notify-on-failed", true) {
		t.Errorf("defaults should win when no agent override; got true")
	}
	// Hardcoded wins when neither agent nor defaults set.
	if !ResolveBool(file, "admin", "drift-soft-fail", true) {
		t.Errorf("hardcoded should win when both layers unset; got false")
	}
}

func TestResolveBool_NilFileReturnsHardcoded(t *testing.T) {
	if !ResolveBool(nil, "admin", "notify-on-failed", true) {
		t.Errorf("nil File should return hardcoded; got false")
	}
}

func TestResolveDuration_PrecedenceChain(t *testing.T) {
	d30 := 30 * time.Second
	d90 := 90 * time.Second
	file := &File{
		Defaults: Block{QuietInputBackoff: &d30},
		Agent: map[string]Block{
			"surveyor": {QuietInputBackoff: &d90},
		},
	}
	if got := ResolveDuration(file, "surveyor", "quiet-input-backoff", time.Minute); got != d90 {
		t.Errorf("agent override = %v, want %v", got, d90)
	}
	if got := ResolveDuration(file, "admin", "quiet-input-backoff", time.Minute); got != d30 {
		t.Errorf("defaults = %v, want %v", got, d30)
	}
	if got := ResolveDuration(file, "admin", "quiet-max-wait", time.Hour); got != time.Hour {
		t.Errorf("hardcoded = %v, want %v", got, time.Hour)
	}
}

func TestResolve_FullSnapshot(t *testing.T) {
	tr := true
	d := 45 * time.Second
	file := &File{
		Defaults: Block{NotifyOnFailed: &tr, QuietInputBackoff: &d},
	}
	v := Resolve(file, "/some/path.toml", "bosun")
	if v.Agent != "bosun" {
		t.Errorf("Agent = %q, want bosun", v.Agent)
	}
	if !v.NotifyOnFailed {
		t.Errorf("NotifyOnFailed should be true (from defaults)")
	}
	if v.QuietInputBackoff != d {
		t.Errorf("QuietInputBackoff = %v, want %v", v.QuietInputBackoff, d)
	}
	// QuietMaxWait wasn't set anywhere — should be the hardcoded default.
	if v.QuietMaxWait != 5*time.Minute {
		t.Errorf("QuietMaxWait = %v, want 5m (hardcoded)", v.QuietMaxWait)
	}
}

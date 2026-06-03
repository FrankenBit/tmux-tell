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

// TestLoadFrom_ParsesQuickPresenceProbeAndPromptSentinelGate pins the
// gap-fix for the #63 Part 1 + Part 2 config knobs. Before this fix,
// the Block struct didn't include QuickPresenceProbe or
// PromptSentinelGate fields, so the TOML decoder silently dropped
// those keys — operators setting them in /etc/cli-semaphore/config.toml
// got no behavior change. The CLI flag was the only working path.
//
// This test pins both per-agent + defaults parsing for both fields so
// the gap-fix can't silently regress.
func TestLoadFrom_ParsesQuickPresenceProbeAndPromptSentinelGate(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "gate.toml")
	content := `
[defaults]
quick-presence-probe = true

[agent.quartermaster]
prompt-sentinel-gate = true
quick-presence-probe = false
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadFrom(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Defaults.QuickPresenceProbe == nil || !*f.Defaults.QuickPresenceProbe {
		t.Errorf("Defaults.QuickPresenceProbe should be true; got %v",
			f.Defaults.QuickPresenceProbe)
	}
	if f.Agent == nil || f.Agent["quartermaster"].PromptSentinelGate == nil {
		t.Fatalf("agent.quartermaster.PromptSentinelGate missing — TOML decoder dropped the key (Block struct lacks the field)")
	}
	if !*f.Agent["quartermaster"].PromptSentinelGate {
		t.Errorf("agent.quartermaster.PromptSentinelGate should be true; got false")
	}
	if f.Agent["quartermaster"].QuickPresenceProbe == nil || *f.Agent["quartermaster"].QuickPresenceProbe {
		t.Errorf("agent.quartermaster.QuickPresenceProbe should be false (per-agent override of defaults); got %v",
			f.Agent["quartermaster"].QuickPresenceProbe)
	}
}

// TestResolveBool_PrecedenceChain_QuickPresenceProbeAndPromptSentinelGate
// pins the precedence chain for the gap-fix fields. Before the fix,
// blockBoolField's switch didn't cover these field names, so
// ResolveBool fell through to the hardcoded default regardless of what
// the TOML config said. This test ensures the agent-override + defaults
// + hardcoded chain works for both new fields.
func TestResolveBool_PrecedenceChain_QuickPresenceProbeAndPromptSentinelGate(t *testing.T) {
	tr := true
	fa := false
	file := &File{
		Defaults: Block{QuickPresenceProbe: &tr, PromptSentinelGate: &fa},
		Agent: map[string]Block{
			"bosun":         {PromptSentinelGate: &tr},
			"quartermaster": {QuickPresenceProbe: &fa, PromptSentinelGate: &tr},
		},
	}

	// Per-agent override wins for both fields.
	if !ResolveBool(file, "bosun", "prompt-sentinel-gate", false) {
		t.Errorf("agent.bosun.prompt-sentinel-gate should be true (agent override)")
	}
	if ResolveBool(file, "quartermaster", "quick-presence-probe", true) {
		t.Errorf("agent.quartermaster.quick-presence-probe should be false (agent override of defaults true)")
	}
	if !ResolveBool(file, "quartermaster", "prompt-sentinel-gate", false) {
		t.Errorf("agent.quartermaster.prompt-sentinel-gate should be true (agent override of defaults false)")
	}

	// Defaults wins when no agent override.
	if !ResolveBool(file, "engineer", "quick-presence-probe", false) {
		t.Errorf("defaults.quick-presence-probe should win for engineer (no agent block); got false")
	}
	if ResolveBool(file, "engineer", "prompt-sentinel-gate", true) {
		t.Errorf("defaults.prompt-sentinel-gate should win for engineer (no agent block); got true")
	}

	// Hardcoded wins when neither agent nor defaults set the field. Test
	// with a fresh file that has neither.
	empty := &File{}
	if !ResolveBool(empty, "quartermaster", "quick-presence-probe", true) {
		t.Errorf("hardcoded should win for empty file; got false")
	}
	if !ResolveBool(empty, "quartermaster", "prompt-sentinel-gate", true) {
		t.Errorf("hardcoded should win for empty file; got false")
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

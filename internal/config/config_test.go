package config

import (
	"os"
	"path/filepath"
	"strings"
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
input-stale-threshold = "45s"

[agent.surveyor]
notify-on-failed = true
input-stale-threshold = "90s"
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
	if f.Defaults.InputStaleThreshold == nil || *f.Defaults.InputStaleThreshold != 45*time.Second {
		t.Errorf("Defaults.InputStaleThreshold = %v, want 45s",
			f.Defaults.InputStaleThreshold)
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

// TestRenderByteMarkerThreshold_HonorsConfig pins the #160 length-marker
// threshold through the full chain: TOML parse → precedence resolution
// (per-agent override beats fleet default beats hardcoded) → byte-size
// parse. This is the AC's "threshold honors config (fleet default +
// per-agent override)" closed loop.
func TestRenderByteMarkerThreshold_HonorsConfig(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "marker.toml")
	content := `
[defaults]
render-byte-marker-threshold = "1k"

[agent.surveyor]
render-byte-marker-threshold = "256b"
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadFrom(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Fleet default applies to an agent with no override → "1k" → 1000.
	fleet := ResolveString(f, "pilot", "render-byte-marker-threshold", "512b")
	if fleet != "1k" {
		t.Errorf("fleet default resolve = %q, want %q", fleet, "1k")
	}
	if n, perr := ParseByteSize(fleet); perr != nil || n != 1000 {
		t.Errorf("ParseByteSize(%q) = %d, %v; want 1000, nil", fleet, n, perr)
	}

	// Per-agent override beats the fleet default for surveyor → "256b" → 256.
	srv := ResolveString(f, "surveyor", "render-byte-marker-threshold", "512b")
	if srv != "256b" {
		t.Errorf("per-agent resolve = %q, want %q", srv, "256b")
	}
	if n, perr := ParseByteSize(srv); perr != nil || n != 256 {
		t.Errorf("ParseByteSize(%q) = %d, %v; want 256, nil", srv, n, perr)
	}

	// Hardcoded default applies when neither layer sets the key. A File
	// with no marker config at all falls through to the supplied default.
	bare := ResolveString(&File{}, "anyone", "render-byte-marker-threshold", "512b")
	if bare != "512b" {
		t.Errorf("hardcoded fallback = %q, want %q", bare, "512b")
	}
}

func TestParseByteSize(t *testing.T) {
	ok := map[string]int{
		"512":    512,
		"512b":   512,
		"512B":   512,
		"2k":     2000,
		"2K":     2000,
		"2.3k":   2300,
		"2kb":    2000,
		"  1k  ": 1000,
		"0":      0,
	}
	for in, want := range ok {
		got, err := ParseByteSize(in)
		if err != nil {
			t.Errorf("ParseByteSize(%q) errored: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "abc", "5zz", "-5", "-1k", "k"} {
		if _, err := ParseByteSize(bad); err == nil {
			t.Errorf("ParseByteSize(%q) should error, got nil", bad)
		}
	}
}

// TestLoadFrom_ParsesGateDisabled pins TOML parsing of the observe-
// gate's bool knob. The sibling tests for legacy probe-and-watch
// fields (QuickPresenceProbe, PromptSentinelGate) were removed in
// #94 along with the fields themselves; this test preserves the
// per-agent + defaults shape for the surviving knob.
func TestLoadFrom_ParsesGateDisabled(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "gate.toml")
	content := `
[defaults]
gate-disabled = false

[agent.quartermaster]
gate-disabled = true
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadFrom(tmp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f.Defaults.GateDisabled == nil || *f.Defaults.GateDisabled {
		t.Errorf("Defaults.GateDisabled should be false; got %v",
			f.Defaults.GateDisabled)
	}
	if f.Agent == nil || f.Agent["quartermaster"].GateDisabled == nil {
		t.Fatalf("agent.quartermaster.GateDisabled missing — TOML decoder dropped the key")
	}
	if !*f.Agent["quartermaster"].GateDisabled {
		t.Errorf("agent.quartermaster.GateDisabled should be true; got false")
	}
}

// TestResolveBool_PrecedenceChain_GateDisabled pins the precedence
// chain for the observe-gate bool knob. Sibling-shape to the legacy
// probe-and-watch precedence test (removed in #94 along with the
// fields it pinned).
func TestResolveBool_PrecedenceChain_GateDisabled(t *testing.T) {
	tr := true
	fa := false
	file := &File{
		Defaults: Block{GateDisabled: &fa},
		Agent: map[string]Block{
			"bosun":         {GateDisabled: &tr},
			"quartermaster": {GateDisabled: &tr},
		},
	}

	// Per-agent override wins.
	if !ResolveBool(file, "bosun", "gate-disabled", false) {
		t.Errorf("agent.bosun.gate-disabled should be true (agent override)")
	}
	if !ResolveBool(file, "quartermaster", "gate-disabled", false) {
		t.Errorf("agent.quartermaster.gate-disabled should be true (agent override of defaults false)")
	}

	// Defaults wins when no agent override.
	if ResolveBool(file, "engineer", "gate-disabled", true) {
		t.Errorf("defaults.gate-disabled should win for engineer (no agent block); got true")
	}

	// Hardcoded wins when neither agent nor defaults set the field.
	empty := &File{}
	if !ResolveBool(empty, "quartermaster", "gate-disabled", true) {
		t.Errorf("hardcoded should win for empty file; got false")
	}
}

// TestLoadFrom_StrictMode_UnknownKeyFails pins the strict-mode TOML
// decoding added in #94. Unknown keys (including the deprecated
// probe-and-watch knobs swept in v0.4.0) cause LoadFrom to return an
// error naming the offending key, rather than silently dropping the
// value. Catches operator typos AND configs that still mention
// retired keys after a deletion sweep.
func TestLoadFrom_StrictMode_UnknownKeyFails(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "stale.toml")
	content := `
[agent.bosun]
prompt-sentinel-gate = true
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := LoadFrom(tmp)
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
	if !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("error should mention 'unknown key'; got %v", err)
	}
	if !strings.Contains(err.Error(), "prompt-sentinel-gate") {
		t.Errorf("error should name the offending key; got %v", err)
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
		Defaults: Block{InputStaleThreshold: &d30},
		Agent: map[string]Block{
			"surveyor": {InputStaleThreshold: &d90},
		},
	}
	if got := ResolveDuration(file, "surveyor", "input-stale-threshold", time.Minute); got != d90 {
		t.Errorf("agent override = %v, want %v", got, d90)
	}
	if got := ResolveDuration(file, "admin", "input-stale-threshold", time.Minute); got != d30 {
		t.Errorf("defaults = %v, want %v", got, d30)
	}
	if got := ResolveDuration(file, "admin", "poll-interval-max", time.Hour); got != time.Hour {
		t.Errorf("hardcoded = %v, want %v", got, time.Hour)
	}
}

func TestResolve_FullSnapshot(t *testing.T) {
	tr := true
	d := 45 * time.Second
	file := &File{
		Defaults: Block{NotifyOnFailed: &tr, InputStaleThreshold: &d},
	}
	v := Resolve(file, "/some/path.toml", "bosun")
	if v.Agent != "bosun" {
		t.Errorf("Agent = %q, want bosun", v.Agent)
	}
	if !v.NotifyOnFailed {
		t.Errorf("NotifyOnFailed should be true (from defaults)")
	}
	if v.InputStaleThreshold != d {
		t.Errorf("InputStaleThreshold = %v, want %v", v.InputStaleThreshold, d)
	}
	// PollIntervalMax wasn't set anywhere — should be the hardcoded default.
	if v.PollIntervalMax != 15*time.Second {
		t.Errorf("PollIntervalMax = %v, want 15s (hardcoded)", v.PollIntervalMax)
	}
	// RenderByteMarkerThreshold wasn't set → hardcoded display default.
	if v.RenderByteMarkerThreshold != "512b" {
		t.Errorf("RenderByteMarkerThreshold = %q, want 512b (hardcoded)", v.RenderByteMarkerThreshold)
	}
}

// TestDeprecatedNotifyOnDeliveredUnverifiedTomlKey verifies the backward-compat
// alias: the old TOML key notify-on-delivered-unverified is accepted and resolves
// via the same precedence chain as the new key (#140, removal v1.0 — extended from v0.12.0 per ADR-0008 §Discretion clause).
func TestDeprecatedNotifyOnDeliveredUnverifiedTomlKey(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "compat.toml")
	content := `
[defaults]
notify-on-delivered-unverified = false
`
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := LoadFrom(tmp)
	if err != nil {
		t.Fatalf("load: %v (deprecated key must not cause parse error)", err)
	}
	// Deprecated key falls through to the resolution chain.
	if got := ResolveBool(f, "anyagent", "notify-on-delivered-in-input-box", true); got {
		t.Errorf("ResolveBool = true, want false (deprecated key should carry through)")
	}
	// HasDeprecatedNotifyOnDeliveredUnverified must detect it.
	if !HasDeprecatedNotifyOnDeliveredUnverified(f, "anyagent") {
		t.Errorf("HasDeprecatedNotifyOnDeliveredUnverified = false; want true for defaults-level key")
	}
}

// TestResolvedView_DeprecatedShadowField verifies that the JSON deprecated shadow
// field NotifyOnDeliveredUnverified mirrors NotifyOnDeliveredInInputBox (#140).
func TestResolvedView_DeprecatedShadowField(t *testing.T) {
	v := Resolve(nil, "", "anon")
	if v.NotifyOnDeliveredInInputBox != v.NotifyOnDeliveredUnverified {
		t.Errorf("shadow mismatch: InInputBox=%v UnverifiedShadow=%v; must be equal",
			v.NotifyOnDeliveredInInputBox, v.NotifyOnDeliveredUnverified)
	}
}

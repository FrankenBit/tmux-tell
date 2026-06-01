// Package config loads cli-semaphore's host-level config file (#54)
// and resolves per-call values against a precedence chain:
//
//  1. CLI flags (operator overrides for ad-hoc testing)
//  2. Per-agent block in the config file ([agent.<name>])
//  3. Defaults block in the config file ([defaults])
//  4. Hardcoded compile-time defaults (the CLI flag defaults)
//
// The resolver is generic over field types via Resolved[T], so the
// same precedence chain serves bools (notify toggles), durations (gate
// tuning), and strings (paths) without per-type plumbing.
//
// Missing-file behavior: silent fallback to hardcoded defaults. A
// fresh-from-install setup with no /etc/cli-semaphore/config.toml just
// gets the CLI-flag defaults.
//
// Malformed-file behavior: error returned to the caller, which can
// log a WARN and fall back to defaults (the mailman startup path
// does this so a bad config doesn't take the mailman down).
package config

import (
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where Load looks by default. Override via the
// CLAUDE_MSG_CONFIG env var or by passing an explicit path to
// LoadFrom.
const DefaultPath = "/etc/cli-semaphore/config.toml"

// File mirrors the TOML schema. Sections in the file map to the
// fields here; missing sections + missing keys decode as zero values.
type File struct {
	Defaults Block            `toml:"defaults"`
	Agent    map[string]Block `toml:"agent"`
}

// Block is the per-section settings struct. Both [defaults] and
// [agent.<name>] sections use this shape so the precedence resolution
// is mechanical (per-agent overrides defaults; both override the
// hardcoded compile-time default).
//
// Field-type choices intentionally mirror the existing CLI flag types
// in cmd/claude-msg/serve.go so the wiring stays one-to-one. Pointer
// fields distinguish "explicitly set" from "zero value": a TOML key
// that's absent stays nil, allowing the precedence chain to fall
// through to the next layer.
type Block struct {
	NotifyOnFailed              *bool          `toml:"notify-on-failed"`
	NotifyOnDeliveredUnverified *bool          `toml:"notify-on-delivered-unverified"`
	DriftSoftFail               *bool          `toml:"drift-soft-fail"`
	QuietDisabled               *bool          `toml:"quiet-disabled"`
	QuietObserveWindow          *time.Duration `toml:"quiet-observe-window"`
	QuietInputBackoff           *time.Duration `toml:"quiet-input-backoff"`
	QuietMaxWait                *time.Duration `toml:"quiet-max-wait"`
}

// Load reads the config from the path resolved by:
//   - the CLAUDE_MSG_CONFIG env var if set
//   - DefaultPath otherwise
//
// Missing file → empty File + nil error (operational default).
// Malformed file → empty File + the toml decode error.
func Load() (*File, error) {
	path := os.Getenv("CLAUDE_MSG_CONFIG")
	if path == "" {
		path = DefaultPath
	}
	return LoadFrom(path)
}

// LoadFrom reads from an explicit path. Same semantics as Load.
func LoadFrom(path string) (*File, error) {
	f := &File{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil // missing-file → defaults, not an error
		}
		return f, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := toml.Unmarshal(raw, f); err != nil {
		return f, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return f, nil
}

// ResolveBool walks the precedence chain for a bool field. The first
// non-nil value wins; if all layers are nil/empty, hardcoded is the
// final fallback.
func ResolveBool(file *File, agent, field string, hardcoded bool) bool {
	if file == nil {
		return hardcoded
	}
	if b := agentBoolField(file, agent, field); b != nil {
		return *b
	}
	if b := defaultBoolField(file, field); b != nil {
		return *b
	}
	return hardcoded
}

// ResolveDuration walks the precedence chain for a duration field.
func ResolveDuration(file *File, agent, field string, hardcoded time.Duration) time.Duration {
	if file == nil {
		return hardcoded
	}
	if d := agentDurField(file, agent, field); d != nil {
		return *d
	}
	if d := defaultDurField(file, field); d != nil {
		return *d
	}
	return hardcoded
}

// agentBoolField returns the agent-block's pointer for the named
// field, or nil if the agent block doesn't exist or the field wasn't
// set.
func agentBoolField(file *File, agent, field string) *bool {
	if file.Agent == nil {
		return nil
	}
	block, ok := file.Agent[agent]
	if !ok {
		return nil
	}
	return blockBoolField(&block, field)
}

func defaultBoolField(file *File, field string) *bool {
	return blockBoolField(&file.Defaults, field)
}

func blockBoolField(b *Block, field string) *bool {
	switch field {
	case "notify-on-failed":
		return b.NotifyOnFailed
	case "notify-on-delivered-unverified":
		return b.NotifyOnDeliveredUnverified
	case "drift-soft-fail":
		return b.DriftSoftFail
	case "quiet-disabled":
		return b.QuietDisabled
	}
	return nil
}

func agentDurField(file *File, agent, field string) *time.Duration {
	if file.Agent == nil {
		return nil
	}
	block, ok := file.Agent[agent]
	if !ok {
		return nil
	}
	return blockDurField(&block, field)
}

func defaultDurField(file *File, field string) *time.Duration {
	return blockDurField(&file.Defaults, field)
}

func blockDurField(b *Block, field string) *time.Duration {
	switch field {
	case "quiet-observe-window":
		return b.QuietObserveWindow
	case "quiet-input-backoff":
		return b.QuietInputBackoff
	case "quiet-max-wait":
		return b.QuietMaxWait
	}
	return nil
}

// ResolvedView is a fully-resolved per-agent snapshot. Useful for
// the `claude-msg config show` subcommand so the operator can see
// what the precedence chain decided for an agent without having to
// trace through TOML manually.
type ResolvedView struct {
	Agent                       string        `json:"agent"`
	ConfigPath                  string        `json:"config_path"`
	NotifyOnFailed              bool          `json:"notify_on_failed"`
	NotifyOnDeliveredUnverified bool          `json:"notify_on_delivered_unverified"`
	DriftSoftFail               bool          `json:"drift_soft_fail"`
	QuietDisabled               bool          `json:"quiet_disabled"`
	QuietObserveWindow          time.Duration `json:"quiet_observe_window"`
	QuietInputBackoff           time.Duration `json:"quiet_input_backoff"`
	QuietMaxWait                time.Duration `json:"quiet_max_wait"`
}

// Resolve builds the resolved snapshot. Hardcoded defaults mirror
// the CLI flag defaults in cmd/claude-msg/serve.go.
func Resolve(file *File, path, agent string) ResolvedView {
	return ResolvedView{
		Agent:                       agent,
		ConfigPath:                  path,
		NotifyOnFailed:              ResolveBool(file, agent, "notify-on-failed", true),
		NotifyOnDeliveredUnverified: ResolveBool(file, agent, "notify-on-delivered-unverified", true),
		DriftSoftFail:               ResolveBool(file, agent, "drift-soft-fail", false),
		QuietDisabled:               ResolveBool(file, agent, "quiet-disabled", true),
		QuietObserveWindow:          ResolveDuration(file, agent, "quiet-observe-window", 3*time.Second),
		QuietInputBackoff:           ResolveDuration(file, agent, "quiet-input-backoff", 60*time.Second),
		QuietMaxWait:                ResolveDuration(file, agent, "quiet-max-wait", 5*time.Minute),
	}
}

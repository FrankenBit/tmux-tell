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
	"strings"
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
	NotifyOnFailed              *bool `toml:"notify-on-failed"`
	NotifyOnDeliveredUnverified *bool `toml:"notify-on-delivered-unverified"`
	DriftSoftFail               *bool `toml:"drift-soft-fail"`
	// GateDisabled disables the read-only-observe-only gate added in
	// #92. Default false (gate on). Operators rarely need to disable;
	// useful only for chambers where collision-avoidance is unwanted
	// (e.g., a chamber that should always receive instantly).
	GateDisabled *bool `toml:"gate-disabled"`
	// PollIntervalMin / PollIntervalMax / InputStaleThreshold tune the
	// observe-gate's polling cadence + abandoned-draft detection (#92).
	PollIntervalMin     *time.Duration `toml:"poll-interval-min"`
	PollIntervalMax     *time.Duration `toml:"poll-interval-max"`
	InputStaleThreshold *time.Duration `toml:"input-stale-threshold"`
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
//
// Strict-mode decoding (#94): unknown keys in the TOML file produce a
// load error rather than getting silently dropped. BurntSushi/toml's
// Unmarshal is non-strict by default — a typo or a deprecated key
// (e.g., `quiet-disabled` from the pre-v0.3.0 probe-and-watch path)
// would land in `MetaData.Undecoded()` and the decoded File would
// silently lose the operator's intent. After v0.4.0's dead-code sweep
// the deprecated keys are gone for real, so an old config that still
// mentions them now fails the load loudly + names the offending keys.
func LoadFrom(path string) (*File, error) {
	f := &File{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, nil // missing-file → defaults, not an error
		}
		return f, fmt.Errorf("config: read %s: %w", path, err)
	}
	meta, err := toml.Decode(string(raw), f)
	if err != nil {
		return f, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return f, fmt.Errorf("config: parse %s: unknown key(s): %s",
			path, strings.Join(keys, ", "))
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
	case "gate-disabled":
		return b.GateDisabled
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
	case "poll-interval-min":
		return b.PollIntervalMin
	case "poll-interval-max":
		return b.PollIntervalMax
	case "input-stale-threshold":
		return b.InputStaleThreshold
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
	GateDisabled                bool          `json:"gate_disabled"`
	PollIntervalMin             time.Duration `json:"poll_interval_min"`
	PollIntervalMax             time.Duration `json:"poll_interval_max"`
	InputStaleThreshold         time.Duration `json:"input_stale_threshold"`
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
		GateDisabled:                ResolveBool(file, agent, "gate-disabled", false),
		PollIntervalMin:             ResolveDuration(file, agent, "poll-interval-min", 3*time.Second),
		PollIntervalMax:             ResolveDuration(file, agent, "poll-interval-max", 15*time.Second),
		InputStaleThreshold:         ResolveDuration(file, agent, "input-stale-threshold", 2*time.Minute),
	}
}

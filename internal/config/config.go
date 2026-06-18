// Package config loads tmux-msg's host-level config file (#54)
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
// fresh-from-install setup with no /etc/tmux-tell/config.toml just
// gets the CLI-flag defaults.
//
// Malformed-file behavior: error returned to the caller, which can
// log a WARN and fall back to defaults (the mailman startup path
// does this so a bad config doesn't take the mailman down).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// DefaultPath is where Load looks by default. Override via $TMUX_TELL_CONFIG
// (or the deprecated $CLAUDE_MSG_CONFIG) or by passing an explicit path to
// LoadFrom.
const DefaultPath = "/etc/tmux-tell/config.toml"

// LegacyPath is the deprecated tmux-msg config location, honored as a lazy
// fallback through v1.0 per ADR-0008 (#440 Phase 3): when DefaultPath does not
// exist but LegacyPath does, an in-place operator keeps their existing config.
const LegacyPath = "/etc/tmux-msg/config.toml"

// fileExists reports whether path names an existing filesystem entry.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ResolvePath returns the config path Load uses + whether it fell back to the
// legacy tmux-msg location. Precedence: $TMUX_TELL_CONFIG, then the deprecated
// $CLAUDE_MSG_CONFIG, then DefaultPath — falling back to LegacyPath only when
// DefaultPath is absent but the legacy file exists (#440 Phase 3 lazy migration).
// An explicit env var is the operator's chosen path, so legacy=false there;
// legacy is reported only for the default-path fallback, which is what the
// migration WARN keys on.
func ResolvePath() (path string, legacy bool) {
	return resolveConfigPath(
		os.Getenv("TMUX_TELL_CONFIG"), os.Getenv("CLAUDE_MSG_CONFIG"),
		DefaultPath, LegacyPath)
}

// resolveConfigPath is the path-injectable core of ResolvePath — the /etc
// consts are not test-overridable, so the precedence + lazy-fallback logic
// lives here where a test can pass temp paths.
func resolveConfigPath(envNew, envOld, defaultPath, legacyPath string) (path string, legacy bool) {
	if envNew != "" {
		return envNew, false
	}
	if envOld != "" {
		return envOld, false
	}
	if !fileExists(defaultPath) && fileExists(legacyPath) {
		return legacyPath, true
	}
	return defaultPath, false
}

// File mirrors the TOML schema. Sections in the file map to the
// fields here; missing sections + missing keys decode as zero values.
type File struct {
	Defaults Block            `toml:"defaults"`
	Agent    map[string]Block `toml:"agent"`
	// PrivilegedAgents is the operator-defined allowlist for the
	// get-by-id surface (#111): agents listed here can fetch any
	// message via `claude-msg get <id>` / `tmux-tell.get`, bypassing
	// the default sender-OR-recipient access rule. Empty (the default)
	// means no privileged agents — every requester is bound by the
	// default access rule. TOML:
	//
	//   privileged-agents = ["bosun", "quartermaster"]
	//
	// Why this lives at the top of File, not inside Block: the
	// allowlist is a property of the system (which agents have
	// privileged read), not of any particular agent. Per-agent
	// override would have incoherent semantics ("when agent X is the
	// requester, override which OTHER agents have privileged access").
	PrivilegedAgents []string `toml:"privileged-agents"`
}

// IsPrivileged returns true when the named agent appears in the
// allowlist. Convenience helper for the get-by-id access check; treats
// nil allowlist (the default) as "no privileged agents" — exact-match
// only, no glob expansion.
func (f *File) IsPrivileged(agent string) bool {
	if f == nil || agent == "" {
		return false
	}
	for _, name := range f.PrivilegedAgents {
		if name == agent {
			return true
		}
	}
	return false
}

// Block is the per-section settings struct. Both [defaults] and
// [agent.<name>] sections use this shape so the precedence resolution
// is mechanical (per-agent overrides defaults; both override the
// hardcoded compile-time default).
//
// Field-type choices intentionally mirror the existing CLI flag types
// in cmd/tmux-tell-claude/serve.go so the wiring stays one-to-one. Pointer
// fields distinguish "explicitly set" from "zero value": a TOML key
// that's absent stays nil, allowing the precedence chain to fall
// through to the next layer.
type Block struct {
	NotifyOnFailed              *bool `toml:"notify-on-failed"`
	NotifyOnDeliveredInInputBox *bool `toml:"notify-on-delivered-in-input-box"`
	// NotifyOnDeliveredUnverified is the deprecated alias for NotifyOnDeliveredInInputBox
	// (renamed v0.10.0 / #140). Accepted for the deprecation cycle; removal v1.0 (extended from v0.12.0 per ADR-0008 §Discretion clause).
	NotifyOnDeliveredUnverified *bool `toml:"notify-on-delivered-unverified"`
	DriftSoftFail               *bool `toml:"drift-soft-fail"`
	// GateDisabled disables the observe-only-with-one-named-visibility-
	// side-effect gate added in #92 (the 📫 typing-notification per #95
	// is the side-effect; opt-out via notify-emoji-disabled). Default
	// false (gate on). Operators rarely need to disable; useful only
	// for agents where collision-avoidance is unwanted (e.g., a agent
	// that should always receive instantly).
	GateDisabled *bool `toml:"gate-disabled"`
	// PollIntervalMin / PollIntervalMax / InputStaleThreshold tune the
	// observe-gate's polling cadence + abandoned-draft detection (#92).
	PollIntervalMin     *time.Duration `toml:"poll-interval-min"`
	PollIntervalMax     *time.Duration `toml:"poll-interval-max"`
	InputStaleThreshold *time.Duration `toml:"input-stale-threshold"`
	// NotifyEmojiDisabled disables the operator-typing 📫 visibility
	// notification (#95). Default false (notification on). Operator
	// escape hatch for the truly inject-averse.
	NotifyEmojiDisabled *bool `toml:"notify-emoji-disabled"`
	// WorkingDeliverImmediately opts the observe-gate's StateWorking
	// branch into a fast-path return (#106). Default false (defer on
	// Working — the v0.3.0-through-v0.6.0 conservative behavior).
	// When true, mid-turn deliveries land in the recipient's input
	// row while Claude is still streaming; Claude reads them as the
	// next operator turn after the current one completes. Eligibility
	// is StateWorking only; AwaitingOperator / Compaction / Unknown
	// stay hard-deferred regardless. See the field's doc-comment in
	// tmuxio/observe_gate.go for the safety-net relationship with the
	// verify-token retry.
	WorkingDeliverImmediately *bool `toml:"working-deliver-immediately"`
	// PrePasteSafetyDisabled bypasses the #105 Half 2 pre-paste safety
	// check (one final AgentState probe before each paste; aborts when
	// paste-unsafe states are observed). Default false (safety check
	// on). Production should leave on — the check is the load-bearing
	// safety net against the popup-as-Unknown failure mode (#105).
	PrePasteSafetyDisabled *bool `toml:"pre-paste-safety-disabled"`
	// DeliveryMode overrides the per-agent delivery_mode column from
	// the agents table (#132 follow-up to #116). When set in the TOML
	// config, the mailman's startup uses this value rather than the DB
	// column. Two valid values: "paste-and-enter" (default behavior;
	// mailman delivers via tmux paste + Enter) and "mailbox-only"
	// (operator-as-bus-participant mode; mailman short-circuits at
	// startup, messages stay queued for operator polling via
	// `claude-msg inbox`). The TOML knob lets operators who manage
	// state via config (rather than via register-time CLI calls)
	// declare the mode without modifying the DB.
	//
	// Precedence: per-agent block > defaults block > DB column. The
	// register-time CLI / MCP path still writes to the DB column;
	// this knob is the OVERRIDE at mailman-startup time.
	DeliveryMode *string `toml:"delivery-mode"`
	// RenderByteMarkerThreshold is the body-byte cutoff above which the
	// rendered bracket header gains a length marker (#160). Stored as a
	// human byte-size string (e.g. "512b", "2k", "2.3k"); parsed via
	// ParseByteSize at mailman startup. Fleet default + per-agent
	// override via the standard precedence chain. Resolves through
	// ResolveString, so an absent/empty key falls through to the next
	// layer and finally to render.DefaultByteMarkerThreshold.
	RenderByteMarkerThreshold *string `toml:"render-byte-marker-threshold"`
	// MaxRecipientsPerSend caps how many recipients a single `send --to a,b,c`
	// call may address (#158). Sends exceeding the cap fail-loud before any
	// row is inserted. Default 10. Per-sender or fleet override via the
	// standard precedence chain.
	MaxRecipientsPerSend *int `toml:"max-recipients-per-send"`
	// OnRegisterBacklog selects the #204 don't-flood policy applied when an
	// agent (re)registers with a queued backlog. Two values:
	//   - "announce" (default): leave the whole backlog queued and deliver a
	//     single "📬 N queued — run inbox" nudge.
	//   - "auto-deliver": paste the newest OnRegisterBacklogCap messages and
	//     announce the rest (or all of them, if the backlog fits the cap).
	// Resolves through ResolveString; an unrecognized value falls back to
	// "announce" at the register call-site (the never-floods safe default).
	OnRegisterBacklog *string `toml:"on-register-backlog"`
	// OnRegisterBacklogCap is the newest-N delivery cap for the
	// "auto-deliver" backlog policy (#204); ignored under "announce".
	// Resolves through ResolveInt (zero is meaningful — cap 0 makes
	// auto-deliver behave like announce — so absence rides the pointer, not
	// a zero sentinel). Hardcoded default 3.
	OnRegisterBacklogCap *int `toml:"on-register-backlog-cap"`
	// MetricsAddr is the bind address for the mailman's Prometheus /metrics
	// endpoint (#146), e.g. ":9099" or "127.0.0.1:9099". Empty/absent
	// disables the endpoint entirely — the no-behavior-change default for
	// deploys that don't scrape. Per-agent override is the load-bearing case:
	// each per-agent mailman is its own process, so the fleet's systemd
	// template assigns a distinct port per agent via an [agent.<name>] block.
	// Resolves through ResolveString (empty = not set → next layer).
	MetricsAddr *string `toml:"metrics-addr"`
	// VerifyRetryBudget is the total verify-token retry window for the
	// post-paste verification backoff (#153). Stored as a human duration
	// string (e.g. "5s", "10s", "15s"); parsed via time.ParseDuration at
	// mailman startup. Default "5s" preserves today's behavior. Per-agent
	// override for hubs that see large-payload verify-timeout pressure
	// in production (e.g. Bosun's heavy review hub).
	//
	// Resolved through ResolveString; the mailman scales the default
	// 100ms / 250ms / 500ms / 1s / 1.5s / 1.65s schedule proportionally
	// to the budget via tmuxio.DeriveRetrySchedule. Operators monitor
	// verify-attempt latency via #146's
	// tmux_tell_delivery_verify_attempt_seconds histogram to inform
	// per-agent tuning.
	VerifyRetryBudget *string `toml:"verify-retry-budget"`
	// RateLimitPattern is the operator-configurable regex that identifies an
	// adapter's rate-limit pane (#504). Empty parks the detector. The startup
	// path validates the regex and installs it into the active pane profile;
	// malformed patterns fail loud rather than quietly disabling the feature.
	RateLimitPattern *string `toml:"rate-limit-pattern"`
	// Retention is the per-agent message retention window (#245). The mailman
	// runs a background sweep that deletes delivered + failed rows older than
	// this window. "infinite" (the default) disables the sweep entirely —
	// zero behavior change for existing deploys. Any positive duration (e.g.
	// "30d", "7d", "24h") enables the sweep. Resolves through ResolveString;
	// an absent/empty key falls through to the next layer and finally to the
	// hardcoded default "infinite".
	Retention *string `toml:"retention"`
	// RetentionSweepInterval controls how often the background retention
	// goroutine wakes to prune old rows. Default 1h. Standard Go duration
	// syntax. Resolves through ResolveDuration.
	RetentionSweepInterval *time.Duration `toml:"retention-sweep-interval"`
	// DedupeWindow is the look-back window for the mailman's recipient-side
	// delivery dedupe (#157 PR2). When a new message arrives whose body
	// matches a prior delivered_in_input_box row from the same sender within
	// this window, the mailman re-verifies the original via scrollback: if
	// now visible it confirms the original and absorbs the duplicate; if not
	// visible it delivers the replay. Default 60s. "0s" (or 0) disables
	// the check entirely — zero behavior change for existing deploys. Resolves
	// through ResolveDuration (per-agent > defaults > compiled default).
	DedupeWindow *time.Duration `toml:"dedupe-window"`
}

// Load reads the config from the path resolved by:
//   - the CLAUDE_MSG_CONFIG env var if set
//   - DefaultPath otherwise
//
// Missing file → empty File + nil error (operational default).
// Malformed file → empty File + the toml decode error.
func Load() (*File, error) {
	path, _ := ResolvePath()
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

// HasDeprecatedNotifyOnDeliveredUnverified reports whether the deprecated TOML key
// notify-on-delivered-unverified is set for the given agent (agent-level or defaults),
// so callers can emit a WARN deprecated_surface_used line. Removal v1.0 (extended from v0.12.0 per ADR-0008 §Discretion clause, #140).
func HasDeprecatedNotifyOnDeliveredUnverified(file *File, agent string) bool {
	if file == nil {
		return false
	}
	if b, ok := file.Agent[agent]; ok && b.NotifyOnDeliveredUnverified != nil {
		return true
	}
	return file.Defaults.NotifyOnDeliveredUnverified != nil
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

// ResolveString walks the precedence chain for a string field. The first
// non-nil-and-non-empty value wins; if all layers are nil/empty,
// hardcoded is the final fallback. Empty-string fields are treated as
// "not set" so an absent TOML key falls through to the next layer
// rather than overriding with empty.
//
// Asymmetry with ResolveBool / ResolveDuration: those helpers treat
// zero-value as "explicitly set" (false / 0 are meaningful semantics
// for those types). ResolveString treats empty-string as "not set"
// because typical string config knobs are enum-ish (delivery-mode is
// "paste-and-enter" | "mailbox-only"; never legitimately ""). The
// asymmetry is intentional design-time: forcing the consumer to handle
// empty as a non-set sentinel would surface "did this operator
// intentionally clear this knob, or did the TOML decoder leave it
// blank?" ambiguity at every call-site. Treating empty as not-set at
// the helper layer codifies the convention once. A future field that
// genuinely wants empty-as-explicit-value should use a different
// resolver or `*string` directly. Per Surveyor PR #135 S1.
func ResolveString(file *File, agent, field string, hardcoded string) string {
	if file == nil {
		return hardcoded
	}
	if s := agentStringField(file, agent, field); s != nil && *s != "" {
		return *s
	}
	if s := defaultStringField(file, field); s != nil && *s != "" {
		return *s
	}
	return hardcoded
}

// ResolveInt walks the precedence chain for an int field. The first
// non-nil value wins; if all layers are nil, hardcoded is the final fallback.
func ResolveInt(file *File, agent, field string, hardcoded int) int {
	if file == nil {
		return hardcoded
	}
	if n := agentIntField(file, agent, field); n != nil {
		return *n
	}
	if n := defaultIntField(file, field); n != nil {
		return *n
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
	case "notify-on-delivered-in-input-box":
		if b.NotifyOnDeliveredInInputBox != nil {
			return b.NotifyOnDeliveredInInputBox
		}
		return b.NotifyOnDeliveredUnverified // deprecated alias, removal v1.0 (extended from v0.12.0 per ADR-0008 §Discretion clause)
	case "drift-soft-fail":
		return b.DriftSoftFail
	case "gate-disabled":
		return b.GateDisabled
	case "notify-emoji-disabled":
		return b.NotifyEmojiDisabled
	case "working-deliver-immediately":
		return b.WorkingDeliverImmediately
	case "pre-paste-safety-disabled":
		return b.PrePasteSafetyDisabled
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
	case "retention-sweep-interval":
		return b.RetentionSweepInterval
	case "dedupe-window":
		return b.DedupeWindow
	}
	return nil
}

func agentStringField(file *File, agent, field string) *string {
	if file.Agent == nil {
		return nil
	}
	block, ok := file.Agent[agent]
	if !ok {
		return nil
	}
	return blockStringField(&block, field)
}

func defaultStringField(file *File, field string) *string {
	return blockStringField(&file.Defaults, field)
}

func blockStringField(b *Block, field string) *string {
	switch field {
	case "delivery-mode":
		return b.DeliveryMode
	case "render-byte-marker-threshold":
		return b.RenderByteMarkerThreshold
	case "on-register-backlog":
		return b.OnRegisterBacklog
	case "metrics-addr":
		return b.MetricsAddr
	case "verify-retry-budget":
		return b.VerifyRetryBudget
	case "rate-limit-pattern":
		return b.RateLimitPattern
	case "retention":
		return b.Retention
	}
	return nil
}

func agentIntField(file *File, agent, field string) *int {
	if file.Agent == nil {
		return nil
	}
	block, ok := file.Agent[agent]
	if !ok {
		return nil
	}
	return blockIntField(&block, field)
}

func defaultIntField(file *File, field string) *int {
	return blockIntField(&file.Defaults, field)
}

func blockIntField(b *Block, field string) *int {
	switch field {
	case "max-recipients-per-send":
		return b.MaxRecipientsPerSend
	case "on-register-backlog-cap":
		return b.OnRegisterBacklogCap
	}
	return nil
}

// ParseByteSize parses a human byte-size string into a byte count.
// Accepted forms (case-insensitive, surrounding whitespace ignored):
//
//	"512"   → 512   (bare number = bytes)
//	"512b"  → 512
//	"2k"    → 2000  (k = ×1000)
//	"2.3k"  → 2300
//	"2kb"   → 2000  (kb accepted as an alias for k)
//
// The k multiplier is 1000 (not 1024) to mirror the render-side marker
// format, where `2.3k` denotes 2300 bytes (#160): an operator setting a
// threshold should not have to reconcile two bases against the marker it
// gates. A negative or non-numeric value is an error; callers on the hot
// path (the mailman) resolve+parse once at startup and WARN-fall-back to
// the hardcoded default rather than failing delivery.
func ParseByteSize(s string) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty byte-size")
	}
	mult := 1.0
	switch lower := strings.ToLower(t); {
	case strings.HasSuffix(lower, "kb"):
		mult, t = 1000, t[:len(t)-2]
	case strings.HasSuffix(lower, "k"):
		mult, t = 1000, t[:len(t)-1]
	case strings.HasSuffix(lower, "b"):
		mult, t = 1, t[:len(t)-1]
	}
	t = strings.TrimSpace(t)
	v, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte-size %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("negative byte-size %q", s)
	}
	return int(v * mult), nil
}

// ResolvedView is a fully-resolved per-agent snapshot. Useful for
// the `claude-msg config show` subcommand so the operator can see
// what the precedence chain decided for an agent without having to
// trace through TOML manually.
type ResolvedView struct {
	Agent                       string `json:"agent"`
	ConfigPath                  string `json:"config_path"`
	NotifyOnFailed              bool   `json:"notify_on_failed"`
	NotifyOnDeliveredInInputBox bool   `json:"notify_on_delivered_in_input_box"`
	// Deprecated: same value as NotifyOnDeliveredInInputBox; removal v1.0 (extended from v0.12.0 per ADR-0008 §Discretion clause) (#140).
	NotifyOnDeliveredUnverified bool          `json:"notify_on_delivered_unverified"`
	DriftSoftFail               bool          `json:"drift_soft_fail"`
	GateDisabled                bool          `json:"gate_disabled"`
	PollIntervalMin             time.Duration `json:"poll_interval_min"`
	PollIntervalMax             time.Duration `json:"poll_interval_max"`
	InputStaleThreshold         time.Duration `json:"input_stale_threshold"`
	NotifyEmojiDisabled         bool          `json:"notify_emoji_disabled"`
	WorkingDeliverImmediately   bool          `json:"working_deliver_immediately"`
	PrePasteSafetyDisabled      bool          `json:"pre_paste_safety_disabled"`
	DeliveryMode                string        `json:"delivery_mode,omitempty"`
	RenderByteMarkerThreshold   string        `json:"render_byte_marker_threshold"`
	MaxRecipientsPerSend        int           `json:"max_recipients_per_send"`
	OnRegisterBacklog           string        `json:"on_register_backlog"`
	OnRegisterBacklogCap        int           `json:"on_register_backlog_cap"`
	MetricsAddr                 string        `json:"metrics_addr,omitempty"`
	VerifyRetryBudget           string        `json:"verify_retry_budget"`
	Retention                   string        `json:"retention"`
	RetentionSweepInterval      time.Duration `json:"retention_sweep_interval"`
	DedupeWindow                time.Duration `json:"dedupe_window"`
	PrivilegedAgents            []string      `json:"privileged_agents"`
}

// Resolve builds the resolved snapshot. Hardcoded defaults mirror
// the CLI flag defaults in cmd/tmux-tell-claude/serve.go.
func Resolve(file *File, path, agent string) ResolvedView {
	return ResolvedView{
		Agent:                       agent,
		ConfigPath:                  path,
		NotifyOnFailed:              ResolveBool(file, agent, "notify-on-failed", true),
		NotifyOnDeliveredInInputBox: ResolveBool(file, agent, "notify-on-delivered-in-input-box", true),
		NotifyOnDeliveredUnverified: ResolveBool(file, agent, "notify-on-delivered-in-input-box", true), // same value, deprecated shadow
		DriftSoftFail:               ResolveBool(file, agent, "drift-soft-fail", false),
		GateDisabled:                ResolveBool(file, agent, "gate-disabled", false),
		PollIntervalMin:             ResolveDuration(file, agent, "poll-interval-min", 3*time.Second),
		PollIntervalMax:             ResolveDuration(file, agent, "poll-interval-max", 15*time.Second),
		InputStaleThreshold:         ResolveDuration(file, agent, "input-stale-threshold", 2*time.Minute),
		NotifyEmojiDisabled:         ResolveBool(file, agent, "notify-emoji-disabled", false),
		WorkingDeliverImmediately:   ResolveBool(file, agent, "working-deliver-immediately", false),
		PrePasteSafetyDisabled:      ResolveBool(file, agent, "pre-paste-safety-disabled", false),
		DeliveryMode:                ResolveString(file, agent, "delivery-mode", ""),
		// Hardcoded default mirrors render.DefaultByteMarkerThreshold
		// (512 bytes) as a display string; config can't import render
		// without a dependency it otherwise doesn't need. Kept in sync by
		// the doc-comment cross-reference on both consts.
		RenderByteMarkerThreshold: ResolveString(file, agent, "render-byte-marker-threshold", "512b"),
		MaxRecipientsPerSend:      ResolveInt(file, agent, "max-recipients-per-send", 10),
		OnRegisterBacklog:         ResolveString(file, agent, "on-register-backlog", DefaultOnRegisterBacklog),
		OnRegisterBacklogCap:      ResolveInt(file, agent, "on-register-backlog-cap", DefaultOnRegisterBacklogCap),
		MetricsAddr:               ResolveString(file, agent, "metrics-addr", ""),
		VerifyRetryBudget:         ResolveString(file, agent, "verify-retry-budget", DefaultVerifyRetryBudget),
		Retention:                 ResolveString(file, agent, "retention", DefaultRetention),
		RetentionSweepInterval:    ResolveDuration(file, agent, "retention-sweep-interval", DefaultRetentionSweepInterval),
		DedupeWindow:              ResolveDuration(file, agent, "dedupe-window", DefaultDedupeWindow),
		PrivilegedAgents:          resolvePrivilegedAgents(file),
	}
}

// Backlog-policy constants for the #204 don't-flood behavior at
// (re)register. The register handler resolves on-register-backlog /
// on-register-backlog-cap through these defaults; an unrecognized policy
// value falls back to BacklogAnnounce (the never-floods safe default).
const (
	// BacklogAnnounce leaves the whole queued backlog in place and delivers
	// a single nudge. The default policy.
	BacklogAnnounce = "announce"
	// BacklogAutoDeliver pastes the newest-N backlog (N = the cap) and
	// announces the older remainder.
	BacklogAutoDeliver = "auto-deliver"
	// DefaultOnRegisterBacklog is the hardcoded fallback policy.
	DefaultOnRegisterBacklog = BacklogAnnounce
	// DefaultOnRegisterBacklogCap is the hardcoded fallback newest-N cap for
	// the auto-deliver policy.
	DefaultOnRegisterBacklogCap = 3
)

// DefaultVerifyRetryBudget is the hardcoded fallback verify-token retry
// window when neither the per-agent block nor [defaults] sets a value
// (#153). Mirrors tmuxio.DefaultRetryBudget (5s) as a duration string so
// config doesn't import tmuxio. Kept in sync via the doc-comment
// cross-reference on both consts.
const DefaultVerifyRetryBudget = "5s"

// DefaultRetention is the hardcoded fallback retention policy (#245). "infinite"
// means no sweep — zero behavior change for existing deploys.
const DefaultRetention = "infinite"

// DefaultRetentionSweepInterval is the fallback sweep cadence (#245).
const DefaultRetentionSweepInterval = time.Hour

// DefaultDedupeWindow is the fallback look-back window for the mailman's
// recipient-side delivery dedupe (#157 PR2). 60 seconds covers any reasonable
// resend-after-unverified cycle. Set dedupe-window = "0s" in TOML to disable.
const DefaultDedupeWindow = 60 * time.Second

// resolvePrivilegedAgents returns a copy of the file's privileged-agents
// list (or nil if the file is nil/empty). The defensive copy prevents
// callers from mutating the underlying config slice.
func resolvePrivilegedAgents(file *File) []string {
	if file == nil || len(file.PrivilegedAgents) == 0 {
		return nil
	}
	out := make([]string, len(file.PrivilegedAgents))
	copy(out, file.PrivilegedAgents)
	return out
}

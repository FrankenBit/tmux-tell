// Package healthscan reads the mailman's operational state from
// external sources (systemd + journalctl) and surfaces it as
// structured data for the status augmentation (#45) and the dedicated
// claude-msg health subcommand (#42).
//
// Why external sources rather than in-process counters? The mailman
// is one process per agent; the CLI tools (status, health) are
// separate processes that can't peek into a mailman's memory. The
// alternatives — DB columns, on-disk counter files, a UNIX socket —
// each add coupling between mailmen and CLI. journalctl + systemd
// are already authoritative (everything the mailman logs goes there)
// and don't require new state.
//
// The trade-off: scans cost CPU per invocation. At homelab scale
// (4 mailmen, daily delivery counts in the hundreds) this is
// negligible — a fresh scan completes in well under 100ms. If the
// fleet grows past a few hundred deliveries per minute, swap in a
// persistent counter store; for now, external-sources is cheap and
// honest.
package healthscan

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AgentHealth is the per-agent snapshot the status + health commands
// render. JSON tags allow direct serialisation; omitempty on the
// percentile fields handles the "no deliveries observed" case.
type AgentHealth struct {
	Name string `json:"name"`

	// Counters for the configured window (typically since midnight
	// local for status; configurable for health).
	Delivered           int `json:"delivered"`
	DeliveredInInputBox int `json:"delivered_in_input_box"`
	// Deprecated: same value as DeliveredInInputBox; removal v1.0 (extended from v0.12.0 per ADR-0008 §Discretion clause, #140).
	DeliveredUnverified int `json:"delivered_unverified"`
	Failed              int `json:"failed"`
	QuietCapExceeded    int `json:"quiet_cap_exceeded"`
	DriftAmbiguous      int `json:"drift_ambiguous"`
	DriftUnrecoverable  int `json:"drift_unrecoverable"`

	// Deliver-time percentiles in milliseconds. Computed from
	// "delivering id=X" → "delivered id=X" pairs in the journal.
	// Zero when no full pairs observed in the window.
	DeliverP50Ms int `json:"deliver_p50_ms,omitempty"`
	DeliverP95Ms int `json:"deliver_p95_ms,omitempty"`
	DeliverP99Ms int `json:"deliver_p99_ms,omitempty"`

	// CrashCount is systemd's NRestarts counter for the mailman
	// unit. Reset on `systemctl reset-failed` or unit re-deploy;
	// otherwise monotonic over the unit's lifetime.
	CrashCount int `json:"crash_count"`
}

// ScanWindow describes the time range a scan covers.
type ScanWindow struct {
	Since time.Time
}

// SystemctlReader queries systemd for unit state. Injected so tests
// can substitute a fake.
type SystemctlReader interface {
	ShowUnit(ctx context.Context, unit string, props ...string) (map[string]string, error)
}

// JournalReader reads structured log lines from the user journal.
// Injected so tests can substitute a fake.
type JournalReader interface {
	ReadLines(ctx context.Context, unit string, since time.Time) ([]string, error)
}

// Scanner is the entry point. Constructs from default readers for
// production use; tests inject fakes.
type Scanner struct {
	Systemctl SystemctlReader
	Journal   JournalReader
	// Resolve returns the systemd mailman unit name for an agent. If nil,
	// the scanner defaults to the claude-adapter prefix — preserving the
	// pre-#708 single-adapter behavior for existing tests. Production
	// callers (health.go, status.go) inject a resolver that reads the
	// agent's registered provider from the store, so codex chambers
	// (provider=openai) get their `tmux-tell-codex-mailman@…` unit
	// probed instead of the (non-existent) claude one.
	Resolve func(agentName string) string
}

// New returns a Scanner wired with the real systemd + journalctl
// readers. Suitable for production CLI use.
func New() *Scanner {
	return &Scanner{
		Systemctl: &execSystemctl{},
		Journal:   &execJournal{},
	}
}

// Scan computes per-agent health for the listed agents over the
// given window.
func (s *Scanner) Scan(ctx context.Context, agents []string, window ScanWindow) ([]AgentHealth, error) {
	out := make([]AgentHealth, 0, len(agents))
	for _, name := range agents {
		ah, err := s.scanOne(ctx, name, window)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", name, err)
		}
		out = append(out, ah)
	}
	return out, nil
}

func (s *Scanner) scanOne(ctx context.Context, name string, window ScanWindow) (AgentHealth, error) {
	ah := AgentHealth{Name: name}
	var unit string
	if s.Resolve != nil {
		unit = s.Resolve(name)
	} else {
		// Backward-compat default: pre-#708 behavior assumed the claude
		// adapter for every agent. Codex chambers landed as silent
		// blindspots. New production callers set Resolve; the default
		// preserves the old semantics for tests.
		unit = fmt.Sprintf("tmux-tell-claude-mailman@%s.service", name)
	}

	// systemd: NRestarts.
	props, err := s.Systemctl.ShowUnit(ctx, unit, "NRestarts")
	if err == nil {
		if v, err := strconv.Atoi(props["NRestarts"]); err == nil {
			ah.CrashCount = v
		}
	}
	// We intentionally swallow systemctl errors — `journalctl` is
	// the primary data source and a missing unit shouldn't kill the
	// scan. The CLI can still report counts; CrashCount stays zero.

	// journalctl: count + timing extraction.
	lines, err := s.Journal.ReadLines(ctx, unit, window.Since)
	if err != nil {
		return ah, fmt.Errorf("journal: %w", err)
	}
	classifyLines(lines, &ah)
	return ah, nil
}

var (
	// reDelivering captures the "delivering id=X" log line emitted by
	// the mailman at the start of delivery. Both the timestamp prefix
	// and the id are extracted; the id is the join key with
	// reDelivered for percentile computation.
	reDelivering = regexp.MustCompile(`(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) delivering id=(\S+)`)
	reDelivered  = regexp.MustCompile(`(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) delivered id=(\S+)`)

	reUnverified         = regexp.MustCompile(`WARN delivered_in_input_box`)
	reFailed             = regexp.MustCompile(`deliver_failed`)
	reQuietCapExceeded   = regexp.MustCompile(`WARN quiet_cap_exceeded`)
	reDriftAmbiguous     = regexp.MustCompile(`WARN drift_check_ambiguous`)
	reDriftUnrecoverable = regexp.MustCompile(`WARN drift_detected_unrecoverable`)
)

// classifyLines walks journal lines and accumulates per-class counts
// + deliver-time percentiles into the AgentHealth. Exposed for
// testing via the package boundary.
func classifyLines(lines []string, ah *AgentHealth) {
	deliveringAt := map[string]time.Time{}
	var durations []time.Duration

	for _, line := range lines {
		switch {
		case reUnverified.MatchString(line):
			ah.DeliveredInInputBox++
			ah.DeliveredUnverified++ // deprecated shadow, same value
		case reFailed.MatchString(line):
			ah.Failed++
		case reQuietCapExceeded.MatchString(line):
			ah.QuietCapExceeded++
		case reDriftAmbiguous.MatchString(line):
			ah.DriftAmbiguous++
		case reDriftUnrecoverable.MatchString(line):
			ah.DriftUnrecoverable++
		}
		// Delivering and delivered are matched independently of the
		// above (a single log line is one of each class, but the same
		// public_id appears in both `delivering` and `delivered` lines
		// on a happy path).
		if m := reDelivering.FindStringSubmatch(line); m != nil {
			if t, ok := parseGoLogTime(m[1]); ok {
				deliveringAt[m[2]] = t
			}
		}
		if m := reDelivered.FindStringSubmatch(line); m != nil {
			ah.Delivered++
			if t, ok := parseGoLogTime(m[1]); ok {
				if start, found := deliveringAt[m[2]]; found {
					durations = append(durations, t.Sub(start))
					delete(deliveringAt, m[2])
				}
			}
		}
	}

	if len(durations) > 0 {
		ah.DeliverP50Ms = percentileMs(durations, 50)
		ah.DeliverP95Ms = percentileMs(durations, 95)
		ah.DeliverP99Ms = percentileMs(durations, 99)
	}
}

// parseGoLogTime parses the time format Go's log package emits with
// LstdFlags|Lmicroseconds: "2006/01/02 15:04:05.999999". Returns
// false if the input doesn't match.
func parseGoLogTime(s string) (time.Time, bool) {
	for _, layout := range []string{"2006/01/02 15:04:05.000000", "2006/01/02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// percentileMs returns the p-th percentile of the durations in
// milliseconds. p in [0, 100]. Linear interpolation between adjacent
// samples per the standard percentile-of-sample definition.
func percentileMs(ds []time.Duration, p int) int {
	if len(ds) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if len(sorted) == 1 {
		return int(sorted[0].Milliseconds())
	}
	// Standard "nearest rank" method: rank = ceil(p/100 * N), index =
	// rank - 1 (zero-indexed). For N=3, p=50 → ceil(1.5)=2 → idx 1
	// (middle sample). For N=3, p=99 → ceil(2.97)=3 → idx 2 (max).
	n := len(sorted)
	rank := (p*n + 99) / 100 // ceiling division of p*n/100
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return int(sorted[rank-1].Milliseconds())
}

// --- Production readers ---

type execSystemctl struct{}

func (e *execSystemctl) ShowUnit(ctx context.Context, unit string, props ...string) (map[string]string, error) {
	args := []string{"--user", "show", unit}
	for _, p := range props {
		args = append(args, "--property="+p)
	}
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	return parseShowOutput(string(out)), nil
}

// parseShowOutput parses systemctl-show's KEY=VALUE\n format.
func parseShowOutput(s string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '='); i > 0 {
			out[line[:i]] = line[i+1:]
		}
	}
	return out
}

type execJournal struct{}

func (e *execJournal) ReadLines(ctx context.Context, unit string, since time.Time) ([]string, error) {
	args := []string{"--user", "-u", unit, "--no-pager", "--output=cat"}
	if !since.IsZero() {
		args = append(args, "--since", since.Format("2006-01-02 15:04:05"))
	}
	cmd := exec.CommandContext(ctx, "journalctl", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("journalctl %s: %w", unit, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n"), nil
}

// SinceMidnight returns a ScanWindow covering today (since 00:00
// local time). Useful default for the status augmentation.
func SinceMidnight(now time.Time) ScanWindow {
	y, m, d := now.Date()
	return ScanWindow{Since: time.Date(y, m, d, 0, 0, 0, 0, now.Location())}
}

// SinceDuration returns a ScanWindow covering the last d, anchored at
// now. Useful for `claude-msg health --since 1h`.
func SinceDuration(now time.Time, d time.Duration) ScanWindow {
	return ScanWindow{Since: now.Add(-d)}
}

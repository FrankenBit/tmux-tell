// Discipline pins for the internal/healthscan package. Per ADR-0001,
// these tests guard architectural commitments rather than behavioral
// contracts. On failure, triage per ADR-0001 §Triage before changing
// the assertion. The pin_test.go file location, the TestPin_ prefix,
// and the testpin.Triage call are the orthogonal grep handles for the
// discipline.
package healthscan

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/testpin"
)

// PIN: external-source-on-demand scan completes in <100ms for the
// current mailman+delivery-rate baseline. When this pin starts
// failing, the baseline has shifted enough that the architectural
// call to read from journalctl on demand (rather than persistent
// in-process counters) should be revisited.
//
// Per Surveyor's #42 retrospective: the migration trigger from
// external-source to persistent counters is fleet growth, and growth
// is measurable. Wiring it as a pin converts "we believe scans are
// fast enough" into a mechanical check; when the belief stops
// being true, the pin's failure is the signal to either:
//
//   (a) optimise the scan (still external-source, just faster)
//   (b) migrate to persistent counters per the CHANGELOG[Unreleased]
//       deferred-architecture flag
//   (c) retract the commitment via superseding ADR if external-source
//       was misjudged
//
// All three are legitimate (c)-class diagnoses on triage; what's NOT
// legitimate is "raise the ceiling because the test failed" without
// going through the same diagnosis.
//
// Baseline: 4 mailmen × ~500 lines/day each is the plausible top end
// for the homelab scale. Each delivery generates 2 lines (delivering +
// delivered); some carry WARN classifications too. The synthetic
// dataset below mirrors that shape.
//
// 100ms is the published architectural commitment from the #42
// closing comment + CHANGELOG.
func TestPin_HealthScanLatencyCeiling_Under100ms(t *testing.T) {
	testpin.Triage(t, "HealthScanLatencyCeiling",
		"external-source scan completes in <100ms for the current mailman+delivery-rate baseline")

	const (
		mailmenCount  = 4
		linesPerAgent = 500
		ceiling       = 100 * time.Millisecond
	)

	// Build synthetic per-agent line sets that mirror plausible journal
	// shape: delivering/delivered pairs + occasional WARN lines.
	byUnit := make(map[string][]string, mailmenCount)
	agents := []string{"admin", "bosun", "pilot", "surveyor"}
	for _, name := range agents {
		unit := "tmux-msg-claude-mailman@" + name + ".service"
		byUnit[unit] = syntheticJournalLines(name, linesPerAgent)
	}

	sc := &Scanner{
		Systemctl: &fakeSystemctl{byUnit: map[string]map[string]string{
			"tmux-msg-claude-mailman@admin.service":    {"NRestarts": "0"},
			"tmux-msg-claude-mailman@bosun.service":    {"NRestarts": "0"},
			"tmux-msg-claude-mailman@pilot.service":    {"NRestarts": "0"},
			"tmux-msg-claude-mailman@surveyor.service": {"NRestarts": "0"},
		}},
		Journal: &fakeJournal{byUnit: byUnit},
	}

	// Run the scan and time it. We measure the scan call only — fake
	// readers return immediately, so the measured duration is pure
	// regex/classification work, the actual cost a real production
	// scan adds on top of the journalctl/systemctl IO it has to do.
	start := time.Now()
	_, err := sc.Scan(context.Background(), agents, ScanWindow{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if elapsed >= ceiling {
		t.Errorf("scan over %d agents × %d lines/agent took %v; commitment is < %v",
			mailmenCount, linesPerAgent, elapsed, ceiling)
		t.Logf("triage per ADR-0001 §Triage; options: (a) optimise the scan, (b) migrate to persistent counters per CHANGELOG deferred-architecture flag, (c) retract the architectural commitment via superseding ADR")
	}
}

// syntheticJournalLines builds a plausible journal-line sequence for
// a single agent. Mixes delivering/delivered pairs, occasional WARN
// classifications, and unrelated info lines. Distribution mirrors
// the empirical 2026-05-31 journal across the four mailmen.
func syntheticJournalLines(agent string, count int) []string {
	lines := make([]string, 0, count)
	baseTime := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count/2; i++ {
		start := baseTime.Add(time.Duration(i) * time.Second)
		delivered := start.Add(time.Duration(50+i%500) * time.Millisecond)
		id := fmt.Sprintf("id%04x", i)
		lines = append(lines,
			fmt.Sprintf("[mailman/%s] %s delivering id=%s kind=message",
				agent, start.Format("2006/01/02 15:04:05.000000"), id),
			fmt.Sprintf("[mailman/%s] %s delivered id=%s",
				agent, delivered.Format("2006/01/02 15:04:05.000000"), id))
		// Sprinkle in WARN lines at a 1-in-50 rate to mirror real
		// production noise.
		if i%50 == 0 {
			lines = append(lines,
				fmt.Sprintf("[mailman/%s] %s WARN quiet_cap_exceeded id=%s",
					agent, start.Format("2006/01/02 15:04:05.000000"), id))
		}
		if i%200 == 0 {
			lines = append(lines,
				fmt.Sprintf("[mailman/%s] %s WARN delivered_unverified id=%s",
					agent, start.Format("2006/01/02 15:04:05.000000"), id))
		}
	}
	// Trim to exactly count lines.
	if len(lines) > count {
		lines = lines[:count]
	}
	return lines
}

// Compile-time guard against accidental linter dead-code claims.
var _ = strings.HasPrefix

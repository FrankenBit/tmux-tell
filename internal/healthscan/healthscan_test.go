package healthscan

import (
	"context"
	"testing"
	"time"
)

// fakeSystemctl returns canned property maps per unit name.
type fakeSystemctl struct {
	byUnit map[string]map[string]string
	err    error
}

func (f *fakeSystemctl) ShowUnit(_ context.Context, unit string, _ ...string) (map[string]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.byUnit == nil {
		return map[string]string{}, nil
	}
	return f.byUnit[unit], nil
}

// fakeJournal returns canned lines per unit name.
type fakeJournal struct {
	byUnit map[string][]string
}

func (f *fakeJournal) ReadLines(_ context.Context, unit string, _ time.Time) ([]string, error) {
	if f.byUnit == nil {
		return nil, nil
	}
	return f.byUnit[unit], nil
}

func TestScan_HappyPath(t *testing.T) {
	sc := &Scanner{
		Systemctl: &fakeSystemctl{byUnit: map[string]map[string]string{
			"tmux-tell-claude-mailman@bosun.service": {"NRestarts": "3"},
		}},
		Journal: &fakeJournal{byUnit: map[string][]string{
			"tmux-tell-claude-mailman@bosun.service": {
				"[mailman/bosun] 2026/05/31 12:00:00.000000 delivering id=abc kind=message",
				"[mailman/bosun] 2026/05/31 12:00:00.500000 delivered id=abc",
				"[mailman/bosun] 2026/05/31 12:01:00.000000 WARN delivered_in_input_box id=def",
				"[mailman/bosun] 2026/05/31 12:02:00.000000 WARN quiet_cap_exceeded id=ghi",
				"[mailman/bosun] 2026/05/31 12:03:00.000000 deliver_failed id=jkl err=tmux gone",
				"[mailman/bosun] 2026/05/31 12:04:00.000000 WARN drift_check_ambiguous id=mno",
				"[mailman/bosun] 2026/05/31 12:05:00.000000 WARN drift_detected_unrecoverable id=pqr",
			},
		}},
	}
	out, err := sc.Scan(context.Background(), []string{"bosun"},
		ScanWindow{Since: time.Time{}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	got := out[0]
	if got.Name != "bosun" {
		t.Errorf("Name = %q, want bosun", got.Name)
	}
	if got.CrashCount != 3 {
		t.Errorf("CrashCount = %d, want 3", got.CrashCount)
	}
	if got.Delivered != 1 {
		t.Errorf("Delivered = %d, want 1", got.Delivered)
	}
	if got.DeliveredInInputBox != 1 {
		t.Errorf("DeliveredInInputBox = %d, want 1", got.DeliveredInInputBox)
	}
	if got.QuietCapExceeded != 1 {
		t.Errorf("QuietCapExceeded = %d, want 1", got.QuietCapExceeded)
	}
	if got.Failed != 1 {
		t.Errorf("Failed = %d, want 1", got.Failed)
	}
	if got.DriftAmbiguous != 1 {
		t.Errorf("DriftAmbiguous = %d, want 1", got.DriftAmbiguous)
	}
	if got.DriftUnrecoverable != 1 {
		t.Errorf("DriftUnrecoverable = %d, want 1", got.DriftUnrecoverable)
	}
	// The single delivering→delivered pair was 500ms; with only one
	// sample, all percentiles equal that sample.
	if got.DeliverP50Ms != 500 {
		t.Errorf("DeliverP50Ms = %d, want 500", got.DeliverP50Ms)
	}
}

// TestScan_DeprecatedDeliveredUnverifiedShadow verifies the dual-field JSON
// backward-compat: DeliveredUnverified mirrors DeliveredInInputBox (#140).
func TestScan_DeprecatedDeliveredUnverifiedShadow(t *testing.T) {
	ah := AgentHealth{}
	classifyLines([]string{
		"[mailman/x] 2026/06/08 00:00:00 WARN delivered_in_input_box id=abc",
		"[mailman/x] 2026/06/08 00:00:01 WARN delivered_in_input_box id=def",
	}, &ah)
	if ah.DeliveredInInputBox != 2 {
		t.Errorf("DeliveredInInputBox = %d, want 2", ah.DeliveredInInputBox)
	}
	if ah.DeliveredUnverified != ah.DeliveredInInputBox {
		t.Errorf("DeliveredUnverified (%d) != DeliveredInInputBox (%d); shadow must mirror",
			ah.DeliveredUnverified, ah.DeliveredInInputBox)
	}
}

// TestScan_ResolverRoutesToCodexUnit covers #708: when Scanner.Resolve is set,
// scanOne queries the resolved unit instead of the hardcoded claude prefix. A
// codex chamber (whose real unit is `tmux-tell-codex-mailman@<name>.service`)
// was silently reporting all-zeros pre-fix because the claude unit doesn't
// exist for it. The fake journal fixture below only has data under the codex
// unit key; the resolver has to route to it for the WARN counter to fire.
func TestScan_ResolverRoutesToCodexUnit(t *testing.T) {
	sc := &Scanner{
		Systemctl: &fakeSystemctl{byUnit: map[string]map[string]string{
			"tmux-tell-codex-mailman@carpenter.service": {"NRestarts": "2"},
		}},
		Journal: &fakeJournal{byUnit: map[string][]string{
			"tmux-tell-codex-mailman@carpenter.service": {
				"[mailman/carpenter] 2026/05/31 12:00:00 WARN quiet_cap_exceeded id=abc",
			},
		}},
		Resolve: func(name string) string {
			return "tmux-tell-codex-mailman@" + name + ".service"
		},
	}
	out, err := sc.Scan(context.Background(), []string{"carpenter"}, ScanWindow{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := out[0]
	if got.CrashCount != 2 {
		t.Errorf("CrashCount = %d, want 2 (systemd probe against codex unit failed to route)", got.CrashCount)
	}
	if got.QuietCapExceeded != 1 {
		t.Errorf("QuietCapExceeded = %d, want 1 (journal probe against codex unit failed to route)", got.QuietCapExceeded)
	}
}

// TestScan_NilResolverPreservesClaudeDefault covers the backward-compat
// contract: when Scanner.Resolve is nil, scanOne falls back to the pre-#708
// claude-prefix hardcode. Existing tests rely on this default.
func TestScan_NilResolverPreservesClaudeDefault(t *testing.T) {
	sc := &Scanner{
		Systemctl: &fakeSystemctl{},
		Journal: &fakeJournal{byUnit: map[string][]string{
			"tmux-tell-claude-mailman@bosun.service": {
				"[mailman/bosun] 2026/05/31 12:00:00 WARN quiet_cap_exceeded id=abc",
			},
		}},
		// Resolve intentionally left nil.
	}
	out, _ := sc.Scan(context.Background(), []string{"bosun"}, ScanWindow{})
	if out[0].QuietCapExceeded != 1 {
		t.Errorf("QuietCapExceeded = %d, want 1 (nil-Resolve default should hit the claude unit)", out[0].QuietCapExceeded)
	}
}

func TestScan_NoDeliveriesNoPercentiles(t *testing.T) {
	sc := &Scanner{
		Systemctl: &fakeSystemctl{},
		Journal: &fakeJournal{byUnit: map[string][]string{
			"tmux-tell-claude-mailman@quiet.service": {
				"[mailman/quiet] 2026/05/31 12:00:00 starting pane=%1",
			},
		}},
	}
	out, _ := sc.Scan(context.Background(), []string{"quiet"}, ScanWindow{})
	got := out[0]
	if got.Delivered != 0 {
		t.Errorf("Delivered = %d, want 0", got.Delivered)
	}
	if got.DeliverP50Ms != 0 {
		t.Errorf("DeliverP50Ms = %d, want 0 (no samples)", got.DeliverP50Ms)
	}
}

func TestClassifyLines_PercentilesAcrossMultiplePairs(t *testing.T) {
	lines := []string{
		"[mailman/x] 2026/05/31 12:00:00.000000 delivering id=a",
		"[mailman/x] 2026/05/31 12:00:00.100000 delivered id=a",
		"[mailman/x] 2026/05/31 12:00:01.000000 delivering id=b",
		"[mailman/x] 2026/05/31 12:00:01.500000 delivered id=b",
		"[mailman/x] 2026/05/31 12:00:02.000000 delivering id=c",
		"[mailman/x] 2026/05/31 12:00:04.000000 delivered id=c",
	}
	var ah AgentHealth
	classifyLines(lines, &ah)
	if ah.Delivered != 3 {
		t.Fatalf("Delivered = %d, want 3", ah.Delivered)
	}
	// Durations: 100ms, 500ms, 2000ms (sorted). p50 → middle → 500ms.
	// p95/p99 → last → 2000ms.
	if ah.DeliverP50Ms != 500 {
		t.Errorf("p50 = %d, want 500", ah.DeliverP50Ms)
	}
	if ah.DeliverP99Ms != 2000 {
		t.Errorf("p99 = %d, want 2000", ah.DeliverP99Ms)
	}
}

func TestParseGoLogTime_Roundtrip(t *testing.T) {
	for _, want := range []string{
		"2026/05/31 12:00:00",
		"2026/05/31 12:00:00.500000",
	} {
		if _, ok := parseGoLogTime(want); !ok {
			t.Errorf("failed to parse %q", want)
		}
	}
	if _, ok := parseGoLogTime("not a time"); ok {
		t.Errorf("garbage parsed successfully")
	}
}

func TestSinceMidnight(t *testing.T) {
	now := time.Date(2026, 5, 31, 15, 30, 0, 0, time.UTC)
	w := SinceMidnight(now)
	want := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	if !w.Since.Equal(want) {
		t.Errorf("Since = %v, want %v", w.Since, want)
	}
}

func TestSinceDuration(t *testing.T) {
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	w := SinceDuration(now, 2*time.Hour)
	want := time.Date(2026, 5, 31, 10, 0, 0, 0, time.UTC)
	if !w.Since.Equal(want) {
		t.Errorf("Since = %v, want %v", w.Since, want)
	}
}

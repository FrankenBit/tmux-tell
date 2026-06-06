package main

import (
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		spec      string
		wantAll   bool
		wantSince time.Time // ignored when wantAll
	}{
		{"", false, now.Add(-24 * time.Hour)}, // default
		{"all", true, time.Time{}},
		{"ALL", true, time.Time{}}, // case-insensitive
		{"1h", false, now.Add(-1 * time.Hour)},
		{"24h", false, now.Add(-24 * time.Hour)},
		{"7d", false, now.Add(-7 * 24 * time.Hour)},
		{"90m", false, now.Add(-90 * time.Minute)},
		{"1h30m", false, now.Add(-90 * time.Minute)},
		{" 7d ", false, now.Add(-7 * 24 * time.Hour)}, // trimmed
	}
	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			w, err := parseWindow(c.spec, now)
			if err != nil {
				t.Fatalf("parseWindow(%q): %v", c.spec, err)
			}
			if w.All != c.wantAll {
				t.Fatalf("All = %v, want %v", w.All, c.wantAll)
			}
			if !c.wantAll && !w.Since.Equal(c.wantSince) {
				t.Errorf("Since = %v, want %v", w.Since, c.wantSince)
			}
		})
	}
}

func TestParseWindow_Errors(t *testing.T) {
	now := time.Now()
	for _, spec := range []string{"0h", "-1h", "bogus", "7", "d", "1x"} {
		if _, err := parseWindow(spec, now); err == nil {
			t.Errorf("parseWindow(%q) = nil err, want error", spec)
		}
	}
}

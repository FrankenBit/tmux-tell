package config

import (
	"testing"
	"time"
)

// TestResolveString_RetentionPrecedence pins the standard precedence chain
// for the retention knob: per-agent > defaults > hardcoded "infinite".
func TestResolveString_RetentionPrecedence(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		name string
		file *File
		want string
	}{
		{"nil file → infinite", nil, "infinite"},
		{"empty file → infinite", &File{}, "infinite"},
		{
			"defaults block wins",
			&File{Defaults: Block{Retention: str("30d")}},
			"30d",
		},
		{
			"per-agent wins over defaults",
			&File{
				Defaults: Block{Retention: str("30d")},
				Agent:    map[string]Block{"alice": {Retention: str("7d")}},
			},
			"7d",
		},
		{
			"non-matching agent falls through to defaults",
			&File{
				Defaults: Block{Retention: str("30d")},
				Agent:    map[string]Block{"bob": {Retention: str("7d")}},
			},
			"30d",
		},
		{
			"per-agent infinite overrides defaults",
			&File{
				Defaults: Block{Retention: str("30d")},
				Agent:    map[string]Block{"alice": {Retention: str("infinite")}},
			},
			"infinite",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveString(c.file, "alice", "retention", DefaultRetention)
			if got != c.want {
				t.Errorf("ResolveString(retention) = %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveDuration_RetentionSweepInterval pins the precedence chain for
// the retention-sweep-interval duration knob.
func TestResolveDuration_RetentionSweepInterval(t *testing.T) {
	dur := func(d time.Duration) *time.Duration { return &d }
	cases := []struct {
		name string
		file *File
		want time.Duration
	}{
		{"nil file → 1h default", nil, time.Hour},
		{"empty file → 1h default", &File{}, time.Hour},
		{
			"defaults block wins",
			&File{Defaults: Block{RetentionSweepInterval: dur(6 * time.Hour)}},
			6 * time.Hour,
		},
		{
			"per-agent wins over defaults",
			&File{
				Defaults: Block{RetentionSweepInterval: dur(6 * time.Hour)},
				Agent:    map[string]Block{"alice": {RetentionSweepInterval: dur(30 * time.Minute)}},
			},
			30 * time.Minute,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveDuration(c.file, "alice", "retention-sweep-interval", DefaultRetentionSweepInterval)
			if got != c.want {
				t.Errorf("ResolveDuration(retention-sweep-interval) = %v, want %v", got, c.want)
			}
		})
	}
}

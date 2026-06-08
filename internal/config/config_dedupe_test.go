package config

import (
	"testing"
	"time"
)

// TestResolveDuration_DedupeWindow pins the precedence chain for the
// dedupe-window duration knob (#157 PR2).
func TestResolveDuration_DedupeWindow(t *testing.T) {
	dur := func(d time.Duration) *time.Duration { return &d }
	cases := []struct {
		name string
		file *File
		want time.Duration
	}{
		{"nil file → 60s default", nil, 60 * time.Second},
		{"empty file → 60s default", &File{}, 60 * time.Second},
		{
			"defaults block wins",
			&File{Defaults: Block{DedupeWindow: dur(30 * time.Second)}},
			30 * time.Second,
		},
		{
			"per-agent wins over defaults",
			&File{
				Defaults: Block{DedupeWindow: dur(30 * time.Second)},
				Agent:    map[string]Block{"alice": {DedupeWindow: dur(0)}},
			},
			0, // "0s" disables
		},
		{
			"non-matching agent falls through to defaults",
			&File{
				Defaults: Block{DedupeWindow: dur(30 * time.Second)},
				Agent:    map[string]Block{"bob": {DedupeWindow: dur(10 * time.Second)}},
			},
			30 * time.Second,
		},
		{
			"zero disables (explicit)",
			&File{Defaults: Block{DedupeWindow: dur(0)}},
			0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveDuration(c.file, "alice", "dedupe-window", DefaultDedupeWindow)
			if got != c.want {
				t.Errorf("ResolveDuration(dedupe-window) = %v, want %v", got, c.want)
			}
		})
	}
}

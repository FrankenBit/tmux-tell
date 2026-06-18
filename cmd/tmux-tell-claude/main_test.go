package main

import "testing"

func TestClaudeProfile_RateLimitPatternSampleGated(t *testing.T) {
	p := claudeProfile()
	if p.Pane.RateLimitPattern != "" {
		t.Fatalf("Claude RateLimitPattern = %q, want empty until real rate-limit pane samples land (#504)", p.Pane.RateLimitPattern)
	}
}

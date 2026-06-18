package main

import "testing"

func TestClaudeProfile_RateLimitMarkersSampleGated(t *testing.T) {
	p := claudeProfile()
	if len(p.Pane.RateLimitMarkers) != 0 {
		t.Fatalf("Claude RateLimitMarkers = %v, want empty until real rate-limit pane samples land (#504)", p.Pane.RateLimitMarkers)
	}
}

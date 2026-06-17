package cli

import (
	"testing"
	"time"
)

// TestPostDeliverDelay pins the #449 cooldown choice deterministically (without
// a flaky wall-clock loop test): a true delivery waits the longer of the
// inter-message delay and the cooldown; a non-delivery, or a cooldown shorter
// than the inter-message delay, waits the plain inter-message delay.
func TestPostDeliverDelay(t *testing.T) {
	const (
		inter    = 200 * time.Millisecond
		cooldown = 5 * time.Second
	)
	cases := []struct {
		name      string
		delivered bool
		cooldown  time.Duration
		want      time.Duration
	}{
		{"delivered uses cooldown when longer", true, cooldown, cooldown},
		{"non-delivery uses inter-message delay", false, cooldown, inter},
		{"delivered but cooldown shorter uses inter-message delay", true, 50 * time.Millisecond, inter},
		{"cooldown disabled (0) uses inter-message delay even on delivery", true, 0, inter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := postDeliverDelay(tc.delivered, inter, tc.cooldown); got != tc.want {
				t.Errorf("postDeliverDelay(%v, %v, %v) = %v, want %v", tc.delivered, inter, tc.cooldown, got, tc.want)
			}
		})
	}
}

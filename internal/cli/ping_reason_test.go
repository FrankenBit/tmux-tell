package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
	"git.frankenbit.de/frankenbit/tmux-msg/internal/tmuxio"
)

// TestClassifyPingReason induces each of the #358 sub-cases through its
// substrate signal-combination and confirms the classifier yields the distinct,
// operator-actionable reason. This is the closed-loop proof for AC#4: the pure
// classifier is the seam, so every branch is exercised without a live system.
func TestClassifyPingReason(t *testing.T) {
	awaiting := tmuxio.StateAwaitingOperator.String()
	cases := []struct {
		name      string
		pingState string
		ev        pingEvidence
		want      pingReason
	}{
		{
			name:      "pane_dead — mailman claimed it, pane not live (failed)",
			pingState: string(store.StateFailed),
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 1, CurrentState: "idle"},
			want:      reasonPaneDead,
		},
		{
			name:      "mailman_down — timeout, unit not active",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: false, QueueDepth: 1, CurrentState: "unknown"},
			want:      reasonMailmanDown,
		},
		{
			name:      "stuck — timeout, agents.stuck_reason set (#291 park)",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 1, CurrentState: "unknown", StuckReason: store.StuckReasonPaneNotFound},
			want:      reasonStuck,
		},
		{
			name:      "blocked_delivery — timeout, observe-gate awaiting operator",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 1, CurrentState: awaiting},
			want:      reasonBlockedDelivery,
		},
		{
			name:      "backlog_draining — timeout, real backlog ahead of the probe row",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 7, CurrentState: "working"},
			want:      reasonBacklogDraining,
		},
		{
			name:      "blocked_delivery — default tail (running + reachable, empty queue, undelivered)",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 1, CurrentState: "idle"},
			want:      reasonBlockedDelivery,
		},
		{
			name:      "stuck precedes backlog — park signal wins over queue depth",
			pingState: pingStateTimeout,
			ev:        pingEvidence{MailmanActive: true, QueueDepth: 9, CurrentState: "unknown", StuckReason: store.StuckReasonPaneNotFound},
			want:      reasonStuck,
		},
		{
			name:      "pane_dead ignores evidence — failed is terminal regardless",
			pingState: string(store.StateFailed),
			ev:        pingEvidence{MailmanActive: false, QueueDepth: 9, CurrentState: awaiting, StuckReason: store.StuckReasonPaneNotFound},
			want:      reasonPaneDead,
		},
	}
	seen := map[pingReason]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPingReason(tc.pingState, tc.ev); got != tc.want {
				t.Errorf("classifyPingReason(%q, %+v) = %q, want %q", tc.pingState, tc.ev, got, tc.want)
			}
		})
		seen[tc.want] = true
	}
	// AC#4: all five distinct reasons are produced by the induced sub-cases.
	for _, r := range []pingReason{reasonPaneDead, reasonMailmanDown, reasonStuck, reasonBlockedDelivery, reasonBacklogDraining} {
		if !seen[r] {
			t.Errorf("no induced sub-case produced reason %q", r)
		}
	}
}

// TestRenderPingResult_ReasonSuffix proves the CLI human-renderer formats the
// reason as the short suffix (#358 AC#3) and that the no-reason / reachable
// paths still render cleanly.
func TestRenderPingResult_ReasonSuffix(t *testing.T) {
	t.Run("blocked_delivery suffix carries reason + phrase + evidence", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: false, Agent: "bob", ID: "abcd", State: pingStateTimeout, ElapsedMs: 5000,
			Reason: reasonBlockedDelivery,
			Evidence: &pingEvidence{
				MailmanActive: true, QueueDepth: 7, CurrentState: tmuxio.StateAwaitingOperator.String(),
			},
		}, "text")
		s := out.String()
		for _, want := range []string{
			"UNREACHABLE",
			"blocked_delivery",
			"observe-gate is refusing delivery",
			"queue=7",
			"state=awaiting-operator",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("text %q missing %q", s, want)
			}
		}
	})

	t.Run("stuck suffix carries the park reason", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: false, Agent: "bob", ID: "abcd", State: pingStateTimeout,
			Reason:   reasonStuck,
			Evidence: &pingEvidence{MailmanActive: true, QueueDepth: 3, CurrentState: "unknown", StuckReason: store.StuckReasonPaneNotFound},
		}, "text")
		if s := out.String(); !strings.Contains(s, "stuck=pane-not-found") {
			t.Errorf("text %q missing stuck park reason", s)
		}
	})

	t.Run("no-reason unreachable still renders (back-compat)", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: false, Agent: "bob", ID: "abcd", State: "failed", Error: "pane gone",
		}, "text")
		s := out.String()
		for _, want := range []string{"failed", "UNREACHABLE", "pane gone"} {
			if !strings.Contains(s, want) {
				t.Errorf("text %q missing %q", s, want)
			}
		}
	})

	t.Run("reachable renders without a reason", func(t *testing.T) {
		var out bytes.Buffer
		renderPingResult(&out, pingResult{
			OK: true, Agent: "bob", ID: "abcd", State: "delivered",
		}, "text")
		s := out.String()
		if !strings.Contains(s, "reachable") || strings.Contains(s, "UNREACHABLE") {
			t.Errorf("reachable text %q malformed", s)
		}
	})
}

// TestPingResult_JSONOmitsReasonOnOK confirms the reason/evidence fields are
// omitted on the reachable path and round-trip on the UNREACHABLE path (#358),
// so the OK wire shape is unchanged for existing consumers.
func TestPingResult_JSONOmitsReasonOnOK(t *testing.T) {
	t.Run("OK omits reason + evidence", func(t *testing.T) {
		b, err := json.Marshal(pingResult{OK: true, Agent: "bob", State: "delivered"})
		if err != nil {
			t.Fatal(err)
		}
		if s := string(b); strings.Contains(s, "reason") || strings.Contains(s, "evidence") {
			t.Errorf("OK ping JSON should omit reason/evidence, got %s", s)
		}
	})

	t.Run("UNREACHABLE round-trips reason + evidence", func(t *testing.T) {
		in := pingResult{
			OK: false, Agent: "bob", State: pingStateTimeout,
			Reason:   reasonBacklogDraining,
			Evidence: &pingEvidence{MailmanActive: true, QueueDepth: 4, CurrentState: "working"},
		}
		b, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var got pingResult
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if got.Reason != reasonBacklogDraining || got.Evidence == nil || got.Evidence.QueueDepth != 4 {
			t.Errorf("round-trip lost reason/evidence: %+v", got)
		}
	})
}

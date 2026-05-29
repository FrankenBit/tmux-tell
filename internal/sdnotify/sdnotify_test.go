package sdnotify

import (
	"testing"
	"time"
)

func TestReadyAndWatchdog_NoSocketIsNoop(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Ready(); err != nil {
		t.Errorf("Ready without socket: %v", err)
	}
	if err := Watchdog(); err != nil {
		t.Errorf("Watchdog without socket: %v", err)
	}
}

func TestSetSender_RoundTrip(t *testing.T) {
	var got []string
	prev := SetSender(func(m string) error {
		got = append(got, m)
		return nil
	})
	t.Cleanup(func() { SetSender(prev) })

	_ = Ready()
	_ = Watchdog()
	_ = Watchdog()
	if len(got) != 3 || got[0] != "READY=1" || got[1] != "WATCHDOG=1" || got[2] != "WATCHDOG=1" {
		t.Errorf("got %v", got)
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "")
	if d, err := WatchdogInterval(); err != nil || d != 0 {
		t.Errorf("unset: got %v err=%v, want 0", d, err)
	}

	t.Setenv("WATCHDOG_USEC", "30000000") // 30 s
	d, err := WatchdogInterval()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if d != 15*time.Second {
		t.Errorf("got %v, want 15s (half of 30s)", d)
	}

	t.Setenv("WATCHDOG_USEC", "garbage")
	if _, err := WatchdogInterval(); err == nil {
		t.Error("garbage should produce error")
	}
}

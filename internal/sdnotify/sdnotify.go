// Package sdnotify is a tiny systemd notification helper. It implements
// just the two messages cli-semaphore needs (READY=1 and WATCHDOG=1) by
// writing directly to the Unix socket pointed at by $NOTIFY_SOCKET.
//
// No external dependencies. If $NOTIFY_SOCKET is unset (not running under
// systemd), every call is a no-op — the daemon stays runnable on a
// developer's laptop or in tests.
package sdnotify

import (
	"errors"
	"net"
	"os"
	"strconv"
	"time"
)

// Sender is the indirection used in tests. Production code uses
// DefaultSender which writes to NOTIFY_SOCKET.
type Sender func(message string) error

// DefaultSender writes message to the abstract or filesystem Unix socket
// at $NOTIFY_SOCKET. Returns nil if the env var is empty.
var DefaultSender Sender = func(message string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	// systemd uses leading "@" for the abstract namespace, which Go's net
	// package converts to a NUL prefix when the name starts with "@".
	if len(socket) > 0 && socket[0] == '@' {
		addr.Name = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(message))
	return err
}

// SetSender installs a test fake and returns the previous Sender.
func SetSender(s Sender) Sender {
	prev := DefaultSender
	DefaultSender = s
	return prev
}

// Ready sends READY=1 — tells systemd the service is up.
func Ready() error {
	return DefaultSender("READY=1")
}

// Watchdog sends WATCHDOG=1 — pings the watchdog timer. Should be called
// at less than half the configured WatchdogSec interval.
func Watchdog() error {
	return DefaultSender("WATCHDOG=1")
}

// WatchdogInterval returns the recommended ping interval, derived from
// $WATCHDOG_USEC (microseconds, set by systemd when WatchdogSec is
// configured on the unit). Returns 0 with no error if the env var isn't
// set, signalling "watchdog not active — don't bother pinging".
//
// Convention: ping at half the systemd-configured interval, leaving
// headroom for jitter and one missed loop iteration.
func WatchdogInterval() (time.Duration, error) {
	raw := os.Getenv("WATCHDOG_USEC")
	if raw == "" {
		return 0, nil
	}
	usec, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, errors.New("WATCHDOG_USEC: " + err.Error())
	}
	return time.Duration(usec) * time.Microsecond / 2, nil
}

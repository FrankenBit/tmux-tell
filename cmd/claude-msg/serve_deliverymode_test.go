package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// TestServe_ConfigDeliveryMode_OverridesDB pins the #132 invariant:
// when the resolved TOML config sets a delivery-mode, it overrides the
// DB column at mailman startup. The DB column is NOT modified; the
// override is purely runtime. This test sets DB = paste-and-enter,
// config = mailbox-only, verifies the mailman short-circuits as
// mailbox-only.
func TestServe_ConfigDeliveryMode_OverridesDB(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "operator", "%5")
	// DB says paste-and-enter (default from migration).
	if err := s.SetDeliveryMode(ctx, "operator", store.DeliveryModePasteAndEnter); err != nil {
		t.Fatalf("setup: %v", err)
	}

	opts := fastOpts("operator")
	opts.ConfigDeliveryMode = store.DeliveryModeMailboxOnly // config overrides

	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, opts, logger,
		io.Discard, io.Discard)

	if exit != exitOK {
		t.Errorf("exit = %d, want exitOK; log=%s", exit, logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "delivery_mode overridden by config") {
		t.Errorf("expected override log line; got %s", logbuf.String())
	}
	if !strings.Contains(logbuf.String(), "delivery_mode=mailbox-only — no daemon work") {
		t.Errorf("expected mailbox-only short-circuit log; got %s", logbuf.String())
	}

	// DB column should remain unchanged (runtime override only).
	a, _ := s.GetAgent(ctx, "operator")
	if a.DeliveryMode != store.DeliveryModePasteAndEnter {
		t.Errorf("DB column = %s, want %s (config override should NOT persist)",
			a.DeliveryMode, store.DeliveryModePasteAndEnter)
	}
}

// TestServe_ConfigDeliveryMode_InvalidLogsAndFallsBack pins the
// fail-loud-not-fail-stop policy: invalid config values log a WARN and
// the DB column wins. A typo in /etc/tmux-msg/config.toml doesn't
// silently break the mailman.
func TestServe_ConfigDeliveryMode_InvalidLogsAndFallsBack(t *testing.T) {
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "operator", "%5")
	if err := s.SetDeliveryMode(ctx, "operator", store.DeliveryModeMailboxOnly); err != nil {
		t.Fatalf("setup: %v", err)
	}

	opts := fastOpts("operator")
	opts.ConfigDeliveryMode = "bogus-mode" // invalid

	logbuf := &bytes.Buffer{}
	logger := log.New(logbuf, "[mailman/test] ", 0)
	exit := runServeWithStore(context.Background(), s, opts, logger,
		io.Discard, io.Discard)

	if exit != exitOK {
		t.Errorf("exit = %d, want exitOK", exit)
	}
	if !strings.Contains(logbuf.String(), "WARN config_delivery_mode_invalid") {
		t.Errorf("expected invalid-mode WARN; got %s", logbuf.String())
	}
	// DB value (mailbox-only) wins, so the mailman still short-circuits.
	if !strings.Contains(logbuf.String(), "delivery_mode=mailbox-only — no daemon work") {
		t.Errorf("expected mailbox-only short-circuit (DB wins); got %s",
			logbuf.String())
	}
}

// TestServe_ConfigDeliveryMode_EmptyConfigUsesDB pins the default
// behavior: when config doesn't set delivery-mode (empty string), the
// DB column is used as-is. No override log fires.
func TestServe_ConfigDeliveryMode_EmptyConfigUsesDB(t *testing.T) {
	withSuccessfulDelivery(t)
	s, _ := store.Open(":memory:")
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	// Bob defaults to paste-and-enter from schema migration. No SetDeliveryMode call.

	_, _ = s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "hello",
	})

	opts := fastOpts("bob")
	opts.ConfigDeliveryMode = "" // no override

	stop, wait, logbuf := runServeInBackground(t, s, opts)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		d, _ := s.ListMessages(ctx, store.ListFilter{
			ToAgent: "bob", State: store.StateDelivered, Limit: 10,
		})
		if len(d) == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	stop()
	wait()

	if strings.Contains(logbuf.String(), "delivery_mode overridden by config") {
		t.Errorf("override log should NOT fire when config is empty; got %s",
			logbuf.String())
	}
}

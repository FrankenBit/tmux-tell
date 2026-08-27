package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// refuseInbound leaves exactly one StateRefused row addressed to `to` by
// filling the recipient queue to cap and tripping it.
func refuseInbound(t *testing.T, s *store.Store, from, to string, cap int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < cap; i++ {
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: from, ToAgent: to, Body: "filler", MaxRecipientQueue: cap,
		}); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	_, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: from, ToAgent: to, Body: "never arrived", MaxRecipientQueue: cap,
	})
	if !errors.Is(err, store.ErrRecipientQueueFull) {
		t.Fatalf("want ErrRecipientQueueFull, got %v", err)
	}
}

// TestInbox_RefusedBanner pins #933's discovery path: refused inbound appears
// in no default view, so without this line the only way to find a message that
// never reached you is to already suspect it exists.
func TestInbox_RefusedBanner(t *testing.T) {
	ctx := context.Background()

	t.Run("warns_on_the_default_queued_view", func(t *testing.T) {
		s := newCmdTestStore(t, "alice", "bob")
		refuseInbound(t, s, "alice", "bob", 2)

		var stdout, stderr bytes.Buffer
		if exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "text", &stdout, &stderr); exit != exitOK {
			t.Fatalf("exit = %d", exit)
		}
		if !strings.Contains(stderr.String(), "REFUSED") {
			t.Errorf("stderr = %q, want a refused-inbound warning", stderr.String())
		}
		// It must name the way OUT, not merely report a number — a warning
		// with no next step is the shape readers learn to skip.
		if !strings.Contains(stderr.String(), "--state refused") {
			t.Errorf("stderr = %q, want it to name `--state refused`", stderr.String())
		}
		// stdout stays the parseable table; the banner must not corrupt it.
		if strings.Contains(stdout.String(), "REFUSED") {
			t.Errorf("stdout carries the banner: %q — it belongs on stderr", stdout.String())
		}
	})

	t.Run("suppressed_when_already_viewing_refused", func(t *testing.T) {
		s := newCmdTestStore(t, "alice", "bob")
		refuseInbound(t, s, "alice", "bob", 2)

		var stdout, stderr bytes.Buffer
		if exit := runInboxWithStore(ctx, s, "bob", store.StateRefused, 100, false, "text", &stdout, &stderr); exit != exitOK {
			t.Fatalf("exit = %d", exit)
		}
		// Telling someone to run the command they just ran is noise.
		if strings.Contains(stderr.String(), "REFUSED") {
			t.Errorf("stderr = %q, want no banner when the caller asked for refused rows", stderr.String())
		}
		// …and the rows themselves must still be there.
		if !strings.Contains(stdout.String(), "never arrived") {
			t.Errorf("stdout = %q, want the refused row listed", stdout.String())
		}
	})

	t.Run("CONTROL_silent_when_nothing_was_refused", func(t *testing.T) {
		s := newCmdTestStore(t, "alice", "bob")
		if _, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "arrived fine",
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if exit := runInboxWithStore(ctx, s, "bob", store.StateQueued, 100, false, "text", &stdout, &stderr); exit != exitOK {
			t.Fatalf("exit = %d", exit)
		}
		// Without this arm the suite cannot fail in the world where the
		// banner prints unconditionally — and an always-on warning is the
		// failure mode this feature is most likely to have.
		if stderr.Len() != 0 {
			t.Errorf("stderr = %q, want empty when nothing was refused", stderr.String())
		}
	})
}

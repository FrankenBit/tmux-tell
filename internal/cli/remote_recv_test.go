package cli

import (
	"context"
	"encoding/json"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// TestDispatchRemoteRecv_InjectsIdentity is the receiver-side anchor: a
// forwarded send lands FROM the injected identity, not from any pane on the
// originating host. The SSH session has no $TMUX_PANE here, so without identity
// injection this would fail to resolve a sender entirely.
func TestDispatchRemoteRecv_InjectsIdentity(t *testing.T) {
	s := newCmdTestStore(t, "Admin", "bosun")

	result, err := dispatchRemoteRecv(context.Background(), s, "tmux-tell.send", "Admin",
		[]byte(`{"to":"bosun","body":"reverse-channel hi"}`))
	if err != nil {
		t.Fatalf("dispatchRemoteRecv: %v", err)
	}

	// The handler's structured result is returned intact (preserved by re-running
	// the actual handler, not by mapping to a text-emitting CLI subcommand).
	raw, _ := json.Marshal(result)
	got := map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result not a JSON object: %v (%s)", err, raw)
	}
	if got["ok"] != true {
		t.Fatalf("send result ok = %v, want true (%s)", got["ok"], raw)
	}

	// The message actually landed in the store FROM Admin → bosun.
	ctx := context.Background()
	msgs, err := s.ListMessages(ctx, store.ListFilter{ToAgent: "bosun"})
	if err != nil {
		t.Fatalf("inbox: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("bosun inbox = %d messages, want 1", len(msgs))
	}
	if msgs[0].FromAgent != "Admin" {
		t.Errorf("sender = %q, want Admin (injected identity)", msgs[0].FromAgent)
	}
	if msgs[0].Body != "reverse-channel hi" {
		t.Errorf("body = %q, want the forwarded body", msgs[0].Body)
	}
}

func TestDispatchRemoteRecv_UnknownToolFailsLoud(t *testing.T) {
	s := newCmdTestStore(t, "Admin")
	if _, err := dispatchRemoteRecv(context.Background(), s, "tmux-tell.nope", "Admin", []byte(`{}`)); err == nil {
		t.Fatal("dispatchRemoteRecv: want error for unknown tool, got nil")
	}
}

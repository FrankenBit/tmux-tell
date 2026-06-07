package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-msg/internal/store"
)

// seedMessage inserts a message from→to and drives it to the requested terminal
// (or in-flight) state, returning its public_id. State lifecycle is the real
// one: queued → ClaimNext(delivering) → MarkDelivered/MarkFailed.
func seedResendMsg(t *testing.T, s *store.Store, from, to, body string, state store.State) string {
	t.Helper()
	ctx := context.Background()
	r, err := s.InsertMessage(ctx, store.InsertParams{FromAgent: from, ToAgent: to, Body: body})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	switch state {
	case store.StateQueued:
		// leave as-is
	case store.StateDelivering:
		if _, err := s.ClaimNext(ctx, to); err != nil {
			t.Fatalf("claim: %v", err)
		}
	case store.StateDelivered:
		if _, err := s.ClaimNext(ctx, to); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.MarkDelivered(ctx, r.PublicID); err != nil {
			t.Fatalf("mark delivered: %v", err)
		}
	case store.StateFailed:
		if _, err := s.ClaimNext(ctx, to); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.MarkFailed(ctx, r.PublicID, "pane gone"); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	default:
		t.Fatalf("unhandled state %q", state)
	}
	return r.PublicID
}

// resendStore registers alice + bob and returns the store ready for resends.
func resendStore(t *testing.T) *store.Store {
	t.Helper()
	s := newCmdTestStore(t)
	ctx := context.Background()
	_ = s.UpsertAgent(ctx, "alice", "%1")
	_ = s.UpsertAgent(ctx, "bob", "%3")
	return s
}

func TestResend_FailedReplaysDirectly(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	orig := seedResendMsg(t, s, "alice", "bob", "the original body", store.StateFailed)

	var stdout, stderr bytes.Buffer
	exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Format: "json"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Replay == nil {
		t.Fatalf("resp = %+v, want ok + replay block", r)
	}
	if r.Replay.OriginalID != orig || r.Replay.OriginalState != string(store.StateFailed) {
		t.Errorf("replay = %+v, want original=%s state=failed", r.Replay, orig)
	}
	if r.ID == orig {
		t.Errorf("replay should be a NEW message id, not the original %s", orig)
	}
}

func TestResend_DeliveredRefusedWithoutForce(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	orig := seedResendMsg(t, s, "alice", "bob", "already delivered", store.StateDelivered)

	var stdout, stderr bytes.Buffer
	exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Format: "json"}, &stdout, &stderr)
	if exit != exitUnavailable {
		t.Fatalf("exit = %d, want exitUnavailable", exit)
	}
	r := decodeSend(t, stdout.Bytes())
	if r.OK || r.Replay == nil || r.Error == "" {
		t.Errorf("resp = %+v, want ok:false + replay + error", r)
	}
	if !strings.Contains(r.Error, "--force") {
		t.Errorf("error %q should point at --force", r.Error)
	}
}

func TestResend_DeliveredWithForceReplays(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	orig := seedResendMsg(t, s, "alice", "bob", "recover this unverified one", store.StateDelivered)

	var stdout, stderr bytes.Buffer
	exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Force: true, Format: "json"}, &stdout, &stderr)
	if exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	r := decodeSend(t, stdout.Bytes())
	if !r.OK || r.Replay == nil || !r.Replay.Forced {
		t.Errorf("resp = %+v / replay %+v, want ok + forced:true", r, r.Replay)
	}
}

func TestResend_InFlightRefusedWithoutForce(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	for _, st := range []store.State{store.StateQueued, store.StateDelivering} {
		orig := seedResendMsg(t, s, "alice", "bob", "in flight "+string(st), st)
		var stdout, stderr bytes.Buffer
		exit := runResendWithStore(context.Background(), s,
			resendParams{OriginalID: orig, Format: "json"}, &stdout, &stderr)
		if exit != exitUnavailable {
			t.Errorf("state %s: exit = %d, want exitUnavailable", st, exit)
		}
		if !strings.Contains(stdout.String(), "in flight") {
			t.Errorf("state %s: error should mention in-flight; got %s", st, stdout.String())
		}
	}
}

func TestResend_ReplayCarriesMarkerMetadata(t *testing.T) {
	// The replay row carries replay_of + replay_of_at so render shows the
	// marker; the body stays byte-identical to the original (PR2 dedupe).
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	orig := seedResendMsg(t, s, "alice", "bob", "byte-identical body", store.StateFailed)
	origMsg, _ := s.GetMessage(context.Background(), orig)

	var stdout, stderr bytes.Buffer
	if exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Format: "json"}, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d; stderr=%s", exit, stderr.String())
	}
	newID := decodeSend(t, stdout.Bytes()).ID
	replay, err := s.GetMessage(context.Background(), newID)
	if err != nil {
		t.Fatalf("get replay: %v", err)
	}
	if replay.Body != origMsg.Body {
		t.Errorf("replay body %q != original %q (must be byte-identical for PR2 dedupe)", replay.Body, origMsg.Body)
	}
	if !replay.ReplayOf.Valid || replay.ReplayOf.String != orig {
		t.Errorf("replay_of = %v, want %s", replay.ReplayOf, orig)
	}
	if !replay.ReplayOfAt.Valid || replay.ReplayOfAt.String != origMsg.CreatedAt {
		t.Errorf("replay_of_at = %v, want %s", replay.ReplayOfAt, origMsg.CreatedAt)
	}
}

func TestResend_UnknownID(t *testing.T) {
	s := resendStore(t)
	var stdout, stderr bytes.Buffer
	exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: "nope", Format: "json"}, &stdout, &stderr)
	if exit != exitDataErr {
		t.Errorf("exit = %d, want exitDataErr", exit)
	}
	if !strings.Contains(stdout.String()+stderr.String(), "unknown message id") {
		t.Errorf("missing 'unknown message id'; out=%s err=%s", stdout.String(), stderr.String())
	}
}

func TestResend_TextFormat(t *testing.T) {
	s := resendStore(t)
	withReachability(t, map[string]bool{"%3": true}, true)
	orig := seedResendMsg(t, s, "alice", "bob", "text replay", store.StateFailed)

	var stdout, stderr bytes.Buffer
	if exit := runResendWithStore(context.Background(), s,
		resendParams{OriginalID: orig, Format: "text"}, &stdout, &stderr); exit != exitOK {
		t.Fatalf("exit = %d", exit)
	}
	out := stdout.String()
	for _, want := range []string{"resent id=", "replay of " + orig} {
		if !strings.Contains(out, want) {
			t.Errorf("text output missing %q; got:\n%s", want, out)
		}
	}
}

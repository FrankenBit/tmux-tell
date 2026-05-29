package tmuxio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// recordedCall captures one invocation of tmuxRun for assertions.
type recordedCall struct {
	args  []string
	stdin string
}

// fakeRunner returns a tmuxRunner closure plus a slice that records every
// invocation. The provided handler can decide what each call returns; if
// nil, returns ("", nil) for every call.
func fakeRunner(handler func(args []string, stdin string) ([]byte, error)) (
	runner func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error),
	calls *[]recordedCall,
) {
	calls = &[]recordedCall{}
	runner = func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		var in string
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			in = string(b)
		}
		*calls = append(*calls, recordedCall{args: append([]string{}, args...), stdin: in})
		if handler == nil {
			return nil, nil
		}
		return handler(args, in)
	}
	return runner, calls
}

func withFakeRunner(t *testing.T, h func(args []string, stdin string) ([]byte, error)) *[]recordedCall {
	t.Helper()
	runner, calls := fakeRunner(h)
	prev := SetTmuxRunner(runner)
	t.Cleanup(func() { SetTmuxRunner(prev) })
	return calls
}

// shortRetries replaces the default retry backoff with near-zero waits so
// tests don't sleep 250 ms when they're meant to verify failure paths.
func shortRetries(t *testing.T) {
	t.Helper()
	prev := retryDelays
	retryDelays = []time.Duration{time.Microsecond, time.Microsecond}
	t.Cleanup(func() { retryDelays = prev })
}

func TestDeliver_HappyPath_PastesAndVerifies(t *testing.T) {
	calls := withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("...some preceding line...\nrendered body with id 7f3a marker\n"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3",
		Body: "rendered body with id 7f3a marker",
		VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Expect: load-buffer, paste-buffer, send-keys, capture-pane, [delete-buffer in defer].
	wantCmds := []string{"load-buffer", "paste-buffer", "send-keys", "capture-pane", "delete-buffer"}
	if len(*calls) < len(wantCmds) {
		t.Fatalf("got %d calls, want at least %d: %v", len(*calls), len(wantCmds), *calls)
	}
	for i, want := range wantCmds {
		if (*calls)[i].args[0] != want {
			t.Errorf("call %d = %q, want %q", i, (*calls)[i].args[0], want)
		}
	}
	// load-buffer stdin should carry the body.
	if (*calls)[0].stdin != "rendered body with id 7f3a marker" {
		t.Errorf("load-buffer stdin = %q", (*calls)[0].stdin)
	}
	// paste-buffer should target the right pane and use -d for buffer cleanup.
	pasteArgs := (*calls)[1].args
	if !contains(pasteArgs, "-t") || !contains(pasteArgs, "%3") {
		t.Errorf("paste-buffer not targeting %%3: %v", pasteArgs)
	}
	if !contains(pasteArgs, "-d") {
		t.Errorf("paste-buffer missing -d: %v", pasteArgs)
	}
}

func TestDeliver_UniqueBufferPerCall(t *testing.T) {
	calls := withFakeRunner(t, nil)
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})
	_ = Deliver(context.Background(), DeliverParams{Pane: "%1", Body: "x"})

	var bufNames []string
	for _, c := range *calls {
		if c.args[0] == "load-buffer" {
			for i, a := range c.args {
				if a == "-b" && i+1 < len(c.args) {
					bufNames = append(bufNames, c.args[i+1])
				}
			}
		}
	}
	if len(bufNames) != 2 {
		t.Fatalf("expected 2 load-buffer calls, got %d", len(bufNames))
	}
	if bufNames[0] == bufNames[1] {
		t.Errorf("buffer names collided: %s", bufNames[0])
	}
	if !strings.HasPrefix(bufNames[0], "inject-") {
		t.Errorf("buffer name should start with inject-, got %q", bufNames[0])
	}
}

func TestDeliver_VerifyRetriesOnMiss(t *testing.T) {
	shortRetries(t)
	captureCount := 0
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] != "capture-pane" {
			return nil, nil
		}
		captureCount++
		if captureCount < 3 {
			return []byte("nothing here yet"), nil
		}
		return []byte("eventually the token id 7f3a appears"), nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "id 7f3a",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if captureCount != 3 {
		t.Errorf("captureCount = %d, want 3 (1 initial + 2 retries)", captureCount)
	}
}

func TestDeliver_FailsAfterRetriesExhausted(t *testing.T) {
	shortRetries(t)
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "capture-pane" {
			return []byte("token never lands"), nil
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{
		Pane: "%3", Body: "x", VerifyToken: "missing",
	})
	if err == nil {
		t.Fatal("want error after retries exhausted")
	}
	if !strings.Contains(err.Error(), "verify token") {
		t.Errorf("error = %v, want mention of verify token", err)
	}
}

func TestDeliver_LoadBufferFailure(t *testing.T) {
	withFakeRunner(t, func(args []string, _ string) ([]byte, error) {
		if args[0] == "load-buffer" {
			return []byte("can't grab a lock on the buffer"), errors.New("exit 1")
		}
		return nil, nil
	})
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "load-buffer") {
		t.Errorf("err = %v, want load-buffer error", err)
	}
}

func TestDeliver_SkipsVerifyWhenTokenEmpty(t *testing.T) {
	calls := withFakeRunner(t, nil)
	err := Deliver(context.Background(), DeliverParams{Pane: "%3", Body: "x"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	for _, c := range *calls {
		if c.args[0] == "capture-pane" {
			t.Errorf("capture-pane should be skipped when VerifyToken=='', got call %v", c.args)
		}
	}
}

func TestDeliver_RequiresPaneAndBody(t *testing.T) {
	withFakeRunner(t, nil)
	if err := Deliver(context.Background(), DeliverParams{Body: "x"}); err == nil {
		t.Error("want error for empty pane")
	}
	if err := Deliver(context.Background(), DeliverParams{Pane: "%1"}); err == nil {
		t.Error("want error for empty body")
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Silence unused warnings when nothing else in this file uses fmt.
var _ = fmt.Sprintf

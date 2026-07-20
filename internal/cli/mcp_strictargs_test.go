package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDecodeStrictArgs exercises the #753 fail-loud decoder directly: unknown
// top-level params error (naming the key), the cc/bcc near-miss gets the
// to-array hint, valid + empty/absent args succeed, and malformed JSON errors.
func TestDecodeStrictArgs(t *testing.T) {
	type sample struct {
		To   string `json:"to"`
		Body string `json:"body"`
	}
	cases := []struct {
		name    string
		args    string
		wantErr bool
		errHas  []string
		wantTo  string
	}{
		{name: "valid", args: `{"to":"bosun","body":"hi"}`, wantTo: "bosun"},
		{name: "unknown key is named", args: `{"to":"bosun","body":"hi","broadcast":true}`,
			wantErr: true, errHas: []string{`unknown parameter "broadcast"`, "tmux-tell.test"}},
		{name: "cc gets to-array hint", args: `{"cc":"surveyor"}`,
			wantErr: true, errHas: []string{`unknown parameter "cc"`, `"to" as an array`, "#158"}},
		{name: "bcc gets to-array hint", args: `{"bcc":"x"}`,
			wantErr: true, errHas: []string{`unknown parameter "bcc"`, `"to" as an array`}},
		{name: "empty args ok (zero value)", args: ``, wantTo: ""},
		{name: "empty object ok", args: `{}`, wantTo: ""},
		{name: "malformed json errors", args: `{"to":`,
			wantErr: true, errHas: []string{"tmux-tell.test", "invalid args"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var in sample
			err := decodeStrictArgs("tmux-tell.test", json.RawMessage(c.args), &in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (in=%+v)", in)
				}
				for _, sub := range c.errHas {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if in.To != c.wantTo {
				t.Errorf("To = %q, want %q", in.To, c.wantTo)
			}
		})
	}
}

// TestUnknownFieldName_MatchesStdlib pins the extraction against the REAL error
// encoding/json's DisallowUnknownFields produces. The stdlib exposes the offending
// field name only through this string, so if a future Go changes the wording this
// test reds — rather than the fail-loud message silently degrading to the raw error.
func TestUnknownFieldName_MatchesStdlib(t *testing.T) {
	var dst struct {
		Known string `json:"known"`
	}
	dec := json.NewDecoder(strings.NewReader(`{"mystery":1}`))
	dec.DisallowUnknownFields()
	err := dec.Decode(&dst)
	if err == nil {
		t.Fatal("expected a DisallowUnknownFields error, got nil")
	}
	name, ok := unknownFieldName(err)
	if !ok {
		t.Fatalf("unknownFieldName did not match stdlib error %q — the format may have changed", err.Error())
	}
	if name != "mystery" {
		t.Errorf("field = %q, want %q (from %q)", name, "mystery", err.Error())
	}
}

// TestMCP_Send_UnknownParam_FailsLoud is the #753 anchor: the exact incident —
// a `cc` param on send — must now return ok:false naming the key, not the
// pre-fix ok:true silent drop. Mutation check: revert the send site to
// json.Unmarshal and this reds (got["ok"]==true, no _isError).
func TestMCP_Send_UnknownParam_FailsLoud(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":   "bob",
		"body": "hi",
		"cc":   "surveyor", // does not exist — fan-out is `to` as an array (#158)
	})
	if got["_isError"] != true {
		t.Fatalf("send with unknown param `cc` must fail loud (ok:false), got=%v", got)
	}
	text, _ := got["_text"].(string)
	for _, sub := range []string{`unknown parameter "cc"`, `"to" as an array`, "#158"} {
		if !strings.Contains(text, sub) {
			t.Errorf("error %q missing %q", text, sub)
		}
	}
}

// TestMCP_Ask_UnknownParam_FailsLoud confirms the strict decode is per-tool:
// `priority` is a valid SEND param but not an ASK param, so ask must reject it.
func TestMCP_Ask_UnknownParam_FailsLoud(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "tmux-tell.ask", map[string]any{
		"to":       "bob",
		"body":     "q?",
		"priority": "high", // valid on send, NOT on ask
	})
	if got["_isError"] != true {
		t.Fatalf("ask with unknown param must fail loud, got=%v", got)
	}
	text, _ := got["_text"].(string)
	if !strings.Contains(text, `unknown parameter "priority"`) {
		t.Errorf("error %q should name the unknown key priority", text)
	}
}

// TestMCP_Send_ValidOptionalParams_StillOk guards the other direction: strict
// decode must NOT reject documented optional params. An incomplete input struct
// would newly-break valid calls — this is the completeness canary for send.
func TestMCP_Send_ValidOptionalParams_StillOk(t *testing.T) {
	t.Setenv("TMUX_AGENT_NAME", "alice")
	s := newCmdTestStore(t, "alice", "bob")

	got := callMCPTool(t, s, "tmux-tell.send", map[string]any{
		"to":                "bob",
		"body":              "hi",
		"quick":             true,
		"no_reply_expected": true,
		"priority":          "high",
		"expects_reply":     false,
	})
	if got["ok"] != true {
		t.Errorf("valid optional params must still succeed; got=%v", got)
	}
}

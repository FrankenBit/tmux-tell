package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"git.frankenbit.de/frankenbit/tmux-tell/internal/config"
	"git.frankenbit.de/frankenbit/tmux-tell/internal/store"
)

// seedTwoMessages inserts two messages addressed alice→bob and
// alice→carol, returning the two IDs. Used by the prefix-disambiguation
// tests so the two IDs may or may not share a prefix.
func seedGetFixture(t *testing.T, s *store.Store) (id1, id2 string) {
	t.Helper()
	ctx := context.Background()
	r1, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "bob", Body: "first",
	})
	if err != nil {
		t.Fatalf("seed 1: %v", err)
	}
	r2, err := s.InsertMessage(ctx, store.InsertParams{
		FromAgent: "alice", ToAgent: "carol", Body: "second",
	})
	if err != nil {
		t.Fatalf("seed 2: %v", err)
	}
	return r1.PublicID, r2.PublicID
}

func TestGet_DoGet_SenderCanFetch(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id, _ := seedGetFixture(t, s)

	res, err := doGet(context.Background(), s, nil, "alice", id)
	if err != nil {
		t.Fatalf("sender access denied: %v", err)
	}
	if res.ID != id {
		t.Errorf("ID = %q, want %q", res.ID, id)
	}
	if res.Body != "first" {
		t.Errorf("Body = %q, want %q", res.Body, "first")
	}
}

func TestGet_DoGet_RecipientCanFetch(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id, _ := seedGetFixture(t, s)

	res, err := doGet(context.Background(), s, nil, "bob", id)
	if err != nil {
		t.Fatalf("recipient access denied: %v", err)
	}
	if res.From != "alice" || res.To != "bob" {
		t.Errorf("From/To = %q/%q, want alice/bob", res.From, res.To)
	}
}

func TestGet_DoGet_UnrelatedAgentCannotFetch(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	idBob, _ := seedGetFixture(t, s)

	// carol is unrelated to the alice→bob message.
	_, err := doGet(context.Background(), s, nil, "carol", idBob)
	if !errors.Is(err, errGetNotFound) {
		t.Errorf("err = %v, want errGetNotFound (unrelated → indistinguishable from not-found)", err)
	}
}

func TestGet_DoGet_UnknownIDReturnsSameErrorClassAsUnauthorized(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	seedGetFixture(t, s)

	// alice asks for a definitely-nonexistent ID.
	_, errUnknown := doGet(context.Background(), s, nil, "alice", "ffff")
	if !errors.Is(errUnknown, errGetNotFound) {
		t.Errorf("unknown ID err = %v, want errGetNotFound", errUnknown)
	}

	// carol (unrelated) asks for a real ID — should produce the same
	// error class. The no-existence-leak invariant.
	s2 := newCmdTestStore(t, "alice", "bob", "carol")
	idBob, _ := seedGetFixture(t, s2)
	_, errUnrelated := doGet(context.Background(), s2, nil, "carol", idBob)
	if !errors.Is(errUnrelated, errGetNotFound) {
		t.Errorf("unauthorized err = %v, want errGetNotFound", errUnrelated)
	}
}

func TestGet_DoGet_PrivilegedAgentCanFetch(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "quartermaster")
	idBob, _ := seedGetFixture(t, s)

	// Without privileged config, quartermaster (unrelated) is denied.
	_, err := doGet(context.Background(), s, nil, "quartermaster", idBob)
	if !errors.Is(err, errGetNotFound) {
		t.Fatalf("baseline: expected errGetNotFound for unrelated agent, got %v", err)
	}

	// With privileged config, quartermaster is granted.
	cfg := &config.File{PrivilegedAgents: []string{"quartermaster"}}
	res, err := doGet(context.Background(), s, cfg, "quartermaster", idBob)
	if err != nil {
		t.Errorf("privileged: expected access, got %v", err)
	}
	if res != nil && res.Body != "first" {
		t.Errorf("privileged: body = %q, want %q", res.Body, "first")
	}
}

func TestGet_DoGet_ShortPrefixHappyPath(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id, other := seedGetFixture(t, s)

	// Use the SHORTEST prefix of `id` that the only other seeded message does
	// not share — a genuinely-unique short prefix, so this still exercises
	// prefix-matching but is deterministic. The prior fixed 2-char prefix flaked
	// when the two random 4-hex public IDs happened to share their first 2 chars
	// (#479: "ambiguous prefix: N messages match …").
	prefix := id
	for n := 1; n <= len(id); n++ {
		if id[:n] != other[:n] {
			prefix = id[:n]
			break
		}
	}
	res, err := doGet(context.Background(), s, nil, "alice", prefix)
	if err != nil {
		t.Fatalf("prefix lookup: %v", err)
	}
	if res.ID != id {
		t.Errorf("ID = %q, want %q (full ID for prefix %q)", res.ID, id, prefix)
	}
}

func TestGet_DoGet_AmbiguousPrefixReturnsDisambiguationError(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob", "carol")
	ctx := context.Background()

	// Seed many messages so SOME pair shares a 1-char prefix. Public IDs
	// are 4-char hex; with ~20 messages the chance of a shared first-char
	// is essentially 1.
	var ids []string
	for i := 0; i < 20; i++ {
		r, err := s.InsertMessage(ctx, store.InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "msg",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		ids = append(ids, r.PublicID)
	}

	// Find a prefix that matches >=2 IDs.
	prefixCount := map[string]int{}
	for _, id := range ids {
		prefixCount[id[:1]]++
	}
	var sharedPrefix string
	for p, n := range prefixCount {
		if n >= 2 {
			sharedPrefix = p
			break
		}
	}
	if sharedPrefix == "" {
		t.Skip("could not synthesize a shared prefix in 20 IDs; rare RNG outcome")
	}

	_, err := doGet(ctx, s, nil, "alice", sharedPrefix)
	if !errors.Is(err, errGetAmbiguous) {
		t.Errorf("err = %v, want errGetAmbiguous for prefix %q matching %d IDs",
			err, sharedPrefix, prefixCount[sharedPrefix])
	}
	// Body should list the matching IDs so the operator can disambiguate.
	if !strings.Contains(err.Error(), sharedPrefix) {
		t.Errorf("err body %q should reference the ambiguous prefix", err.Error())
	}
}

func TestGet_CLI_JSON_HappyPath(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	id, _ := seedGetFixture(t, s)
	t.Setenv("CLAUDE_MSG_DB", ":memory:")
	t.Setenv("TMUX_AGENT_NAME", "alice")

	// Inject the store explicitly via direct doGet path; the CLI's
	// store.Open would point at the env-var DB which doesn't carry our
	// seeds. This test exercises the JSON-render path on doGet's output.
	res, err := doGet(context.Background(), s, nil, "alice", id)
	if err != nil {
		t.Fatalf("doGet: %v", err)
	}
	var stdout bytes.Buffer
	if err := writeJSONResult(&stdout, res); err != nil {
		t.Fatalf("writeJSONResult: %v", err)
	}
	got := parseJSONResult(t, stdout.Bytes())
	if got["ok"] != true || got["id"] != id {
		t.Errorf("got %v", got)
	}
	if got["body"] != "first" {
		t.Errorf("body = %v, want first", got["body"])
	}
}

// TestGet_DoGet_LikeWildcardEscape pins the LIKE-injection guard added
// per Surveyor's PR #128 S1: a literal `%` (or `_`) in the prefix must
// match LITERALLY, not as a SQL LIKE wildcard. Without the escape, a
// privileged agent calling `get %` (or any other LIKE wildcard) could
// enumerate every message they're authorized to see. With the escape
// + ESCAPE clause, `%` matches only public IDs that LITERALLY start
// with `%` — and since real public IDs are 4-char hex, that set is
// always empty.
func TestGet_DoGet_LikeWildcardEscape(t *testing.T) {
	s := newCmdTestStore(t, "alice", "bob")
	// Seed several messages so a hypothetical `%`-as-wildcard would
	// match all of them.
	for i := 0; i < 5; i++ {
		if _, err := s.InsertMessage(context.Background(), store.InsertParams{
			FromAgent: "alice", ToAgent: "bob", Body: "msg",
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// alice (sender on all 5) requests prefix `%`. With proper escape,
	// `%` matches NO real ID → errGetNotFound. Without escape, `%`
	// would match all 5 → errGetAmbiguous listing every ID (enumeration
	// backdoor).
	_, err := doGet(context.Background(), s, nil, "alice", "%")
	if !errors.Is(err, errGetNotFound) {
		t.Errorf("prefix `%%` err = %v, want errGetNotFound (LIKE wildcard must be escaped)", err)
	}
	// Same check for `_`.
	_, err = doGet(context.Background(), s, nil, "alice", "_")
	if !errors.Is(err, errGetNotFound) {
		t.Errorf("prefix `_` err = %v, want errGetNotFound (LIKE wildcard must be escaped)", err)
	}
}

func TestConfig_IsPrivileged(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.File
		want bool
	}{
		{"nil cfg", nil, false},
		{"empty list", &config.File{}, false},
		{"exact match", &config.File{PrivilegedAgents: []string{"bosun", "quartermaster"}}, true},
		{"no match", &config.File{PrivilegedAgents: []string{"bosun"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.cfg.IsPrivileged("quartermaster")
			if got != c.want {
				t.Errorf("IsPrivileged(quartermaster) = %v, want %v", got, c.want)
			}
		})
	}
	// Empty-string requester always returns false.
	cfg := &config.File{PrivilegedAgents: []string{""}}
	if cfg.IsPrivileged("") {
		t.Errorf("IsPrivileged(\"\") should return false even when \"\" is in list")
	}
}

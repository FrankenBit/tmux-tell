package config

import "testing"

// TestResolveString_PrecedenceChain pins the standard precedence chain
// shape for string fields (#132 — delivery-mode is the first string
// knob; ResolveString sister to ResolveBool / ResolveDuration).
// Same shape: per-agent block > defaults block > hardcoded fallback.
func TestResolveString_PrecedenceChain(t *testing.T) {
	mode := func(s string) *string { return &s }
	cases := []struct {
		name      string
		file      *File
		hardcoded string
		want      string
	}{
		{"nil file falls to hardcoded", nil, "paste-and-enter", "paste-and-enter"},
		{
			"empty file falls to hardcoded",
			&File{},
			"paste-and-enter",
			"paste-and-enter",
		},
		{
			"defaults block wins over hardcoded",
			&File{Defaults: Block{DeliveryMode: mode("mailbox-only")}},
			"paste-and-enter",
			"mailbox-only",
		},
		{
			"per-agent block wins over defaults",
			&File{
				Defaults: Block{DeliveryMode: mode("paste-and-enter")},
				Agent: map[string]Block{
					"operator": {DeliveryMode: mode("mailbox-only")},
				},
			},
			"paste-and-enter",
			"mailbox-only",
		},
		{
			"empty string at per-agent falls through to defaults",
			&File{
				Defaults: Block{DeliveryMode: mode("mailbox-only")},
				Agent: map[string]Block{
					"operator": {DeliveryMode: mode("")},
				},
			},
			"paste-and-enter",
			"mailbox-only",
		},
		{
			"non-matching agent falls through to defaults",
			&File{
				Defaults: Block{DeliveryMode: mode("mailbox-only")},
				Agent: map[string]Block{
					"someone-else": {DeliveryMode: mode("paste-and-enter")},
				},
			},
			"paste-and-enter",
			"mailbox-only",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveString(c.file, "operator", "delivery-mode", c.hardcoded)
			if got != c.want {
				t.Errorf("ResolveString = %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveString_UnknownField returns the hardcoded fallback —
// blockStringField only knows about registered fields.
func TestResolveString_UnknownField(t *testing.T) {
	file := &File{
		Defaults: Block{DeliveryMode: stringPtr("mailbox-only")},
	}
	got := ResolveString(file, "operator", "unknown-string-field", "fallback")
	if got != "fallback" {
		t.Errorf("ResolveString(unknown) = %q, want fallback", got)
	}
}

func stringPtr(s string) *string { return &s }

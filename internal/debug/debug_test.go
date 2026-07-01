package debug

import "testing"

func TestEnabled(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"1", true},
		{"true", true},
		{"0", true},   // any non-empty value is on — the gate is presence, not truthiness
		{"off", true}, // ditto: documented as "any non-empty = on", so even "off" enables
	}
	for _, c := range cases {
		t.Setenv(EnvVar, c.val)
		if got := Enabled(); got != c.want {
			t.Errorf("Enabled() with %s=%q = %v, want %v", EnvVar, c.val, got, c.want)
		}
	}
}

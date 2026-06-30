package control

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateForTask_AcceptsIssueRefShapes pins that the #286 target-task
// labels the feature is designed to carry all pass: a bare project#issue, a
// cross-repo owner/project#issue, and a short human descriptor with spaces.
func TestValidateForTask_AcceptsIssueRefShapes(t *testing.T) {
	valid := []string{
		"tmux-tell#286",
		"frankenbit/tmux-tell#286",
		"alcatraz-infra#89",
		"e_train#1",
		"v1.0-cut",
		"rescue tmux-tell#286",
		"A", // single alnum is the minimal legal label
	}
	for _, s := range valid {
		if err := ValidateForTask(s); err != nil {
			t.Errorf("ValidateForTask(%q) = %v, want nil", s, err)
		}
	}
}

// TestValidateForTask_RejectsUnsafe is the load-bearing safety test: every
// case that could turn the pasted `/rename <Chamber> <task>` line into a
// second, caller-chosen command — or is otherwise malformed — must be
// rejected with ErrForTaskInvalid. The newline/carriage-return cases are the
// injection-critical ones (a second line could be its own slash-command); the
// leading-slash case forecloses the label itself reading as a command.
func TestValidateForTask_RejectsUnsafe(t *testing.T) {
	bad := map[string]string{
		"empty":          "",
		"newline-inject": "x\n/clear",
		"crlf-inject":    "x\r\n/quit",
		"bare-newline":   "tmux-tell#286\n",
		"tab":            "x\ty",
		"leading-slash":  "/rename evil",
		"leading-space":  " tmux-tell#286",
		"leading-hash":   "#286",  // must start alphanumeric
		"semicolon":      "a;b",   // shell-ish punctuation outside allowlist
		"backtick":       "a`b`",  // ditto
		"dollar":         "a$(x)", // ditto
		"pipe":           "a|b",   // ditto
		"quote":          `a"b`,   // ditto
	}
	for name, s := range bad {
		t.Run(name, func(t *testing.T) {
			err := ValidateForTask(s)
			if err == nil {
				t.Fatalf("ValidateForTask(%q) = nil, want ErrForTaskInvalid", s)
			}
			if !errors.Is(err, ErrForTaskInvalid) {
				t.Errorf("ValidateForTask(%q) error %v, want wrapped ErrForTaskInvalid", s, err)
			}
		})
	}
}

// TestValidateForTask_LengthCap pins the exact boundary: a label at the cap is
// accepted, one byte over is rejected. The cap doubles as a paste-safety bound.
func TestValidateForTask_LengthCap(t *testing.T) {
	atCap := "a" + strings.Repeat("b", maxForTaskLen-1) // exactly maxForTaskLen bytes
	if len(atCap) != maxForTaskLen {
		t.Fatalf("test setup: len(atCap)=%d, want %d", len(atCap), maxForTaskLen)
	}
	if err := ValidateForTask(atCap); err != nil {
		t.Errorf("ValidateForTask(at cap, %d bytes) = %v, want nil", len(atCap), err)
	}
	overCap := atCap + "c"
	if err := ValidateForTask(overCap); err == nil || !errors.Is(err, ErrForTaskInvalid) {
		t.Errorf("ValidateForTask(over cap, %d bytes) = %v, want ErrForTaskInvalid", len(overCap), err)
	}
}

package main

import (
	"flag"
	"strings"
)

// reorderFlagsFirst pulls all flag tokens to the front of args and
// positional tokens to the back, so a subsequent fs.Parse() picks up
// flags regardless of operator typing order.
//
// Closes the #44 trap: Go's flag.Parse stops at the first non-flag
// positional, which silently drops every flag after it. Operator
// typing `tmux-msg-claude control alice --command compact` (natural English
// order — recipient first) had `--command` dropped because `alice` was
// a positional. With this helper, `alice` slides to the back and
// `--command compact` parses correctly.
//
// Handles:
//   - `--flag value` (separate value, looked up to see if flag takes one)
//   - `--flag=value` (bundled, single token)
//   - `-f value` / `-f=value` (single-dash short forms)
//   - bool flags (`--quiet-disabled`, `--confirm` — no value to swallow)
//   - the `--` terminator (everything after stays in order at the back)
//
// fs is consulted for flag definitions (via Lookup) so the helper
// knows whether a flag swallows the next token as its value. Call
// reorderFlagsFirst AFTER defining flags on fs but BEFORE calling
// fs.Parse.
func reorderFlagsFirst(fs *flag.FlagSet, args []string) []string {
	flagTokens := []string{}
	positionalTokens := []string{}
	i := 0
	for i < len(args) {
		tok := args[i]
		// "--" terminator: by Go convention, everything after `--` is
		// positional. Preserve order: dump remainder into positionals.
		if tok == "--" {
			positionalTokens = append(positionalTokens, args[i+1:]...)
			break
		}
		if isFlagToken(tok) {
			flagTokens = append(flagTokens, tok)
			// `--flag=value` or `-f=value`: value bundled in the same
			// token. No follow-up value to swallow.
			if strings.Contains(tok, "=") {
				i++
				continue
			}
			// Plain `--flag` or `-f`: check if the FlagSet defines it
			// as a non-bool — if so, swallow the next token as its
			// value.
			name := strings.TrimLeft(tok, "-")
			f := fs.Lookup(name)
			if f != nil && !isBoolFlag(f) && i+1 < len(args) {
				i++
				flagTokens = append(flagTokens, args[i])
			}
			i++
		} else {
			positionalTokens = append(positionalTokens, tok)
			i++
		}
	}
	return append(flagTokens, positionalTokens...)
}

// isFlagToken returns true for tokens that look like Go-style flags:
// `--name`, `--name=value`, `-n`, `-n=value`. A bare `-` is treated as
// a positional (stdin convention in many CLIs); we follow that.
func isFlagToken(tok string) bool {
	return len(tok) > 1 && strings.HasPrefix(tok, "-")
}

// isBoolFlag interrogates a *flag.Flag for the IsBoolFlag() method
// the standard library's bool-flag implementation exposes. Returns
// true for `fs.Bool(...)`-style flags; false for everything else.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

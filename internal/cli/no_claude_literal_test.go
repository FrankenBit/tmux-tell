package cli

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoClaudeLiteralInCLISource guards that no non-test source file in
// internal/cli contains a hardcoded "tmux-msg-claude" string literal outside
// profile.go. Contributors adding new usage hints or error messages must route
// through active.BinaryName so the codex adapter (and future adapters) name
// themselves correctly (#280, #315, #324).
//
// Comments are excluded — the AST walk visits only *ast.BasicLit STRING nodes.
// _test.go files are not scanned (test assertions may reference the literal).
// profile.go is the one allowed source-of-truth for the BinaryName default.
func TestNoClaudeLiteralInCLISource(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)

	files, err := filepath.Glob(filepath.Join(pkgDir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", pkgDir, err)
	}
	if len(files) == 0 {
		t.Fatal("no .go files found — pkgDir may be wrong")
	}

	const target = "tmux-msg-claude"
	const allowFile = "profile.go"

	fset := token.NewFileSet()
	var violations []string
	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") || base == allowFile {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", base, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(lit.Value, target) {
				pos := fset.Position(lit.Pos())
				violations = append(violations, fmt.Sprintf("  %s:%d: %s", pos.Filename, pos.Line, lit.Value))
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf("hardcoded %q string literal in internal/cli outside profile.go"+
			" — use active.BinaryName instead:\n%s",
			target, strings.Join(violations, "\n"))
	}
}

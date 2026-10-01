package tmux

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
)

func TestProductionStartOpsCarryRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	p := NewProviderWithConfig(Config{RuntimeDir: dir})
	ops := p.startOps()
	if ops.runtimeDir != dir {
		t.Fatalf("startOps runtimeDir = %q, want %q", ops.runtimeDir, dir)
	}
	if got, want := citylayout.SessionDiagnosticsDirForRuntimeDir(ops.runtimeDir), filepath.Join(dir, "sessions"); got != want {
		t.Fatalf("session diagnostics dir = %q, want %q", got, want)
	}
}

// Keep construction centralized: every start path must inherit Provider cfg.
func TestTmuxStartOpsHasOneProductionConstruction(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				if n == nil {
					return true
				}
				u, ok := n.(*ast.UnaryExpr)
				if !ok || u.Op != token.AND {
					return true
				}
				lit, ok := u.X.(*ast.CompositeLit)
				if !ok {
					return true
				}
				id, ok := lit.Type.(*ast.Ident)
				if ok && id.Name == "tmuxStartOps" {
					sites = append(sites, fn.Name.Name)
				}
				return true
			})
		}
	}
	if len(sites) != 1 || sites[0] != "startOps" {
		t.Fatalf("tmuxStartOps constructions = %v, want only startOps", sites)
	}
}

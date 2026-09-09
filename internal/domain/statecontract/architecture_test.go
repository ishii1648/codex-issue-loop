package statecontract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSnapshotContractHasNoExternalObservationDependencies(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	allowedDomain := map[string]bool{
		"github.com/ishii1648/codex-issue-loop/internal/domain/issue":       true,
		"github.com/ishii1648/codex-issue-loop/internal/domain/publication": true,
		"github.com/ishii1648/codex-issue-loop/internal/domain/queue":       true,
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(name, ".") && !allowedDomain[name] || name == "os" || strings.HasPrefix(name, "os/") || name == "net" || strings.HasPrefix(name, "net/") || name == "syscall" || name == "crypto/rand" {
				t.Errorf("%s imports an observation or undeclared contract dependency %q", path, name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			owner, ok := selector.X.(*ast.Ident)
			if ok && owner.Name == "time" && (selector.Sel.Name == "Now" || selector.Sel.Name == "Sleep" || selector.Sel.Name == "After" || selector.Sel.Name == "NewTimer" || selector.Sel.Name == "NewTicker") {
				t.Errorf("%s observes time inside the contract", path)
			}
			return true
		})
	}
}

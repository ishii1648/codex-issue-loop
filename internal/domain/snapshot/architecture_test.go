package snapshot

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
	allowed := map[string]bool{}
	for _, name := range []string{"crypto/sha256", "encoding/hex", "encoding/json", "errors", "fmt", "sort", "strconv", "strings", "time"} {
		allowed[name] = true
	}
	for _, name := range []string{"issue", "publication", "queue", "statecontract"} {
		allowed["github.com/ishii1648/codex-issue-loop/internal/domain/"+name] = true
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed[path] {
				t.Errorf("%s imports %s outside the contract dependency paths", file, path)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name, ok := selector.X.(*ast.Ident)
			if ok && name.Name == "time" && (selector.Sel.Name == "Now" || selector.Sel.Name == "Sleep" || selector.Sel.Name == "After" || selector.Sel.Name == "NewTimer" || selector.Sel.Name == "NewTicker") {
				t.Errorf("%s observes an external clock", file)
			}
			return true
		})
	}
}

package supervisor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Status commits are centralized so persistence closures cannot quietly grow
// new lifecycle policy. Named domain decisions use applyIssueTransition; paths
// still awaiting extraction go through the explicit compatibility boundary.
func TestIssueStatusAssignmentsUseTransitionBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	dir := filepath.Dir(currentFile)
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	issueNames := map[string]bool{"item": true, "current": true, "issue": true, "scheduled": true}
	for _, path := range files {
		base := filepath.Base(path)
		if strings.HasSuffix(base, "_test.go") || base == "issue_transition.go" {
			continue
		}
		fset := token.NewFileSet()
		parsed, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, expression := range assignment.Lhs {
				selector, ok := expression.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Status" {
					continue
				}
				identifier, direct := selector.X.(*ast.Ident)
				if direct && issueNames[identifier.Name] {
					position := fset.Position(selector.Pos())
					t.Errorf("%s:%d assigns Issue.Status outside the transition boundary", base, position.Line)
				}
			}
			return true
		})
	}
}

package supervisor

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/go/packages"
)

const statePackagePath = "github.com/ishii1648/codex-issue-loop/internal/state"

// TestIssueStatusAssignmentsStayWithinKnownBoundaries uses type information,
// rather than receiver variable names, to find every production assignment to
// state.Issue.Status in app, state, and supervisor. The compatibility paths are
// counted explicitly so a new raw assignment fails this test. Each listed path
// is removed when that lifecycle flow gains a named domain decision.
func TestIssueStatusAssignmentsStayWithinKnownBoundaries(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	config := &packages.Config{
		Dir: repoRoot,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
	}
	loaded, err := packages.Load(config, "./internal/app", "./internal/state", "./internal/supervisor")
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range loaded {
		for _, packageErr := range pkg.Errors {
			t.Errorf("load %s: %v", pkg.PkgPath, packageErr)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	allowed := map[string]int{
		"internal/supervisor/issue_transition.go":     2, // named decision and compatibility commit boundaries
		"internal/app/app.go":                         3, // answer, conflict retry, environment resume CLI paths
		"internal/app/answered_workspace_recovery.go": 1,
		"internal/app/checks_recovery.go":             2,
		"internal/app/merged_pr_adoption.go":          1,
		"internal/app/publication_recovery.go":        1,
		"internal/state/lease.go":                     1, // atomic claim reservation; decision extraction pending
	}
	seen := map[string]int{}
	for _, pkg := range loaded {
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, expression := range assignment.Lhs {
					selector, ok := expression.(*ast.SelectorExpr)
					if !ok || !isIssueStatusSelector(pkg.TypesInfo, selector) {
						continue
					}
					position := pkg.Fset.Position(selector.Pos())
					relative, relErr := filepath.Rel(repoRoot, position.Filename)
					if relErr != nil {
						t.Errorf("resolve assignment path %s: %v", position.Filename, relErr)
						continue
					}
					relative = filepath.ToSlash(relative)
					if _, known := allowed[relative]; !known {
						t.Errorf("%s:%d assigns state.Issue.Status outside a known transition boundary", relative, position.Line)
					}
					seen[relative]++
				}
				return true
			})
		}
	}
	for path, want := range allowed {
		if got := seen[path]; got != want {
			t.Errorf("%s has %d state.Issue.Status assignments; want %d until its compatibility paths are migrated", path, got, want)
		}
	}
}

func isIssueStatusSelector(info *types.Info, selector *ast.SelectorExpr) bool {
	selection := info.Selections[selector]
	if selection == nil || selection.Obj().Name() != "Status" {
		return false
	}
	receiver := selection.Recv()
	if pointer, ok := receiver.(*types.Pointer); ok {
		receiver = pointer.Elem()
	}
	named, ok := receiver.(*types.Named)
	return ok && named.Obj().Name() == "Issue" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == statePackagePath
}

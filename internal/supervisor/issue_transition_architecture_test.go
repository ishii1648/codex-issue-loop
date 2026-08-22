package supervisor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

const statePackagePath = "github.com/ishii1648/codex-issue-loop/internal/state"

// TestIssueStatusAssignmentsStayWithinKnownBoundaries uses type information,
// rather than receiver variable names, to find every production assignment to
// state.Issue.Status in app, state, and supervisor. Raw writes are confined to
// the state commit boundary, so application transaction closures cannot add
// lifecycle policy without a domain decision or the validated compatibility
// API.
func TestIssueStatusAssignmentsStayWithinKnownBoundaries(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	loaded := loadTypedProductionPackages(t, repoRoot,
		"./internal/app", "./internal/state", "./internal/supervisor")

	allowed := map[string]int{
		"internal/state/issue_transition.go": 2, // named decision and compatibility commit boundaries
	}
	seen := map[string]int{}
	for _, pkg := range loaded {
		for _, file := range pkg.syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				assignment, ok := node.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, expression := range assignment.Lhs {
					selector, ok := expression.(*ast.SelectorExpr)
					if !ok || !isIssueStatusSelector(pkg.info, selector) {
						continue
					}
					position := pkg.fset.Position(selector.Pos())
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
			t.Errorf("%s has %d state.Issue.Status assignments; want %d at the central commit boundary", path, got, want)
		}
	}
}

type listedPackage struct {
	Dir        string
	ImportPath string
	Export     string
	GoFiles    []string
}

type typedProductionPackage struct {
	fset   *token.FileSet
	syntax []*ast.File
	info   *types.Info
}

// loadTypedProductionPackages asks the active Go toolchain for module-aware
// package metadata and export files, then performs the source type check with
// the standard library. This avoids coupling the architecture guard to a
// golang.org/x/tools version tied to one Go release.
func loadTypedProductionPackages(t *testing.T, repoRoot string, patterns ...string) []typedProductionPackage {
	t.Helper()
	args := append([]string{"list", "-json", "-export", "-deps"}, patterns...)
	command := exec.Command("go", args...)
	command.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("go list production packages: %v\n%s", err, stderr.String())
	}

	all := map[string]listedPackage{}
	decoder := json.NewDecoder(&stdout)
	for {
		var listed listedPackage
		err := decoder.Decode(&listed)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		all[listed.ImportPath] = listed
	}

	wanted := []string{
		"github.com/ishii1648/codex-issue-loop/internal/app",
		statePackagePath,
		"github.com/ishii1648/codex-issue-loop/internal/supervisor",
	}
	loaded := make([]typedProductionPackage, 0, len(wanted))
	for _, importPath := range wanted {
		listed, ok := all[importPath]
		if !ok {
			t.Fatalf("go list omitted %s", importPath)
		}
		fset := token.NewFileSet()
		syntax := make([]*ast.File, 0, len(listed.GoFiles))
		for _, name := range listed.GoFiles {
			file, err := parser.ParseFile(fset, filepath.Join(listed.Dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			syntax = append(syntax, file)
		}
		info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
		lookup := func(path string) (io.ReadCloser, error) {
			dependency, ok := all[path]
			if !ok || dependency.Export == "" {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(dependency.Export)
		}
		checker := types.Config{Importer: importer.ForCompiler(fset, runtime.Compiler, lookup)}
		if _, err := checker.Check(importPath, fset, syntax, info); err != nil {
			t.Fatalf("type-check %s: %v", importPath, err)
		}
		loaded = append(loaded, typedProductionPackage{fset: fset, syntax: syntax, info: info})
	}
	return loaded
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

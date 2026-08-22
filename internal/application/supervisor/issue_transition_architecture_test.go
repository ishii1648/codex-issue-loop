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
	"sort"
	"strings"
	"testing"
)

const statePackagePath = "github.com/ishii1648/codex-issue-loop/internal/adapter/state"
const issueDomainPackagePath = "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
const internalPackagePrefix = "github.com/ishii1648/codex-issue-loop/internal/"

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
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	loaded := loadTypedPackages(t, repoRoot, false)

	allowed := map[string]int{
		"internal/adapter/state/issue_transition.go": 1, // the named decision commit boundary
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

func TestIssueStatusLogicUsesTypedVocabulary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	loaded := loadTypedPackages(t, repoRoot, true)
	for _, pkg := range loaded {
		for _, file := range pkg.syntax {
			definitions := statusConstantDefinitionLiterals(file)
			ast.Inspect(file, func(node ast.Node) bool {
				if literal, ok := node.(*ast.BasicLit); ok && literal.Kind == token.STRING && isIssueLifecycleVocabularyType(pkg.info.TypeOf(literal)) && !definitions[literal] {
					position := pkg.fset.Position(literal.Pos())
					relative, _ := filepath.Rel(repoRoot, position.Filename)
					t.Errorf("%s:%d uses an untyped lifecycle string; use an issuedomain constant", filepath.ToSlash(relative), position.Line)
				}
				return true
			})
		}
	}
}

func statusConstantDefinitionLiterals(file *ast.File) map[*ast.BasicLit]bool {
	definitions := map[*ast.BasicLit]bool{}
	for _, declaration := range file.Decls {
		group, ok := declaration.(*ast.GenDecl)
		if !ok || group.Tok != token.CONST {
			continue
		}
		for _, specification := range group.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, expression := range value.Values {
				if literal, ok := expression.(*ast.BasicLit); ok {
					definitions[literal] = true
				}
			}
		}
	}
	return definitions
}

func TestIssueStatusStringConversionsStayAtSerializationBoundaries(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	loaded := loadTypedPackages(t, repoRoot, false)
	allowed := map[string]int{
		"internal/application/app/status.go":             1,
		"internal/application/app/workspace_recovery.go": 1,
		"internal/application/lifecycle/worktrees.go":    1,
		"internal/application/migration/migration.go":    2,
		"internal/adapter/state/semantic.go":             1,
		"internal/application/supervisor/scheduler.go":   1,
		"internal/application/supervisor/supervisor.go":  1,
	}
	seen := map[string]int{}
	for _, pkg := range loaded {
		for _, file := range pkg.syntax {
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !isIssueStatusStringCall(pkg.info, call) {
					return true
				}
				position := pkg.fset.Position(call.Pos())
				relative, _ := filepath.Rel(repoRoot, position.Filename)
				path := filepath.ToSlash(relative)
				if _, ok := allowed[path]; !ok {
					t.Errorf("%s:%d strips the issue.Status type outside a serialization boundary", path, position.Line)
				}
				seen[path]++
				return true
			})
		}
	}
	for path, want := range allowed {
		if got := seen[path]; got != want {
			t.Errorf("%s has %d issue.Status String calls; want %d", path, got, want)
		}
	}
}

func isIssueStatusStringCall(info *types.Info, call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "String" && isIssueStatusType(info.TypeOf(selector.X))
}

func isIssueStatusType(value types.Type) bool {
	named, ok := value.(*types.Named)
	return ok && named.Obj().Name() == "Status" && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == issueDomainPackagePath
}

func isIssueLifecycleVocabularyType(value types.Type) bool {
	named, ok := value.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != issueDomainPackagePath {
		return false
	}
	switch named.Obj().Name() {
	case "Status", "GitHubSync", "ResourceParkStatus", "RequestStatus", "EnvironmentResumeStatus",
		"PublicationRecoveryStatus", "PublicationRecoveryAttemptStatus", "ConflictAttemptStatus",
		"PullRequestChecksRecoveryStatus", "AnsweredWorkspaceRecoveryStatus", "WorkspaceProvenanceRecoveryStatus",
		"MergedPullRequestAdoptionStatus":
		return true
	default:
		return false
	}
}

type listedPackage struct {
	Dir          string
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Export       string
	GoFiles      []string
	TestGoFiles  []string
	XTestGoFiles []string
	ForTest      string
	Module       *struct{ Path string }
}

func TestInternalPackagesFollowLayerDependencies(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", ".."))
	command := exec.Command("go", "list", "-json", "./internal/...")
	command.Dir = repoRoot
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("list internal packages: %v\n%s", err, stderr.String())
	}

	allowed := map[string]map[string]bool{
		"domain":      {"domain": true},
		"platform":    {"domain": true, "platform": true},
		"adapter":     {"domain": true, "platform": true, "adapter": true},
		"application": {"domain": true, "platform": true, "adapter": true, "application": true},
	}
	decoder := json.NewDecoder(&stdout)
	for {
		var listed listedPackage
		err := decoder.Decode(&listed)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode package metadata: %v", err)
		}
		source, ok := packageLayer(listed.ImportPath)
		if !ok {
			t.Errorf("%s is outside domain/application/adapter/platform", listed.ImportPath)
			continue
		}
		for group, imports := range map[string][]string{
			"production":     listed.Imports,
			"internal tests": listed.TestImports,
			"external tests": listed.XTestImports,
		} {
			for _, imported := range imports {
				target, internal := packageLayer(imported)
				if !internal {
					continue
				}
				if !allowed[source][target] {
					t.Errorf("%s %s import crosses the layer boundary: %s -> %s", listed.ImportPath, group, source, target)
				}
			}
		}
	}
}

func packageLayer(importPath string) (string, bool) {
	if !strings.HasPrefix(importPath, internalPackagePrefix) {
		return "", false
	}
	relative := strings.TrimPrefix(importPath, internalPackagePrefix)
	layer, _, _ := strings.Cut(relative, "/")
	switch layer {
	case "domain", "application", "adapter", "platform":
		return layer, true
	default:
		return "", false
	}
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
func loadTypedPackages(t *testing.T, repoRoot string, includeTests bool) []typedProductionPackage {
	t.Helper()
	args := []string{"list", "-json", "-export", "-deps", "-test", "./..."}
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
		if listed.ForTest == "" && !strings.Contains(listed.ImportPath, " [") && !strings.HasSuffix(listed.ImportPath, ".test") {
			all[listed.ImportPath] = listed
		}
	}

	wanted := make([]string, 0)
	for importPath, listed := range all {
		if listed.Module != nil && listed.Module.Path == "github.com/ishii1648/codex-issue-loop" {
			wanted = append(wanted, importPath)
		}
	}
	sort.Strings(wanted)
	loaded := make([]typedProductionPackage, 0, len(wanted))
	for _, importPath := range wanted {
		listed, ok := all[importPath]
		if !ok {
			t.Fatalf("go list omitted %s", importPath)
		}
		fset := token.NewFileSet()
		files := append([]string(nil), listed.GoFiles...)
		if includeTests {
			files = append(files, listed.TestGoFiles...)
		}
		syntax := make([]*ast.File, 0, len(files))
		for _, name := range files {
			file, err := parser.ParseFile(fset, filepath.Join(listed.Dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			syntax = append(syntax, file)
		}
		info := &types.Info{
			Selections: map[*ast.SelectorExpr]*types.Selection{},
			Types:      map[ast.Expr]types.TypeAndValue{},
		}
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
		if includeTests && len(listed.XTestGoFiles) > 0 {
			testSet := token.NewFileSet()
			testSyntax := make([]*ast.File, 0, len(listed.XTestGoFiles))
			for _, name := range listed.XTestGoFiles {
				file, err := parser.ParseFile(testSet, filepath.Join(listed.Dir, name), nil, 0)
				if err != nil {
					t.Fatalf("parse %s: %v", name, err)
				}
				testSyntax = append(testSyntax, file)
			}
			testInfo := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Types: map[ast.Expr]types.TypeAndValue{}}
			testChecker := types.Config{Importer: importer.ForCompiler(testSet, runtime.Compiler, lookup)}
			if _, err := testChecker.Check(importPath+"_test", testSet, testSyntax, testInfo); err != nil {
				t.Fatalf("type-check %s external tests: %v", importPath, err)
			}
			loaded = append(loaded, typedProductionPackage{fset: testSet, syntax: testSyntax, info: testInfo})
		}
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

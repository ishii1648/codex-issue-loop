package supervisor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVerticalLifecycleFilesBoundOrchestrationSize(t *testing.T) {
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve architecture test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", ".."))
	mainFiles := map[string]int{
		"internal/application/supervisor/supervisor.go": 1200,
		"internal/application/app/app.go":               1200,
	}
	verticalFiles := []string{
		"internal/application/supervisor/worker_execution.go",
		"internal/application/supervisor/continuation_stage.go",
		"internal/application/supervisor/checks_lifecycle.go",
		"internal/application/supervisor/conflict_lifecycle.go",
		"internal/application/supervisor/github_sync_lifecycle.go",
		"internal/application/app/installation.go",
		"internal/application/app/operator_control.go",
		"internal/application/app/operator_attention.go",
		"internal/application/app/issue_resolution.go",
	}
	for path, limit := range mainFiles {
		assertLineLimit(t, filepath.Join(root, path), limit)
	}
	for _, path := range verticalFiles {
		assertLineLimit(t, filepath.Join(root, path), 800)
	}

	exceptions := map[string]string{
		"internal/application/supervisor/supervisor.go:processExisting": "one observation is converted to one domain decision before a single lifecycle dispatch switch",
	}
	for path := range mainFiles {
		assertFunctionLimits(t, root, path, exceptions)
	}
	for _, path := range verticalFiles {
		assertFunctionLimits(t, root, path, exceptions)
	}
}

func assertLineLimit(t *testing.T, path string, limit int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Count(string(data), "\n") + 1
	if lines > limit {
		t.Errorf("%s has %d lines; limit=%d", path, lines, limit)
	}
}

func assertFunctionLimits(t *testing.T, root, relative string, exceptions map[string]string) {
	t.Helper()
	path := filepath.Join(root, relative)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		lines := fset.Position(function.End()).Line - fset.Position(function.Pos()).Line + 1
		if lines <= 200 {
			continue
		}
		key := relative + ":" + function.Name.Name
		if strings.TrimSpace(exceptions[key]) == "" {
			t.Errorf("%s has %d lines without a recorded invariant reason", key, lines)
		}
	}
}

func TestVerticalLifecyclesOwnDecisionPersistenceAndEffects(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(current), "..", "..", ".."))
	required := map[string][]string{
		"internal/application/supervisor/worker_execution.go":      {"issuedomain.", "l.Store.Update(", "l.runWorker(", "l.GitHub."},
		"internal/application/supervisor/continuation_stage.go":    {"issuedomain.", "l.Store.Update(", "l.Publisher.", "l.GitHub.Inspect("},
		"internal/application/supervisor/checks_lifecycle.go":      {"issuedomain.", "l.Store.Update(", "l.GitHub.", "l.inspectIssue("},
		"internal/application/supervisor/conflict_lifecycle.go":    {"conflict.", "l.Store.Update(", "l.Conflicts.", "l.runWorker("},
		"internal/application/supervisor/github_sync_lifecycle.go": {"state.", "l.Store.Update(", "l.GitHub."},
		"internal/application/app/issue_resolution.go":             {"issuedomain.", "planned.store.Update(", "client."},
	}
	for path, markers := range required {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		for _, marker := range markers {
			if !strings.Contains(string(data), marker) {
				t.Errorf("%s does not own required boundary %q", path, marker)
			}
		}
	}
}

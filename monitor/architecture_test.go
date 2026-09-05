package monitor_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMonitorDoesNotDependOnSupervisorLifecyclePackages(t *testing.T) {
	banned := []string{
		"github.com/ishii1648/codex-issue-loop/internal/application/supervisor",
		"github.com/ishii1648/codex-issue-loop/internal/domain/issue",
		"github.com/ishii1648/codex-issue-loop/internal/adapter/state",
	}
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		for _, spec := range file.Imports {
			importPath, _ := strconv.Unquote(spec.Path.Value)
			for _, prefix := range banned {
				if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
					t.Errorf("%s imports forbidden package %s", path, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGitHubAdapterExposesNoMutationCapability(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join("internal", "github", "github.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range general.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || typeSpec.Name.Name != "Observer" {
				continue
			}
			iface := typeSpec.Type.(*ast.InterfaceType)
			if len(iface.Methods.List) != 1 || iface.Methods.List[0].Names[0].Name != "Observe" {
				t.Fatalf("GitHub adapter interface contains mutation capability")
			}
		}
	}
	data, err := os.ReadFile(filepath.Join("internal", "github", "github.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"issue edit", "issue comment", "--add-label", "--remove-label", `"POST"`, `"PATCH"`, `"DELETE"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("GitHub adapter contains mutation surface %q", forbidden)
		}
	}
}

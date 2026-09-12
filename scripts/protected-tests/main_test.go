package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func put(t *testing.T, repo, p, data string) {
	t.Helper()
	full := filepath.Join(repo, p)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "--quiet")
	policy, err := os.ReadFile("../../" + definition)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := readRules(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		p := rule.path
		if strings.HasSuffix(p, "/") {
			p += "fixture"
		}
		put(t, repo, p, "original\n")
	}
	put(t, repo, definition, string(policy)+"test\ttests with spaces/\tspace-path\ncontrol\tfixture helpers/\thelper\n")
	put(t, repo, "tests with spaces/original_test.go", "original\n")
	put(t, repo, "fixture helpers/input data", "original\n")
	source, err := os.ReadFile("../../internal/application/supervisor/scheduler_test.go")
	if err != nil {
		t.Fatal(err)
	}
	put(t, repo, "internal/application/supervisor/scheduler_test.go", string(source))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base")
	return repo, runGit(t, repo, "rev-parse", "HEAD")
}

func TestProtectedChanges(t *testing.T) {
	scheduler := "internal/application/supervisor/scheduler_test.go"
	cases := []struct {
		name, p, target, content, status string
		remove                           bool
		required                         bool
	}{
		{name: "edit", p: scheduler, content: "edited", status: "M", required: true},
		{name: "expectation inversion", p: scheduler, status: "M", required: true},
		{name: "skip", p: scheduler, content: "t.Skip()", status: "M", required: true},
		{name: "format", p: scheduler, content: "\n", status: "M", required: true},
		{name: "delete", p: scheduler, remove: true, status: "D", required: true},
		{name: "rename outside", p: scheduler, target: "unprotected/moved_test.go", status: "D", required: true},
		{name: "delete plus replacement", p: scheduler, target: "new_test.go", content: "replacement", status: "D", required: true},
		{name: "definition delete", p: definition, remove: true, status: "D", required: true},
		{name: "definition shrink", p: definition, content: "", status: "M", required: true},
		{name: "checker delete", p: "scripts/protected-tests/fixture", remove: true, status: "D", required: true},
		{name: "new checker", p: "scripts/protected-tests/bypass.go", content: "bypass", status: "A", required: true},
		{name: "execution setting", p: "Makefile", content: "skip", status: "M", required: true},
		{name: "new workflow", p: ".github/workflows/bypass.yml", content: "skip", status: "A", required: true},
		{name: "helper", p: "fixture helpers/input data", content: "weakened", status: "M", required: true},
		{name: "new helper", p: "fixture helpers/new data", content: "new", status: "A", required: true},
		{name: "spaces", p: "tests with spaces/original_test.go", content: "change", status: "M", required: true},
		{name: "newline path", p: "fixture helpers/a\nb\t_test.go", content: "new", status: "A", required: true},
		{name: "independent addition", p: "tests with spaces/new_test.go", content: "new"},
		{name: "unprotected", p: "other_test.go", content: "change"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, base := fixture(t)
			if tc.target != "" {
				data, err := os.ReadFile(filepath.Join(repo, tc.p))
				if err != nil {
					t.Fatal(err)
				}
				if tc.content != "" {
					data = []byte(tc.content)
				}
				put(t, repo, tc.target, string(data))
			}
			if tc.remove || tc.target != "" {
				if err := os.Remove(filepath.Join(repo, tc.p)); err != nil {
					t.Fatal(err)
				}
			} else if tc.name == "expectation inversion" {
				data, err := os.ReadFile(filepath.Join(repo, tc.p))
				if err != nil {
					t.Fatal(err)
				}
				before := "case number := <-pool.started:\n\t\t\t\tif number != 2 {"
				after := "case number := <-pool.started:\n\t\t\t\tif number == 2 {"
				if !strings.Contains(string(data), before) {
					t.Fatal("isolation expectation fixture drifted")
				}
				put(t, repo, tc.p, strings.Replace(string(data), before, after, 1))
			} else {
				put(t, repo, tc.p, tc.content)
			}
			put(t, repo, "production.go", "mixed production change")
			runGit(t, repo, "add", ".")
			runGit(t, repo, "commit", "-qm", "head")
			head := runGit(t, repo, "rev-parse", "HEAD")
			runGit(t, repo, "checkout", "--detach", base)
			got := inspect(repo, base, head)
			if got.Error != "" || got.DedicatedPRRequired != tc.required {
				t.Fatalf("%+v", got)
			}
			if tc.required && (len(got.Changes) != 1 || got.Changes[0].Status != tc.status) {
				t.Fatalf("%+v", got)
			}
			first, _ := json.Marshal(got)
			runGit(t, repo, "config", "diff.renames", "true")
			second, _ := json.Marshal(inspect(repo, base, head))
			if string(first) != string(second) {
				t.Fatal("nondeterministic classification")
			}
		})
	}
}

func TestFailClosed(t *testing.T) {
	repo, base := fixture(t)
	for _, tc := range []struct{ base, head string }{{"main", base}, {base, "missing"}, {base, strings.Repeat("0", 40)}} {
		got := inspect(repo, tc.base, tc.head)
		if got.Error == "" || !got.DedicatedPRRequired {
			t.Fatalf("%+v", got)
		}
	}
	put(t, repo, definition, "bad definition")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "invalid base")
	head := runGit(t, repo, "rev-parse", "HEAD")
	for _, pair := range [][2]string{{base, head}, {head, head}} {
		got := inspect(repo, pair[0], pair[1])
		if got.Error == "" || !got.DedicatedPRRequired {
			t.Fatalf("%+v", got)
		}
	}
	got := inspect(t.TempDir(), base, base)
	if got.Error == "" || !got.DedicatedPRRequired {
		t.Fatalf("%+v", got)
	}
}

func TestDefinitionValidation(t *testing.T) {
	data, err := os.ReadFile("../../" + definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", "test\t..\treason", "test\t../escape\treason", "unknown\tx\treason", string(data) + "test\tMakefile\tduplicate\n", strings.Replace(string(data), "control\tscripts/protected-tests/", "test\tscripts/protected-tests/", 1)} {
		if _, err := readRules([]byte(invalid)); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestMergeBaseAndBaseUpdate(t *testing.T) {
	repo, base := fixture(t)
	put(t, repo, "ordinary_test.go", "new")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "head")
	head := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "--detach", base)
	put(t, repo, "Makefile", "base-only change")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base advance")
	updated := runGit(t, repo, "rev-parse", "HEAD")
	got := inspect(repo, updated, head)
	if got.Error != "" || got.DedicatedPRRequired || got.MergeBase != base || got.Base != updated {
		t.Fatalf("%+v", got)
	}
}

func TestDiffFailure(t *testing.T) {
	repo, base := fixture(t)
	put(t, repo, "new.go", "new")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "head")
	head := runGit(t, repo, "rev-parse", "HEAD")
	tree := runGit(t, repo, "rev-parse", "HEAD^{tree}")
	runGit(t, repo, "checkout", "--detach", base)
	if err := os.Remove(filepath.Join(repo, ".git", "objects", tree[:2], tree[2:])); err != nil {
		t.Fatal(err)
	}
	got := inspect(repo, base, head)
	if !strings.Contains(got.Error, "git diff failed") || !got.DedicatedPRRequired {
		t.Fatalf("%+v", got)
	}
}

func TestAdditionAlreadyPresentInUpdatedBase(t *testing.T) {
	repo, base := fixture(t)
	p := "tests with spaces/concurrent_test.go"
	put(t, repo, p, "head expectation")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "head")
	head := runGit(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "checkout", "--detach", base)
	put(t, repo, p, "base expectation")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "base update")
	updated := runGit(t, repo, "rev-parse", "HEAD")
	got := inspect(repo, updated, head)
	if got.Error != "" || !got.DedicatedPRRequired || len(got.Changes) != 1 {
		t.Fatalf("%+v", got)
	}
}

package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHighRiskReviewExecutionEvidence(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/high-risk-review.yml"))
	if err != nil {
		t.Fatal(err)
	}
	prefix := `jq -e --arg base "$BASE_SHA" --arg head "$HEAD_SHA" '`
	parts := strings.SplitN(string(workflow), prefix, 2)
	if len(parts) != 2 {
		t.Fatal("workflow JSON predicate not found")
	}
	predicate := strings.SplitN(parts[1], "' review-artifacts", 2)[0]

	for _, tc := range []struct {
		name, change, mode string
		want, invariant    string
	}{
		{"production without tests", "internal/application/supervisor/change.go", "empty", "unverified", "passed"},
		{"unrelated empty test", "internal/application/supervisor/change.go", "unrelated", "unverified", "passed"},
		{"statecontract empty adapter test", "internal/domain/statecontract/change.go", "state-empty", "unverified", "unverified"},
		{"existing suites", "internal/application/supervisor/change.go", "", "passed", "passed"},
		{"large suite evidence", "internal/application/supervisor/change.go", "large", "passed", "passed"},
		{"failed suite", "internal/application/supervisor/change.go", "fail", "failed", "passed"},
		{"skipped suite", "internal/application/supervisor/change.go", "skip", "unverified", "passed"},
		{"missing package", "internal/application/supervisor/change.go", "missing", "failed", "passed"},
		{"missing results", "internal/application/supervisor/change.go", "no-results", "unverified", "unverified"},
		{"unavailable go", "internal/application/supervisor/change.go", "unavailable-results", "unverified", "unverified"},
		{"invalid results", "internal/application/supervisor/change.go", "invalid-results", "unverified", "unverified"},
		{"wrong package results", "internal/application/supervisor/change.go", "wrong-results", "unverified", "unverified"},
		{"head mismatch", "internal/application/supervisor/change.go", "head-mismatch", "unverified", "unverified"},
		{"invalid base", "internal/application/supervisor/change.go", "base-mismatch", "unverified", "unverified"},
		{"dirty checkout", "internal/application/supervisor/change.go", "dirty", "unverified", "unverified"},
		{"not applicable", "README.md", "", "not_applicable", "not_applicable"},
		{"diverged base", "README.md", "diverged", "not_applicable", "not_applicable"},
		{"workflow", ".github/workflows/test.yml", "", "passed", "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = root
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(path, content string) {
				t.Helper()
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			commit := func() string {
				t.Helper()
				git("add", ".")
				git("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
				return git("rev-parse", "HEAD")
			}
			git("init", "-q")
			git("config", "user.name", "fixture")
			git("config", "user.email", "fixture@example.invalid")
			write("go.mod", "module github.com/ishii1648/codex-issue-loop\n\ngo 1.22\n")
			for _, pkg := range []string{"adapter/state", "domain/issue", "domain/statecontract", "application/conformance", "application/migration", "application/supervisor", "application/delivery"} {
				body := "package fixture\nimport \"testing\"\nfunc TestFaultExisting(t *testing.T) {}\n"
				if pkg == "application/supervisor" {
					switch tc.mode {
					case "empty", "unrelated":
						body = "package fixture\n"
					case "large":
						body = "package fixture\nimport \"testing\"\nfunc TestFaultExisting(t *testing.T) {for i:=0;i<2000;i++ {t.Run(\"existing regression\",func(t *testing.T){})}}\n"
					case "fail":
						body = "package fixture\nimport \"testing\"\nfunc TestFaultExisting(t *testing.T) {t.Fatal(\"fault\")}\n"
					case "skip":
						body = "package fixture\nimport \"testing\"\nfunc TestFaultExisting(t *testing.T) {t.Skip(\"not executed\")}\n"
					}
				}
				if pkg == "application/migration" && tc.mode == "missing" {
					continue
				}
				if pkg == "adapter/state" && tc.mode == "state-empty" {
					body = "package fixture\n"
				}
				write("internal/"+pkg+"/existing_test.go", body)
			}
			base := commit()
			if tc.mode == "diverged" {
				write(".github/workflows/base.yml", "name: base\n")
				advanced := commit()
				git("checkout", "--detach", base)
				base = advanced
			}
			content := "package fixture\n"
			if !strings.HasSuffix(tc.change, ".go") {
				content = "changed\n"
			}
			write(tc.change, content)
			if tc.mode == "unrelated" {
				write("unrelated/noop_test.go", "package unrelated\n")
			}
			if tc.mode == "state-empty" {
				write("internal/adapter/state/noop_test.go", "package fixture\n")
			}
			head := commit()
			if tc.mode == "head-mismatch" {
				git("checkout", "--detach", base)
			}
			if tc.mode == "base-mismatch" {
				base = strings.Repeat("0", 40)
			}
			if tc.mode == "dirty" {
				write(tc.change, "package dirty\n")
			}
			output := filepath.Join(t.TempDir(), "report.json")
			cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts/high-risk-review.sh"))
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "BASE_SHA="+base, "HEAD_SHA="+head, "REVIEW_OUTPUT="+output, "GOWORK=off")
			if strings.HasSuffix(tc.mode, "results") {
				bin := t.TempDir()
				body := "#!/bin/sh\nexit 0\n"
				if tc.mode == "unavailable-results" {
					body = "#!/bin/sh\nexit 127\n"
				}
				if tc.mode == "invalid-results" {
					body = "#!/bin/sh\nprintf 'invalid json\\n'\n"
				}
				if tc.mode == "wrong-results" {
					body = "#!/bin/sh\nprintf '%s\\n' '{\"Action\":\"pass\",\"Package\":\"wrong\",\"Test\":\"TestFault\"}' '{\"Action\":\"pass\",\"Package\":\"wrong\"}'\n"
				}
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
				cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			logs, runErr := cmd.CombinedOutput()
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatalf("report: %v; run: %v %s", err, runErr, logs)
			}
			var report struct {
				Schema     int `json:"schema_version"`
				Base, Head string
				HighRisk   bool `json:"high_risk"`
				Count      int  `json:"finding_count"`
				Findings   []string
				Checks     map[string]struct {
					Status   string
					Required bool
					Evidence struct {
						Base, Head string
						Tests      []json.RawMessage
					}
				}
			}
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if report.Schema != 2 || report.Base != base || report.Head != head || report.Count != len(report.Findings) {
				t.Fatalf("invalid report: %s", data)
			}
			if (runErr == nil) != (report.Count == 0) {
				t.Fatalf("exit/report mismatch: %v %s", runErr, data)
			}
			if report.Checks["fault_tests"].Status != tc.want || report.Checks["invariants"].Status != tc.invariant {
				t.Fatalf("unexpected checks: %s %s", data, logs)
			}
			rejectedRevision := strings.Contains(tc.mode, "mismatch") || tc.mode == "dirty"
			shouldPass := !rejectedRevision && (tc.want == "passed" || tc.want == "not_applicable")
			consumer := exec.Command("jq", "-e", "--arg", "base", base, "--arg", "head", head, predicate, output)
			consumerOutput, consumerErr := consumer.CombinedOutput()
			if (consumerErr == nil) != shouldPass {
				t.Fatalf("workflow disagrees with script: %v %s; report %s", consumerErr, consumerOutput, data)
			}
			if tc.name == "existing suites" {
				for _, mutation := range []string{
					`.base = "wrong"`, `.head = "wrong"`, `del(.checks.fault_tests)`,
					`.checks.fault_tests.status = "failed"`, `.checks.fault_tests.status = "unverified"`,
					`.checks.fault_tests.required = false`, `.schema_version = 1`,
					`.checks.fault_tests.evidence.head = "wrong"`, `.checks.fault_tests.evidence.base = "wrong"`,
					`.checks.fault_tests.evidence.tests = []`, `.checks.fault_tests.evidence.exit_code = 1`,
					`.checks.rollback.status = "passed"`, `.high_risk = null`,
				} {
					mutate := exec.Command("jq", mutation, output)
					altered, err := mutate.Output()
					if err != nil {
						t.Fatal(err)
					}
					validate := exec.Command("jq", "-e", "--arg", "base", base, "--arg", "head", head, predicate)
					validate.Stdin = strings.NewReader(string(altered))
					if err := validate.Run(); err == nil {
						t.Errorf("workflow accepted %s", mutation)
					}
				}
			}
			if (runErr == nil) != shouldPass {
				t.Fatalf("unexpected result: %v %s", runErr, data)
			}
			if report.HighRisk {
				for _, name := range []string{"specification_mapping", "rollback"} {
					if report.Checks[name].Status != "unverified" || report.Checks[name].Required {
						t.Fatalf("human judgment claimed: %s", data)
					}
				}
				for _, name := range []string{"invariants", "migration", "fault_tests", "release_compatibility"} {
					check := report.Checks[name]
					if !check.Required || check.Evidence.Base != base || check.Evidence.Head != head {
						t.Fatalf("unbound suite: %s", data)
					}
					if check.Status == "passed" && len(check.Evidence.Tests) == 0 {
						t.Fatalf("missing execution evidence: %s", data)
					}
				}
			}
		})
	}
}

func TestHighRiskReviewWorkflowContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/high-risk-review.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: High-risk review gate", "ref: refs/pull/${{ github.event.pull_request.number }}/head", "scripts/high-risk-review.sh", ".schema_version == 2", ".base == $base and .head == $head", ".finding_count == 0", "(.findings | length) == 0", "$report.checks[$key].required == true", `$report.checks[$key].status == "passed"`, "if: always()", "if-no-files-found: error"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing workflow contract: %s", want)
		}
	}
}

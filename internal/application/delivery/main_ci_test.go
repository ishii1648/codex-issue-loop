package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseMainCIFailsClosed(t *testing.T) {
	for _, scenario := range []string{"rerun-success", "latest-run-success", "wrong-sha", "pull-request", "wrong-workflow", "wrong-path", "wrong-repository", "wrong-head-repository", "wrong-branch", "no-runs", "running", "failed", "cancelled", "skipped", "missing-quality", "duplicate-quality", "quality-failed", "quality-skipped", "quality-cancelled", "quality-running", "wrong-job-attempt", "wrong-job-run", "wrong-job-sha", "truncated-runs", "truncated-jobs", "too-many-runs", "old-success-new-failure", "old-failure-new-success", "rerun-started", "api-workflow", "api-runs", "api-jobs", "api-current", "malformed"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			run := map[string]any{"id": 42, "run_attempt": 2, "workflow_id": 7, "path": ".github/workflows/ci.yml", "repository": map[string]any{"full_name": "owner/repo"}, "head_repository": map[string]any{"full_name": "owner/repo"}, "head_sha": evidenceCommit, "head_branch": "main", "event": "push", "status": "completed", "conclusion": "success"}
			job := map[string]any{"name": "Quality gates", "run_id": 42, "run_attempt": 2, "head_sha": evidenceCommit, "status": "completed", "conclusion": "success"}
			jobs := []any{job}
			runs := []any{run}
			workflow := map[string]any{"id": 7, "path": ".github/workflows/ci.yml", "state": "active"}
			switch scenario {
			case "wrong-sha":
				run["head_sha"] = strings.Repeat("f", 40)
			case "pull-request":
				run["event"] = "pull_request"
			case "wrong-workflow":
				run["workflow_id"] = 8
			case "wrong-path":
				run["path"] = ".github/workflows/other.yml"
			case "wrong-repository":
				run["repository"] = map[string]any{"full_name": "other/repo"}
			case "wrong-head-repository":
				run["head_repository"] = map[string]any{"full_name": "other/repo"}
			case "wrong-branch":
				run["head_branch"] = "feature"
			case "no-runs":
				runs = nil
			case "running":
				run["status"] = "in_progress"
			case "failed":
				run["conclusion"] = "failure"
			case "cancelled", "skipped":
				run["conclusion"] = scenario
			case "missing-quality":
				job["name"] = "Other check"
			case "duplicate-quality":
				jobs = append(jobs, job)
			case "quality-failed":
				job["conclusion"] = "failure"
			case "quality-skipped":
				job["conclusion"] = "skipped"
			case "quality-cancelled":
				job["conclusion"] = "cancelled"
			case "quality-running":
				job["status"] = "in_progress"
			case "wrong-job-attempt":
				job["run_attempt"] = 1
			case "wrong-job-run":
				job["run_id"] = 41
			case "wrong-job-sha":
				job["head_sha"] = strings.Repeat("f", 40)
			}
			if strings.Contains(scenario, "new-") || scenario == "latest-run-success" {
				older := make(map[string]any)
				for k, v := range run {
					older[k] = v
				}
				older["id"] = 41
				if scenario == "old-failure-new-success" {
					older["conclusion"] = "failure"
				}
				if scenario == "old-success-new-failure" {
					run["conclusion"] = "failure"
				}
				runs = []any{older, run}
			}
			runCount, jobCount := len(runs), len(jobs)
			if scenario == "truncated-runs" {
				runCount++
			}
			if scenario == "too-many-runs" {
				runCount = 101
			}
			if scenario == "truncated-jobs" {
				jobCount++
			}
			writeJSON := func(name string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON("workflow", workflow)
			writeJSON("runs", map[string]any{"total_count": runCount, "workflow_runs": runs})
			writeJSON("jobs", map[string]any{"total_count": jobCount, "jobs": jobs})
			if scenario == "rerun-started" {
				run["run_attempt"] = 3
				run["status"] = "in_progress"
			}
			writeJSON("current", run)
			writeExecutable(t, root, "gh", `#!/bin/sh
set -eu
[ "$1" = api ]
case "$2" in
 repos/owner/repo/actions/workflows/ci.yml) file=workflow ;;
 "repos/owner/repo/actions/workflows/7/runs?branch=main&event=push&head_sha=$RELEASE_COMMIT&per_page=100") file=runs ;;
 'repos/owner/repo/actions/runs/42/attempts/2/jobs?per_page=100') file=jobs ;;
 repos/owner/repo/actions/runs/42) file=current ;;
 *) exit 90 ;;
esac
[ "$SCENARIO" != "api-$file" ] || exit 1
if [ "$SCENARIO" = malformed ]; then printf '{'; exit 0; fi
cat "$FIXTURE/$file"
`)
			cmd := exec.Command("bash", filepath.Join(repositoryRoot(t), "scripts/check-main-ci.sh"))
			cmd.Env = append(os.Environ(), "PATH="+root+":"+os.Getenv("PATH"), "FIXTURE="+root, "SCENARIO="+scenario, "GITHUB_REPOSITORY=owner/repo", "RELEASE_COMMIT="+evidenceCommit, "GH_TOKEN=", "GITHUB_STEP_SUMMARY="+filepath.Join(root, "summary"))
			output, err := cmd.CombinedOutput()
			wantOK := scenario == "rerun-success" || scenario == "latest-run-success" || scenario == "old-failure-new-success"
			if (err == nil) != wantOK {
				t.Fatalf("success=%v want=%v: %s", err == nil, wantOK, output)
			}
			if wantOK && !strings.Contains(string(output), "https://github.com/owner/repo/actions/runs/42/attempts/2 commit "+evidenceCommit) {
				t.Fatalf("missing run/attempt trace: %s", output)
			}
		})
	}
}

func TestReleaseTagCommitValidationBothTriggers(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		On   map[string]any    `yaml:"on"`
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"push", "workflow_dispatch"} {
		if _, ok := workflow.On[event]; !ok {
			t.Fatalf("missing trigger %s", event)
		}
	}
	if workflow.Env["RELEASE_TAG"] != "${{ github.event_name == 'workflow_dispatch' && inputs.release_tag || github.ref_name }}" || workflow.Env["RELEASE_COMMIT"] != "${{ github.event_name == 'workflow_dispatch' && inputs.release_commit || github.sha }}" {
		t.Fatal("release inputs must identify the tag and commit for both triggers")
	}
	validation := ""
	for _, step := range workflow.Jobs["build-candidate"].Steps {
		if step.Name == "Validate annotated release tag" {
			validation = step.Run
		}
	}
	if validation == "" {
		t.Fatal("tag validation missing")
	}
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
	git("init", "-b", "main")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	sha := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/main", sha)
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "tag.gpgsign=false", "tag", "-a", "v1.2.3", "-m", "fixture")
	git("-c", "tag.gpgsign=false", "tag", "v1.2.4")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "outside main")
	outside := git("rev-parse", "HEAD")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "tag.gpgsign=false", "tag", "-a", "v1.2.5", "-m", "fixture")
	for _, event := range []string{"push", "workflow_dispatch"} {
		for _, tc := range []struct {
			name, tag, commit string
			ok                bool
		}{
			{"valid", "v1.2.3", sha, true}, {"mismatch", "v1.2.3", outside, false}, {"lightweight", "v1.2.4", sha, false}, {"outside-main", "v1.2.5", outside, false}, {"invalid-tag", "main", sha, false}, {"invalid-sha", "v1.2.3", "bad", false},
		} {
			t.Run(event+"/"+tc.name, func(t *testing.T) {
				cmd := exec.Command("bash", "-c", validation)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "RELEASE_TAG="+tc.tag, "RELEASE_COMMIT="+tc.commit)
				out, err := cmd.CombinedOutput()
				if (err == nil) != tc.ok {
					t.Fatalf("success=%v want=%v: %s", err == nil, tc.ok, out)
				}
			})
		}
	}
}

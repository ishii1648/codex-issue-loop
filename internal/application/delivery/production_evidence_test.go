package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const evidenceCommit = "0123456789abcdef0123456789abcdef01234567"

func TestProductionStateIsolationRunsCredentiallessContractBetweenSnapshots(t *testing.T) {
	for _, test := range []struct {
		name     string
		mismatch bool
		wantOK   bool
	}{{name: "identical", wantOK: true}, {name: "revision changed", mismatch: true}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			productionRepo := filepath.Join(root, "production")
			if err := os.MkdirAll(filepath.Join(productionRepo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			productionBinary := writeExecutable(t, root, "production-agent-loop", `#!/bin/sh
if [ "$1" = doctor ]; then
  printf '%s\n' '{"schema_version":1,"ok":true,"diagnostics":[]}'
  exit
fi
count=0
if [ -f "$COUNT_PATH" ]; then count=$(cat "$COUNT_PATH"); fi
count=$((count + 1))
printf '%s\n' "$count" >"$COUNT_PATH"
revision=7
if [ "${MISMATCH:-0}" = 1 ] && [ "$count" -gt 1 ]; then revision=8; fi
printf '{"worker_pool":{"active":0,"limit":1},"pending_requests":[],"state":{"repo_id":"production-id","state_revision":%s,"supervisor":{"state":"idle"},"issues":{}}}\n' "$revision"
`)
			candidate := writeExecutable(t, root, "candidate", "#!/bin/sh\nexit 0\n")
			fakeContract := writeExecutable(t, root, "offline-contract", `#!/bin/sh
mkdir -p "$CONTRACT_ARTIFACT_DIR"
printf '%s\n' '{"schema_version":1,"mode":"credentialless-offline","credentials":{"canary_github_token":false,"openai_api_key":false},"external_network":false,"sequences":[{"status":"completed"},{"status":"completed"}],"supervisor_starts":2,"webhook_fixture_replay":1,"transaction_crash_recovery":1,"final":{"active_workers":0,"active_leases":0,"pending_requests":0,"orphan_pid_pgid":0,"duplicate_prs":0,"duplicate_comment_markers":0}}' >"$CONTRACT_ARTIFACT_DIR/offline-contract-report.json"
`)
			artifactDir := filepath.Join(root, "artifacts")
			cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "production-state-isolation.sh"))
			cmd.Dir = repositoryRoot(t)
			cmd.Env = append(os.Environ(),
				"PRODUCTION_REPOSITORY_PATH="+productionRepo,
				"PRODUCTION_AGENT_LOOP_BINARY="+productionBinary,
				"CANDIDATE_BINARY="+candidate,
				"CONTRACT_ARTIFACT_DIR="+artifactDir,
				"RELEASE_TAG=v0.8.0",
				"RELEASE_COMMIT="+evidenceCommit,
				"CANDIDATE_TAG=candidate-v0.8.0-123",
				"OFFLINE_CONTRACT_SCRIPT="+fakeContract,
				"COUNT_PATH="+filepath.Join(root, "count"),
			)
			if test.mismatch {
				cmd.Env = append(cmd.Env, "MISMATCH=1")
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("err=%v output=%s", err, output)
			}
			if !test.wantOK {
				return
			}
			data, err := os.ReadFile(filepath.Join(artifactDir, "production-state-report.json"))
			if err != nil {
				t.Fatal(err)
			}
			var report struct {
				Commit        string         `json:"release_commit"`
				Digest        string         `json:"candidate_binary_sha256"`
				StateAccessed bool           `json:"production_state_accessed"`
				Before        map[string]any `json:"production_before"`
				After         map[string]any `json:"production_after"`
				Contract      map[string]any `json:"offline_contract"`
			}
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			if report.Commit != evidenceCommit || len(report.Digest) != 64 || !report.StateAccessed || !mapsEqual(report.Before, report.After) || report.Contract["mode"] != "credentialless-offline" {
				t.Fatalf("report=%s", data)
			}
		})
	}
}

func TestBreakGlassStopTargetsOnlyDeliveryAndExactRepositoryLaunchAgents(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "managed")
	launchAgents := filepath.Join(root, "launch")
	if err := os.MkdirAll(launchAgents, 0o700); err != nil {
		t.Fatal(err)
	}
	repoID := "repo-a-deadbeef"
	registryData := `{"version":4,"repos":{"` + repoID + `":{"repo_id":"` + repoID + `","repo_path":"/private/repo-a","github_repo":"owner/a","registered_at":"2026-01-01T00:00:00Z"},"repo-b-deadbeef":{"repo_id":"repo-b-deadbeef","repo_path":"/private/repo-b","github_repo":"owner/b","registered_at":"2026-01-01T00:00:00Z"}}}`
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "registry.json"), []byte(registryData), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"com.codex-issue-loop.delivery", "com.codex-issue-loop." + repoID, "com.codex-issue-loop.repo-b-deadbeef"} {
		plist := `<?xml version="1.0"?><plist><dict><key>Label</key><string>` + label + `</string></dict></plist>`
		if err := os.WriteFile(filepath.Join(launchAgents, label+".plist"), []byte(plist), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "launchctl.log")
	launchctl := writeExecutable(t, root, "launchctl", "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$BREAK_GLASS_LOG\"\nexit 0\n")
	cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "break-glass-stop.sh"), "--repo-id", repoID, "--managed-root", managed, "--launch-agents-dir", launchAgents)
	cmd.Env = append(os.Environ(), "AGENT_LOOP_LAUNCHCTL="+launchctl, "BREAK_GLASS_LOG="+logPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("err=%v output=%s", err, output)
	}
	var report struct {
		RepositoryID  string   `json:"repo_id"`
		Stopped       []string `json:"stopped"`
		StateModified bool     `json:"state_modified"`
	}
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatal(err)
	}
	if report.RepositoryID != repoID || report.StateModified || len(report.Stopped) != 2 {
		t.Fatalf("report=%+v", report)
	}
	logData, _ := os.ReadFile(logPath)
	logText := string(logData)
	if strings.Contains(logText, "repo-b-deadbeef") || strings.Count(logText, "bootout ") != 2 {
		t.Fatalf("unexpected launchctl targets: %s", logText)
	}
}

func TestProductionReleaseHealthFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name           string
		bad            bool
		digestMismatch bool
		wantOK         bool
	}{{name: "healthy", wantOK: true}, {name: "rollback failed", bad: true}, {name: "installed bytes differ", digestMismatch: true}} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			productionBinary := writeExecutable(t, root, "production-agent-loop", `#!/bin/sh
case "$1" in
  delivery)
    result=succeeded
    if [ "${BAD_HEALTH:-0}" = 1 ]; then result=rollback_failed; fi
    printf '{"phase":"succeeded","result":"%s","current":{"version":"v0.8.0","commit":"`+evidenceCommit+`"}}\n' "$result" ;;
  doctor) printf '%s\n' '{"schema_version":1,"ok":true,"diagnostics":[]}' ;;
  status) printf '%s\n' '{"worker_pool":{"active":0,"limit":1},"pending_requests":[],"state":{"state_revision":9,"supervisor":{"state":"idle"},"issues":{}}}' ;;
  version) printf '%s\n' '{"version":"v0.8.0","commit":"`+evidenceCommit+`"}' ;;
  *) exit 2 ;;
esac
`)
			stableDigest, err := fileDigest(productionBinary)
			if err != nil {
				t.Fatal(err)
			}
			if test.digestMismatch {
				stableDigest = strings.Repeat("a", 64)
			}
			artifactDir := filepath.Join(root, "artifacts")
			cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "production-release-health.sh"))
			cmd.Dir = repositoryRoot(t)
			cmd.Env = append(os.Environ(),
				"PRODUCTION_REPOSITORY_PATH="+root,
				"PRODUCTION_AGENT_LOOP_BINARY="+productionBinary,
				"HEALTH_ARTIFACT_DIR="+artifactDir,
				"RELEASE_TAG=v0.8.0",
				"RELEASE_COMMIT="+evidenceCommit,
				"STABLE_BINARY_SHA256="+stableDigest,
				"HEALTH_SOAK_SECONDS=0",
			)
			if test.bad {
				cmd.Env = append(cmd.Env, "BAD_HEALTH=1")
			}
			output, err := cmd.CombinedOutput()
			if (err == nil) != test.wantOK {
				t.Fatalf("err=%v output=%s", err, output)
			}
		})
	}
}

func TestProductionAssignmentHealthRequiresExactStableAssignmentsAndRollbackDrill(t *testing.T) {
	root := t.TempDir()
	operator := writeExecutable(t, root, "assignment-agent-loop", `#!/bin/sh
case "$1 $2 $3" in
  "delivery assignment status")
    printf '{"version":1,"assignments":[{"repository_id":"repo-a","assignment":{"repository_id":"repo-a","version":"v0.9.0","commit":"%s","artifact_sha256":"%s","slot":"/private/slot","generation":4,"previous":{"version":"v0.8.5"}},"runtime":{"digest":"%s","matches":true,"launchd":{"loaded":true,"running":true,"pid":1001}},"transaction":{"phase":"succeeded"},"fence_active":false}]}\n' "$RELEASE_COMMIT" "$STABLE_BINARY_SHA256" "$STABLE_BINARY_SHA256" ;;
  "delivery assignment verify")
    printf '%s\n' '{"version":1,"verified":true,"assignment":{"result":"verified"}}' ;;
  *)
    if [ "$1" = doctor ]; then
      printf '%s\n' '{"schema_version":1,"ok":true,"diagnostics":[]}'
    elif [ "$1" = status ]; then
      printf '%s\n' '{"worker_pool":{"active":0,"limit":1},"pending_requests":[],"state":{"state_revision":19,"supervisor":{"state":"polling"},"issues":{}}}'
    else
      exit 2
    fi ;;
esac
`)
	digest, err := fileDigest(operator)
	if err != nil {
		t.Fatal(err)
	}
	repositories := filepath.Join(root, "repositories.json")
	if err := os.WriteFile(repositories, []byte(`[{"repo_id":"repo-a","path":"/private/repo-a"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback := filepath.Join(root, "rollback.json")
	rollbackJSON := `{"schema_version":1,"typed_rollback":true,"same_artifact_reapplied":true,"before":{"version":"v0.9.0"},"rollback":{"result":"succeeded"},"reapplied":{"version":"v0.9.0"},"preserved":{"state":true,"issues":true,"leases":true,"worktrees":true},"other_repository_unchanged":{"assignment":true,"pid":true,"binary":true,"state_revision":true}}`
	if err := os.WriteFile(rollback, []byte(rollbackJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(root, "artifacts")
	cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "production-assignment-health.sh"))
	cmd.Dir = repositoryRoot(t)
	cmd.Env = append(os.Environ(),
		"PRODUCTION_AGENT_LOOP_BINARY="+operator,
		"PRODUCTION_REPOSITORIES_FILE="+repositories,
		"ROLLBACK_DRILL_FILE="+rollback,
		"HEALTH_ARTIFACT_DIR="+artifactDir,
		"RELEASE_TAG=v0.9.0",
		"RELEASE_COMMIT="+evidenceCommit,
		"STABLE_BINARY_SHA256="+digest,
		"HEALTH_SOAK_SECONDS=0",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("assignment health failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(artifactDir, "production-health-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		SchemaVersion int  `json:"schema_version"`
		Healthy       bool `json:"healthy"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.SchemaVersion != 2 || !report.Healthy {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestReleaseWorkflowPreservesRequiredGateChain(t *testing.T) {
	type step struct {
		Run  string `yaml:"run"`
		Uses string `yaml:"uses"`
	}
	type job struct {
		Needs       any    `yaml:"needs"`
		Environment string `yaml:"environment"`
		Steps       []step `yaml:"steps"`
	}
	var workflow struct {
		Jobs map[string]job `yaml:"jobs"`
	}
	path := filepath.Join(repositoryRoot(t), ".github", "workflows", "release.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"build-candidate":                 nil,
		"verify-reproducibility":          {"build-candidate"},
		"verify-attestation-and-manifest": {"verify-reproducibility"},
		"replay-production-fixtures":      {"verify-attestation-and-manifest"},
		"lifecycle-conformance":           {"replay-production-fixtures"},
		"cli-surface-contract":            {"lifecycle-conformance"},
		"isolated-canary":                 {"build-candidate", "cli-surface-contract"},
		"production-state-isolation":      {"build-candidate", "isolated-canary"},
		"candidate-integrity":             {"production-state-isolation"},
		"promotion-evidence":              {"candidate-integrity"},
		"promote-stable":                  {"build-candidate", "promotion-evidence"},
		"post-release-health":             {"promote-stable"},
	}
	for name, dependencies := range want {
		current, ok := workflow.Jobs[name]
		if !ok {
			t.Fatalf("required release job %q is missing", name)
		}
		if got := normalizedNeeds(current.Needs); !reflect.DeepEqual(got, dependencies) {
			t.Fatalf("job %s needs=%v want=%v", name, got, dependencies)
		}
		if len(current.Steps) == 0 {
			t.Fatalf("job %s has no executable steps", name)
		}
		for index, step := range current.Steps {
			if (step.Run == "") == (step.Uses == "") {
				t.Fatalf("job %s step %d must contain exactly one of run or uses", name, index+1)
			}
		}
	}
	text := string(data)
	if strings.Contains(text, "continue-on-error: true") {
		t.Fatal("release gate permits continue-on-error")
	}
	if strings.Count(text, "scripts/build-release.sh") != 2 {
		t.Fatal("release workflow must build the canonical candidate once and one comparison-only rebuild")
	}
	if !strings.Contains(text, `[[ "${RELEASE_TAG}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]`) ||
		strings.Contains(text, `([.-][0-9A-Za-z.-]+)?`) {
		t.Fatal("stable workflow accepts a prerelease tag suffix")
	}
	for _, required := range []string{
		`workflow_dispatch:`,
		`test "$(git rev-parse "${RELEASE_TAG}^{commit}")" = "${RELEASE_COMMIT}"`,
		`chmod 0755 "$CANDIDATE_BINARY"`,
		`chmod 0755 dist/stable/agent-loop_Darwin_arm64`,
		`gh release download "$candidate_tag" --repo "$GITHUB_REPOSITORY"`,
		`candidate-integrity/report.json`,
		`cmp "dist/prerelease/$asset" "dist/stable/$asset"`,
		`agent-loop_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json release-manifest.json checksums.txt cli-surface-report.json offline-contract-report.json production-state-report.json`,
		`mode:"machine-verifiable"`,
		`.candidate_sha256 == $digest`,
		`required_evidence:["cli-surface","offline-contract","production-isolation","candidate-integrity"]`,
		`subject-path: promotion-evidence.json`,
		`.assignment_protocol == 1`,
		`.rollout_mode == "per-repository-stable-assignment"`,
		`.rollback_drill.typed_rollback == true`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("release workflow is missing byte-promotion evidence %q", required)
		}
	}
	if strings.Contains(text, "sleep 900") || strings.Contains(text, "minute-30") {
		t.Fatal("release workflow contains a time-only artifact soak")
	}
	if workflow.Jobs["production-state-isolation"].Environment != "" || workflow.Jobs["promotion-evidence"].Environment != "" || workflow.Jobs["promote-stable"].Environment != "production" {
		t.Fatal("only stable publication may use the protected production environment")
	}
	if strings.Count(text, "environment: production") != 1 {
		t.Fatal("release workflow must enter the production environment exactly once")
	}
}

func TestContractWorkflowsRequireNoLongLivedSecrets(t *testing.T) {
	for _, path := range []string{
		".github/workflows/contracts.yml",
		".github/workflows/release.yml",
		"scripts/cli-surface-contract.sh",
		"scripts/offline-release-contract.sh",
		"scripts/production-assignment-health.sh",
	} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, forbidden := range []string{"secrets.CANARY_GITHUB_TOKEN", "secrets.OPENAI_API_KEY", "codex login"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s depends on forbidden credential flow %q", path, forbidden)
			}
		}
	}
	for _, path := range []string{"scripts/cli-surface-contract.sh", "scripts/offline-release-contract.sh"} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !strings.Contains(text, `[ -z "${CANARY_GITHUB_TOKEN:-}" ]`) || !strings.Contains(text, `[ -z "${OPENAI_API_KEY:-}" ]`) {
			t.Fatalf("%s does not fail closed when a forbidden credential is present", path)
		}
	}
}

func TestHighRiskReviewUsesMachineVerifiableEvidence(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	write := func(path, content string) {
		t.Helper()
		fullPath := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		runGit("add", ".")
		runGit("-c", "commit.gpgsign=false", "commit", "-m", message)
		return runGit("rev-parse", "HEAD")
	}
	runReview := func(base, head, name string) struct {
		HighRisk     bool `json:"high_risk"`
		FindingCount int  `json:"finding_count"`
	} {
		t.Helper()
		outputPath := filepath.Join(root, name+".json")
		cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "high-risk-review.sh"))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "BASE_SHA="+base, "HEAD_SHA="+head, "REVIEW_OUTPUT="+outputPath)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("review %s: %v: %s", name, err, output)
		}
		var report struct {
			HighRisk     bool `json:"high_risk"`
			FindingCount int  `json:"finding_count"`
		}
		data, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}

	runGit("init", "-q")
	runGit("config", "user.name", "review-test")
	runGit("config", "user.email", "review-test@example.invalid")
	write("README.md", "base\n")
	base := commit("base")
	write("docs/note.md", "ordinary documentation\n")
	lowRisk := commit("ordinary docs")
	if report := runReview(base, lowRisk, "low-risk"); report.HighRisk || report.FindingCount != 0 {
		t.Fatalf("low-risk report=%+v", report)
	}
	write(".github/workflows/test.yml", "name: test\n")
	write("docs/rollback.md", "rollback evidence\n")
	highRisk := commit("release workflow")
	if report := runReview(lowRisk, highRisk, "high-risk"); !report.HighRisk || report.FindingCount != 0 {
		t.Fatalf("high-risk report=%+v", report)
	}

	workflow, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "high-risk-review.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(workflow)
	for _, required := range []string{".finding_count == 0", "(.findings | length) == 0", "(.checks | all(.[]; . == true))", "if: always()"} {
		if !strings.Contains(text, required) {
			t.Fatalf("high-risk review workflow is missing fail-closed evidence check %q", required)
		}
	}
	for _, forbidden := range []string{"pull_request_review:", "latestOpinionatedReviews", `state == "APPROVED"`, "PR_AUTHOR"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("high-risk review workflow still requires unavailable human reviewer flow %q", forbidden)
		}
	}
}

func normalizedNeeds(value any) []string {
	switch typed := value.(type) {
	case nil:
		return nil
	case string:
		return []string{typed}
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, item.(string))
		}
		return result
	default:
		return []string{"<invalid>"}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(root))
}

func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mapsEqual(left, right map[string]any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

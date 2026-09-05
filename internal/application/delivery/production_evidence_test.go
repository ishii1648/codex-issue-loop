package delivery

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"gopkg.in/yaml.v3"
)

const evidenceCommit = "0123456789abcdef0123456789abcdef01234567"

func TestProductionStateIsolationRunsCredentiallessContractBetweenSnapshots(t *testing.T) {
	for _, test := range []struct {
		name       string
		mismatch   bool
		doctorMode string
		wantOK     bool
	}{
		{name: "identical", wantOK: true},
		{name: "intentionally stopped", doctorMode: "stopped", wantOK: true},
		{name: "stopped with unrelated failure", doctorMode: "stopped-corrupt"},
		{name: "revision changed", mismatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			ambientHome := filepath.Join(root, "ambient-home")
			if err := os.MkdirAll(ambientHome, 0o700); err != nil {
				t.Fatal(err)
			}
			productionRepo := filepath.Join(root, "production")
			if err := os.MkdirAll(filepath.Join(productionRepo, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			productionBinary := writeExecutable(t, root, "production-agent-loop", `#!/bin/sh
if [ "$1" = doctor ]; then
  case " $* " in
    *" --assignment-health "*) ;;
    *) printf '%s\n' 'assignment-scoped doctor flag is required' >&2; exit 9 ;;
  esac
  if [ "${DOCTOR_MODE:-}" = stopped ]; then
    printf '%s\n' '{"schema_version":1,"ok":false,"diagnostics":[{"code":"SUPERVISOR_STOPPED","ok":false}]}'
    exit 1
  fi
  if [ "${DOCTOR_MODE:-}" = stopped-corrupt ]; then
    printf '%s\n' '{"schema_version":1,"ok":false,"diagnostics":[{"code":"SUPERVISOR_STOPPED","ok":false},{"code":"STATE_CORRUPT","ok":false}]}'
    exit 1
  fi
  printf '%s\n' '{"schema_version":1,"ok":true,"diagnostics":[]}'
  exit
fi
count=0
if [ -f "$COUNT_PATH" ]; then count=$(cat "$COUNT_PATH"); fi
count=$((count + 1))
printf '%s\n' "$count" >"$COUNT_PATH"
revision=7
if [ "${MISMATCH:-0}" = 1 ] && [ "$count" -gt 1 ]; then revision=8; fi
supervisor=idle
active_workers=1
active_execution='{"issue_number":7,"run_id":"run_7","generation":1,"started_at":"2026-09-04T00:00:00Z"}'
case "${DOCTOR_MODE:-}" in stopped*) supervisor=stopped; active_workers=0; active_execution=null ;; esac
printf '{"worker_pool":{"active":%s,"limit":1,"issues":[]},"pending_requests":[],"state":{"repo_id":"production-id","state_revision":%s,"supervisor":{"state":"%s"},"active_execution":%s,"issues":{"7":{"generation":1,"run_id":"run_7","status":"running"}}}}\n' "$active_workers" "$revision" "$supervisor" "$active_execution"
`)
			candidate := writeExecutable(t, root, "candidate", "#!/bin/sh\nexit 0\n")
			fakeContract := writeExecutable(t, root, "offline-contract", `#!/bin/sh
[ "$HOME" != "$EXPECTED_AMBIENT_HOME" ]
[ -d "$HOME" ]
mkdir -p "$CONTRACT_ARTIFACT_DIR"
printf '%s\n' '{"schema_version":1,"mode":"credentialless-offline","home_isolated":true,"credentials":{"canary_github_token":false,"openai_api_key":false},"external_network":false,"sequences":[{"status":"completed"},{"status":"completed"}],"supervisor_starts":2,"webhook_fixture_replay":1,"transaction_crash_recovery":1,"final":{"active_workers":0,"active_executions":0,"pending_requests":0,"orphan_pid_pgid":0,"duplicate_prs":0,"duplicate_comment_markers":0}}' >"$CONTRACT_ARTIFACT_DIR/offline-contract-report.json"
`)
			artifactDir := filepath.Join(root, "artifacts")
			cmd := exec.Command("sh", filepath.Join(repositoryRoot(t), "scripts", "production-state-isolation.sh"))
			cmd.Dir = repositoryRoot(t)
			cmd.Env = append(os.Environ(),
				"HOME="+ambientHome,
				"EXPECTED_AMBIENT_HOME="+ambientHome,
				"PRODUCTION_REPOSITORY_PATH="+productionRepo,
				"PRODUCTION_AGENT_LOOP_BINARY="+productionBinary,
				"CANDIDATE_BINARY="+candidate,
				"CONTRACT_ARTIFACT_DIR="+artifactDir,
				"RELEASE_TAG=v0.8.0",
				"RELEASE_COMMIT="+evidenceCommit,
				"CANDIDATE_TAG=candidate-v0.8.0-123",
				"OFFLINE_CONTRACT_SCRIPT="+fakeContract,
				"COUNT_PATH="+filepath.Join(root, "count"),
				"DOCTOR_MODE="+test.doctorMode,
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
				SchemaVersion       int    `json:"schema_version"`
				PublicPayload       string `json:"public_payload"`
				Commit              string `json:"release_commit"`
				Digest              string `json:"candidate_binary_sha256"`
				StateAccessed       bool   `json:"production_state_accessed"`
				StateEqual          bool   `json:"production_state_equal"`
				PrivateEvidenceHash string `json:"private_evidence_sha256"`
				ProductionHealth    struct {
					DoctorSafe               bool `json:"doctor_safe"`
					WorkerLimitEnforced      bool `json:"worker_limit_enforced"`
					ActiveWorkersWithinLimit bool `json:"active_workers_within_limit"`
				} `json:"production_health"`
				Contract struct {
					Mode                       string `json:"mode"`
					CredentialsAbsent          bool   `json:"credentials_absent"`
					ExternalNetwork            bool   `json:"external_network"`
					LifecycleSequencesComplete bool   `json:"lifecycle_sequences_complete"`
					FinalResourcesClean        bool   `json:"final_resources_clean"`
				} `json:"offline_contract"`
			}
			if err := json.Unmarshal(data, &report); err != nil {
				t.Fatal(err)
			}
			var publicObject map[string]json.RawMessage
			if err := json.Unmarshal(data, &publicObject); err != nil {
				t.Fatal(err)
			}
			allowedPublicKeys := []string{
				"schema_version", "public_payload", "release_tag", "release_commit", "candidate_tag",
				"candidate_binary_sha256", "production_state_accessed", "production_state_changes",
				"production_state_equal", "production_health", "offline_contract", "private_evidence_sha256",
			}
			if len(publicObject) != len(allowedPublicKeys) {
				t.Fatalf("public report keys=%v", publicObject)
			}
			for _, key := range allowedPublicKeys {
				if _, ok := publicObject[key]; !ok {
					t.Fatalf("public report is missing %q: %s", key, data)
				}
			}
			if report.SchemaVersion != 2 || report.PublicPayload != "redacted-summary" ||
				report.Commit != evidenceCommit || len(report.Digest) != 64 || !report.StateAccessed || !report.StateEqual ||
				!report.ProductionHealth.DoctorSafe || !report.ProductionHealth.WorkerLimitEnforced || !report.ProductionHealth.ActiveWorkersWithinLimit ||
				report.Contract.Mode != "credentialless-offline" || !report.Contract.CredentialsAbsent || report.Contract.ExternalNetwork ||
				!report.Contract.LifecycleSequencesComplete || !report.Contract.FinalResourcesClean {
				t.Fatalf("report=%s", data)
			}
			for _, forbidden := range []string{"production_before", "production_after", "repo_id", "state_revision", "issue_count", "active_execution", "run_id"} {
				if strings.Contains(string(data), `"`+forbidden+`"`) {
					t.Fatalf("public report contains %q: %s", forbidden, data)
				}
			}
			privateData, err := os.ReadFile(filepath.Join(artifactDir, "production-state-private-evidence.json"))
			if err != nil {
				t.Fatal(err)
			}
			privateInfo, err := os.Stat(filepath.Join(artifactDir, "production-state-private-evidence.json"))
			if err != nil {
				t.Fatal(err)
			}
			if privateInfo.Mode().Perm() != 0o600 {
				t.Fatalf("private evidence mode=%#o", privateInfo.Mode().Perm())
			}
			privateHash := sha256.Sum256(privateData)
			if report.PrivateEvidenceHash != fmt.Sprintf("%x", privateHash) {
				t.Fatalf("private evidence hash=%s want=%x", report.PrivateEvidenceHash, privateHash)
			}
			var privateEvidence struct {
				Before   map[string]any `json:"production_before"`
				After    map[string]any `json:"production_after"`
				Contract map[string]any `json:"offline_contract"`
			}
			if err := json.Unmarshal(privateData, &privateEvidence); err != nil {
				t.Fatal(err)
			}
			if !mapsEqual(privateEvidence.Before, privateEvidence.After) || privateEvidence.Contract["home_isolated"] != true {
				t.Fatalf("private evidence=%s", privateData)
			}
			if test.doctorMode == "" {
				execution, ok := privateEvidence.Before["active_execution"].(map[string]any)
				if !ok || execution["issue_number"] != float64(7) || execution["run_id"] != "run_7" {
					t.Fatalf("active execution was not captured from the canonical status schema: %s", privateData)
				}
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
  status) printf '%s\n' '{"worker_pool":{"active":1,"limit":1},"pending_requests":[],"state":{"state_revision":9,"supervisor":{"state":"idle"},"active_execution":{"issue_number":7,"run_id":"run_7","generation":1},"issues":{"7":{"generation":1,"run_id":"run_7","status":"running"}}}}' ;;
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
			if test.wantOK {
				data, readErr := os.ReadFile(filepath.Join(artifactDir, "production-health-report.json"))
				if readErr != nil {
					t.Fatal(readErr)
				}
				var report struct {
					Status struct {
						ActiveExecutions int `json:"active_executions"`
					} `json:"status"`
				}
				if err := json.Unmarshal(data, &report); err != nil || report.Status.ActiveExecutions != 1 {
					t.Fatalf("release health did not read active_execution: report=%s err=%v", data, err)
				}
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
      printf '%s\n' '{"worker_pool":{"active":1,"limit":1},"pending_requests":[],"state":{"state_revision":19,"supervisor":{"state":"polling"},"active_execution":{"issue_number":7,"run_id":"run_7","generation":1},"issues":{"7":{"generation":1,"run_id":"run_7","status":"running"}}}}'
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
	rollbackJSON := `{"schema_version":1,"typed_rollback":true,"same_artifact_reapplied":true,"before":{"version":"v0.9.0"},"rollback":{"result":"succeeded"},"reapplied":{"version":"v0.9.0"},"preserved":{"state":true,"issues":true,"execution":true,"worktrees":true},"other_repository_unchanged":{"assignment":true,"pid":true,"binary":true,"state_revision":true}}`
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
		Repositories  []struct {
			Status struct {
				ActiveExecutions int `json:"active_executions"`
			} `json:"status"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(data, &report); err != nil || report.SchemaVersion != 2 || !report.Healthy || len(report.Repositories) != 1 || report.Repositories[0].Status.ActiveExecutions != 1 {
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
		"verify-stable-release":           {"promote-stable"},
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
	semanticPredicate := fmt.Sprintf(".semantic_contract_current == %d", statecontract.CurrentVersion)
	if strings.Count(text, semanticPredicate) != 1 {
		t.Fatalf("release workflow does not require current semantic contract %d", statecontract.CurrentVersion)
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
		`chmod 0755 dist/stable/agent-loop-monitor_Darwin_arm64`,
		`gh release download "$candidate_tag" --repo "$GITHUB_REPOSITORY"`,
		`candidate-integrity/report.json`,
		`cmp "dist/prerelease/$asset" "dist/stable/$asset"`,
		`agent-loop_Darwin_arm64 agent-loop-monitor_Darwin_arm64 agent-loop_Darwin_arm64.spdx.json release-manifest.json checksums.txt cli-surface-report.json offline-contract-report.json production-state-report.json`,
		`mode:"machine-verifiable"`,
		`.candidate_sha256 == $digest`,
		`required_evidence:["cli-surface","offline-contract","production-isolation","candidate-integrity"]`,
		`subject-path: promotion-evidence.json`,
		`.assignment_protocol == 1`,
		`.public_payload == "redacted-summary"`,
		`.production_health.doctor_safe == true`,
		`def exact_keys($expected)`,
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
	if strings.Contains(text, "post-release-health") || strings.Contains(text, "production-health-report.json") {
		t.Fatal("release workflow must not depend on repository rollout health")
	}
}

func TestRepositoryRolloutHealthIsAnIndependentWorkflow(t *testing.T) {
	path := filepath.Join(repositoryRoot(t), ".github", "workflows", "rollout.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run  string `yaml:"run"`
				Uses string `yaml:"uses"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	job, ok := workflow.Jobs["verify-rollout-health"]
	if !ok || len(job.Steps) == 0 {
		t.Fatal("rollout workflow has no verify-rollout-health job")
	}
	for index, step := range job.Steps {
		if (step.Run == "") == (step.Uses == "") {
			t.Fatalf("rollout workflow step %d must contain exactly one of run or uses", index+1)
		}
	}
	text := string(data)
	for _, required := range []string{
		`workflow_dispatch:`,
		`Existing stable release tag to verify after repository rollout`,
		`verify-rollout-health`,
		`production-health-report.json`,
		`.rollout_mode == "per-repository-stable-assignment"`,
		`.rollback_drill.typed_rollback == true`,
		`.rollback_drill.preserved.execution == true`,
		`.soak.duration_seconds == 300`,
		`subject-path: dist/stable/production-health-report.json`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("rollout workflow is missing %q", required)
		}
	}
	for _, forbidden := range []string{"gh release create", "promote-stable", "sleep 30", "environment: production", "preserved.leases", "active_leases"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("rollout workflow contains release coupling %q", forbidden)
		}
	}
}

func TestContractWorkflowsRequireNoLongLivedSecrets(t *testing.T) {
	for _, path := range []string{
		".github/workflows/contracts.yml",
		".github/workflows/release.yml",
		".github/workflows/rollout.yml",
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
	productionIsolation, err := os.ReadFile(filepath.Join(repositoryRoot(t), "scripts/production-state-isolation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`host_gomodcache=$(go env GOMODCACHE)`, `host_gocache=$(go env GOCACHE)`, `GOMODCACHE="$host_gomodcache"`, `GOCACHE="$host_gocache"`} {
		if !strings.Contains(string(productionIsolation), required) {
			t.Fatalf("production isolation does not preserve the primed Go cache through HOME isolation: missing %q", required)
		}
	}
	if !strings.Contains(string(productionIsolation), `doctor --repo "$production_repo" --assignment-health --json`) {
		t.Fatal("production isolation does not use assignment-scoped doctor")
	}
	for _, path := range []string{"scripts/offline-release-contract.sh", "scripts/production-state-isolation.sh", "scripts/production-assignment-health.sh", "scripts/production-release-health.sh"} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `execution_lease`) || !strings.Contains(string(data), `active_execution`) {
			t.Fatalf("%s does not consume the canonical active_execution status field", path)
		}
	}
	for _, path := range []string{".github/workflows/release.yml", ".github/workflows/rollout.yml"} {
		data, err := os.ReadFile(filepath.Join(repositoryRoot(t), path))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `active_leases`) || strings.Contains(string(data), `preserved.leases`) {
			t.Fatalf("%s retains removed lease evidence", path)
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
	runGit("checkout", "--detach", base)
	write(".github/workflows/test.yml", "name: base-only workflow\n")
	advancedBase := commit("advance base workflow")
	runGit("checkout", "--detach", base)
	write("monitor/queue.go", "package monitor\n")
	monitorHead := commit("monitor change")
	if report := runReview(advancedBase, monitorHead, "diverged-low-risk"); report.HighRisk || report.FindingCount != 0 {
		t.Fatalf("base-only workflow change affected monitor review: %+v", report)
	}
	if report := runReview(advancedBase, highRisk, "diverged-high-risk"); !report.HighRisk || report.FindingCount != 0 {
		t.Fatalf("head workflow change was not reviewed: %+v", report)
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

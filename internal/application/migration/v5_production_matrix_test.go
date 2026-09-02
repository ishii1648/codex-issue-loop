package migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestProductionDerivedV4RecoveryMatrixMigratesElevenIssuesAndFourteenSubstatesWithoutLoss(t *testing.T) {
	const beforeRevision = 900
	capturedAt := "2026-08-20T00:00:00Z"
	lease := func(runID string, generation int) map[string]any {
		return map[string]any{
			"owner": map[string]any{"run_id": runID, "generation": generation}, "slot": 0,
			"declared_resources": []string{"repo:*"}, "resolved_resources": []string{"repo:*"},
			"base_sha": fmt.Sprintf("%040d", generation), "reserved_at": capturedAt,
		}
	}
	issue := func(number int, status string) map[string]any {
		return map[string]any{
			"number": number, "status": status, "run_id": fmt.Sprintf("run_%d", number), "lease_generation": 1,
			"last_error": "sanitized production recovery boundary", "updated_at": capturedAt,
		}
	}
	legacy := func(item map[string]any, fields ...string) {
		for _, field := range fields {
			item[field] = map[string]any{"status": "succeeded"}
		}
	}

	issues := map[string]any{}
	for _, number := range []int{102, 107, 129, 134, 183, 65, 69, 93, 314, 442, 449} {
		status := "completed"
		if number == 183 || number == 65 || number == 69 || number == 93 || number == 314 || number == 449 {
			status = "blocked"
		}
		issues[fmt.Sprint(number)] = issue(number, status)
	}
	legacy(issues["102"].(map[string]any), "blocked_cause", "environment_resume", "publication_failure", "publication_recovery")
	legacy(issues["107"].(map[string]any), "blocked_cause", "environment_resume")
	legacy(issues["134"].(map[string]any), "blocked_cause", "merged_pull_request_adoption")
	legacy(issues["65"].(map[string]any), "workspace_provenance_recovery")
	legacy(issues["69"].(map[string]any), "workspace_provenance_recovery")
	legacy(issues["93"].(map[string]any), "workspace_provenance_recovery")
	legacy(issues["314"].(map[string]any), "blocked_cause", "workspace_provenance_recovery")
	legacy(issues["449"].(map[string]any), "workspace_provenance_recovery")

	issue183 := issues["183"].(map[string]any)
	issue183["lease"] = lease("run_183", 1)
	issue183["last_error"] = "stat sanitized worktree: no such file or directory"
	issue314 := issues["314"].(map[string]any)
	issue314["worktree"], issue314["branch"] = "/sanitized/worktrees/314", "codex/issue-314"
	issue314["workspace"] = map[string]any{"path": issue314["worktree"], "branch": issue314["branch"], "repo_id": "repo_production_matrix", "repository": "owner/repo", "git_common_dir": "/sanitized/repository/.git", "main_checkout": "/sanitized/repository", "captured_at": capturedAt}
	issue314["resource_park"] = map[string]any{
		"id": "park_314", "kind": "environment_block", "status": "parked", "original_lease": lease("run_314", 1), "parked_at": capturedAt,
	}
	issue449 := issues["449"].(map[string]any)
	issue449["lease_generation"] = 2
	issue449["worktree"], issue449["branch"] = "/sanitized/worktrees/449", "codex/issue-449"
	issue449["workspace"] = map[string]any{"path": issue449["worktree"], "branch": issue449["branch"], "repo_id": "repo_production_matrix", "repository": "owner/repo", "git_common_dir": "/sanitized/repository/.git", "main_checkout": "/sanitized/repository", "captured_at": capturedAt}
	issue449["lease"] = lease("run_449", 2)
	issue449["resource_park"] = map[string]any{
		"id": "park_449", "kind": "needs_input", "request_id": "req_449", "status": "resuming",
		"original_lease": lease("run_449", 1), "parked_at": capturedAt, "resumed_at": capturedAt,
		"resume_owner": map[string]any{"run_id": "run_449", "generation": 2},
	}
	issue449["answers"] = []map[string]any{{"request_id": "req_449", "question": "continue", "answer": "yes", "answered_at": capturedAt}}
	issue442 := issues["442"].(map[string]any)
	issue442["resource_park"] = map[string]any{
		"id": "park_442", "kind": "needs_input", "request_id": "req_historical_442", "status": "resumed",
		"original_lease": lease("run_442", 1), "parked_at": capturedAt, "resumed_at": capturedAt,
		"resume_owner": map[string]any{"run_id": "run_442", "generation": 2},
	}
	issue442["lease_generation"] = 2

	root := map[string]any{
		"version": 4, "semantic_contract_version": 1, "repo_id": "repo_production_matrix", "repo_path": "/sanitized/repository",
		"state_revision": beforeRevision, "supervisor": map[string]any{"state": "stopped", "updated_at": capturedAt}, "issues": issues,
		"pending_requests": map[string]any{
			"req_449": map[string]any{
				"id": "req_449", "issue_number": 449, "question": "continue", "run_id": "run_449", "resource_park_id": "park_449",
				"released_owner": map[string]any{"run_id": "run_449", "generation": 1}, "status": "answered", "answer": "yes",
				"created_at": capturedAt, "answered_at": capturedAt,
			},
		},
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	legacyBefore, _, answersBefore, generationsBefore := migratedRecoveryCounts(t, encoded)
	if len(issues) != 11 || legacyBefore != 14 || answersBefore != 1 || generationsBefore != 13 {
		t.Fatalf("production-derived fixture drifted: issues=%d legacy_substates=%d answers=%d generations=%d", len(issues), legacyBefore, answersBefore, generationsBefore)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	startedAt := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	if err := migrateState(path, journal{StartedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(migrated, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Issues) != 11 || len(snapshot.PendingRequests) != 1 || snapshot.StateRevision != beforeRevision+1 {
		t.Fatalf("migration cardinality changed: issues=%d requests=%d revision=%d", len(snapshot.Issues), len(snapshot.PendingRequests), snapshot.StateRevision)
	}
	legacyCount, terminalLeases, answerCount, generationTotal := migratedRecoveryCounts(t, migrated)
	if legacyCount != 0 || terminalLeases != 0 || answerCount != 1 || generationTotal != 13 {
		t.Fatalf("migration lost or retained data: legacy=%d terminal_leases=%d answers=%d generations=%d", legacyCount, terminalLeases, answerCount, generationTotal)
	}
	if snapshot.Issues["183"].Suspension.Recoverability != issuedomain.RecoverabilityNone || snapshot.Issues["183"].ResourcePark == nil {
		t.Fatalf("#183 stale lease was not converted to a non-executable checkpoint: %+v", snapshot.Issues["183"])
	}
	if snapshot.Issues["449"].Lease != nil || snapshot.Issues["449"].ResourcePark == nil || !containsResolution(snapshot.Issues["449"].Suspension.AllowedActions, issuedomain.ResolutionResume) {
		t.Fatalf("#449 was not converted to an operator-resumable suspension: %+v", snapshot.Issues["449"])
	}
}

func TestV5MigrationQuarantinesOnlyAmbiguousExecutingIssue(t *testing.T) {
	now := "2026-09-02T00:00:00Z"
	root := map[string]any{
		"version": 4, "semantic_contract_version": 1, "repo_id": "repo_isolation", "repo_path": "/sanitized/repository",
		"state_revision": 4, "supervisor": map[string]any{"state": "stopped", "updated_at": now},
		"issues": map[string]any{
			"1": map[string]any{"number": 1, "status": "running", "run_id": "run_1", "attempts": 1, "last_error": "lost authority", "updated_at": now},
			"2": map[string]any{"number": 2, "status": "completed", "updated_at": now},
		},
		"pending_requests": map[string]any{},
	}
	path := filepath.Join(t.TempDir(), "state.json")
	encoded, _ := json.Marshal(root)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateState(path, journal{StartedAt: time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	isolated := snapshot.Issues["1"]
	if isolated.Status != issuedomain.StatusBlocked || isolated.Lease != nil || isolated.Suspension == nil ||
		isolated.Suspension.Status != issuedomain.SuspensionQuarantined || isolated.Suspension.Recoverability != issuedomain.RecoverabilityAmbiguous {
		t.Fatalf("ambiguous Issue was not isolated: %+v", isolated)
	}
	if snapshot.Issues["2"].Status != issuedomain.StatusCompleted || snapshot.Issues["2"].Suspension != nil {
		t.Fatalf("unrelated Issue changed: %+v", snapshot.Issues["2"])
	}
}

func migratedRecoveryCounts(t *testing.T, data []byte) (legacyCount, terminalLeases, answerCount, generationTotal int) {
	t.Helper()
	var object struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	for _, item := range object.Issues {
		for _, field := range legacyRecoveryFields {
			if raw := item[field]; len(raw) > 0 && string(raw) != "null" {
				legacyCount++
			}
		}
		status := rawString(item["status"])
		if (status == "blocked" || status == "failed" || status == "completed") && len(item["execution_lease"]) > 0 {
			terminalLeases++
		}
		var answers []json.RawMessage
		_ = json.Unmarshal(item["answers"], &answers)
		answerCount += len(answers)
		var generation int
		_ = json.Unmarshal(item["lease_generation"], &generation)
		generationTotal += generation
	}
	return legacyCount, terminalLeases, answerCount, generationTotal
}

func containsResolution(actions []issuedomain.ResolutionAction, target issuedomain.ResolutionAction) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
}

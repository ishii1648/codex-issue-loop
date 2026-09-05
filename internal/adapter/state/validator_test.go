package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

const (
	invalidIssueStatus           issuedomain.Status                = "invalid-test-status"
	invalidEffectKind            issuedomain.EffectKind            = "invalid-test-effect"
	invalidConflictAttemptStatus issuedomain.ConflictAttemptStatus = "invalid-test-conflict-attempt"
)

func validSnapshotForInvariantTest() Snapshot {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	return Snapshot{
		Version: CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion, IssueLifecycleAPIVersion: issuedomain.LifecycleAPICurrent,
		RepoID: "repo-deadbeef", RepoPath: "/tmp/repo",
		Supervisor: Supervisor{State: SupervisorStateStopped, UpdatedAt: now},
		Issues:     map[string]*Issue{"1": {Number: 1}}, PendingEffects: map[string]*EffectIntent{}, QuarantinedIssues: map[string]*QuarantineRecord{},
		IntakeVerifications: map[string]*queuedomain.AuthorVerification{}, PendingRequests: map[string]*Request{},
	}
}

func TestV5DecodeRejectsRemovedRecoveryAxes(t *testing.T) {
	base, err := json.Marshal(validSnapshotForInvariantTest())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "blocked cause", mutate: func(issue map[string]any) { issue["blocked_cause"] = map[string]any{} }},
		{name: "legacy lease", mutate: func(issue map[string]any) { issue["lease"] = map[string]any{} }},
		{name: "legacy resource park", mutate: func(issue map[string]any) { issue["resource_park"] = map[string]any{} }},
		{name: "legacy checkpoint lease", mutate: func(issue map[string]any) {
			issue["continuation_checkpoint"] = map[string]any{"original_lease": map[string]any{}}
		}},
		{name: "environment resume", mutate: func(issue map[string]any) { issue["environment_resume"] = map[string]any{} }},
		{name: "answered workspace", mutate: func(issue map[string]any) { issue["answered_workspace_recovery"] = map[string]any{} }},
		{name: "workspace provenance", mutate: func(issue map[string]any) { issue["workspace_provenance_recovery"] = map[string]any{} }},
		{name: "publication failure", mutate: func(issue map[string]any) { issue["publication_failure"] = map[string]any{} }},
		{name: "publication recovery", mutate: func(issue map[string]any) { issue["publication_recovery"] = map[string]any{} }},
		{name: "checks failure", mutate: func(issue map[string]any) { issue["pull_request_checks_failure"] = map[string]any{} }},
		{name: "checks recovery", mutate: func(issue map[string]any) { issue["pull_request_checks_recovery"] = map[string]any{} }},
		{name: "merged Pull Request adoption", mutate: func(issue map[string]any) { issue["merged_pull_request_adoption"] = map[string]any{} }},
		{name: "environment status", mutate: func(issue map[string]any) { issue["status"] = "environment_resume_pending" }},
		{name: "publication status", mutate: func(issue map[string]any) { issue["status"] = "publication_recovery_pending" }},
		{name: "checks status", mutate: func(issue map[string]any) { issue["status"] = "pull_request_checks_recovery_pending" }},
		{name: "environment sync", mutate: func(issue map[string]any) { issue["github_sync"] = "environment_resume" }},
		{name: "answered sync", mutate: func(issue map[string]any) { issue["github_sync"] = "answered_workspace_recovery" }},
		{name: "publication sync", mutate: func(issue map[string]any) { issue["github_sync"] = "publication_recovery" }},
		{name: "checks sync", mutate: func(issue map[string]any) { issue["github_sync"] = "pull_request_checks_recovery" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var object map[string]any
			if err := json.Unmarshal(base, &object); err != nil {
				t.Fatal(err)
			}
			issue := object["issues"].(map[string]any)["1"].(map[string]any)
			test.mutate(issue)
			data, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			var decoded Snapshot
			if err := json.Unmarshal(data, &decoded); err == nil {
				t.Fatal("removed v5 recovery axis was accepted")
			}
		})
	}
}

func TestSnapshotValidateRejectsEveryCrossFieldInvariantClass(t *testing.T) {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "status and effect", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Status = issuedomain.StatusRunning
			snapshot.Issues["1"].RunID = "run_1"
			snapshot.PendingEffects["1"] = &EffectIntent{ID: "effect_1", IssueNumber: 1, RunID: "run_1", Kind: issuedomain.EffectMarkDone, CreatedAt: now}
		}},
		{name: "run and execution generation", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.Generation = issuedomain.StatusClaiming, "run_1", 2
			snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1, StartedAt: now}
		}},
		{name: "active execution and continuation", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.Generation = issuedomain.StatusBlocked, "run_1", 1
			issue.Continuation = &ContinuationCheckpoint{ID: "checkpoint_1", RunID: "run_1", Generation: 1, CreatedAt: now, Stage: issuedomain.ContinuationStageResume}
			snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1, StartedAt: now}
		}},
		{name: "multiple active lifecycles", mutate: func(snapshot *Snapshot) {
			for number := 1; number <= 2; number++ {
				key := string(rune('0' + number))
				runID := "run_" + key
				snapshot.Issues[key] = &Issue{Number: number, Status: issuedomain.StatusClaiming, RunID: runID, Generation: 1}
			}
			snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1, StartedAt: now}
		}},
		{name: "pending request answer", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 1, Status: issuedomain.RequestStatusPending, Answer: "already answered"}
		}},
		{name: "pending request resume status", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 1, Status: issuedomain.RequestStatusPending, ResumeStatus: invalidIssueStatus}
		}},
		{name: "pending request checkpoint identity", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 1, Status: issuedomain.RequestStatusPending, CheckpointID: "checkpoint_1"}
		}},
		{name: "workspace provenance", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.Attempts = issuedomain.StatusRunning, "run_1", 1
			issue.Worktree, issue.Branch = "/tmp/issue-1", "codex/issue-1"
			issue.Workspace = &WorkerWorkspace{Path: "/tmp/other", Branch: issue.Branch, RepoID: snapshot.RepoID, Repository: "owner/repo", GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: now}
		}},
		{name: "conflict recovery vocabulary", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].ConflictRecovery = &ConflictRecovery{History: []ConflictAttempt{{Number: 1, Status: invalidConflictAttemptStatus}}}
		}},
		{name: "worker process lifecycle", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.WorkerPID = issuedomain.StatusRunning, "run_1", 42
		}},
		{name: "Pull Request merge identity", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].PullRequestMerged = true
		}},
		{name: "Pull Request number and URL", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].PullRequestNumber = 1
		}},
		{name: "Pull Request review decision", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].ReviewDecision = "DISMISSED"
		}},
		{name: "retry and attempt counters", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Attempts = -1
		}},
		{name: "retry deadline", mutate: func(snapshot *Snapshot) {
			zero := time.Time{}
			snapshot.Issues["1"].RetryAfter = &zero
		}},
		{name: "continuation counter", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Continuations = -1
		}},
		{name: "unknown lifecycle vocabulary", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Status = invalidIssueStatus
		}},
		{name: "unknown effect vocabulary", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].RunID = "run_1"
			snapshot.PendingEffects["1"] = &EffectIntent{ID: "effect_1", IssueNumber: 1, RunID: "run_1", Kind: invalidEffectKind, CreatedAt: now}
		}},
		{name: "unknown semantic contract", mutate: func(snapshot *Snapshot) {
			snapshot.SemanticContractVersion++
		}},
		{name: "unknown schema", mutate: func(snapshot *Snapshot) {
			snapshot.Version++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validSnapshotForInvariantTest()
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("invalid aggregate was accepted")
			}
		})
	}
}

func TestSnapshotValidateAcceptsSupportedPullRequestReviewDecisions(t *testing.T) {
	for _, decision := range []string{"", "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"} {
		snapshot := validSnapshotForInvariantTest()
		snapshot.Issues["1"].ReviewDecision = decision
		if err := snapshot.Validate(); err != nil {
			t.Fatalf("decision=%q err=%v", decision, err)
		}
	}
}

func TestSnapshotValidateRejectsRunningWithoutProcessIdentity(t *testing.T) {
	now := time.Now().UTC()
	snapshot := validSnapshotForInvariantTest()
	item := snapshot.Issues["1"]
	item.Status, item.RunID, item.Generation = issuedomain.StatusRunning, "run_1", 1
	snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1, StartedAt: now}
	if err := snapshot.Validate(); err == nil || !strings.Contains(err.Error(), "running worker process identity is missing") {
		t.Fatalf("validation error=%v", err)
	}
}

func TestStoreUpdateQuarantinesInvalidIssueAndAllowsFollowingIssue(t *testing.T) {
	store := newStore(t)
	snapshot, err := store.Update("invalid", 1, "run_1", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"] = &Issue{Number: 1, Attempts: -1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Issues["1"] != nil || snapshot.QuarantinedIssues["1"] == nil || snapshot.QuarantinedIssues["1"].ReasonCode != "issue_invariant_violation" {
		t.Fatalf("invalid Issue was not isolated: %+v", snapshot.QuarantinedIssues["1"])
	}
	snapshot, err = store.Update("following_issue", 2, "run_2", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["2"] = &Issue{Number: 2, Title: "following Issue"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Issues["2"] == nil || snapshot.QuarantinedIssues["1"] == nil || snapshot.Supervisor.State == SupervisorStateBlocked {
		t.Fatalf("following Issue did not progress independently: %+v", snapshot)
	}
}

package state

import (
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

func TestApplyIssueTransitionFencesStaleDecision(t *testing.T) {
	transition, err := issuedomain.RetryConflict(issuedomain.StatusBlocked)
	if err != nil {
		t.Fatal(err)
	}
	item := &Issue{Status: issuedomain.StatusFailed}
	if err := ApplyIssueTransition(item, transition); err == nil {
		t.Fatal("expected stale transition to be rejected")
	}
	if item.Status != issuedomain.StatusFailed {
		t.Fatalf("stale transition changed status to %q", item.Status)
	}
}

func TestLifecycleBoundaryNormalizerAtomicallySplitsTerminalLease(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	item := &Issue{
		Number: 9, Status: issuedomain.StatusRunning, RunID: "run_9", Branch: "codex/issue-9",
		Worktree: "/managed/issue-9", Workspace: &WorkerWorkspace{Path: "/managed/issue-9", Branch: "codex/issue-9", RepoID: "repo"},
		Lease:     &ExecutionLease{Owner: LeaseOwner{RunID: "run_9", Generation: 3}, Slot: 0, ResolvedResources: []string{RepositoryResource}, ReservedAt: now},
		LastError: "worker stopped", UpdatedAt: now,
	}
	decision, err := issuedomain.Fail(item.Status, item.LastError, "issue", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyIssueTransition(item, decision.Transition); err != nil {
		t.Fatal(err)
	}
	item.LastError = "final worker reason"
	snapshot := Snapshot{Issues: map[string]*Issue{"9": item}}
	normalizeLifecycleBoundaries(&snapshot, now)
	if item.Lease != nil || item.ResourcePark == nil || item.Suspension == nil || item.ResourcePark.OriginalLease.Owner.Generation != 3 {
		t.Fatalf("terminal boundary did not split execution capacity from continuation state: %+v", item)
	}
	if item.Suspension.Reason != "final worker reason" || !item.Suspension.SuspendedAt.Equal(now) {
		t.Fatalf("terminal boundary captured stale outcome evidence: %+v", item.Suspension)
	}
}

func TestLifecycleBoundaryNormalizerKeepsAnsweredRequestAuditAfterCheckpointCleanup(t *testing.T) {
	now := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	answeredAt := now.Add(-time.Minute)
	originalOwner := LeaseOwner{RunID: "run_1", Generation: 1}
	resumeOwner := LeaseOwner{RunID: "run_1", Generation: 2}
	item := &Issue{
		Number: 1, Status: issuedomain.StatusAwaitingChecks, RunID: "run_1", LeaseGeneration: 2,
		Branch: "codex/issue-1", Worktree: "/managed/issue-1",
		Workspace: &WorkerWorkspace{Path: "/managed/issue-1", Branch: "codex/issue-1", RepoID: "repo"},
		Lease:     &ExecutionLease{Owner: resumeOwner, Slot: 0, ResolvedResources: []string{RepositoryResource}, ReservedAt: now},
		ResourcePark: &ContinuationCheckpoint{
			ID: "park_1", Kind: ResourceParkKindNeedsInput, RequestID: "req_1", Status: issuedomain.ResourceParkStatusResumed,
			OriginalLease: ExecutionLease{Owner: originalOwner, Slot: 0, ResolvedResources: []string{RepositoryResource}, ReservedAt: now.Add(-2 * time.Minute)},
			ParkedAt:      now.Add(-2 * time.Minute), ResumedAt: now.Add(-time.Minute), ResumeOwner: &resumeOwner, RunID: "run_1",
		},
		UpdatedAt: now,
	}
	decision, err := issuedomain.Complete(item.Status, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyIssueTransition(item, decision.Transition); err != nil {
		t.Fatal(err)
	}
	snapshot := Snapshot{
		Version: CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion,
		RepoID: "repo", RepoPath: "/repo", Issues: map[string]*Issue{"1": item},
		PendingRequests: map[string]*Request{"req_1": {
			ID: "req_1", IssueNumber: 1, RunID: "run_1", ResourceParkID: "park_1", ReleasedOwner: &originalOwner,
			Status: issuedomain.RequestStatusAnswered, Answer: "continue", AnsweredAt: &answeredAt,
		}},
	}
	normalizeLifecycleBoundaries(&snapshot, now)
	if item.Lease != nil || item.ResourcePark != nil || item.Suspension != nil {
		t.Fatalf("completed lifecycle retained active continuation state: %+v", item)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("completed answered continuation is invalid: %v", err)
	}
}

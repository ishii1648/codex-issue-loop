package state

import (
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
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

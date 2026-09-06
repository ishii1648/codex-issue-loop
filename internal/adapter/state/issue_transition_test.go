package state

import (
	"reflect"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestCancelQuarantinedIssueRetainsArtifactsAndOtherExecution(t *testing.T) {
	now := time.Now().UTC()
	saved := &Issue{Number: 1, RunID: "run_1", Generation: 2, Status: issuedomain.StatusBlocked,
		Branch: "codex/issue-1", Worktree: "/saved/worktree", SessionID: "session_1", PullRequestURL: "https://github.com/owner/repo/pull/2"}
	active := &ActiveExecution{IssueNumber: 3, RunID: "run_3", Generation: 1}
	snapshot := Snapshot{Issues: map[string]*Issue{}, QuarantinedIssues: map[string]*QuarantineRecord{
		"1": {IssueNumber: 1, RunID: "run_1", Generation: 2, LastValid: saved},
	}, ActiveExecution: active}
	if err := CancelQuarantinedIssue(&snapshot, 1, now); err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if item == nil || item.Status != issuedomain.StatusCanceled || item.RunID != saved.RunID || item.Generation != saved.Generation ||
		item.Branch != saved.Branch || item.Worktree != saved.Worktree || item.SessionID != saved.SessionID || item.PullRequestURL != saved.PullRequestURL ||
		item.Cancellation == nil || item.Cancellation.Source != "operator_quarantine_resolution" || snapshot.QuarantinedIssues["1"] != nil ||
		snapshot.ActiveExecution != active || saved.Status != issuedomain.StatusBlocked {
		t.Fatalf("quarantine cancellation lost evidence or execution ownership: %+v", snapshot)
	}
}

func TestCancelQuarantinedIssueRejectsRetainedExecutionWithoutMutation(t *testing.T) {
	record := &QuarantineRecord{IssueNumber: 1, RunID: "run_1", Generation: 2}
	active := &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 2}
	snapshot := Snapshot{Issues: map[string]*Issue{}, QuarantinedIssues: map[string]*QuarantineRecord{"1": record}, ActiveExecution: active}
	if err := CancelQuarantinedIssue(&snapshot, 1, time.Now().UTC()); err == nil {
		t.Fatal("canceled quarantine retaining execution authority")
	}
	if len(snapshot.Issues) != 0 || !reflect.DeepEqual(snapshot.QuarantinedIssues, map[string]*QuarantineRecord{"1": record}) || snapshot.ActiveExecution != active {
		t.Fatal("rejected cancellation changed state")
	}
}

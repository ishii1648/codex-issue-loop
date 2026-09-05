package state

import (
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func legacyAnsweredLaunchFixture() Snapshot {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	snapshot := validSnapshotForInvariantTest()
	workspace := &WorkerWorkspace{
		Path: "/tmp/repo", Branch: "codex/issue-1", RepoID: snapshot.RepoID, Repository: "owner/repo",
		GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: now,
	}
	snapshot.Issues["1"] = &Issue{
		Number: 1, Status: issuedomain.StatusRunning, RunID: "run_1", Generation: 4, Attempts: 2,
		Branch: workspace.Branch, Worktree: workspace.Path, Workspace: workspace, UpdatedAt: now,
		Continuation: &ContinuationCheckpoint{
			ID: "checkpoint_1", Kind: ContinuationKindNeedsInput, RequestID: "req_1", CreatedAt: now,
			RunID: "run_1", Generation: 3, Stage: issuedomain.ContinuationStageResume,
		},
		Answers: []AnswerRecord{{RequestID: "req_1", Question: "Continue?", Answer: "yes", AnsweredAt: now}},
	}
	snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 4, StartedAt: now}
	snapshot.PendingRequests["req_1"] = &Request{
		ID: "req_1", IssueNumber: 1, Question: "Continue?", RunID: "run_1", CheckpointID: "checkpoint_1",
		ReleasedExecution: &ExecutionIdentity{RunID: "run_1", Generation: 3}, Status: issuedomain.RequestStatusAnswered,
		Answer: "yes", CreatedAt: now, AnsweredAt: &now,
	}
	return snapshot
}

func TestNormalizeLegacyWorkerLaunchesRestoresOnlyMatchingAnsweredResumeAuthority(t *testing.T) {
	tests := []struct {
		name        string
		mutate      func(*Snapshot)
		wantStatus  issuedomain.Status
		wantSource  issuedomain.Status
		wantInvalid bool
	}{
		{name: "matching answered resume", wantStatus: issuedomain.StatusLaunching, wantSource: issuedomain.StatusResumePending},
		{name: "stale callback generation", mutate: func(snapshot *Snapshot) {
			snapshot.ActiveExecution.Generation--
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "continuation run mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Continuation.RunID = "run_stale"
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "continuation generation mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Continuation.Generation--
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "answer mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Answers[0].Answer = "no"
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "released generation mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"].ReleasedExecution.Generation = 2
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "released run mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"].ReleasedExecution.RunID = "run_stale"
		}, wantStatus: issuedomain.StatusBlocked},
		{name: "request mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"].ID = "req_stale"
		}, wantStatus: issuedomain.StatusBlocked, wantInvalid: true},
		{name: "ordinary retry without answer evidence", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Continuation.Kind, issue.Continuation.RequestID = "", ""
			issue.Answers = nil
			snapshot.PendingRequests = map[string]*Request{}
		}, wantStatus: issuedomain.StatusLaunching, wantSource: issuedomain.StatusRetryWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := legacyAnsweredLaunchFixture()
			if test.mutate != nil {
				test.mutate(&snapshot)
			}
			NormalizeLegacyWorkerLaunches(&snapshot)
			issue := snapshot.Issues["1"]
			if issue.Status != test.wantStatus || issue.LaunchSource != test.wantSource {
				t.Fatalf("issue=%+v", issue)
			}
			if test.wantStatus == issuedomain.StatusBlocked {
				if snapshot.ActiveExecution != nil || issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionQuarantined ||
					issue.Suspension.Recoverability != issuedomain.RecoverabilityAmbiguous {
					t.Fatalf("ambiguous launch was not isolated: active=%+v issue=%+v", snapshot.ActiveExecution, issue)
				}
			} else if snapshot.ActiveExecution == nil {
				t.Fatal("proven launch authority was released")
			}
			if err := snapshot.Validate(); (err != nil) != test.wantInvalid {
				t.Fatalf("normalized snapshot is invalid: %v", err)
			}
		})
	}
}

func TestValidatorRejectsInconsistentAnsweredLaunchEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "forged retry source", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].LaunchSource = issuedomain.StatusRetryWait
		}},
		{name: "released generation mismatch", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"].ReleasedExecution.Generation--
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := legacyAnsweredLaunchFixture()
			NormalizeLegacyWorkerLaunches(&snapshot)
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("inconsistent answered continuation evidence was accepted")
			}
		})
	}
}

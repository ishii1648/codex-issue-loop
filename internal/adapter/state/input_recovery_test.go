package state

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rejectedInputFixture(now time.Time) *QuarantineRecord {
	workspace := &WorkerWorkspace{Path: "/tmp/issue-1", Branch: "codex/issue-1", RepoID: "repo-deadbeef", Repository: "owner/repo", GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: now}
	return &QuarantineRecord{IssueNumber: 1, RunID: "run_1", Generation: 6, RejectedStatus: issuedomain.StatusNeedsInput, ReasonCode: "issue_invariant_violation", Reason: "suspension checkpoint identity is inconsistent", QuarantinedAt: now,
		LastValid: &Issue{Number: 1, RunID: "run_1", Generation: 6, Status: issuedomain.StatusRunning, Worktree: workspace.Path, Branch: workspace.Branch, Workspace: workspace, Session: &WorkerSession{Backend: "codex", ID: "session_original"}, SessionID: "session_original", WorkerPID: 90001, WorkerPGID: 90001,
			Continuation: &ContinuationCheckpoint{ID: "checkpoint_old", Session: &WorkerSession{Backend: "codex", ID: "session_original"}, BaseSHA: strings.Repeat("a", 40), Workspace: workspace},
			Suspension:   &Suspension{Status: issuedomain.SuspensionResolved, CheckpointID: "checkpoint_old"}},
		Requests: []*Request{{ID: "req_original", IssueNumber: 1, RunID: "run_1", CheckpointID: "checkpoint_new", ReleasedExecution: &ExecutionIdentity{RunID: "run_1", Generation: 6}, Question: "Which source?", Status: issuedomain.RequestStatusPending, CreatedAt: now}},
	}
}

func TestRestoreInputPreservesMailboxAndOriginalSession(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()
	_, err := store.Update("fixture", 1, "run_1", nil, func(s *Snapshot) error { s.QuarantinedIssues["1"] = rejectedInputFixture(now); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RecordAnswer("req_original", "Find a dated source", now); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Update("restore", 1, "run_1", nil, func(s *Snapshot) error {
		return RestoreInput(s, 1, strings.Repeat("b", 40), strings.Repeat("c", 64), now)
	})
	if err != nil {
		t.Fatal(err)
	}
	item := restored.Issues["1"]
	if item == nil || item.Status != issuedomain.StatusNeedsInput || item.Suspension != nil || item.Session.ID != "session_original" || item.Generation != 6 || restored.ActiveExecution != nil || len(restored.QuarantinedIssues) != 0 {
		t.Fatalf("invalid restored boundary: %+v", item)
	}
	resumed, err := store.PrepareAnsweredRequests(now)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Issues["1"].Status != issuedomain.StatusResumePending || len(resumed.Issues["1"].Answers) != 1 || resumed.Issues["1"].Answers[0].Answer != "Find a dated source" {
		t.Fatalf("answer lost: %+v", resumed.Issues["1"])
	}
}

func TestInputRecoveryRejectsOtherQuarantines(t *testing.T) {
	for _, change := range []func(*QuarantineRecord){
		func(q *QuarantineRecord) { q.Requests[0].ReleasedExecution.Generation++ },
		func(q *QuarantineRecord) { q.Requests = append(q.Requests, q.Requests[0]) },
		func(q *QuarantineRecord) { q.LastValid.Suspension.Status = issuedomain.SuspensionActive },
		func(q *QuarantineRecord) { q.LastValid.PullRequestURL = "https://example.com/pr" },
		func(q *QuarantineRecord) { q.LastValid.Session = nil },
		func(q *QuarantineRecord) { q.Requests[0].CheckpointID = q.LastValid.Continuation.ID },
	} {
		q := rejectedInputFixture(time.Now())
		change(q)
		if _, err := InputRecoveryCandidate(q); err == nil {
			t.Fatal("ambiguous recovery accepted")
		}
	}
}

func TestAnsweredPublicationBoundaryPreservesQuestionIdentity(t *testing.T) {
	snapshot := legacyAnsweredLaunchFixture()
	item := snapshot.Issues["1"]
	questionCheckpoint := *item.Continuation
	request := *snapshot.PendingRequests["req_1"]
	answers := append([]AnswerRecord(nil), item.Answers...)
	item.Status = issuedomain.StatusAwaitingChecks
	item.HeadSHA = strings.Repeat("a", 40)
	item.PullRequestURL, item.PullRequestNumber = "https://example.test/pull/1", 1
	finalizeLifecycleBoundaries(&snapshot, time.Now().UTC())
	checkpoint := item.Continuation
	if snapshot.ActiveExecution != nil || checkpoint.ID == questionCheckpoint.ID || checkpoint.Kind != "" || checkpoint.RequestID != "" || checkpoint.Generation != 4 || checkpoint.Stage != issuedomain.ContinuationStageChecks || checkpoint.HeadSHA != item.HeadSHA {
		t.Fatalf("publication reused answer authority: %+v", checkpoint)
	}
	if !reflect.DeepEqual(request, *snapshot.PendingRequests["req_1"]) || !reflect.DeepEqual(answers, item.Answers) || request.ReleasedExecution.Generation != 3 {
		t.Fatal("publication changed the saved question or answer")
	}
}

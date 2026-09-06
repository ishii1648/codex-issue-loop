package state

import (
	"reflect"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestQuarantinedMailboxRecordsAnswerAcrossRestartWithoutResuming(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()
	before, err := store.Update("fixture", 1, "run_1", nil, func(s *Snapshot) error {
		s.QuarantinedIssues["1"] = &QuarantineRecord{IssueNumber: 1, RunID: "run_1", Generation: 6, ReasonCode: "issue_invariant_violation", Reason: "checkpoint mismatch", QuarantinedAt: now, Requests: []*Request{{ID: "req_question", IssueNumber: 1, RunID: "run_1", Question: "Which source?", Status: issuedomain.RequestStatusPending, CreatedAt: now}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := before.Attention(false, true); !ok || reason != "needs_input" {
		t.Fatalf("question hidden: %s %v", reason, ok)
	}
	answered, request, err := store.RecordAnswer("req_question", "Find a dated source", now.Add(time.Second))
	if err != nil || request.Answer != "Find a dated source" {
		t.Fatalf("request=%+v err=%v", request, err)
	}
	restarted := Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}
	after, err := restarted.PrepareAnsweredRequests(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if after.ActiveExecution != nil || after.Issues["1"] != nil || !reflect.DeepEqual(after.QuarantinedIssues, answered.QuarantinedIssues) {
		t.Fatal("receipt changed execution recovery")
	}
	duplicate, _, err := restarted.RecordAnswer(request.ID, request.Answer, now.Add(time.Minute))
	if err != nil || duplicate.StateRevision != answered.StateRevision {
		t.Fatalf("duplicate was not idempotent: %v", err)
	}
	if _, _, err := restarted.RecordAnswer(request.ID, "different", now); err == nil {
		t.Fatal("answer overwritten")
	}
	if _, _, err := restarted.RecordAnswer("req_unknown", "yes", now); err == nil {
		t.Fatal("unknown request accepted")
	}
}

func TestIntakeQuestionBlocksAdmissionUntilAnsweredAndPreservesAnswer(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()
	before, question, err := store.AskIntake(1, "scope", "Which scope?", "The acceptance criteria depend on it", "small", nil, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if !before.NeedsHuman(1, true) || before.ActiveExecution != nil {
		t.Fatal("intake requires a worker")
	}
	if _, _, err := store.StartExecution(ExecutionStart{IssueNumber: 1, RunID: "run_1", StartedAt: now}); err == nil {
		t.Fatal("unanswered Issue was admitted")
	}
	if _, _, err := store.RecordAnswer(question.ID, "small", now); err != nil {
		t.Fatal(err)
	}
	ready, err := store.PrepareAnsweredRequests(now)
	if err != nil || ready.ActiveExecution != nil || ready.NeedsHuman(1, true) {
		t.Fatalf("answer changed admission: %v", err)
	}
	started, _, err := store.StartExecution(ExecutionStart{IssueNumber: 1, RunID: "run_1", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(started.Issues["1"].Answers) != 1 || started.Issues["1"].Answers[0].Answer != "small" {
		t.Fatal("intake answer not supplied to first worker")
	}
}

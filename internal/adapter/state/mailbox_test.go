package state

import (
	"fmt"
	"reflect"
	"strings"
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

func TestMailboxAnswerValidationIsSharedAcrossSources(t *testing.T) {
	for _, remote := range []bool{false, true} {
		for _, test := range []struct {
			name, answer    string
			freeText, valid bool
		}{
			{"option", "safe", false, true},
			{"option label", "Safe mode", false, false},
			{"free text", "explanation", true, true},
			{"not allowed", "explanation", false, false},
			{"empty", "   ", true, false},
			{"control", "bad\x00answer", true, false},
			{"utf8", string([]byte{0xff}), true, false},
			{"oversize", strings.Repeat("a", MaxAnswerBytes+1), true, false},
			{"secret", "private-fixture-secret", true, false},
		} {
			t.Run(fmt.Sprintf("%t/%s", remote, test.name), func(t *testing.T) {
				store := newStore(t)
				store.Secrets = []string{"private-fixture-secret"}
				now := time.Now().UTC()
				before, request, err := store.AskIntake(1, "test", "Choose", "", "", []Option{{ID: "safe", Label: "Safe mode"}}, test.freeText, now)
				if err != nil {
					t.Fatal(err)
				}
				var provenance *AnswerProvenance
				if remote {
					provenance = &AnswerProvenance{Source: "github_issue_comment", CommentID: 42, Actor: "operator", Permission: "write",
						RequestID: request.ID, IssueNumber: 1, BodySHA256: BodyDigest(test.answer), CommentedAt: now, CommentEdited: now}
				}
				_, _, err = store.RecordAnswer(request.ID, test.answer, now, provenance)
				if (err == nil) != test.valid {
					t.Fatalf("valid=%t err=%v", test.valid, err)
				}
				after, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !test.valid && after.StateRevision != before.StateRevision {
					t.Fatal("invalid answer changed state")
				}
				if after.ActiveExecution != nil || len(after.Issues["1"].Answers) != 0 {
					t.Fatal("receipt delivered to worker")
				}
			})
		}
	}
}

func TestFaultConcurrentAnswerReceiptCommitsOneObservation(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()
	before, request, err := store.AskIntake(1, "test", "Choose", "", "", nil, true, now)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, id := range []int64{1, 2} {
		go func(id int64) {
			<-start
			provenance := &AnswerProvenance{Source: "github_issue_comment", CommentID: id, Actor: "operator", Permission: "write",
				RequestID: request.ID, IssueNumber: 1, BodySHA256: BodyDigest("answer"), CommentedAt: now, CommentEdited: now}
			_, _, err := store.RecordAnswer(request.ID, "answer", now, provenance)
			results <- err
		}(id)
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if successes != 1 || after.StateRevision != before.StateRevision+1 || after.ActiveExecution != nil {
		t.Fatalf("successes=%d revision=%d", successes, after.StateRevision)
	}
	accepted := after.PendingRequests[request.ID]
	restarted := Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}
	duplicate, _, err := restarted.RecordAnswer(request.ID, "answer", now, accepted.AnswerProvenance)
	if err != nil || duplicate.StateRevision != after.StateRevision {
		t.Fatalf("duplicate changed receipt: %v", err)
	}
}

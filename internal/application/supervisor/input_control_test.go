package supervisor

import (
	"context"
	"errors"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type fakeInputControlGitHub struct {
	*fakeGitHub
	comments     []gh.InputComment
	acks         []gh.InputAcknowledgement
	syncs        int
	unauthorized bool
	ackError     error
}

func (f *fakeInputControlGitHub) SyncInputRequest(context.Context, config.Config, state.Request) error {
	f.syncs++
	return nil
}

func (f *fakeInputControlGitHub) ListInputComments(context.Context, config.Config, int) ([]gh.InputComment, error) {
	return append([]gh.InputComment(nil), f.comments...), nil
}

func (f *fakeInputControlGitHub) VerifyInputActor(context.Context, config.Config, gh.InputComment) (gh.AuthorVerification, error) {
	return gh.AuthorVerification{Trusted: !f.unauthorized, Login: "operator", Permission: "write", Reason: "repository_permission"}, nil
}

func (f *fakeInputControlGitHub) SyncInputAcknowledgement(_ context.Context, _ config.Config, _ int, ack gh.InputAcknowledgement) error {
	if f.ackError != nil {
		return f.ackError
	}
	f.acks = append(f.acks, ack)
	return nil
}

func TestParseAnswerCommandIsStrict(t *testing.T) {
	tests := []struct {
		name              string
		body              string
		request, answer   string
		recognized, valid bool
	}{
		{name: "valid", body: "/agent-loop answer req_123 safe", request: "req_123", answer: "safe", recognized: true, valid: true},
		{name: "multiline free text", body: "/agent-loop answer req_123 first\nsecond", request: "req_123", answer: "first\nsecond", recognized: true, valid: true},
		{name: "casual", body: "I think req_123 should use safe"},
		{name: "leading text", body: "please /agent-loop answer req_123 safe"},
		{name: "wrong verb", body: "/agent-loop approve req_123 safe", recognized: true},
		{name: "double space", body: "/agent-loop answer  req_123 safe", recognized: true},
		{name: "trailing whitespace", body: "/agent-loop answer req_123 safe ", request: "req_123", answer: "safe", recognized: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, answer, recognized, valid := parseAnswerCommand(test.body)
			if request != test.request || answer != test.answer || recognized != test.recognized || valid != test.valid {
				t.Fatalf("got request=%q answer=%q recognized=%t valid=%t", request, answer, recognized, valid)
			}
		})
	}
}

func TestFaultGitHubAnswerCrashReplayUsesOneCanonicalTransition(t *testing.T) {
	loop, baseGitHub := testLoop(t, worker.Result{})
	now := time.Date(2026, 9, 5, 1, 2, 3, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	owner := state.ExecutionIdentity{RunID: "run_1", Generation: 1}
	request := &state.Request{
		ID: "req_1", IssueNumber: 1, RunID: owner.RunID, Question: "Choose?", Options: []state.Option{{ID: "safe", Label: "Safe"}},
		CheckpointID: "checkpoint_1", ReleasedExecution: &owner, Status: issuedomain.RequestStatusPending, CreatedAt: now,
	}
	_, err := loop.Store.Update("input_requested", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		workspace := fixtureWorkspace(loop, loop.Config.RepoPath, "codex/issue-1-test")
		workspace.CapturedAt = now
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: issuedomain.StatusNeedsInput, RunID: owner.RunID, Generation: 1,
			Worktree: loop.Config.RepoPath, Branch: "codex/issue-1-test", Workspace: workspace,
			Continuation: &state.ContinuationCheckpoint{
				ID: request.CheckpointID, Kind: state.ContinuationKindNeedsInput, RequestID: request.ID, CreatedAt: now,
				RunID: owner.RunID, Generation: 1, Workspace: workspace, Stage: issuedomain.ContinuationStageResume,
			},
			UpdatedAt: now,
		}
		snapshot.PendingRequests[request.ID] = request
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	control := &fakeInputControlGitHub{fakeGitHub: baseGitHub, comments: []gh.InputComment{{
		ID: 42, Body: "/agent-loop answer req_1 safe", Actor: "operator", ActorType: "User", CreatedAt: now, UpdatedAt: now,
	}}}
	loop.GitHub = control
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	first, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	answered := first.PendingRequests[request.ID]
	if first.Issues["1"].Status != issuedomain.StatusNeedsInput || answered.Status != issuedomain.RequestStatusAnswered ||
		answered.AnswerProvenance == nil || answered.AnswerProvenance.CommentID != 42 || len(first.Issues["1"].Answers) != 0 ||
		len(control.acks) != 1 || control.acks[0].Outcome != "accepted" {
		t.Fatalf("snapshot=%+v request=%+v acks=%+v", first.Issues["1"], answered, control.acks)
	}
	if answered.AnswerProvenance.Source != "github_issue_comment" || answered.AnswerProvenance.Actor != "operator" ||
		answered.AnswerProvenance.Permission != "write" || answered.AnswerProvenance.BodySHA256 == "" {
		t.Fatalf("provenance=%+v", answered.AnswerProvenance)
	}
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	replayed, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if replayed.StateRevision != first.StateRevision || len(replayed.Issues["1"].Answers) != 0 || control.acks[len(control.acks)-1].Outcome != "accepted" {
		t.Fatalf("replay changed canonical answer: first=%d replay=%d answers=%d acks=%+v", first.StateRevision, replayed.StateRevision, len(replayed.Issues["1"].Answers), control.acks)
	}
	control.comments = []gh.InputComment{
		{ID: 42, Body: "/agent-loop answer req_1 safe", Actor: "operator", ActorType: "User", CreatedAt: now, UpdatedAt: now},
		{ID: 43, Body: "/agent-loop answer req_1 safe", Actor: "operator", ActorType: "User", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
		{ID: 44, Body: "/agent-loop answer req_1 unsafe", Actor: "operator", ActorType: "User", CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)},
		{ID: 45, Body: "/agent-loop approve req_1 safe", Actor: "operator", ActorType: "User", CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)},
		{ID: 46, Body: "/agent-loop answer req_1 safe", Actor: "loop[bot]", ActorType: "Bot", CreatedAt: now.Add(4 * time.Second), UpdatedAt: now.Add(4 * time.Second)},
		{ID: 47, Body: "/agent-loop answer req_stale safe", Actor: "operator", ActorType: "User", CreatedAt: now.Add(5 * time.Second), UpdatedAt: now.Add(5 * time.Second)},
	}
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	wantOutcomes := []string{"accepted", "stale", "conflict", "malformed", "unauthorized", "stale"}
	got := control.acks[len(control.acks)-len(wantOutcomes):]
	for index, want := range wantOutcomes {
		if got[index].Outcome != want {
			t.Fatalf("ack %d=%+v, want outcome %s", index, got[index], want)
		}
	}
	afterOutcomes, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if afterOutcomes.StateRevision != first.StateRevision || len(afterOutcomes.Issues["1"].Answers) != 0 {
		t.Fatalf("rejected commands changed canonical state: revision=%d answers=%d", afterOutcomes.StateRevision, len(afterOutcomes.Issues["1"].Answers))
	}
	control.comments = []gh.InputComment{{
		ID: 42, Body: "/agent-loop answer req_1 unsafe", Actor: "operator", ActorType: "User", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
	}}
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if control.acks[len(control.acks)-1].Outcome != "conflict" {
		t.Fatalf("edited accepted comment was not rejected: %+v", control.acks[len(control.acks)-1])
	}
	control.comments = nil
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if afterDelete.StateRevision != first.StateRevision || len(afterDelete.Issues["1"].Answers) != 0 {
		t.Fatalf("deleted accepted comment changed canonical state: revision=%d answers=%d", afterDelete.StateRevision, len(afterDelete.Issues["1"].Answers))
	}
}

func TestGitHubInputMailboxValidation(t *testing.T) {
	for _, test := range []struct {
		name, answer, outcome                                 string
		unauthorized, otherIssue, older, quarantine, staleRun bool
	}{
		{name: "option", answer: "safe", outcome: "accepted"},
		{name: "permission", answer: "safe", outcome: "unauthorized", unauthorized: true},
		{name: "unknown option", answer: "unknown", outcome: "malformed"},
		{name: "control", answer: "sa\x00fe", outcome: "malformed"},
		{name: "oversize", answer: strings.Repeat("a", state.MaxAnswerBytes+1), outcome: "malformed"},
		{name: "other Issue", answer: "safe", outcome: "stale", otherIssue: true},
		{name: "before request", answer: "safe", outcome: "stale", older: true},
		{name: "quarantined", answer: "safe", outcome: "accepted", quarantine: true},
		{name: "old run", answer: "safe", outcome: "stale", quarantine: true, staleRun: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			loop, base := testLoop(t, worker.Result{})
			now := time.Now().UTC()
			_, request, err := loop.Store.AskIntake(1, "test", "Choose", "", "safe", []state.Option{{ID: "safe", Label: "Safe"}}, false, now)
			if err != nil {
				t.Fatal(err)
			}
			number := 1
			if test.otherIssue {
				number = 2
				if _, _, err := loop.Store.AskIntake(2, "other", "Choose", "", "", nil, true, now); err != nil {
					t.Fatal(err)
				}
			}
			if test.quarantine {
				_, err = loop.Store.Update("fixture", 1, "", nil, func(s *state.Snapshot) error {
					s.QuarantinedIssues["1"] = &state.QuarantineRecord{IssueNumber: 1, ReasonCode: "issue_invariant_violation", Reason: "checkpoint mismatch", QuarantinedAt: now, Requests: []*state.Request{s.PendingRequests[request.ID]}}
					if test.staleRun {
						s.QuarantinedIssues["1"].RunID = "run_new"
						s.QuarantinedIssues["1"].Generation = 1
					}
					delete(s.PendingRequests, request.ID)
					delete(s.Issues, "1")
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			created := now
			if test.older {
				created = now.Add(-time.Second)
			}
			control := &fakeInputControlGitHub{fakeGitHub: base, unauthorized: test.unauthorized, comments: []gh.InputComment{{
				ID: 1, Actor: "operator", ActorType: "User", Body: "/agent-loop answer " + request.ID + " " + test.answer, CreatedAt: created, UpdatedAt: created,
			}}}
			loop.GitHub = control
			if err := loop.reconcileInputIssue(context.Background(), number); err != nil {
				t.Fatal(err)
			}
			if len(control.acks) != 1 || control.acks[0].Outcome != test.outcome {
				t.Fatalf("acks=%+v", control.acks)
			}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			saved, err := snapshot.Request(request.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (saved.Status == issuedomain.RequestStatusAnswered) != (test.outcome == "accepted") {
				t.Fatalf("request=%+v", saved)
			}
			if snapshot.ActiveExecution != nil {
				t.Fatal("receipt started a worker")
			}
		})
	}
}

func TestFaultGitHubMailboxAckFailureRestartAndReordering(t *testing.T) {
	loop, base := testLoop(t, worker.Result{})
	now := time.Now().UTC()
	_, request, err := loop.Store.AskIntake(1, "test", "Choose", "", "", nil, true, now)
	if err != nil {
		t.Fatal(err)
	}
	control := &fakeInputControlGitHub{fakeGitHub: base, ackError: errors.New("ack unavailable"), comments: []gh.InputComment{
		{ID: 2, Actor: "operator", ActorType: "User", Body: "/agent-loop answer " + request.ID + " second", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)},
		{ID: 1, Actor: "operator", ActorType: "User", Body: "/agent-loop answer " + request.ID + " first", CreatedAt: now, UpdatedAt: now},
	}}
	loop.GitHub = control
	if err := loop.reconcileInputIssue(context.Background(), 1); err == nil {
		t.Fatal("missing ack failure")
	}
	first, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if first.PendingRequests[request.ID].Answer != "first" {
		t.Fatal("delivery order selected answer")
	}
	loop.Store = state.Store{Dir: loop.Store.Dir, RepoID: loop.Store.RepoID, RepoPath: loop.Store.RepoPath}
	control.ackError = nil
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	replay, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if replay.StateRevision != first.StateRevision || replay.ActiveExecution != nil ||
		control.acks[0].Outcome != "accepted" || control.acks[1].Outcome != "conflict" {
		t.Fatalf("replay changed receipt: %+v", control.acks)
	}
	control.comments = nil
	control.acks = nil
	if err := loop.reconcileInputIssue(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if len(control.acks) != 1 || control.acks[0].Outcome != "accepted" {
		t.Fatalf("deleted answer lost ack: %+v", control.acks)
	}
}

func TestGitHubInputWebhookAndReconciliation(t *testing.T) {
	for _, fromWebhook := range []bool{true, false} {
		t.Run(fmt.Sprint(fromWebhook), func(t *testing.T) {
			loop, base := testLoop(t, worker.Result{})
			now := time.Now().UTC()
			_, request, err := loop.Store.AskIntake(1, "test", "Choose", "", "", nil, true, now)
			if err != nil {
				t.Fatal(err)
			}
			control := &fakeInputControlGitHub{fakeGitHub: base, comments: []gh.InputComment{{
				ID: 1, Actor: "operator", ActorType: "User", Body: "/agent-loop answer " + request.ID + " answer", CreatedAt: now, UpdatedAt: now,
			}}}
			loop.GitHub = control
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			scheduler := &scheduler{loop: loop, active: map[int]activeJob{}, terminalPoll: map[int]time.Time{}}
			if fromWebhook {
				loop.Config.Webhook.Mode = "webhook"
				delivery := webhook.Delivery{DeliveryID: "input-answer", Event: "issue_comment", Action: "created", IssueNumber: 1}
				if err := webhook.EnqueueMailbox(loop.Store.Dir, delivery); err != nil {
					t.Fatal(err)
				}
				if _, _, err := scheduler.processMailbox(context.Background(), snapshot); err != nil {
					t.Fatal(err)
				}
			} else {
				scheduler.active[1] = activeJob{}
				if _, err := scheduler.dispatchManagedReconciliation(context.Background(), snapshot); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved.PendingRequests[request.ID].Status != issuedomain.RequestStatusAnswered || saved.ActiveExecution != nil {
				t.Fatal("answer was not collected without launching a worker")
			}
		})
	}
}

func TestGitHubAnswerReplayDoesNotResumeWorkerTwice(t *testing.T) {
	loop, base := testLoop(t, worker.Result{
		Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "decision", SessionID: "session",
		Question: &worker.Question{Text: "Which source?", AllowFreeText: true},
	})
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	asked, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	var request *state.Request
	for _, value := range asked.PendingRequests {
		request = value
	}
	if request == nil {
		t.Fatal("worker did not ask")
	}
	now := time.Now().UTC()
	control := &fakeInputControlGitHub{fakeGitHub: base, comments: []gh.InputComment{{
		ID: 42, Actor: "operator", ActorType: "User", Body: "/agent-loop answer " + request.ID + " dated source", CreatedAt: now, UpdatedAt: now,
	}}}
	loop.GitHub = control
	recorder := &recordingWorker{result: worker.Result{
		Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "next decision", SessionID: "session",
		Question: &worker.Question{Text: "Which date?", AllowFreeText: true},
	}}
	loop.Worker = recorder
	for range 3 {
		if _, err := loop.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(recorder.runPrompts) != 0 || len(recorder.resumePrompts) != 1 {
		t.Fatalf("runs=%d resumes=%d", len(recorder.runPrompts), len(recorder.resumePrompts))
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Issues["1"].Answers) != 1 || after.ActiveExecution != nil || after.PendingRequests[request.ID].AnswerProvenance.CommentID != 42 {
		t.Fatal("replay changed answer or execution ownership")
	}
}

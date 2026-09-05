package supervisor

import (
	"context"
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
	comments []gh.InputComment
	acks     []gh.InputAcknowledgement
	syncs    int
}

func (f *fakeInputControlGitHub) SyncInputRequest(context.Context, config.Config, state.Request) error {
	f.syncs++
	return nil
}

func (f *fakeInputControlGitHub) ListInputComments(context.Context, config.Config, int) ([]gh.InputComment, error) {
	return append([]gh.InputComment(nil), f.comments...), nil
}

func (f *fakeInputControlGitHub) VerifyInputActor(context.Context, config.Config, gh.InputComment) (gh.AuthorVerification, error) {
	return gh.AuthorVerification{Trusted: true, Login: "operator", Permission: "write", Reason: "repository_permission"}, nil
}

func (f *fakeInputControlGitHub) SyncInputAcknowledgement(_ context.Context, _ config.Config, _ int, ack gh.InputAcknowledgement) error {
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
	if first.Issues["1"].Status != issuedomain.StatusResumePending || answered.Status != issuedomain.RequestStatusAnswered ||
		answered.AnswerProvenance == nil || answered.AnswerProvenance.CommentID != 42 || len(first.Issues["1"].Answers) != 1 ||
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
	if replayed.StateRevision != first.StateRevision || len(replayed.Issues["1"].Answers) != 1 || control.acks[len(control.acks)-1].Outcome != "accepted" {
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
	if afterOutcomes.StateRevision != first.StateRevision || len(afterOutcomes.Issues["1"].Answers) != 1 {
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
	if afterDelete.StateRevision != first.StateRevision || len(afterDelete.Issues["1"].Answers) != 1 {
		t.Fatalf("deleted accepted comment changed canonical state: revision=%d answers=%d", afterDelete.StateRevision, len(afterDelete.Issues["1"].Answers))
	}
}

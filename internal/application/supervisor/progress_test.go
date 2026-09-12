package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type progressGitHub struct {
	*fakeGitHub
	keys, bodies []string
	err          error
}

func (c *progressGitHub) CommentProgress(_ context.Context, _ config.Config, _ int, key, body string) error {
	c.keys = append(c.keys, key)
	c.bodies = append(c.bodies, body)
	return c.err
}

type countingPublisher struct {
	fakePublisher
	calls int
}

func (p *countingPublisher) Publish(ctx context.Context, cfg config.Config, issue gh.Issue, path, branch, url, summary, base string) (worker.GitResult, publication.Audit, error) {
	p.calls++
	return p.fakePublisher.Publish(ctx, cfg, issue, path, branch, url, summary, base)
}

func TestProgressPublicationAndMergeFailuresDoNotRepeatWork(t *testing.T) {
	for _, auto := range []bool{true, false} {
		t.Run(map[bool]string{true: "auto", false: "manual"}[auto], func(t *testing.T) {
			loop, base := testLoop(t, worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done"})
			loop.Config.Completion.AutoMerge = auto
			client := &progressGitHub{fakeGitHub: base, err: errors.New("ambiguous comment result")}
			loop.GitHub = client
			publisher := &countingPublisher{fakePublisher: fakePublisher{result: worker.GitResult{Branch: "codex/issue-1-test", Commit: "head-1", PullRequestURL: "https://example.test/pr/1"}}}
			loop.Publisher = publisher
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(client.bodies) != 1 || !strings.Contains(client.bodies[0], "実装結果をPRに公開しました") {
				t.Fatalf("comments=%v", client.bodies)
			}
			if strings.Contains(client.bodies[0], "自動マージします") != auto {
				t.Fatal(client.bodies[0])
			}
			base.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN"}, PullRequests: []gh.PullRequest{{Number: 1, URL: publisher.result.PullRequestURL, State: "OPEN", HeadRefName: publisher.result.Branch, HeadSHA: "head-1", MergeStateStatus: "CLEAN", ChecksStatus: "success"}}}
			current, _ := loop.issueState(1)
			if err := loop.processPullRequest(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if len(client.bodies) != 2 {
				t.Fatalf("comments=%v", client.bodies)
			}
			if auto && !strings.Contains(client.bodies[1], "マージを要求しました") || !auto && !strings.Contains(client.bodies[1], "手動でのマージ") {
				t.Fatal(client.bodies[1])
			}
			restarted := &Loop{Config: loop.Config, Store: loop.Store, GitHub: client, Worktrees: loop.Worktrees, Worker: loop.Worker, Publisher: publisher, Logger: loop.Logger}
			for i := 0; i < 2; i++ {
				current, _ = restarted.issueState(1)
				if err := restarted.processPullRequest(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			if len(client.bodies) != 2 || publisher.calls != 1 {
				t.Fatalf("posts=%d publication=%d", len(client.bodies), publisher.calls)
			}
			now := time.Now()
			base.remote.PullRequests[0].MergedAt = &now
			current, _ = restarted.issueState(1)
			if err := restarted.processPullRequest(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			current, _ = restarted.issueState(1)
			if current.Status != issuedomain.StatusCompleted || !base.done {
				t.Fatalf("state=%+v", current)
			}
		})
	}
}

func TestProgressObservationsDoNotRetryMissingComments(t *testing.T) {
	for _, observation := range []string{"REVIEW_REQUIRED", "CHANGES_REQUESTED", "behind"} {
		t.Run(observation, func(t *testing.T) {
			loop, base := testLoop(t, worker.Result{Version: 1, Status: "completed", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}})
			loop.Config.Completion.AutoMerge = true
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			client := &progressGitHub{fakeGitHub: base, err: errors.New("post failed")}
			loop.GitHub = client
			pr := gh.PullRequest{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", HeadRefName: "codex/issue-1-test", HeadSHA: "head-1", ChecksStatus: "pending", MergeStateStatus: "CLEAN"}
			if observation == "behind" {
				pr.MergeStateStatus = "behind"
			} else {
				pr.ReviewDecision = observation
			}
			base.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN"}, PullRequests: []gh.PullRequest{pr}}
			for i := 0; i < 3; i++ {
				current, _ := loop.issueState(1)
				if err := loop.processPullRequest(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			if len(client.bodies) != 1 {
				t.Fatalf("comments=%v", client.bodies)
			}
			base.remote.PullRequests[0].HeadSHA = "head-2"
			current, _ := loop.issueState(1)
			if err := loop.processPullRequest(context.Background(), current); err != nil {
				t.Fatal(err)
			}
			if len(client.bodies) != 2 || client.keys[0] == client.keys[1] {
				t.Fatalf("keys=%v", client.keys)
			}
			if base.mergedPullRequest {
				t.Fatal("merged during wait")
			}
		})
	}
}

func TestProgressRetryUsesCommittedDeadline(t *testing.T) {
	loop, base := testLoop(t, worker.Result{Version: 1, Status: "retryable_failure", Summary: "validation failed"})
	client := &progressGitHub{fakeGitHub: base, err: errors.New("post failed")}
	loop.GitHub = client
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, _ := loop.issueState(1)
	if current.Status != issuedomain.StatusRetryWait || current.RetryAfter == nil || len(client.bodies) != 1 {
		t.Fatalf("state=%+v comments=%v", current, client.bodies)
	}
	if !strings.Contains(client.bodies[0], current.RetryAfter.UTC().Format("2006-01-02 15:04:05 MST")) || !strings.Contains(client.bodies[0], "validation failed") {
		t.Fatal(client.bodies[0])
	}
}

func TestProgressResumeOnlyAfterWorkerStart(t *testing.T) {
	for _, startFails := range []bool{false, true} {
		t.Run(map[bool]string{true: "failed", false: "started"}[startFails], func(t *testing.T) {
			loop, base := testLoop(t, worker.Result{Version: 1, Status: "needs_input", Summary: "question", Question: &worker.Question{Text: "Choose", AllowFreeText: true}})
			client := &progressGitHub{fakeGitHub: base}
			loop.GitHub = client
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := loop.Store.Load()
			for id := range snapshot.PendingRequests {
				if _, _, err := loop.Store.RecordAnswer(id, "yes", time.Now(), nil); err != nil {
					t.Fatal(err)
				}
			}
			if len(client.bodies) != 0 {
				t.Fatal("acceptance announced a start")
			}
			if startFails {
				loop.Worker = processStartFailureWorker{err: errors.New("spawn failed")}
			} else {
				loop.Worker = fakeWorker{result: worker.Result{Version: 1, Status: "completed", Git: &worker.GitResult{}}}
			}
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, body := range client.bodies {
				found = found || strings.Contains(body, "回答に基づき、実装を再開しました")
			}
			if found == startFails {
				t.Fatalf("startFails=%v comments=%v", startFails, client.bodies)
			}
		})
	}
}

func TestProgressPublicationUpdatesAndResumeStagesHaveDistinctMarkers(t *testing.T) {
	base := &fakeGitHub{}
	client := &progressGitHub{fakeGitHub: base}
	loop := Loop{Config: config.Defaults(), GitHub: client}
	current := state.Issue{Number: 1, RunID: "run", Generation: 1, PullRequestURL: "https://example.test/pr/1"}
	loop.commentPublication(context.Background(), current, current.PullRequestURL, "head-1")
	loop.commentPublication(context.Background(), current, current.PullRequestURL, "head-2")
	current.Generation++
	loop.commentPublication(context.Background(), current, current.PullRequestURL, "head-2")
	for _, stage := range []string{"実装", "PR公開", "CI確認", "競合解消"} {
		loop.commentResume(context.Background(), current, stage)
	}
	seen := map[string]bool{}
	for _, key := range client.keys {
		if seen[key] {
			t.Fatalf("duplicate key %s", key)
		}
		seen[key] = true
	}
	if !strings.Contains(client.bodies[0], "PRに修正を反映しました") {
		t.Fatal(client.bodies[0])
	}
}

func TestProgressChecksResumeIsNotReplayedByPolling(t *testing.T) {
	loop, base := testLoop(t, worker.Result{})
	now := time.Now().UTC()
	_, err := loop.Store.Update("issue_suspension_resolved", 1, "run_checks", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: issuedomain.StatusAwaitingChecks, RunID: "run_checks", Generation: 1,
			Branch: "codex/issue-1-test", Worktree: loop.Config.RepoPath, PullRequestURL: "https://example.test/pr/1", HeadSHA: "head-1",
			Continuation: &state.ContinuationCheckpoint{ID: "checkpoint_checks", CreatedAt: now.Add(-time.Minute), RunID: "run_checks", Generation: 1, Stage: issuedomain.ContinuationStageChecks},
			Suspension:   &state.Suspension{ID: "suspension_checks", Status: issuedomain.SuspensionResolved, ReasonCode: "checks", Recoverability: issuedomain.RecoverabilityOperator, Reason: "checks failed", AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionRetryStage}, CheckpointID: "checkpoint_checks", SuspendedAt: now.Add(-time.Minute), ResolvedAt: now, Resolution: issuedomain.ResolutionRetryStage},
			UpdatedAt:    now,
		}
		setSupervisorTestWorkspace(snapshot, snapshot.Issues["1"])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &progressGitHub{fakeGitHub: base, err: errors.New("post failed")}
	loop.GitHub = client
	base.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN"}, PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", HeadRefName: "codex/issue-1-test", HeadSHA: "head-1", State: "OPEN", ChecksStatus: "pending", MergeStateStatus: "CLEAN"}}}
	for i := 0; i < 2; i++ {
		current, err := loop.issueState(1)
		if err != nil {
			t.Fatal(err)
		}
		if err := loop.processPullRequest(context.Background(), current); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.bodies) != 1 || !strings.Contains(client.bodies[0], "回答に基づき、CI確認を再開しました") {
		t.Fatalf("comments=%v", client.bodies)
	}
}

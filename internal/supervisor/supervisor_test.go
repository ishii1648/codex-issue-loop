package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/conflict"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type failingNotificationDispatcher struct{ calls int }

func (d *failingNotificationDispatcher) Dispatch(context.Context) error {
	d.calls++
	return errors.New("notification provider unavailable")
}

func TestSessionResumeNeverCrossesBackendNamespace(t *testing.T) {
	loop := Loop{Config: config.Defaults()}
	loop.Config.Worker.Backend = "opencode"
	current := state.Issue{SessionID: "ses_1", Session: &state.WorkerSession{Backend: "claude-code", ID: "ses_1"}}
	if loop.canResume(current) {
		t.Fatal("cross-backend session was accepted")
	}
	current.Session.Backend = "opencode"
	if !loop.canResume(current) {
		t.Fatal("same-backend session was rejected")
	}
	loop.Config.Worker.Backend = "codex"
	current.Session = nil
	if !loop.canResume(current) {
		t.Fatal("legacy Codex session did not retain compatibility")
	}
}

func TestNotificationFailureDoesNotStopSupervisor(t *testing.T) {
	var logs bytes.Buffer
	dispatcher := &failingNotificationDispatcher{}
	loop := Loop{Logger: log.New(&logs, "", 0), Notifications: dispatcher}
	loop.dispatchNotifications(context.Background())
	if dispatcher.calls != 1 || !strings.Contains(logs.String(), "without stopping supervisor") {
		t.Fatalf("calls=%d logs=%q", dispatcher.calls, logs.String())
	}
}

type fakeGitHub struct {
	issue                     gh.Issue
	remote                    *gh.RemoteState
	claimed, done, needsInput bool
	markedRunning             bool
	readyPullRequest          bool
	updatedPullRequest        bool
	mergedPullRequest         bool
	inspectCalls              int
	claimErr                  error
	doneErr                   error
	listErr                   error
}

func (f *fakeGitHub) ListReady(context.Context, config.Config) ([]gh.Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []gh.Issue{f.issue}, nil
}
func (f *fakeGitHub) Get(context.Context, config.Config, int) (gh.Issue, error) { return f.issue, nil }
func (f *fakeGitHub) Inspect(context.Context, config.Config, int, string) (gh.RemoteState, error) {
	f.inspectCalls++
	if f.remote != nil {
		return *f.remote, nil
	}
	return gh.RemoteState{Issue: f.issue}, nil
}
func (f *fakeGitHub) Claim(context.Context, config.Config, gh.Issue, string) error {
	if f.claimErr != nil {
		err := f.claimErr
		f.claimErr = nil
		return err
	}
	f.claimed = true
	return nil
}
func (f *fakeGitHub) MarkNeedsInput(context.Context, config.Config, int, string, string) error {
	f.needsInput = true
	return nil
}
func (f *fakeGitHub) MarkDone(context.Context, config.Config, int, string) error {
	if f.doneErr != nil {
		err := f.doneErr
		f.doneErr = nil
		return err
	}
	f.done = true
	return nil
}
func (f *fakeGitHub) MarkFailed(context.Context, config.Config, int, string, bool) error { return nil }
func (f *fakeGitHub) MarkRunning(context.Context, config.Config, int) error {
	f.markedRunning = true
	return nil
}
func (f *fakeGitHub) MarkConflictRetry(context.Context, config.Config, int, string) error {
	f.markedRunning = true
	return nil
}
func (f *fakeGitHub) ReadyPullRequest(context.Context, config.Config, string) error {
	f.readyPullRequest = true
	return nil
}
func (f *fakeGitHub) UpdatePullRequest(context.Context, config.Config, string) error {
	f.updatedPullRequest = true
	return nil
}
func (f *fakeGitHub) MergePullRequest(context.Context, config.Config, string) error {
	f.mergedPullRequest = true
	return nil
}

type fakeWorktree struct {
	path       string
	inspection *worktree.Inspection
}

func (f fakeWorktree) Ensure(context.Context, config.Config, string, int, string) (worktree.Result, error) {
	return worktree.Result{Path: f.path, Branch: "codex/issue-1-test"}, nil
}
func (f fakeWorktree) Inspect(context.Context, config.Config, string, string) (worktree.Inspection, error) {
	if f.inspection != nil {
		return *f.inspection, nil
	}
	return worktree.Inspection{Exists: true, Valid: true, Branch: "codex/issue-1-test", LocalBranchExists: true, RemoteBranchExists: true}, nil
}

type fakeWorker struct {
	result worker.Result
	err    error
}

type fakePublisher struct {
	called bool
	result worker.GitResult
	audit  publication.Audit
	err    error
}

type fakeConflictResolver struct {
	preparation  conflict.Preparation
	published    worker.GitResult
	prepareErr   error
	publishErr   error
	prepareCalls int
	publishCalls int
}

func (f *fakeConflictResolver) Prepare(context.Context, config.Config, string, string, *state.ConflictRecovery) (conflict.Preparation, error) {
	f.prepareCalls++
	return f.preparation, f.prepareErr
}

func (f *fakeConflictResolver) Publish(context.Context, config.Config, gh.Issue, string, string, state.ConflictRecovery, []worker.Test) (worker.GitResult, error) {
	f.publishCalls++
	return f.published, f.publishErr
}

func (f *fakePublisher) Publish(_ context.Context, _ config.Config, _ gh.Issue, _, _, _, baseSHA string, declared []string) (worker.GitResult, publication.Audit, error) {
	f.called = true
	audit := f.audit
	if audit.BaseSHA == "" {
		audit.BaseSHA = baseSHA
	}
	if audit.DeclaredResources == nil {
		audit.DeclaredResources = declared
	}
	if audit.ActualResources == nil {
		audit.ActualResources = declared
	}
	return f.result, audit, f.err
}

type recordingWorker struct {
	result        worker.Result
	runPrompts    []string
	resumePrompts []string
}

type scriptedWorker struct {
	results []worker.Result
	errors  []error
	runs    int
	resumes int
	cursor  int
}

func (s *scriptedWorker) next() (worker.Result, error) {
	index := s.cursor
	s.cursor++
	var result worker.Result
	var err error
	if index < len(s.results) {
		result = s.results[index]
	}
	if index < len(s.errors) {
		err = s.errors[index]
	}
	return result, err
}

func (s *scriptedWorker) Run(context.Context, config.Config, gh.Issue, state.Issue, string, worker.Started) (worker.Result, error) {
	s.runs++
	return s.next()
}

func (s *scriptedWorker) Resume(context.Context, config.Config, gh.Issue, state.Issue, string, worker.Started) (worker.Result, error) {
	s.resumes++
	return s.next()
}

func (f *recordingWorker) Run(_ context.Context, _ config.Config, _ gh.Issue, _ state.Issue, prompt string, _ worker.Started) (worker.Result, error) {
	f.runPrompts = append(f.runPrompts, prompt)
	return f.result, nil
}

func (f *recordingWorker) Resume(_ context.Context, _ config.Config, _ gh.Issue, _ state.Issue, prompt string, _ worker.Started) (worker.Result, error) {
	f.resumePrompts = append(f.resumePrompts, prompt)
	return f.result, nil
}

func (f fakeWorker) Run(context.Context, config.Config, gh.Issue, state.Issue, string, worker.Started) (worker.Result, error) {
	return f.result, f.err
}
func (f fakeWorker) Resume(context.Context, config.Config, gh.Issue, state.Issue, string, worker.Started) (worker.Result, error) {
	return f.result, f.err
}

func testLoop(t *testing.T, result worker.Result) (*Loop, *fakeGitHub) {
	t.Helper()
	repo := t.TempDir()
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.RepoPath = repo
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update("started", 0, "", nil, func(s *state.Snapshot) error { s.Supervisor.State = "polling"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	github := &fakeGitHub{issue: gh.Issue{Number: 1, Title: "Test", Body: "Implement it", Labels: []string{"codex-loop:ready"}}}
	return &Loop{Config: cfg, Store: store, GitHub: github, Worktrees: fakeWorktree{path: repo}, Worker: fakeWorker{result: result}}, github
}

func TestFaultStandardWorkerCompletesWithoutAdditionalRun(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	scripted := &scriptedWorker{results: []worker.Result{result}}
	loop.Worker = scripted
	worked, err := loop.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked || !github.claimed || github.done {
		t.Fatalf("worked=%v github=%+v", worked, github)
	}
	snapshot, _ := loop.Store.Load()
	issue := snapshot.Issues["1"]
	if issue.Status != "awaiting_checks" || issue.PullRequestURL == "" || issue.SessionID == "" || issue.Lease == nil {
		t.Fatalf("unexpected Issue: %+v", issue)
	}
	if scripted.runs != 1 || scripted.resumes != 0 {
		t.Fatalf("runs=%d resumes=%d", scripted.runs, scripted.resumes)
	}
}

func TestCompletedWorkerIsPublishedOutsideSandbox(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done"}
	loop, github := testLoop(t, result)
	publisher := &fakePublisher{result: worker.GitResult{
		Branch: "codex/issue-1-test", Commit: "abc", PullRequestURL: "https://example.test/pr/1",
	}}
	loop.Publisher = publisher
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !publisher.called || github.done {
		t.Fatalf("publisher called=%v github done=%v", publisher.called, github.done)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Issues["1"].PullRequestURL; got != publisher.result.PullRequestURL {
		t.Fatalf("pull request=%q, want %q", got, publisher.result.PullRequestURL)
	}
}

func TestResourceClaimMismatchPersistsAuditAndRefusesPublicationIntoNeedsInput(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done"}
	loop, github := testLoop(t, result)
	loop.Config.Resources.Definitions = []config.ResourceDefinition{
		{Name: "git", Paths: []string{"internal/git/**"}},
		{Name: "docs", Paths: []string{"docs/**"}},
	}
	github.issue.Body = "<!-- agent-loop:metadata\nversion: 1\ndepends_on: []\n-->"
	github.issue.Labels = append(github.issue.Labels, "area:git")
	publisher := &fakePublisher{
		audit: publication.Audit{
			ChangedPaths: []string{"docs/architecture.md"}, DeclaredResources: []string{"git"},
			ActualResources: []string{"docs"}, Reason: publication.ReasonResourceClaimMismatch,
		},
		err: publication.ClaimMismatchError{Declared: []string{"git"}, Actual: []string{"docs"}},
	}
	loop.Publisher = publisher
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues["1"]
	if issue.Status != "needs_input" || issue.Lease == nil || strings.Join(issue.DeclaredResources, ",") != "git" || strings.Join(issue.ActualResources, ",") != "docs" || len(snapshot.PendingRequests) != 1 {
		t.Fatalf("issue=%+v requests=%+v", issue, snapshot.PendingRequests)
	}
	if !github.needsInput || issue.PullRequestURL != "" {
		t.Fatalf("publication was not safely refused: issue=%+v github=%+v", issue, github)
	}
	events, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(events), publication.ReasonResourceClaimMismatch) || !strings.Contains(string(events), `"actual_resources":["docs"]`) {
		t.Fatalf("audit event missing: err=%v events=%s", err, events)
	}
}

func TestPullRequestLifecycleAnomaliesBlockWithoutReleasingLease(t *testing.T) {
	tests := []struct {
		name       string
		pulls      []gh.PullRequest
		inspection *worktree.Inspection
		want       string
	}{
		{
			name:  "closed without merge",
			pulls: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "CLOSED", HeadRefName: "codex/issue-1-test"}},
			want:  "closed without merge",
		},
		{
			name: "multiple Pull Requests",
			pulls: []gh.PullRequest{
				{Number: 2, URL: "https://example.test/pr/2", State: "OPEN", HeadRefName: "codex/issue-1-test"},
				{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", HeadRefName: "codex/issue-1-test"},
			},
			want: "multiple Pull Requests",
		},
		{
			name:       "remote branch disappeared",
			pulls:      []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", HeadRefName: "codex/issue-1-test"}},
			inspection: &worktree.Inspection{Exists: true, Valid: true, Branch: "codex/issue-1-test", LocalBranchExists: true},
			want:       "disappeared",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
			loop, github := testLoop(t, result)
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			github.remote = &gh.RemoteState{
				Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
				PullRequests: test.pulls,
			}
			if test.inspection != nil {
				loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: test.inspection}
			}
			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			issue := snapshot.Issues["1"]
			if issue.Status != "blocked" || issue.Lease == nil || !strings.Contains(issue.LastError, test.want) {
				t.Fatalf("issue=%+v", issue)
			}
		})
	}
}

func TestPullRequestChecksGateReadyAndOptionalMerge(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", IsDraft: true, HeadRefName: "codex/issue-1-test", ChecksStatus: "success"}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "awaiting_merge" || !github.readyPullRequest || github.mergedPullRequest || github.done {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}

	loop.Config.Completion.AutoMerge = true
	_, err := loop.Store.Update("test_reset", 1, "", nil, func(s *state.Snapshot) error {
		s.Issues["1"].RetryAfter = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.readyPullRequest = false
	github.remote.PullRequests[0].IsDraft = false
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = loop.Store.Load()
	if snapshot.Issues["1"].Status != "awaiting_merge" || github.readyPullRequest || !github.mergedPullRequest {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestFailedPullRequestChecksReturnWorkerToRetry(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", IsDraft: true, HeadRefName: "codex/issue-1-test", ChecksStatus: "failure"}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "retry_wait" || !strings.Contains(snapshot.Issues["1"].LastError, "checks failed") {
		t.Fatalf("issue=%+v", snapshot.Issues["1"])
	}
	recorder := &recordingWorker{result: worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", SessionID: "session", Summary: "retry later",
	}}
	loop.Worker = recorder
	_, err := loop.Store.Update("test_retry_due", 1, "", nil, func(s *state.Snapshot) error {
		s.Issues["1"].RetryAfter = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.resumePrompts) != 1 || !strings.Contains(recorder.resumePrompts[0], "https://example.test/pr/1") {
		t.Fatalf("resume prompts=%q", recorder.resumePrompts)
	}
}

func TestQueueOnlyWaitsForMergeWhenAutoMergeIsEnabled(t *testing.T) {
	snapshot := state.Snapshot{Issues: map[string]*state.Issue{
		"1": {Number: 1, Status: "awaiting_merge"},
	}}
	if queueBlockedByPullRequest(snapshot, false) {
		t.Fatal("manual merge wait blocked the Issue queue")
	}
	if !queueBlockedByPullRequest(snapshot, true) {
		t.Fatal("auto merge wait did not retain queue ownership")
	}
}

func TestAutoMergeUpdatesBehindBranchBeforeMerging(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	loop.Config.Completion.AutoMerge = true
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", IsDraft: true, HeadRefName: "codex/issue-1-test", MergeStateStatus: "BEHIND", ChecksStatus: "success"}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "awaiting_checks" || !github.updatedPullRequest || github.readyPullRequest || github.mergedPullRequest {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestDirtyPullRequestRunsDurableConflictWorkerAndReturnsToChecks(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Completion.AutoMerge = true
	prURL := "https://example.test/pr/1"
	_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "awaiting_checks", RunID: "run_1",
			Branch: "codex/issue-1-test", Worktree: loop.Config.RepoPath, PullRequestURL: prURL,
			Attempts: 1, UpdatedAt: time.Now().UTC(),
			ConflictRecovery: &state.ConflictRecovery{
				PullRequestURL: prURL, TargetBaseSHA: "base-old", OriginalHeadSHA: "older-head",
				ConflictFiles: []string{"older.txt"}, AllowedPaths: []string{"older.txt"}, Attempts: 1, BaseUpdates: 1,
				History: []state.ConflictAttempt{{Number: 1, BaseSHA: "base-old", Status: "completed"}},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	github.remote = &gh.RemoteState{Issue: github.issue, PullRequests: []gh.PullRequest{{
		Number: 1, URL: prURL, State: "OPEN", HeadRefName: "codex/issue-1-test",
		MergeStateStatus: "DIRTY", ChecksStatus: "success",
	}}}
	resolver := &fakeConflictResolver{
		preparation: conflict.Preparation{
			PreviousBaseSHA: "base-old", TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
			ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"},
			OriginalDiff: "+pr intent", BaseCommits: "abc base design", ConflictContent: "<<<<<<< conflict",
		},
		published: worker.GitResult{Branch: "codex/issue-1-test", Commit: "merge-commit", PullRequestURL: prURL},
	}
	recorder := &recordingWorker{result: worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "resolved",
		Tests: []worker.Test{{Command: "go test ./...", Result: "passed"}},
	}}
	loop.Conflicts, loop.Worker = resolver, recorder
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	item := snapshot.Issues["1"]
	if item.Status != "awaiting_checks" || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 || item.ConflictRecovery.BaseUpdates != 2 || len(item.ConflictRecovery.History) != 2 || item.ConflictRecovery.History[1].Status != "completed" {
		t.Fatalf("item=%+v", item)
	}
	if resolver.prepareCalls != 2 || resolver.publishCalls != 1 || len(recorder.runPrompts) != 1 {
		t.Fatalf("resolver=%+v prompts=%d", resolver, len(recorder.runPrompts))
	}
	prompt := recorder.runPrompts[0]
	for _, expected := range []string{"base-new", "shared.txt", "+pr intent", "abc base design", "git add", "force push"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("conflict prompt missing %q: %s", expected, prompt)
		}
	}
}

func TestConflictRecoveryRestartRecognizesPublishedCommitWithoutDuplicateWorkerOrPush(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	prURL := "https://example.test/pr/1"
	_, err := loop.Store.Update("fixture", 1, "conflict_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "resolving_conflict", RunID: "conflict_1", Branch: "codex/issue-1-test",
			Worktree: loop.Config.RepoPath, PullRequestURL: prURL, UpdatedAt: time.Now().UTC(),
			ConflictRecovery: &state.ConflictRecovery{
				PullRequestURL: prURL, TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
				ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"}, Attempts: 1, BaseUpdates: 1,
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeConflictResolver{preparation: conflict.Preparation{Published: true, Resolved: true, Commit: "merge-commit", TargetBaseSHA: "base-new"}}
	recorder := &recordingWorker{}
	loop.Conflicts, loop.Worker = resolver, recorder
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "awaiting_checks" || resolver.prepareCalls != 1 || resolver.publishCalls != 0 || len(recorder.runPrompts) != 0 {
		t.Fatalf("item=%+v resolver=%+v prompts=%v", snapshot.Issues["1"], resolver, recorder.runPrompts)
	}
}

func TestConflictRecoveryBlocksOnlyAfterPerBaseBudgetIsExhausted(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	loop.Config.ConflictRecovery.MaxAttemptsPerBase = 3
	_, err := loop.Store.Update("fixture", 1, "conflict_3", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "resolving_conflict", RunID: "conflict_3", Branch: "codex/issue-1-test",
			Worktree: loop.Config.RepoPath, PullRequestURL: "https://example.test/pr/1", UpdatedAt: time.Now().UTC(),
			ConflictRecovery: &state.ConflictRecovery{
				PullRequestURL: "https://example.test/pr/1", TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
				ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"}, Attempts: 3, BaseUpdates: 1,
				History: []state.ConflictAttempt{{Number: 1, BaseSHA: "base-new", Status: "retryable_failure"}},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Conflicts = &fakeConflictResolver{preparation: conflict.Preparation{TargetBaseSHA: "base-new", ConflictFiles: []string{"shared.txt"}}}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	item := snapshot.Issues["1"]
	if item.Status != "blocked" || !strings.Contains(item.LastError, "after 3 attempts") || !strings.Contains(item.LastError, "shared.txt") || !strings.Contains(item.LastError, "agent-loop retry") {
		t.Fatalf("item=%+v", item)
	}
}

func TestConflictWorkerNeedsInputKeepsConflictResumeTarget(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	_, err := loop.Store.Update("fixture", 1, "conflict_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "resolving_conflict", RunID: "conflict_1", Branch: "codex/issue-1-test",
			Worktree: loop.Config.RepoPath, PullRequestURL: "https://example.test/pr/1", UpdatedAt: time.Now().UTC(),
			ConflictRecovery: &state.ConflictRecovery{
				PullRequestURL: "https://example.test/pr/1", TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
				ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"}, BaseUpdates: 1,
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Conflicts = &fakeConflictResolver{preparation: conflict.Preparation{TargetBaseSHA: "base-new", ConflictFiles: []string{"shared.txt"}}}
	loop.Worker = fakeWorker{result: worker.Result{
		Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "choice required",
		Question: &worker.Question{Text: "Which behavior?", Reason: "requirements choice", AllowFreeText: true}, Tests: []worker.Test{},
	}}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "needs_input" || len(snapshot.PendingRequests) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	for _, request := range snapshot.PendingRequests {
		if request.ResumeStatus != "resolving_conflict" {
			t.Fatalf("request=%+v", request)
		}
	}
}

func TestMergedPullRequestCompletesAndClosesIssue(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "CLOSED", MergedAt: &now, HeadRefName: "codex/issue-1-test", ChecksStatus: "success"}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "completed" || !github.done {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestRunOncePersistsQuestion(t *testing.T) {
	result := worker.Result{Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "decision", SessionID: "session", Question: &worker.Question{Text: "Choose?", Reason: "public API", AllowFreeText: true}}
	loop, github := testLoop(t, result)
	_, err := loop.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !github.needsInput {
		t.Fatal("GitHub was not marked needs-input")
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "needs_input" || snapshot.Issues["1"].Lease == nil {
		t.Fatalf("status=%s", snapshot.Issues["1"].Status)
	}
	if len(snapshot.PendingRequests) != 1 {
		t.Fatalf("requests=%d", len(snapshot.PendingRequests))
	}
	for id, request := range snapshot.PendingRequests {
		if request.ID != id || request.Question != "Choose?" {
			t.Fatalf("request=%+v", request)
		}
	}
}

func TestRunOnceDefaultsAmbiguousFailureToExtended(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{SessionID: "session"})
	loop.Worker = fakeWorker{result: worker.Result{SessionID: "session"}, err: fmt.Errorf("timeout")}
	_, err := loop.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	issue := snapshot.Issues["1"]
	if issue.ExecutionProfile != "extended" || issue.Status != "retry_wait" || issue.Lease == nil {
		t.Fatalf("issue=%+v", issue)
	}
}

func TestWorkerTimeoutStageIsPersistedForRetry(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{SessionID: "session"})
	loop.Worker = fakeWorker{result: worker.Result{SessionID: "session"}, err: &worker.TerminationError{
		Timeout: time.Hour, GracePeriod: 30 * time.Second, Forced: true, Cause: context.DeadlineExceeded,
	}}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	issue := snapshot.Issues["1"]
	if issue.Status != "retry_wait" || !strings.Contains(issue.LastError, "SIGKILL") || issue.WorkerPID != 0 {
		t.Fatalf("issue=%+v", issue)
	}
}

func TestFaultGitHubSyncPartialFailureIsRetried(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("completion_pending_sync", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{Number: 1, Status: "completed", RunID: "run_1", PullRequestURL: "https://example.test/pr/1", GitHubSync: "done"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.doneErr = fmt.Errorf("temporary GitHub error")
	if _, err := loop.RunOnce(context.Background()); err == nil {
		t.Fatal("expected initial sync error")
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].GitHubSync != "done" {
		t.Fatalf("sync=%q", snapshot.Issues["1"].GitHubSync)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = loop.Store.Load()
	if snapshot.Issues["1"].GitHubSync != "" || !github.done {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestFaultInterruptedClaimIsRecovered(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", Git: &worker.GitResult{}}
	loop, github := testLoop(t, result)
	github.claimErr = fmt.Errorf("temporary claim error")
	if _, err := loop.RunOnce(context.Background()); err == nil {
		t.Fatal("expected claim error")
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "claiming" || snapshot.Issues["1"].Lease == nil {
		t.Fatalf("status=%s", snapshot.Issues["1"].Status)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = loop.Store.Load()
	if snapshot.Issues["1"].Status != "completed" || snapshot.Issues["1"].Lease != nil || !github.claimed {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestFaultSupervisorWakesOnRecordedAnswer(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := watcher.Add(loop.Store.Dir); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { loop.waitForWork(context.Background(), time.Second, watcher); close(done) }()
	_, err = loop.Store.Update("answer_recorded", 1, "run", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{Number: 1, Status: "resume_pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("supervisor did not wake on state event")
	}
}

func TestFaultExtendedWorkerResumesOnlyWithinConfiguredLimit(t *testing.T) {
	retry := worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "continue",
		SessionID: "session-extended", Retry: &worker.Retry{Reason: "continue"},
	}
	completed := worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "done",
		Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"},
	}
	loop, _ := testLoop(t, retry)
	loop.Config.Worker.Profiles["extended"] = config.Profile{MaxContinuations: 2}
	loop.Config.Queue.MaxAttempts = 2
	scripted := &scriptedWorker{results: []worker.Result{retry, retry, retry, completed}}
	loop.Worker = scripted
	for cycle := 0; cycle < 4; cycle++ {
		if _, err := loop.RunOnce(context.Background()); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
		if cycle < 3 {
			_, err := loop.Store.Update("fault_retry_due", 1, "", nil, func(snapshot *state.Snapshot) error {
				snapshot.Issues["1"].RetryAfter = nil
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	snapshot, _ := loop.Store.Load()
	if scripted.runs != 2 || scripted.resumes != 2 || snapshot.Issues["1"].Status != "awaiting_checks" {
		t.Fatalf("runs=%d resumes=%d issue=%+v", scripted.runs, scripted.resumes, snapshot.Issues["1"])
	}
}

func TestFaultSupervisorRestartResumesWithDurableAnswers(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "done", Git: &worker.GitResult{}}
	loop, _ := testLoop(t, result)
	now := time.Now().UTC()
	_, err := loop.Store.Update("answer_recorded", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "resume_pending", RunID: "run_1",
			Worktree: loop.Config.RepoPath, SessionID: "session-123", Attempts: 1,
			ExecutionProfile: "extended", UpdatedAt: now,
			Answers: []state.AnswerRecord{
				{RequestID: "req_1", Question: "Which API?", Answer: "Use v2", AnsweredAt: now},
				{RequestID: "req_2", Question: "Publish now?", Answer: "Keep it draft", AnsweredAt: now.Add(time.Second)},
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A new Loop value models a supervisor process restarted from durable state.
	recorder := &recordingWorker{result: result}
	restarted := &Loop{
		Config: loop.Config, Store: loop.Store, GitHub: loop.GitHub,
		Worktrees: loop.Worktrees, Worker: recorder,
	}
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.resumePrompts) != 1 || len(recorder.runPrompts) != 0 {
		t.Fatalf("run=%d resume=%d", len(recorder.runPrompts), len(recorder.resumePrompts))
	}
	prompt := recorder.resumePrompts[0]
	for _, expected := range []string{"req_1", "Which API?", "Use v2", "req_2", "Publish now?", "Keep it draft"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("resume prompt missing %q: %s", expected, prompt)
		}
	}
}

func TestFaultSupervisorStopsBeforeRecoveryBlockedWork(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	f, err := os.OpenFile(loop.Store.EventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(fmt.Sprintf("{\"version\":%d,\"event_id\":\"evt_gap\",\"sequence\":99,\"repo_id\":\"repo-deadbeef\",\"type\":\"gap\"}\n", state.CurrentVersion)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = loop.Run(context.Background())
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected BlockedError, got %v", err)
	}
	snapshot, loadErr := loop.Store.Load()
	if loadErr != nil || snapshot.Supervisor.State != "blocked" || snapshot.Recovery == nil {
		t.Fatalf("snapshot=%+v err=%v", snapshot, loadErr)
	}
}

func TestWorkerRunLogPruningPreservesActiveAndAuditsDeletion(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	loop.Config.Logs.WorkerRunMaxAge = config.Duration{Duration: 24 * time.Hour}
	loop.Config.Logs.WorkerRunMaxCount = 1
	runs := filepath.Join(loop.Store.Dir, "runs")
	for _, name := range []string{"run_old", "run_recent", "run_active"} {
		if err := os.MkdirAll(filepath.Join(runs, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(runs, "run_old"), old, old); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Update("active", 1, "run_active", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{Number: 1, RunID: "run_active", Status: "needs_input"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.pruneRunLogs(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(runs, "run_old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old run was not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runs, "run_active")); err != nil {
		t.Fatalf("active run removed: %v", err)
	}
	data, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(data), "worker_logs_pruned") {
		t.Fatalf("missing audit event: %s err=%v", data, err)
	}
}

func TestFaultDiskSafetyReserveBlocksSupervisor(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	loop.DiskAvailable = func(string) (uint64, error) { return 0, nil }
	err := loop.Run(context.Background())
	var blocked BlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("expected BlockedError, got %v", err)
	}
	snapshot, loadErr := loop.Store.Load()
	if loadErr != nil || snapshot.Supervisor.State != "blocked" || !strings.Contains(snapshot.Supervisor.Message, "safety reserve") {
		t.Fatalf("snapshot=%+v err=%v", snapshot.Supervisor, loadErr)
	}
}

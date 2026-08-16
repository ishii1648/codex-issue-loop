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
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type failingNotificationDispatcher struct{ calls int }

func (d *failingNotificationDispatcher) Dispatch(context.Context) error {
	d.calls++
	return errors.New("notification provider unavailable")
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
	return worktree.Inspection{Exists: true, Valid: true, Branch: "codex/issue-1-test", LocalBranchExists: true}, nil
}

type fakeWorker struct {
	result worker.Result
	err    error
}

type fakePublisher struct {
	called bool
	result worker.GitResult
	err    error
}

func (f *fakePublisher) Publish(context.Context, config.Config, gh.Issue, string, string, string) (worker.GitResult, error) {
	f.called = true
	return f.result, f.err
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
	if issue.Status != "awaiting_checks" || issue.PullRequestURL == "" || issue.SessionID == "" {
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
	if snapshot.Issues["1"].Status != "needs_input" {
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
	if issue.ExecutionProfile != "extended" || issue.Status != "retry_wait" {
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
	if snapshot.Issues["1"].Status != "claiming" {
		t.Fatalf("status=%s", snapshot.Issues["1"].Status)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = loop.Store.Load()
	if snapshot.Issues["1"].Status != "completed" || !github.claimed {
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
	if _, err := f.WriteString("{\"version\":2,\"event_id\":\"evt_gap\",\"sequence\":99,\"repo_id\":\"repo-deadbeef\",\"type\":\"gap\"}\n"); err != nil {
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

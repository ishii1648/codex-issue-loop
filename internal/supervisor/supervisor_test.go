package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
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

func TestRestartCompletesRequestedMergedPullRequestAdoption(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	mergedAt := time.Now().UTC()
	github.remote = &gh.RemoteState{
		Issue: gh.Issue{
			Number: 1, State: "CLOSED", Labels: []string{"blocked"},
			Comments: []string{"<!-- codex-issue-loop:failed:1 -->"},
		},
		PullRequests: []gh.PullRequest{{
			Number: 17, URL: "https://example.test/pr/17", State: "MERGED", MergedAt: &mergedAt,
			HeadRefName: "codex/issue-1-manual", BaseRefName: "main", HeadSHA: "head-17", MergeCommitSHA: "merge-17", HeadRepository: "owner/repo",
		}},
	}
	_, err := loop.Store.Update("merged_pull_request_adopted", 1, "run_adoption_1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Status: "completed", RunID: "run_adoption_1", Branch: "codex/issue-1-manual",
			PullRequestURL: "https://example.test/pr/17", PullRequestNumber: 17, HeadSHA: "head-17",
			PullRequestMerged: true, GitHubSync: "done",
			MergedPullRequestAdoption: &state.MergedPullRequestAdoption{
				ID: "merged_pr_adoption_restart", Status: "github_sync_pending", Generation: 1,
				PreviousStatus: "blocked", PullRequestURL: "https://example.test/pr/17", PullRequestNumber: 17,
				Branch: "codex/issue-1-manual", BaseBranch: "main", HeadSHA: "head-17", MergeSHA: "merge-17",
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := loop.issueState(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.processExisting(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues["1"]
	if github.doneCalls != 1 || github.inspectCalls != 1 || issue.GitHubSync != "" || issue.MergedPullRequestAdoption.Status != "synced" {
		t.Fatalf("github=%+v issue=%+v", github, issue)
	}
}

type fakeGitHub struct {
	issue                     gh.Issue
	remote                    *gh.RemoteState
	claimed, done, needsInput bool
	doneCalls                 int
	markedRunning             bool
	readyPullRequest          bool
	updatedPullRequest        bool
	mergedPullRequest         bool
	checksRecoveryCalls       int
	checksRecoveryID          string
	inspectCalls              int
	claimErr                  error
	doneErr                   error
	listErr                   error
	inspectHook               func()
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
	if f.inspectHook != nil {
		f.inspectHook()
	}
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
	f.doneCalls++
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
func (f *fakeGitHub) MarkPullRequestChecksRecovery(_ context.Context, _ config.Config, _ int, recoveryID string) error {
	f.markedRunning = true
	f.checksRecoveryCalls++
	f.checksRecoveryID = recoveryID
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
	digest     string
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
func (f fakeWorktree) ContentDigest(context.Context, string) (string, error) { return f.digest, nil }

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

func (f *fakePublisher) Publish(_ context.Context, _ config.Config, _ gh.Issue, _, _, _, _, baseSHA string, declared []string) (worker.GitResult, publication.Audit, error) {
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
	result            worker.Result
	runPrompts        []string
	resumePrompts     []string
	resumeConfigPaths []string
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

func (f *recordingWorker) Resume(_ context.Context, cfg config.Config, _ gh.Issue, _ state.Issue, prompt string, _ worker.Started) (worker.Result, error) {
	f.resumePrompts = append(f.resumePrompts, prompt)
	f.resumeConfigPaths = append(f.resumeConfigPaths, cfg.RepoPath)
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

func TestWorkerEnvironmentBlockPreservesContinuationAndResourceState(t *testing.T) {
	result := worker.Result{
		Version: 1, Status: "blocked", ExecutionProfile: "extended",
		Summary: "local HTTP binding is unavailable", SessionID: "session-blocked",
	}
	loop, _ := testLoop(t, result)
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues["1"]
	if issue.Status != "blocked" || issue.SessionID != "session-blocked" || issue.Session == nil || issue.Lease == nil {
		t.Fatalf("continuation state was not preserved: %+v", issue)
	}
	if issue.BlockedCause == nil || issue.BlockedCause.Origin != "worker" || issue.BlockedCause.Kind != "environment" || !issue.BlockedCause.Resumable {
		t.Fatalf("blocked provenance=%+v", issue.BlockedCause)
	}
	if len(issue.DeclaredResources) == 0 || len(issue.Lease.ResolvedResources) == 0 {
		t.Fatalf("resource metadata was not preserved: %+v", issue.Lease)
	}
}

func TestEnvironmentResumeContinuesSameSessionAndWorktree(t *testing.T) {
	result := worker.Result{Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "verification pending", Retry: &worker.Retry{Reason: "verification pending"}}
	loop, _ := testLoop(t, result)
	worktreePath := loop.Config.RepoPath
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_environment", Slot: 0,
		DeclaredResources: []string{"repo:*"}, ResolvedResources: []string{"repo:*"}, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("environment_resume_requested", 1, "run_environment", nil, func(s *state.Snapshot) error {
		item := s.Issues["1"]
		item.Status = "environment_resume_pending"
		item.Worktree = worktreePath
		item.Branch = "codex/issue-1-test"
		item.ExecutionProfile = "extended"
		item.SessionID = "session-blocked"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-blocked"}
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "CDP unavailable", BlockedAt: time.Now().UTC()}
		item.EnvironmentResume = &state.EnvironmentResume{ID: "resume_1", Status: "github_synced", ConfirmedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recordingWorker{result: result}
	loop.Worker = recorder
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	if len(recorder.resumePrompts) != 1 || len(recorder.runPrompts) != 0 || recorder.resumeConfigPaths[0] != worktreePath {
		t.Fatalf("run=%d resume=%d paths=%v", len(recorder.runPrompts), len(recorder.resumePrompts), recorder.resumeConfigPaths)
	}
	if !strings.Contains(recorder.resumePrompts[0], "CDP unavailable") {
		t.Fatalf("resume prompt=%q", recorder.resumePrompts[0])
	}
	snapshot, _ := loop.Store.Load()
	if item := snapshot.Issues["1"]; item.RunID != "run_environment" || item.Worktree != worktreePath || item.SessionID != "session-blocked" || item.Lease == nil {
		t.Fatalf("resume replaced durable state: %+v", item)
	}
}

func TestEnvironmentResumeGitHubSyncConvergesBeforeWorkerStarts(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("environment_resume_requested", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "environment_resume_pending", RunID: "run_1", GitHubSync: "environment_resume",
			EnvironmentResume: &state.EnvironmentResume{ID: "resume_1", Status: "requested", ConfirmedAt: time.Now().UTC()},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	snapshot, _ := loop.Store.Load()
	item := snapshot.Issues["1"]
	if !github.markedRunning || item.Status != "environment_resume_pending" || item.GitHubSync != "" || item.EnvironmentResume.Status != "github_synced" {
		t.Fatalf("github=%+v item=%+v", github, item)
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

func TestFormatterFailurePersistsStructuredAuditAndSchedulesRetry(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done"}
	loop, _ := testLoop(t, result)
	loop.Publisher = &fakePublisher{
		audit: publication.Audit{
			Reason: publication.ReasonFormatterFailed,
			Formatter: publication.FormatterAudit{
				Name: "gofmt", Enabled: true, FileCount: 2, Result: "failed", FailureCode: "timeout",
			},
		},
		err: publication.FormatterError{Code: "timeout", Detail: "deadline exceeded"},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if issue := snapshot.Issues["1"]; issue.Status != "retry_wait" || issue.PullRequestURL != "" || issue.PublicationAudit == nil || issue.PublicationAudit.Formatter.FailureCode != "timeout" {
		t.Fatalf("formatter failure crossed publication boundary: %+v", issue)
	}
	events, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(events), `"reason":"formatter_failed"`) || !strings.Contains(string(events), `"failure_code":"timeout"`) || !strings.Contains(string(events), `"file_count":2`) {
		t.Fatalf("structured formatter event missing: err=%v events=%s", err, events)
	}
}

func TestTypedMissingBaseFailurePreservesRecoveryProvenanceAndSession(t *testing.T) {
	result := worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "verified", SessionID: "session-publication",
	}
	loop, _ := testLoop(t, result)
	loop.Config.Queue.MaxAttempts = 1
	loop.Publisher = &fakePublisher{err: publication.DurableBaseMissingError{}}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues["1"]
	if issue.Status != "failed" || issue.GitHubSync != "" || issue.Lease != nil || issue.SessionID != "session-publication" || issue.Session == nil {
		t.Fatalf("recoverable publication boundary was not preserved: %+v", issue)
	}
	if issue.PublicationFailure == nil || issue.PublicationFailure.Origin != publication.FailureOriginPublisher || issue.PublicationFailure.Phase != publication.FailurePhasePrePublication || issue.PublicationFailure.Code != publication.FailureCodeDurableBaseMissing || !issue.PublicationFailure.Recoverable {
		t.Fatalf("typed publication provenance missing: %+v", issue.PublicationFailure)
	}
	if len(issue.PublicationFailure.ResolvedResources) != 1 || issue.PublicationFailure.ResolvedResources[0] != state.RepositoryResource {
		t.Fatalf("publication resource metadata was not retained: %+v", issue.PublicationFailure)
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

func TestPullRequestChecksRecoveryResumesSamePRAndReleasesLeaseOnlyAfterMerge(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Completion.AutoMerge = true
	loop.Config.Queue.MaxAttempts = 1
	runID := "run_checks_recovery"
	branch := "codex/issue-1-checks"
	prURL := "https://example.test/pr/1"
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: runID, Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("fixture", 1, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "awaiting_checks"
		item.Worktree = loop.Config.RepoPath
		item.Branch = branch
		item.PullRequestURL = prURL
		item.PullRequestNumber = 1
		item.Attempts = 1
		item.ExecutionProfile = "standard"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	failedPR := gh.PullRequest{Number: 1, URL: prURL, State: "OPEN", IsDraft: true, HeadRefName: branch, BaseRefName: "main", HeadSHA: "old-head", ChecksStatus: "failure"}
	current, err := loop.issueState(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.failPullRequestChecks(context.Background(), current, failedPR, "Pull Request checks failed: "+prURL); err != nil {
		t.Fatal(err)
	}
	failed, _ := loop.issueState(1)
	if failed.Status != "failed" || failed.Lease == nil || !state.RecoverablePullRequestChecksFailure(&failed) || failed.PullRequestChecksFailure.HeadSHA != "old-head" {
		t.Fatalf("typed terminal failure did not retain the lease: %+v", failed)
	}

	recoveryID := "checks_recovery_1"
	_, err = loop.Store.Update("pull_request_checks_recovery_requested", 1, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "pull_request_checks_recovery_pending"
		item.GitHubSync = "pull_request_checks_recovery"
		item.HeadSHA = "new-head"
		item.PullRequestChecksRecovery = &state.PullRequestChecksRecovery{
			ID: recoveryID, Status: "requested", Generation: 1, ConfirmedAt: time.Now().UTC(),
			OldHeadSHA: "old-head", NewHeadSHA: "new-head", ChecksStatus: "success",
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.FailedLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: prURL, State: "OPEN", IsDraft: true, HeadRefName: branch, BaseRefName: "main", HeadSHA: "new-head", MergeStateStatus: "CLEAN", ChecksStatus: "success"}},
	}
	pending, _ := loop.issueState(1)
	if err := loop.processExisting(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	resumed, _ := loop.issueState(1)
	if resumed.Status != "awaiting_checks" || resumed.Lease == nil || resumed.Attempts != 1 || resumed.PullRequestChecksRecovery.Status != "resumed" || github.checksRecoveryCalls != 1 || github.checksRecoveryID != recoveryID {
		t.Fatalf("same PR lifecycle was not resumed safely: issue=%+v github=%+v", resumed, github)
	}

	github.remote.Issue.Labels = []string{loop.Config.GitHub.RunningLabel}
	if err := loop.processExisting(context.Background(), resumed); err != nil {
		t.Fatal(err)
	}
	awaitingMerge, _ := loop.issueState(1)
	if awaitingMerge.Status != "awaiting_merge" || awaitingMerge.Lease == nil || !github.readyPullRequest || !github.mergedPullRequest {
		t.Fatalf("Draft/auto-merge lifecycle did not retain the lease: %+v", awaitingMerge)
	}
	mergedAt := time.Now().UTC()
	github.remote.PullRequests[0].IsDraft = false
	github.remote.PullRequests[0].MergedAt = &mergedAt
	github.remote.PullRequests[0].State = "MERGED"
	if err := loop.processExisting(context.Background(), awaitingMerge); err != nil {
		t.Fatal(err)
	}
	completed, _ := loop.issueState(1)
	if completed.Status != "completed" || completed.Lease != nil || !completed.PullRequestMerged {
		t.Fatalf("merge did not release the retained lease exactly at completion: %+v", completed)
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
	if issueUsesWorkerSlot(state.Issue{Status: "pull_request_checks_recovery_pending", GitHubSync: "pull_request_checks_recovery"}) {
		t.Fatal("checks recovery synchronization consumed a worker slot")
	}
}

func TestAutoMergeUpdatesBehindBranchBeforePendingChecks(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	loop.Config.Completion.AutoMerge = true
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pr/1", State: "OPEN", IsDraft: true, HeadRefName: "codex/issue-1-test", MergeStateStatus: "BEHIND", ChecksStatus: "pending"}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].Status != "awaiting_checks" || !github.updatedPullRequest || github.readyPullRequest || github.mergedPullRequest {
		t.Fatalf("issue=%+v github=%+v", snapshot.Issues["1"], github)
	}
}

func TestDirtyPullRequestStartsConflictRecoveryWithoutCheckRuns(t *testing.T) {
	for _, checksStatus := range []string{"", "pending"} {
		t.Run("checks_"+checksStatus, func(t *testing.T) {
			loop, github := testLoop(t, worker.Result{})
			loop.Config.Completion.AutoMerge = true
			prURL := "https://example.test/pr/1"
			_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
				s.Issues["1"] = &state.Issue{
					Number: 1, Title: "Test", Status: "awaiting_checks", RunID: "run_1",
					Branch: "codex/issue-1-test", Worktree: loop.Config.RepoPath, PullRequestURL: prURL,
					Attempts: 1, UpdatedAt: time.Now().UTC(),
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
			github.remote = &gh.RemoteState{Issue: github.issue, PullRequests: []gh.PullRequest{{
				Number: 1, URL: prURL, State: "OPEN", HeadRefName: "codex/issue-1-test",
				MergeStateStatus: "DIRTY", ChecksStatus: checksStatus,
			}}}
			resolver := &fakeConflictResolver{preparation: conflict.Preparation{
				PreviousBaseSHA: "base-old", TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
				ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"},
			}}
			loop.Conflicts = resolver
			loop.Worker = fakeWorker{result: worker.Result{
				Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "retry later",
			}}

			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			item := snapshot.Issues["1"]
			if item.Status != "resolving_conflict" || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 || resolver.prepareCalls != 2 {
				t.Fatalf("item=%+v resolver=%+v", item, resolver)
			}
			if github.updatedPullRequest || github.readyPullRequest || github.mergedPullRequest {
				t.Fatalf("unexpected additional Pull Request mutation: %+v", github)
			}
		})
	}
}

func TestNonDirtyPendingChecksAndUnstableMergeStateKeepPolling(t *testing.T) {
	tests := []struct {
		name       string
		mergeState string
		checks     string
	}{
		{name: "clean pending checks", mergeState: "CLEAN", checks: "pending"},
		{name: "unknown pending checks", mergeState: "UNKNOWN", checks: "pending"},
		{name: "unstable successful checks", mergeState: "UNSTABLE", checks: "success"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop, github := testLoop(t, worker.Result{})
			loop.Config.Completion.AutoMerge = true
			prURL := "https://example.test/pr/1"
			_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
				s.Issues["1"] = &state.Issue{
					Number: 1, Status: "awaiting_checks", RunID: "run_1", Branch: "codex/issue-1-test",
					Worktree: loop.Config.RepoPath, PullRequestURL: prURL, UpdatedAt: time.Now().UTC(),
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			github.remote = &gh.RemoteState{
				Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
				PullRequests: []gh.PullRequest{{Number: 1, URL: prURL, State: "OPEN", HeadRefName: "codex/issue-1-test", MergeStateStatus: test.mergeState, ChecksStatus: test.checks}},
			}
			resolver := &fakeConflictResolver{}
			loop.Conflicts = resolver

			if _, err := loop.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if item := snapshot.Issues["1"]; item.Status != "awaiting_checks" || item.RetryAfter == nil || resolver.prepareCalls != 0 {
				t.Fatalf("item=%+v resolver=%+v", item, resolver)
			}
			if github.updatedPullRequest || github.readyPullRequest || github.mergedPullRequest {
				t.Fatalf("unexpected Pull Request mutation: %+v", github)
			}
		})
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
	budget := int64(4321)
	result := worker.Result{Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "decision", SessionID: "session", Question: &worker.Question{Text: "Choose?", Reason: "public API", AllowFreeText: true}, Goal: &worker.Goal{
		ThreadID: "session", Objective: "Complete Issue", Status: "blocked", TokenBudget: &budget,
		TimeBudgetSeconds: 3600, TokensUsed: 123, TimeUsedSeconds: 17,
	}}
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
	if goal := snapshot.Issues["1"].Goal; goal == nil || goal.Status != "blocked" || goal.TokensUsed != 123 || goal.TimeBudgetSeconds != 3600 {
		t.Fatalf("goal=%+v", goal)
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

func TestPublicationRecoveryPublishesSavedCompletedResultWithoutWorker(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.issue.State = "OPEN"
	github.issue.Labels = []string{loop.Config.GitHub.RunningLabel}
	runID := "run_publication_recovery"
	runDir := filepath.Join(loop.Store.Dir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	resultData := []byte(`{"version":1,"status":"completed","execution_profile":"extended","summary":"verified implementation","question":null,"tests":[{"command":"go test ./...","result":"pass"}],"git":null,"retry":null}`)
	if err := os.WriteFile(filepath.Join(runDir, "result-1.json"), resultData, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(resultData))
	now := time.Now().UTC()
	_, err := loop.Store.Update("publication_recovery_requested", 1, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "publication_recovery_pending", RunID: runID,
			Branch: "codex/issue-1-test", Worktree: loop.Config.RepoPath, Attempts: 3,
			LeaseGeneration: 1,
			Lease: &state.ResourceLease{
				Owner: state.LeaseOwner{RunID: runID, Generation: 1}, Slot: 0,
				DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
				BaseSHA: "base-sha", ReservedAt: now,
			},
			DeclaredResources: []string{state.RepositoryResource},
			PublicationRecovery: &state.PublicationRecovery{
				ID: "publication_recovery_1", Status: "github_synced", Generation: 1,
				MaxAttempts: 3, ConfirmedAt: now, ResultSHA256: digest,
				Summary: "verified implementation", ExpectedHeadSHA: "worker-head", WorktreeSHA256: "worktree-digest", OriginalDirty: true,
			},
			UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, digest: "worktree-digest", inspection: &worktree.Inspection{
		Exists: true, Valid: true, Branch: "codex/issue-1-test", Head: "worker-head", LocalBranchExists: true, RemoteBranchExists: true, RemoteConsistent: true,
	}}
	publisher := &fakePublisher{result: worker.GitResult{
		Branch: "codex/issue-1-test", Commit: "published-head", PullRequestURL: "https://example.test/pr/1",
	}}
	loop.Publisher = publisher
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues["1"]
	if !publisher.called || issue.Status != "awaiting_checks" || issue.PullRequestURL != "https://example.test/pr/1" || issue.Attempts != 3 || issue.PublicationRecovery.Attempts != 1 || issue.PublicationRecovery.Status != "succeeded" {
		t.Fatalf("publication-only recovery did not converge: publisher=%+v issue=%+v", publisher, issue)
	}
	if len(issue.PublicationRecovery.History) != 1 || issue.PublicationRecovery.History[0].Status != "succeeded" || issue.PublicationAudit == nil || issue.PublicationAudit.BaseSHA != "base-sha" {
		t.Fatalf("publication recovery audit/history missing: %+v", issue)
	}
	github.remote = &gh.RemoteState{Issue: github.issue, PullRequests: []gh.PullRequest{{
		URL: issue.PullRequestURL, State: "OPEN", HeadRefName: issue.Branch, ChecksStatus: "success",
	}}}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("checks worked=%v err=%v", worked, err)
	}
	_, err = loop.Store.Update("fault_merge_due", 1, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].RetryAfter = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mergedAt := time.Now().UTC()
	github.remote.PullRequests[0].State = "MERGED"
	github.remote.PullRequests[0].MergedAt = &mergedAt
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("merge worked=%v err=%v", worked, err)
	}
	snapshot, err = loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if issue = snapshot.Issues["1"]; issue.Status != "completed" || issue.Lease != nil || !issue.PullRequestMerged || !github.done {
		t.Fatalf("recovered publication did not reach done: issue=%+v github=%+v", issue, github)
	}
}

func TestFaultPublicationRecoveryRecognizesInterruptedAttemptWithoutResettingBudget(t *testing.T) {
	recovery := &state.PublicationRecovery{
		Status: "publishing", Attempts: 3, MaxAttempts: 3,
		History: []state.PublicationRecoveryAttempt{
			{Number: 1, Status: "failed", FinishedAt: time.Now().UTC()},
			{Number: 2, Status: "failed", FinishedAt: time.Now().UTC()},
			{Number: 3, Status: "running", StartedAt: time.Now().UTC()},
		},
	}
	if !publicationRecoveryAttemptRunning(recovery, 3) {
		t.Fatal("interrupted write-ahead publication attempt was not resumable")
	}
	recovery.History[2].Status = "failed"
	recovery.History[2].FinishedAt = time.Now().UTC()
	if publicationRecoveryAttemptRunning(recovery, 3) {
		t.Fatal("finished publication attempt was treated as resumable")
	}
}

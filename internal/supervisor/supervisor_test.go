package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/capability"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/conflict"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type workspaceMutationWorker struct {
	mainPath string
	runs     int
	resumes  int
	paths    []string
}

func (w *workspaceMutationWorker) result(cfg config.Config) (worker.Result, error) {
	if cfg.RepoPath == w.mainPath {
		return worker.Result{}, fmt.Errorf("worker received dirty main checkout as cwd")
	}
	w.paths = append(w.paths, cfg.RepoPath)
	if err := os.WriteFile(filepath.Join(cfg.RepoPath, "continuation.txt"), []byte(fmt.Sprintf("invocations=%d\n", len(w.paths))), 0o600); err != nil {
		return worker.Result{}, err
	}
	return worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "continue",
		SessionID: "session-workspace", Tests: []worker.Test{}, Retry: &worker.Retry{Reason: "continue"},
	}, nil
}

func (w *workspaceMutationWorker) Run(_ context.Context, cfg config.Config, _ gh.Issue, _ state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	w.runs++
	return w.result(cfg)
}

func (w *workspaceMutationWorker) Resume(_ context.Context, cfg config.Config, _ gh.Issue, _ state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	w.resumes++
	return w.result(cfg)
}

func testGitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
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
	issue                       gh.Issue
	remote                      *gh.RemoteState
	claimed, done, needsInput   bool
	doneCalls                   int
	markedRunning               bool
	readyPullRequest            bool
	updatedPullRequest          bool
	mergedPullRequest           bool
	checksRecoveryCalls         int
	checksRecoveryID            string
	answeredWorkspaceRecoveries int
	inspectCalls                int
	failedCalls                 int
	claimErr                    error
	doneErr                     error
	failedErr                   error
	listErr                     error
	inspectHook                 func()
}

type rawCapabilityGitHub struct{ *fakeGitHub }

func (f rawCapabilityGitHub) Get(context.Context, config.Config, int) (gh.Issue, error) {
	return f.issue, nil
}

const supervisorTestCapabilityMetadata = "\n<!-- agent-loop:capabilities\nversion: 1\nprofile: standard\nnetwork: none\nbrowser_cdp: false\ndownload: false\nexternal_time_gate: false\n-->"

func capabilityReadyIssue(issue gh.Issue) gh.Issue {
	if !strings.Contains(issue.Body, "<!-- agent-loop:capabilities") {
		issue.Body += supervisorTestCapabilityMetadata
	}
	return issue
}

func (f *fakeGitHub) ListReady(context.Context, config.Config) ([]gh.Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []gh.Issue{capabilityReadyIssue(f.issue)}, nil
}
func (f *fakeGitHub) Get(context.Context, config.Config, int) (gh.Issue, error) {
	return capabilityReadyIssue(f.issue), nil
}
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
func (f *fakeGitHub) MarkFailed(context.Context, config.Config, int, string, bool) error {
	f.failedCalls++
	if f.failedErr != nil {
		err := f.failedErr
		f.failedErr = nil
		return err
	}
	return nil
}
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
func (f *fakeGitHub) MarkAnsweredWorkspaceRecovery(context.Context, config.Config, int, string) error {
	f.markedRunning = true
	f.answeredWorkspaceRecoveries++
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
func (f fakeWorktree) ValidateLaunch(_ context.Context, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
	return worktree.LaunchValidation{
		Valid: true, ExpectedCWD: path, CanonicalCWD: path, TopLevel: path, Branch: branch,
		CommonDir: filepath.Join(cfg.RepoPath, ".git"), MainCheckout: cfg.RepoPath,
		Checks: map[string]bool{"fixture": true},
	}, nil
}
func (f fakeWorktree) ContentDigest(context.Context, string) (string, error) { return f.digest, nil }

type fakeWorker struct {
	result worker.Result
	err    error
}

func fixtureLease(runID string) *state.ResourceLease {
	return &state.ResourceLease{
		Owner: state.LeaseOwner{RunID: runID, Generation: 1}, Slot: 0,
		DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, ReservedAt: time.Now().UTC(),
	}
}

func fixtureWorkspace(loop *Loop, path, branch string) *state.WorkerWorkspace {
	return &state.WorkerWorkspace{
		Path: path, Branch: branch, RepoID: loop.Store.RepoID,
		Repository: loop.Config.GitHub.Repo, RepositoryID: loop.Config.GitHub.RepositoryID,
		GitCommonDir: filepath.Join(loop.Config.RepoPath, ".git"), MainCheckout: loop.Config.RepoPath,
		CapturedAt: time.Now().UTC(),
	}
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
	states  []state.Issue
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

func (s *scriptedWorker) Run(_ context.Context, _ config.Config, _ gh.Issue, issue state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	s.runs++
	s.states = append(s.states, issue)
	return s.next()
}

func (s *scriptedWorker) Resume(_ context.Context, _ config.Config, _ gh.Issue, issue state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	s.resumes++
	s.states = append(s.states, issue)
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
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	body := "Implement it\n" + strings.Replace(supervisorTestCapabilityMetadata, "profile: standard", "profile: "+profile, 1)
	github := &fakeGitHub{issue: gh.Issue{Number: 1, Title: "Test", Body: body, Labels: []string{"codex-loop:ready"}}}
	return &Loop{Config: cfg, Store: store, GitHub: github, Worktrees: fakeWorktree{path: repo}, Worker: fakeWorker{result: result}}, github
}

func TestCapabilityMismatchPrecedesLeaseClaimWorktreeAndGitHubMutation(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	before, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	github.issue.Body = "<!-- agent-loop:capabilities\nversion: 1\nprofile: standard\nnetwork: public\nbrowser_cdp: false\ndownload: false\nexternal_time_gate: false\n-->"
	loop.GitHub = rawCapabilityGitHub{fakeGitHub: github}
	if err := loop.startIssueAtSlotWithResources(context.Background(), github.issue, "run_capability", 0); err != nil {
		t.Fatal(err)
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != before.StateRevision || after.Issues["1"] != nil || github.claimed {
		t.Fatalf("capability mismatch caused side effects: before=%d after=%d issue=%+v claimed=%v", before.StateRevision, after.StateRevision, after.Issues["1"], github.claimed)
	}
}

func TestStartupClaimRechecksPersistedCapabilityBeforeFurtherSideEffects(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	requirements := &capability.Requirements{
		Version: 1, Profile: "standard", Network: capability.NetworkPublic,
	}
	provided := &capability.Provider{Version: 1, Profile: "standard", Network: capability.NetworkPublic}
	before, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_restart", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: time.Now().UTC(),
		CapabilityRequirements: requirements, WorkerCapabilities: provided,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := *before.Issues["1"]
	if err := loop.processExisting(context.Background(), current); err == nil || !strings.Contains(err.Error(), capability.CodeNetworkMismatch) {
		t.Fatalf("startup mismatch was not rejected: %v", err)
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != before.StateRevision || github.claimed {
		t.Fatalf("startup mismatch caused side effects: before=%d after=%d claimed=%v", before.StateRevision, after.StateRevision, github.claimed)
	}
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

func TestWorkerEnvironmentBlockParksLeaseAndPreservesContinuationState(t *testing.T) {
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
	if issue.Status != "blocked" || issue.SessionID != "session-blocked" || issue.Session == nil || issue.Lease != nil || issue.ResourcePark == nil || issue.ResourcePark.Status != "parked" {
		t.Fatalf("continuation state was not preserved: %+v", issue)
	}
	if issue.BlockedCause == nil || issue.BlockedCause.Origin != "worker" || issue.BlockedCause.Kind != "environment" || !issue.BlockedCause.Resumable {
		t.Fatalf("blocked provenance=%+v", issue.BlockedCause)
	}
	if len(issue.DeclaredResources) == 0 || len(issue.ResourcePark.OriginalLease.ResolvedResources) == 0 || issue.ResourcePark.OriginalLease.Owner.Generation != issue.LeaseGeneration {
		t.Fatalf("resource metadata was not preserved: %+v", issue.ResourcePark)
	}
}

func TestWorkerEnvironmentBlockParkAllowsFollowingRepositoryIssue(t *testing.T) {
	blocked := worker.Result{
		Version: 1, Status: "blocked", ExecutionProfile: "extended",
		Summary: "public network is unavailable", SessionID: "session-314",
	}
	loop, github := testLoop(t, blocked)
	loop.Config.Queue.Concurrency = 1
	github.issue = gh.Issue{Number: 314, Title: "Public verification", Body: "Verify production", Labels: []string{"codex-loop:ready"}}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("Issue #314 worked=%v err=%v", worked, err)
	}
	first, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if issue := first.Issues["314"]; issue == nil || issue.Status != "blocked" || issue.Lease != nil || issue.ResourcePark == nil || issue.SessionID != "session-314" {
		t.Fatalf("Issue #314 was not safely parked: %+v", issue)
	}

	github.issue = gh.Issue{Number: 448, Title: "Local follow-up", Body: "Continue queue", Labels: []string{"codex-loop:ready"}}
	loop.Worker = fakeWorker{result: worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session-448",
		Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/448"},
	}}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("Issue #448 worked=%v err=%v", worked, err)
	}
	second, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if issue := second.Issues["448"]; issue == nil || issue.Status != "awaiting_checks" || issue.Lease == nil || issue.Lease.Owner.RunID == "" {
		t.Fatalf("following Issue was not admitted: %+v", issue)
	}
	if issue := second.Issues["314"]; issue.Lease != nil || issue.ResourcePark == nil || issue.ResourcePark.Status != "parked" || issue.SessionID != "session-314" {
		t.Fatalf("following Issue changed parked continuation: %+v", issue)
	}
}

func TestFaultWorkerEnvironmentParkSurvivesGitHubSyncCrashIdempotently(t *testing.T) {
	loop, github := testLoop(t, worker.Result{
		Version: 1, Status: "blocked", ExecutionProfile: "extended",
		Summary: "network unavailable", SessionID: "session-blocked",
	})
	github.failedErr = errors.New("injected blocked label failure")
	if worked, err := loop.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("initial block worked=%v err=%v", worked, err)
	}
	parked, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := parked.Issues["1"]
	if issue.Status != "blocked" || issue.GitHubSync != "blocked" || issue.Lease != nil || issue.ResourcePark == nil {
		t.Fatalf("write-ahead park was not retained: %+v", issue)
	}
	parkID := issue.ResourcePark.ID
	parkOwner := issue.ResourcePark.OriginalLease.Owner
	generation := issue.LeaseGeneration
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("GitHub retry worked=%v err=%v", worked, err)
	}
	converged, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	issue = converged.Issues["1"]
	if issue.GitHubSync != "" || issue.Lease != nil || issue.ResourcePark == nil || issue.ResourcePark.ID != parkID || issue.ResourcePark.OriginalLease.Owner != parkOwner || issue.LeaseGeneration != generation || github.failedCalls != 2 {
		t.Fatalf("park retry duplicated or changed state: github=%+v issue=%+v", github, issue)
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
		item.Workspace = fixtureWorkspace(loop, worktreePath, item.Branch)
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

func TestFaultResumedEnvironmentParkRetriesAcrossRestartAndReleasesLease(t *testing.T) {
	retry := worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "verification pending",
		SessionID: "session-blocked", Retry: &worker.Retry{Reason: "verification pending"},
	}
	completed := worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "done",
		Git: &worker.GitResult{},
	}
	loop, github := testLoop(t, retry)
	loop.Config.Queue.Concurrency = 1
	loop.Config.Queue.MaxAttempts = 3
	loop.Config.Worker.Profiles["extended"] = config.Profile{MaxContinuations: 1}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	_, originalOwner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_environment", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: "base-environment", ReservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.Store.Update("worker_environment_blocked", 1, originalOwner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Worktree = loop.Config.RepoPath
		item.Branch = "codex/issue-1-test"
		item.Workspace = fixtureWorkspace(loop, item.Worktree, item.Branch)
		item.ExecutionProfile = "extended"
		item.SessionID = "session-blocked"
		item.Session = &state.WorkerSession{Backend: "codex", ID: "session-blocked"}
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: now.Add(time.Minute)}
		if parkErr := state.ParkIssueLease(item, originalOwner, "park_environment", now.Add(time.Minute)); parkErr != nil {
			return parkErr
		}
		item.ResourcePark.Kind = state.ResourceParkKindEnvironmentBlock
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var resumeOwner state.LeaseOwner
	if _, err := loop.Store.Update("environment_resume_requested", 1, originalOwner.RunID, nil, func(snapshot *state.Snapshot) error {
		var resumeErr error
		resumeOwner, resumeErr = state.ResumeParkedLease(snapshot, 1, "park_environment", 0, now.Add(2*time.Minute))
		if resumeErr != nil {
			return resumeErr
		}
		item := snapshot.Issues["1"]
		item.Status = "environment_resume_pending"
		item.EnvironmentResume = &state.EnvironmentResume{ID: "resume_environment", Status: "github_synced", ConfirmedAt: now.Add(2 * time.Minute)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	scripted := &scriptedWorker{results: []worker.Result{retry, retry, completed}}
	loop.Worker = scripted
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("environment resume worked=%v err=%v", worked, err)
	}
	if _, err := loop.Store.Update("fault_retry_due", 1, originalOwner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].RetryAfter = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("same-session continuation worked=%v err=%v", worked, err)
	}
	beforeRestart, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	before := beforeRestart.Issues["1"]
	if before.Status != "retry_wait" || before.RunID != originalOwner.RunID || before.SessionID != "session-blocked" ||
		before.Lease == nil || before.Lease.Owner != resumeOwner || before.Lease.BaseSHA != "base-environment" ||
		before.ResourcePark == nil || before.ResourcePark.Status != "resumed" || before.ResourcePark.ResumeOwner == nil || *before.ResourcePark.ResumeOwner != resumeOwner {
		t.Fatalf("resumed continuation provenance before restart=%+v", before)
	}
	if len(scripted.states) != 2 || scripted.states[1].Worktree != loop.Config.RepoPath || scripted.states[1].SessionID != "session-blocked" ||
		scripted.states[1].Lease == nil || scripted.states[1].Lease.Owner != resumeOwner || scripted.states[1].Lease.BaseSHA != "base-environment" {
		t.Fatalf("same-session retry did not preserve continuation boundary: %+v", scripted.states)
	}
	if _, err := loop.Store.Update("fault_retry_due", 1, originalOwner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].RetryAfter = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	restarted := &Loop{
		Config: loop.Config, Store: loop.Store, GitHub: loop.GitHub,
		Worktrees: loop.Worktrees, Worker: scripted,
	}
	restartSnapshot, err := restarted.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileStartup(context.Background(), restartSnapshot); err != nil {
		t.Fatal(err)
	}
	if worked, err := restarted.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("fresh retry after restart worked=%v err=%v", worked, err)
	}

	finished, err := restarted.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := finished.Issues["1"]
	if scripted.runs != 1 || scripted.resumes != 2 || item.Status != "completed" || item.Lease != nil ||
		item.RunID == originalOwner.RunID || item.LeaseGeneration != resumeOwner.Generation+1 || item.Worktree != loop.Config.RepoPath ||
		item.ResourcePark == nil || item.ResourcePark.Status != "resumed" || item.ResourcePark.OriginalLease.Owner != originalOwner ||
		item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != resumeOwner {
		t.Fatalf("retry after restart did not converge: runs=%d resumes=%d issue=%+v", scripted.runs, scripted.resumes, item)
	}
	if len(scripted.states) != 3 || scripted.states[2].Lease == nil || scripted.states[2].Lease.Owner.RunID != item.RunID ||
		scripted.states[2].Lease.Owner.Generation != resumeOwner.Generation+1 || scripted.states[2].Lease.BaseSHA != "base-environment" ||
		scripted.states[2].ResourcePark == nil || scripted.states[2].ResourcePark.ResumeOwner == nil || *scripted.states[2].ResourcePark.ResumeOwner != resumeOwner {
		t.Fatalf("fresh retry lost active lease or park provenance: %+v", scripted.states)
	}
}

func TestRetryContinuationKeepsDirtyBehindMainCheckoutUntouched(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	mainCheckout := filepath.Join(root, "main")
	updater := filepath.Join(root, "updater")
	testGitOutput(t, "init", "--bare", "-q", remote)
	testGitOutput(t, "clone", "-q", remote, mainCheckout)
	for _, pair := range [][2]string{{"user.name", "Test"}, {"user.email", "test@example.test"}, {"commit.gpgsign", "false"}} {
		testGitOutput(t, "-C", mainCheckout, "config", pair[0], pair[1])
	}
	if err := os.WriteFile(filepath.Join(mainCheckout, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGitOutput(t, "-C", mainCheckout, "add", "tracked.txt")
	testGitOutput(t, "-C", mainCheckout, "commit", "-q", "-m", "base")
	testGitOutput(t, "-C", mainCheckout, "branch", "-M", "main")
	testGitOutput(t, "-C", mainCheckout, "push", "-q", "-u", "origin", "main")
	testGitOutput(t, "clone", "-q", "--branch", "main", remote, updater)
	for _, pair := range [][2]string{{"user.name", "Test"}, {"user.email", "test@example.test"}, {"commit.gpgsign", "false"}} {
		testGitOutput(t, "-C", updater, "config", pair[0], pair[1])
	}
	if err := os.WriteFile(filepath.Join(updater, "remote.txt"), []byte("newer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGitOutput(t, "-C", updater, "add", "remote.txt")
	testGitOutput(t, "-C", updater, "commit", "-q", "-m", "newer")
	testGitOutput(t, "-C", updater, "push", "-q", "origin", "main")

	if err := os.WriteFile(filepath.Join(mainCheckout, "tracked.txt"), []byte("dirty main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mainCheckout, "staged.txt"), []byte("staged main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGitOutput(t, "-C", mainCheckout, "add", "staged.txt")
	if err := os.WriteFile(filepath.Join(mainCheckout, "untracked.txt"), []byte("untracked main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainHead := testGitOutput(t, "-C", mainCheckout, "rev-parse", "HEAD")
	mainStatus := testGitOutput(t, "-C", mainCheckout, "status", "--porcelain=v1", "--untracked-files=all")
	mainIndex := testGitOutput(t, "-C", mainCheckout, "diff", "--cached", "--binary")
	mainFiles := testGitOutput(t, "-C", mainCheckout, "diff", "--binary")

	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.RepositoryID = 123
	cfg.RepoPath = mainCheckout
	cfg.Git.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worker.Profiles["extended"] = config.Profile{MaxContinuations: 2}
	store := state.Store{Dir: filepath.Join(root, "state"), RepoID: "repo-fixture", RepoPath: mainCheckout}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	github := &fakeGitHub{issue: gh.Issue{
		Number: 1, Title: "Continue safely", Body: "fixture\n" + strings.Replace(supervisorTestCapabilityMetadata, "profile: standard", "profile: extended", 1),
		Labels: cfg.GitHub.ReadyLabels,
	}}
	runtime := &workspaceMutationWorker{mainPath: mainCheckout}
	loop := &Loop{
		Config: cfg, Store: store, GitHub: github,
		Worktrees: worktree.Manager{StateRoot: store.Dir, GitPath: "git"}, Worker: runtime,
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("initial worked=%v err=%v", worked, err)
	}
	_, err := store.Update("retry_due", 1, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].RetryAfter = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("continuation worked=%v err=%v", worked, err)
	}
	if runtime.runs != 1 || runtime.resumes != 1 || len(runtime.paths) != 2 || runtime.paths[0] != runtime.paths[1] || runtime.paths[0] == mainCheckout {
		t.Fatalf("runtime=%+v", runtime)
	}
	if behind := testGitOutput(t, "-C", mainCheckout, "rev-list", "--count", "HEAD..origin/main"); behind != "1" {
		t.Fatalf("main checkout is no longer the behind fixture: %s", behind)
	}
	if got := testGitOutput(t, "-C", mainCheckout, "rev-parse", "HEAD"); got != mainHead {
		t.Fatalf("main HEAD changed: got=%s want=%s", got, mainHead)
	}
	if got := testGitOutput(t, "-C", mainCheckout, "status", "--porcelain=v1", "--untracked-files=all"); got != mainStatus {
		t.Fatalf("main status changed:\ngot:\n%s\nwant:\n%s", got, mainStatus)
	}
	if got := testGitOutput(t, "-C", mainCheckout, "diff", "--cached", "--binary"); got != mainIndex {
		t.Fatal("main index changed")
	}
	if got := testGitOutput(t, "-C", mainCheckout, "diff", "--binary"); got != mainFiles {
		t.Fatal("main files changed")
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if item.Workspace == nil || item.Workspace.Path != runtime.paths[0] || item.Workspace.GitCommonDir == "" {
		t.Fatalf("workspace provenance=%+v", item.Workspace)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil || !strings.Contains(string(events), `"type":"worker_workspace_validated"`) || !strings.Contains(string(events), `"expected_cwd"`) {
		t.Fatalf("workspace audit event missing: err=%v events=%s", err, events)
	}
}

func TestContinuationFailsClosedWhenSavedWorkspaceProvenanceChanges(t *testing.T) {
	retry := worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "continue",
		SessionID: "session-provenance", Tests: []worker.Test{}, Retry: &worker.Retry{Reason: "continue"},
	}
	loop, github := testLoop(t, retry)
	scripted := &scriptedWorker{results: []worker.Result{retry, retry}}
	loop.Worker = scripted
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("initial worked=%v err=%v", worked, err)
	}
	_, err := loop.Store.Update("tamper_workspace_provenance", 1, "", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.RetryAfter = nil
		item.Workspace.GitCommonDir = filepath.Join(t.TempDir(), ".git")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("continuation worked=%v err=%v", worked, err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if scripted.runs != 1 || scripted.resumes != 0 {
		t.Fatalf("backend was invoked after rejection: runs=%d resumes=%d", scripted.runs, scripted.resumes)
	}
	if item.Status != "blocked" || item.Lease == nil || item.SessionID != "session-provenance" || item.Workspace == nil ||
		item.BlockedCause == nil || item.BlockedCause.Kind != "worker_workspace" || item.BlockedCause.Resumable {
		t.Fatalf("rejected continuation state=%+v", item)
	}
	github.issue.State = "OPEN"
	github.issue.Labels = []string{"blocked"}
	beforeRestart := snapshot
	if err := loop.reconcileStartup(context.Background(), beforeRestart); err != nil {
		t.Fatal(err)
	}
	afterRestart, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item = afterRestart.Issues["1"]
	if item.Lease == nil || item.SessionID != "session-provenance" || item.Workspace == nil {
		t.Fatalf("restart discarded rejected continuation boundary: %+v", item)
	}
	events, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(events), `"type":"worker_workspace_rejected"`) || !strings.Contains(string(events), `"expected_cwd"`) {
		t.Fatalf("workspace rejection event missing: err=%v events=%s", err, events)
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

func TestFailedChecksWhileAwaitingAutoMergeReturnIssueToRetry(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done", SessionID: "session", Git: &worker.GitResult{PullRequestURL: "https://example.test/pr/1"}}
	loop, github := testLoop(t, result)
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{
			Number: 1, URL: "https://example.test/pr/1", State: "OPEN", IsDraft: true,
			HeadRefName: "codex/issue-1-test", MergeStateStatus: "CLEAN", ChecksStatus: "success",
		}},
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	loop.Config.Completion.AutoMerge = true
	github.remote.PullRequests[0].IsDraft = false
	github.remote.PullRequests[0].MergeStateStatus = "BLOCKED"
	github.remote.PullRequests[0].ChecksStatus = "failure"
	_, err := loop.Store.Update("test_retry_due", 1, "", nil, func(s *state.Snapshot) error {
		s.Issues["1"].RetryAfter = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatalf("awaiting_merge checks failure stopped the lifecycle: %v", err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if item := snapshot.Issues["1"]; item.Status != "retry_wait" || !strings.Contains(item.LastError, "checks failed") {
		t.Fatalf("issue=%+v", item)
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
		PullRequests: []gh.PullRequest{{Number: 1, URL: prURL, State: "OPEN", IsDraft: true, HeadRefName: branch, BaseRefName: "main", HeadSHA: "new-head", MergeStateStatus: "CLEAN", ChecksStatus: "success", HeadRepository: loop.Config.GitHub.Repo}},
	}
	pending, _ := loop.issueState(1)
	github.remote.PullRequests[0].HeadRepository = "attacker/repo"
	if err := loop.processExisting(context.Background(), pending); err == nil {
		t.Fatal("fork Pull Request was accepted during recovery reconciliation")
	}
	stillPending, _ := loop.issueState(1)
	if stillPending.Status != "pull_request_checks_recovery_pending" || stillPending.GitHubSync != "pull_request_checks_recovery" {
		t.Fatalf("fork rejection mutated recovery state: %+v", stillPending)
	}
	github.remote.PullRequests[0].HeadRepository = loop.Config.GitHub.Repo
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
				branch := "codex/issue-1-test"
				s.Issues["1"] = &state.Issue{
					Number: 1, Title: "Test", Status: "awaiting_checks", RunID: "run_1",
					Branch: branch, Worktree: loop.Config.RepoPath, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
					LeaseGeneration: 1, Lease: fixtureLease("run_1"), PullRequestURL: prURL,
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
		branch := "codex/issue-1-test"
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "awaiting_checks", RunID: "run_1",
			Branch: branch, Worktree: loop.Config.RepoPath, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch), PullRequestURL: prURL,
			LeaseGeneration: 1, Lease: fixtureLease("run_1"),
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

func TestZeitreise442V0619ResumedNeedsInputStartsFencedConflictWorker(t *testing.T) {
	fixtureState, err := os.ReadFile("testdata/zeitreise-442-v0619-needs-input-conflict-state.json")
	if err != nil {
		t.Fatal(err)
	}
	fixtureEvents, err := os.ReadFile("testdata/zeitreise-442-v0619-needs-input-conflict-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "state.json"), fixtureState, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "events.jsonl"), fixtureEvents, 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: stateDir, RepoID: "repo_zeitreise", RepoPath: "/sanitized/zeitreise"}
	before, err := store.Load()
	if err != nil {
		t.Fatalf("v0.6.19 fixture was rejected: %v", err)
	}
	beforeIssue := before.Issues["442"]
	request := before.PendingRequests["req_b24ba2cd328c461f"]
	if beforeIssue == nil || request == nil || beforeIssue.Lease == nil || beforeIssue.Lease.Owner.Generation != 4 ||
		beforeIssue.ResourcePark == nil || beforeIssue.ResourcePark.OriginalLease.Owner.Generation != 3 ||
		beforeIssue.ResourcePark.ResumeOwner == nil || beforeIssue.ResourcePark.ResumeOwner.Generation != 4 {
		t.Fatalf("fixture lost needs-input resume provenance: issue=%+v request=%+v", beforeIssue, request)
	}

	cfg := config.Defaults()
	cfg.GitHub.Repo = "ishii1648/zeitreise"
	cfg.RepoPath = "/sanitized/zeitreise"
	github := &fakeGitHub{issue: gh.Issue{Number: 442, Title: beforeIssue.Title, State: "OPEN", Labels: []string{cfg.GitHub.RunningLabel}}}
	resolver := &fakeConflictResolver{preparation: conflict.Preparation{
		TargetBaseSHA: beforeIssue.ConflictRecovery.TargetBaseSHA,
		ConflictFiles: append([]string(nil), beforeIssue.ConflictRecovery.ConflictFiles...),
	}}
	recorder := &recordingWorker{result: worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "fixture stops after worker launch",
		Tests: []worker.Test{}, Retry: &worker.Retry{Reason: "fixture stops after worker launch"},
	}}
	loop := &Loop{
		Config: cfg, Store: store, GitHub: github, Conflicts: resolver, Worker: recorder,
		Worktrees: fakeWorktree{path: beforeIssue.Worktree},
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("conflict recovery worked=%v err=%v", worked, err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := after.Issues["442"]
	if len(recorder.runPrompts) != 1 || resolver.prepareCalls != 1 {
		t.Fatalf("conflict worker was not started exactly once: resolver=%+v prompts=%d", resolver, len(recorder.runPrompts))
	}
	if item.RunID == beforeIssue.RunID || !strings.HasPrefix(item.RunID, "conflict_") || item.LeaseGeneration != 5 ||
		item.Lease == nil || item.Lease.Owner != (state.LeaseOwner{RunID: item.RunID, Generation: 5}) {
		t.Fatalf("conflict lease transfer was not fenced: %+v", item)
	}
	if item.ResourcePark == nil || item.ResourcePark.Status != "resumed" ||
		item.ResourcePark.OriginalLease.Owner != beforeIssue.ResourcePark.OriginalLease.Owner ||
		item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != *beforeIssue.ResourcePark.ResumeOwner ||
		after.PendingRequests[request.ID].RunID != request.RunID || after.PendingRequests[request.ID].ReleasedOwner == nil ||
		*after.PendingRequests[request.ID].ReleasedOwner != *request.ReleasedOwner {
		t.Fatalf("historical needs-input provenance changed: before=%+v after=%+v", beforeIssue, item)
	}
	if len(item.ConflictRecovery.History) != 1 || item.ConflictRecovery.History[0].Status != "retryable_failure" ||
		!strings.Contains(recorder.runPrompts[0], "sanitized/conflict-file.ts") {
		t.Fatalf("conflict attempt or prompt was not preserved: recovery=%+v prompt=%q", item.ConflictRecovery, recorder.runPrompts[0])
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"conflict_recovery_attempt_started"`) ||
		!strings.Contains(string(events), `"lease_owner":{"generation":5,"run_id":"`+item.RunID+`"}`) {
		t.Fatalf("fenced conflict attempt event was not persisted: %s", events)
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
		branch := "codex/issue-1-test"
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "resolving_conflict", RunID: "conflict_1", Branch: branch,
			Worktree: loop.Config.RepoPath, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			LeaseGeneration: 1, Lease: fixtureLease("conflict_1"), PullRequestURL: "https://example.test/pr/1", UpdatedAt: time.Now().UTC(),
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
	if snapshot.Issues["1"].Status != "needs_input" || snapshot.Issues["1"].Lease != nil || snapshot.Issues["1"].ResourcePark == nil || snapshot.Issues["1"].ResourcePark.Kind != state.ResourceParkKindNeedsInput {
		t.Fatalf("issue=%+v", snapshot.Issues["1"])
	}
	if len(snapshot.PendingRequests) != 1 {
		t.Fatalf("requests=%d", len(snapshot.PendingRequests))
	}
	for id, request := range snapshot.PendingRequests {
		if request.ID != id || request.Question != "Choose?" || request.ResourceParkID != snapshot.Issues["1"].ResourcePark.ID || request.ReleasedOwner == nil {
			t.Fatalf("request=%+v", request)
		}
	}
}

func TestAnsweredNeedsInputClaimWaitsThenReacquiresOnce(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Queue.Concurrency = 1
	github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.NeedsInputLabel}}
	now := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	originalOwner := state.LeaseOwner{RunID: "run_1", Generation: 1}
	competingOwner := state.LeaseOwner{RunID: "run_2", Generation: 1}
	answer := state.AnswerRecord{RequestID: "req_1", Question: "Continue?", Answer: "yes", AnsweredAt: now}
	_, err := loop.Store.Update("fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		branch := "codex/issue-1-test"
		item := &state.Issue{
			Number: 1, Title: "Test", Status: "needs_input", RunID: "run_1", LeaseGeneration: 1,
			Worktree: loop.Config.RepoPath, Branch: branch, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			SessionID: "session-1", Session: &state.WorkerSession{Backend: "codex", ID: "session-1"},
			Attempts: 2, Continuations: 1, Answers: []state.AnswerRecord{answer},
			Lease: &state.ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base-1", ReservedAt: now},
		}
		if parkErr := state.ParkIssueLease(item, originalOwner, "park_1", now.Add(time.Minute)); parkErr != nil {
			return parkErr
		}
		item.ResourcePark.Kind = state.ResourceParkKindNeedsInput
		item.ResourcePark.RequestID = "req_1"
		item.Status = "answer_claim_waiting"
		snapshot.Issues["1"] = item
		snapshot.PendingRequests["req_1"] = &state.Request{
			ID: "req_1", IssueNumber: 1, Question: "Continue?", RunID: "run_1", ResourceParkID: "park_1",
			ReleasedOwner: &originalOwner, Status: "answered", Answer: "yes", CreatedAt: now, AnsweredAt: &now,
		}
		snapshot.Issues["2"] = &state.Issue{
			Number: 2, Status: "running", RunID: "run_2", LeaseGeneration: 1,
			Lease: &state.ResourceLease{Owner: competingOwner, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base-2", ReservedAt: now},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := loop.Store.Load()
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	waiting, _ := loop.Store.Load()
	if waiting.Issues["1"].Status != "answer_claim_waiting" || waiting.Issues["1"].Lease != nil || waiting.Issues["2"].Lease.Owner != competingOwner {
		t.Fatalf("conflicting wait changed a lease: %+v", waiting.Issues)
	}
	_, err = loop.Store.Update("competing_completed", 2, "run_2", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["2"]
		if releaseErr := state.ReleaseIssueLease(item, competingOwner); releaseErr != nil {
			return releaseErr
		}
		item.Status = "completed"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("worked=%v err=%v", worked, err)
	}
	resumed, _ := loop.Store.Load()
	item := resumed.Issues["1"]
	if item.Status != "resume_pending" || item.Lease == nil || item.Lease.Owner.Generation != originalOwner.Generation+1 || item.ResourcePark.Status != "resuming" || item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != item.Lease.Owner {
		t.Fatalf("resumed Issue=%+v", item)
	}
	if item.RunID != before.Issues["1"].RunID || item.Worktree != before.Issues["1"].Worktree || item.Branch != before.Issues["1"].Branch || item.SessionID != before.Issues["1"].SessionID || item.Attempts != 2 || item.Continuations != 1 || !reflect.DeepEqual(item.Answers, before.Issues["1"].Answers) || !reflect.DeepEqual(item.ResourcePark.OriginalLease, before.Issues["1"].ResourcePark.OriginalLease) {
		t.Fatalf("continuation provenance changed: before=%+v after=%+v", before.Issues["1"], item)
	}
	github.issue.State = "CLOSED"
	if worked, err := loop.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("closed Issue rejection worked=%v err=%v", worked, err)
	}
	rejected, _ := loop.Store.Load()
	if item := rejected.Issues["1"]; item.Status != "blocked" || item.Lease != nil || item.ResourcePark.Status != "resumed" || item.BlockedCause == nil || item.BlockedCause.Kind != "answer_resume" {
		t.Fatalf("closed answered continuation was not rejected: %+v", item)
	}
}

func TestAnsweredWorkspaceRecoverySyncThenResumesSameSessionAndWorktree(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	recorder := &recordingWorker{result: worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "continue",
		Retry: &worker.Retry{Reason: "continue"},
	}}
	loop.Worker = recorder
	runID := "run_answered_workspace"
	branch := "codex/issue-1-test"
	now := time.Now().UTC()
	originalOwner := state.LeaseOwner{RunID: runID, Generation: 1}
	resumeOwner := state.LeaseOwner{RunID: runID, Generation: 2}
	activeOwner := state.LeaseOwner{RunID: runID, Generation: 3}
	reason := fmt.Sprintf("worker workspace validation failed for %s: saved workspace provenance is missing", loop.Config.RepoPath)
	recoveryID := "answered_workspace_recovery_1"
	failureDigest := sha256.Sum256([]byte(reason))
	github.issue = gh.Issue{
		Number: 1, Title: "Test", State: "OPEN", Labels: []string{"blocked"},
		Comments: []string{
			"<!-- codex-issue-loop:request:req_1 -->",
			fmt.Sprintf("<!-- codex-issue-loop:failed:1 -->\n<!-- codex-issue-loop:failure:%x -->", failureDigest[:8]),
		},
	}
	_, err := loop.Store.Update("fixture", 1, runID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "resume_pending", RunID: runID, LeaseGeneration: 3,
			Lease: &state.ResourceLease{Owner: activeOwner, Slot: 0, DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base", ReservedAt: now},
			ResourcePark: &state.ResourceLeasePark{
				ID: "park_1", Kind: state.ResourceParkKindNeedsInput, RequestID: "req_1", Status: "resumed",
				OriginalLease: state.ResourceLease{Owner: originalOwner, Slot: 0, DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource}, BaseSHA: "base", ReservedAt: now.Add(-time.Hour)},
				ParkedAt:      now.Add(-30 * time.Minute), ResumedAt: now.Add(-20 * time.Minute), ResumeOwner: &resumeOwner,
			},
			Worktree: loop.Config.RepoPath, Branch: branch, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			SessionID: "session-answer", Session: &state.WorkerSession{Backend: "codex", ID: "session-answer"},
			ExecutionProfile: "extended", Attempts: 1, Continuations: 0, FailureKind: "issue", LastError: reason,
			GitHubSync:   "answered_workspace_recovery",
			Answers:      []state.AnswerRecord{{RequestID: "req_1", Question: "Continue?", Answer: "yes", AnsweredAt: now.Add(-20 * time.Minute)}},
			BlockedCause: &state.BlockedCause{Origin: "supervisor", Kind: "worker_workspace", Resumable: false, Reason: reason, BlockedAt: now.Add(-time.Minute)},
			AnsweredWorkspaceRecovery: &state.AnsweredWorkspaceRecovery{
				ID: recoveryID, Status: "requested", ConfirmedAt: now, OperatorConfirmed: true, OldProvenanceMissing: true,
				RequestID: "req_1", ResourceParkID: "park_1", OldOwner: resumeOwner, NewOwner: activeOwner,
			},
		}
		snapshot.PendingRequests["req_1"] = &state.Request{
			ID: "req_1", IssueNumber: 1, Question: "Continue?", RunID: runID, ResourceParkID: "park_1",
			ReleasedOwner: &originalOwner, Status: "answered", Answer: "yes", CreatedAt: now.Add(-30 * time.Minute), AnsweredAt: deadlinePointer(now.Add(-20 * time.Minute)),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := loop.issueState(1)
	if err := loop.processExisting(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	synced, _ := loop.issueState(1)
	if synced.GitHubSync != "" || synced.AnsweredWorkspaceRecovery.Status != "github_synced" || github.answeredWorkspaceRecoveries != 1 {
		t.Fatalf("sync did not converge: issue=%+v github=%+v", synced, github)
	}
	revision := func() uint64 { snapshot, _ := loop.Store.Load(); return snapshot.StateRevision }()
	if err := loop.syncGitHub(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if after := func() uint64 { snapshot, _ := loop.Store.Load(); return snapshot.StateRevision }(); after != revision {
		t.Fatalf("stale synchronization duplicated its durable event: before=%d after=%d", revision, after)
	}
	github.issue.Labels = []string{loop.Config.GitHub.RunningLabel}
	if err := loop.processExisting(context.Background(), synced); err != nil {
		t.Fatal(err)
	}
	if len(recorder.resumePrompts) != 1 || recorder.resumeConfigPaths[0] != loop.Config.RepoPath {
		t.Fatalf("same-workspace continuation was not resumed: recorder=%+v", recorder)
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
	loop, github := testLoop(t, result)
	github.issue.State = "OPEN"
	github.issue.Labels = []string{loop.Config.GitHub.RunningLabel}
	now := time.Now().UTC()
	_, err := loop.Store.Update("answer_recorded", 1, "run_1", nil, func(s *state.Snapshot) error {
		branch := "codex/issue-1-test"
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "resume_pending", RunID: "run_1",
			Worktree: loop.Config.RepoPath, Branch: branch, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			SessionID: "session-123", Attempts: 1, LeaseGeneration: 1, Lease: fixtureLease("run_1"),
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
	before, err := restarted.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileStartup(context.Background(), before); err != nil {
		t.Fatal(err)
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

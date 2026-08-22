package supervisor

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/conflict"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type fakeProcesses map[int]bool

func (f fakeProcesses) Alive(pid int) bool { return f[pid] }

func TestFaultWorkerAndGitHubStateReconciliationDecisions(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	base := state.Issue{
		Number: 7, Status: "running", RunID: "run_1", Branch: "codex/issue-7-test",
		Worktree: "/tmp/worktree", WorkerPID: 123, GitHubSync: "", UpdatedAt: time.Now().UTC(),
	}
	valid := worktree.Inspection{
		Exists: true, Valid: true, Branch: base.Branch,
		LocalBranchExists: true, RemoteBranchExists: true,
	}
	runningIssue := gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.RunningLabel}}

	tests := []struct {
		name       string
		current    state.Issue
		remote     gh.RemoteState
		inspection worktree.Inspection
		alive      bool
		status     issuedomain.Status
		prURL      string
		githubSync string
		prMerged   bool
		reason     string
	}{
		{
			name: "merged PR completes local state", current: base, inspection: worktree.Inspection{},
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED", MergedAt: timePointer()}}},
			status: "completed", prURL: "https://example.test/pull/11", githubSync: "done", prMerged: true, reason: "merged Pull Request",
		},
		{
			name: "closed PR blocks", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED"}}},
			status: "blocked", prURL: "https://example.test/pull/11", reason: "closed without merge",
		},
		{
			name: "open PR is discovered before retry", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", HeadRefName: base.Branch}}},
			status: "retry_wait", prURL: "https://example.test/pull/11", reason: "open Pull Request discovered",
		},
		{
			name: "deleted open PR branch blocks", current: base,
			inspection: worktree.Inspection{Exists: true, Valid: true, Branch: base.Branch, LocalBranchExists: true},
			remote:     gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, State: "OPEN", HeadRefName: base.Branch}}},
			status:     "blocked", reason: "head branch is missing",
		},
		{
			name: "manual exclusion label blocks", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{"blocked"}}},
			status: "blocked", reason: "exclusion label",
		},
		{
			name: "live saved PID blocks duplicate worker", current: base, inspection: valid, alive: true,
			remote: gh.RemoteState{Issue: runningIssue}, status: "blocked", reason: "still alive",
		},
		{
			name: "completed claim retries from durable work ahead", current: func() state.Issue {
				value := base
				value.Status = "claiming"
				value.Worktree = ""
				value.WorkerPID = 0
				return value
			}(),
			remote: gh.RemoteState{Issue: runningIssue}, status: "retry_wait", reason: "write-ahead claim",
		},
		{
			name: "partial done sync preserves pending comment write", current: func() state.Issue { value := base; value.GitHubSync = "done"; return value }(),
			remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}}},
			status: "completed", githubSync: "done", reason: "done label",
		},
		{
			name: "checks recovery before label sync survives restart", current: func() state.Issue {
				value := base
				value.Status = "pull_request_checks_recovery_pending"
				value.GitHubSync = "pull_request_checks_recovery"
				return value
			}(),
			remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.FailedLabel}}}, inspection: valid,
			status: "pull_request_checks_recovery_pending", githubSync: "pull_request_checks_recovery", reason: "waiting for GitHub label synchronization",
		},
		{
			name: "checks recovery after label sync survives restart", current: func() state.Issue {
				value := base
				value.Status = "pull_request_checks_recovery_pending"
				value.GitHubSync = "pull_request_checks_recovery"
				return value
			}(),
			remote: gh.RemoteState{Issue: runningIssue}, inspection: valid,
			status: "pull_request_checks_recovery_pending", githubSync: "pull_request_checks_recovery", reason: "remains pending",
		},
		{
			name: "legacy completed draft returns to check monitoring", current: func() state.Issue {
				value := base
				value.Status = "completed"
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue:        gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}},
				PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", IsDraft: true, HeadRefName: base.Branch, ChecksStatus: "success"}},
			},
			inspection: valid, status: "awaiting_checks", prURL: "https://example.test/pull/11", reason: "legacy completed",
		},
		{
			name: "done label cannot release unmerged PR lease", current: func() state.Issue {
				value := base
				value.Status = "awaiting_merge"
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue:        gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}},
				PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", HeadRefName: base.Branch}},
			},
			inspection: valid, status: "awaiting_merge", prURL: "https://example.test/pull/11", reason: "state already converged",
		},
		{
			name: "multiple Pull Requests block before merge authority", current: func() state.Issue {
				value := base
				value.Status = "awaiting_merge"
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue: runningIssue,
				PullRequests: []gh.PullRequest{
					{Number: 12, URL: "https://example.test/pull/12", State: "OPEN", HeadRefName: base.Branch},
					{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED", MergedAt: timePointer(), HeadRefName: base.Branch},
				},
			},
			inspection: valid, status: "blocked", prURL: "https://example.test/pull/11", reason: "multiple Pull Requests",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop := &Loop{Config: cfg, Processes: fakeProcesses{123: test.alive}}
			decision := loop.decideReconciliation(state.Snapshot{}, test.current, test.remote, test.inspection)
			if decision.status != test.status || decision.pullRequest != test.prURL || decision.githubSync != test.githubSync || decision.prMerged != test.prMerged || !strings.Contains(decision.reason, test.reason) {
				t.Fatalf("decision=%+v", decision)
			}
		})
	}
}

func TestTerminalPullRequestReconciliationRequiresAuthoritativeSavedMerge(t *testing.T) {
	cfg := config.Defaults()
	loop := &Loop{Config: cfg}
	merged := timePointer()
	base := state.Issue{
		Number: 7, Status: "blocked", RunID: "run_7", Branch: "codex/issue-7-test",
		PullRequestURL: "https://example.test/pull/7", FailureKind: "issue",
	}
	automationIssue := gh.Issue{
		Number: 7, State: "OPEN", Labels: []string{"blocked"},
		Comments: []string{"<!-- codex-issue-loop:failed:7 -->\nAutomation stopped."},
	}
	matching := gh.PullRequest{
		Number: 7, URL: base.PullRequestURL, State: "MERGED", MergedAt: merged,
		HeadRefName: base.Branch,
	}
	tests := []struct {
		name       string
		current    state.Issue
		remote     gh.RemoteState
		completed  bool
		reasonPart string
	}{
		{name: "automation blocked merged PR completes", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{matching}}, completed: true, reasonPart: "merge discovered"},
		{name: "unmerged PR remains sticky", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{func() gh.PullRequest { value := matching; value.MergedAt = nil; value.State = "OPEN"; return value }()}}, reasonPart: "not merged"},
		{name: "closed without merge remains sticky", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{func() gh.PullRequest { value := matching; value.MergedAt = nil; value.State = "CLOSED"; return value }()}}, reasonPart: "closed without merge"},
		{name: "multiple PRs remain sticky", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{matching, matching}}, reasonPart: "multiple Pull Requests"},
		{name: "different saved URL remains sticky", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{func() gh.PullRequest { value := matching; value.URL += "-other"; return value }()}}, reasonPart: "does not match"},
		{name: "different head remains sticky", current: base, remote: gh.RemoteState{Issue: automationIssue, PullRequests: []gh.PullRequest{func() gh.PullRequest { value := matching; value.HeadRefName += "-other"; return value }()}}, reasonPart: "head does not match"},
		{name: "manual blocked label remains sticky", current: func() state.Issue { value := base; value.FailureKind = ""; return value }(), remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{"blocked"}}, PullRequests: []gh.PullRequest{matching}}, reasonPart: "applied manually"},
		{name: "manual exclusion remains sticky", current: base, remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{"blocked", "do-not-automate"}, Comments: automationIssue.Comments}, PullRequests: []gh.PullRequest{matching}}, reasonPart: "applied manually"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, ok := loop.decideTerminalPullRequestReconciliation(test.current, test.remote)
			if !ok || (decision.status == "completed") != test.completed || !strings.Contains(decision.reason, test.reasonPart) {
				t.Fatalf("decision=%+v ok=%v", decision, ok)
			}
		})
	}
}

func TestPeriodicTerminalReconciliationCompletesAndIsIdempotent(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	runID := "run_blocked"
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Blocked", RunID: runID, Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("blocked_fixture", 1, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Branch = "codex/issue-1-test"
		item.PullRequestURL = "https://example.test/pull/1"
		item.FailureKind = "issue"
		item.LastError = "merge conflict"
		item.Lease.Owner = owner
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}, Comments: []string{"<!-- codex-issue-loop:failed:1 -->"}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pull/1", State: "MERGED", MergedAt: timePointer(), HeadRefName: "codex/issue-1-test"}},
	}
	current, err := loop.issueState(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileTerminalPullRequest(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if item.Status != "completed" || !item.PullRequestMerged || item.Lease != nil || item.GitHubSync != "" || !github.done {
		t.Fatalf("issue=%+v github.done=%v", item, github.done)
	}
	if err := loop.reconcileTerminalPullRequest(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if github.inspectCalls != 1 || github.doneCalls != 1 {
		t.Fatalf("inspect calls=%d done calls=%d", github.inspectCalls, github.doneCalls)
	}
}

func TestStartupReconciliationSkipsMergeConfirmedHistory(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("completed", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Status: "completed", PullRequestURL: "https://example.test/pull/1", PullRequestMerged: true,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if github.inspectCalls != 0 {
		t.Fatalf("inspect calls=%d", github.inspectCalls)
	}
}

func TestStartupReconciliationStopsAllOrphanGroupsBeforeInspectingIssues(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.issue = gh.Issue{State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	_, err := loop.Store.Update("workers_running", 0, "", nil, func(snapshot *state.Snapshot) error {
		for _, number := range []int{1, 2} {
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, RunID: fmt.Sprintf("run_%d", number), Status: "running",
				WorkerPID: 100 + number, WorkerPGID: 100 + number,
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := &fakeProcessGroups{
		alive: map[int]bool{101: true, 102: true}, owned: map[int]bool{101: true, 102: true},
		signals: map[int][]syscall.Signal{},
	}
	loop.Processes = groups
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if github.inspectCalls != 2 || len(groups.signals) != 2 {
		t.Fatalf("inspect calls=%d signals=%v", github.inspectCalls, groups.signals)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"1", "2"} {
		if issue := loaded.Issues[key]; issue.Status != "retry_wait" || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
			t.Fatalf("Issue %s=%+v", key, issue)
		}
	}
}

func TestFaultStartupReconciliationPersistsDiscoveredPullRequest(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Git: &worker.GitResult{}}
	loop, github := testLoop(t, result)
	now := time.Now().UTC()
	_, err := loop.Store.Update("worker_started", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "running", RunID: "run_1", Branch: "codex/issue-1-test",
			Worktree: loop.Config.RepoPath, WorkerPID: 987, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 2, URL: "https://example.test/pull/2", State: "OPEN", HeadRefName: "codex/issue-1-test"}},
	}
	inspection := worktree.Inspection{Exists: true, Valid: true, Branch: "codex/issue-1-test", LocalBranchExists: true, RemoteBranchExists: true}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &inspection}
	loop.Processes = fakeProcesses{987: false}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Issues["1"]
	if item.Status != "retry_wait" || item.PullRequestURL != "https://example.test/pull/2" || item.WorkerPID != 0 || item.RetryAfter == nil {
		t.Fatalf("item=%+v", item)
	}
	events, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(events), "startup_reconciled") ||
		!strings.Contains(string(events), `"predicate_report":{`) ||
		!strings.Contains(string(events), `"operation":"startup-reconciliation"`) ||
		!strings.Contains(string(events), `"schema_version":1`) {
		t.Fatalf("events=%s err=%v", events, err)
	}
}

func TestFaultStartupReconciliationConvergesOnDirtyPullRequestWithoutDuplicateConflictAttempt(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Completion.AutoMerge = true
	prURL := "https://example.test/pull/1"
	_, err := loop.Store.Update("awaiting_checks", 1, "run_1", nil, func(s *state.Snapshot) error {
		branch := "codex/issue-1-test"
		s.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "awaiting_checks", RunID: "run_1",
			Branch: branch, Worktree: loop.Config.RepoPath, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			LeaseGeneration: 1, Lease: fixtureLease("run_1"), PullRequestURL: prURL,
			UpdatedAt: time.Now().UTC(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.issue = gh.Issue{Number: 1, Title: "Test", State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	github.remote = &gh.RemoteState{Issue: github.issue, PullRequests: []gh.PullRequest{{
		Number: 1, URL: prURL, State: "OPEN", HeadRefName: "codex/issue-1-test",
		MergeStateStatus: "DIRTY", ChecksStatus: "pending",
	}}}
	resolver := &fakeConflictResolver{preparation: conflict.Preparation{
		PreviousBaseSHA: "base-old", TargetBaseSHA: "base-new", OriginalHeadSHA: "head-pr",
		ConflictFiles: []string{"shared.txt"}, AllowedPaths: []string{"shared.txt"},
	}}
	loop.Conflicts = resolver
	loop.Worker = fakeWorker{result: worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "extended", Summary: "retry later",
	}}

	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if item := first.Issues["1"]; item.Status != "resolving_conflict" || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 {
		t.Fatalf("first convergence=%+v", item)
	}
	if resolver.prepareCalls != 2 {
		t.Fatalf("prepare calls=%d, want 2", resolver.prepareCalls)
	}

	if err := loop.reconcileStartup(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if item := second.Issues["1"]; item.Status != "resolving_conflict" || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 || len(item.ConflictRecovery.History) != 1 {
		t.Fatalf("restart convergence=%+v", item)
	}
	if resolver.prepareCalls != 2 {
		t.Fatalf("restart created duplicate conflict work: prepare calls=%d", resolver.prepareCalls)
	}
}

func TestFaultStartupReconciliationDoesNotOverwriteConcurrentEnvironmentResume(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_1", Slot: 0,
		DeclaredResources: []string{"scheduler"}, ResolvedResources: []string{"scheduler"}, BaseSHA: "base-sha", ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-test"
	_, err = loop.Store.Update("issue_blocked", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.GitHubSync = "blocked"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}}}
	inspection := worktree.Inspection{Exists: true, Valid: true, Branch: branch, LocalBranchExists: true, RemoteBranchExists: true}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &inspection}
	github.inspectHook = func() {
		github.inspectHook = nil
		_, updateErr := loop.Store.Update("environment_resume_requested", 1, owner.RunID, map[string]string{"resume_id": "resume_race", "base_sha": "base-sha"}, func(snapshot *state.Snapshot) error {
			item := snapshot.Issues["1"]
			item.Status = "environment_resume_pending"
			item.GitHubSync = "environment_resume"
			item.EnvironmentResume = &state.EnvironmentResume{ID: "resume_race", Status: "requested", ConfirmedAt: time.Now().UTC(), BaseSHA: "base-sha"}
			return nil
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	}

	if err := loop.reconcileStartup(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Issues["1"]
	if item.Status != "environment_resume_pending" || item.GitHubSync != "environment_resume" || item.EnvironmentResume == nil || item.EnvironmentResume.ID != "resume_race" || item.Lease == nil || item.Lease.Owner != owner {
		t.Fatalf("concurrent environment resume was overwritten: %+v", item)
	}
	if github.inspectCalls != 2 {
		t.Fatalf("inspect calls=%d, want stale inspection plus retry", github.inspectCalls)
	}
}

func TestStartupReconciliationParksExistingTypedEnvironmentBlock(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Processes = fakeProcesses{}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Existing typed block", RunID: "run_typed", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: "base-sha", ReservedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-typed-block"
	_, err = loop.Store.Update("issue_blocked", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.SessionID = "session-typed"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: now.Add(-time.Minute)}
		item.LastError = "worker blocked: network unavailable"
		item.FailureKind = "issue"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}}}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &worktree.Inspection{
		Exists: true, Valid: true, Branch: branch, LocalBranchExists: true, RemoteBranchExists: true,
	}}
	if err := loop.reconcileStartup(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := after.Issues["1"]
	if item.Lease != nil || item.ResourcePark == nil || item.ResourcePark.Status != "parked" || item.ResourcePark.OriginalLease.Owner != owner {
		t.Fatalf("existing typed block was not parked: %+v", item)
	}
	if item.RunID != "run_typed" || item.Worktree != loop.Config.RepoPath || item.Branch != branch || item.SessionID != "session-typed" || item.BlockedCause == nil || item.BlockedCause.Reason != "network unavailable" {
		t.Fatalf("startup park changed continuation state: %+v", item)
	}
	if github.inspectCalls != 1 {
		t.Fatalf("typed block inspections=%d want=1", github.inspectCalls)
	}
}

func TestStartupReconciliationParksExistingNeedsInputLease(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Processes = fakeProcesses{}
	now := time.Date(2026, 8, 18, 7, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Existing input", RunID: "run_input", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: "base-input", ReservedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-input"
	_, err = loop.Store.Update("input_requested", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "needs_input"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.Workspace = fixtureWorkspace(loop, loop.Config.RepoPath, branch)
		item.SessionID = "session-input"
		snapshot.PendingRequests["req_input"] = &state.Request{ID: "req_input", IssueNumber: 1, Question: "Continue?", Status: "pending", CreatedAt: now.Add(-time.Minute)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := loop.Store.Load()
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.NeedsInputLabel}}}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &worktree.Inspection{
		Exists: true, Valid: true, Branch: branch, LocalBranchExists: true, RemoteBranchExists: true,
	}}
	if err := loop.reconcileStartup(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	after, _ := loop.Store.Load()
	item := after.Issues["1"]
	request := after.PendingRequests["req_input"]
	if item.Status != "needs_input" || item.Lease != nil || item.ResourcePark == nil || item.ResourcePark.Kind != state.ResourceParkKindNeedsInput || item.ResourcePark.RequestID != request.ID || item.ResourcePark.OriginalLease.Owner != owner {
		t.Fatalf("needs-input lease was not parked: item=%+v request=%+v", item, request)
	}
	if request.RunID != item.RunID || request.ResourceParkID != item.ResourcePark.ID || request.ReleasedOwner == nil || *request.ReleasedOwner != owner || item.SessionID != "session-input" || item.Worktree != loop.Config.RepoPath || item.Branch != branch {
		t.Fatalf("startup park changed request/continuation provenance: item=%+v request=%+v", item, request)
	}
}

func TestStartupReconciliationNormalizesSynchronizedLegacyWorkerBlock(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	blockedAt := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: blockedAt.Add(time.Hour)}
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Legacy block", RunID: "run_legacy", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: "base-sha", ReservedAt: blockedAt.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-legacy-block"
	_, err = loop.Store.Update("worker_started", 1, owner.RunID, map[string]string{"worktree": loop.Config.RepoPath, "branch": branch}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "running"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyError := "worker blocked: localhost listen denied"
	_, err = loop.Store.Update("issue_blocked", 1, owner.RunID, map[string]string{"error": legacyError, "failure_kind": "issue"}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.FailureKind = "issue"
		item.LastError = legacyError
		item.GitHubSync = "blocked"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("github_state_synced", 1, owner.RunID, map[string]string{"state": "blocked"}, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].GitHubSync = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("startup_reconciled", 1, owner.RunID, map[string]string{
		"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually",
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.LastError = "startup reconciliation blocked: GitHub exclusion label was applied manually"
		return state.ReleaseIssueLease(item, owner)
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	expectedCause, err := loop.Store.LegacyWorkerBlockProvenance(*before.Issues["1"])
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}}}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &worktree.Inspection{
		Exists: true, Valid: true, Branch: branch, LocalBranchExists: true, RemoteBranchExists: true,
	}}

	if err := loop.reconcileStartup(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := after.Issues["1"]
	if item.Status != "blocked" || item.LastError != legacyError || item.BlockedCause == nil || item.BlockedCause.Origin != "worker" || item.BlockedCause.Kind != "environment" || !item.BlockedCause.Resumable {
		t.Fatalf("legacy provenance was not normalized: %+v", item)
	}
	if item.BlockedCause.Reason != "localhost listen denied" || !item.BlockedCause.BlockedAt.Equal(expectedCause.BlockedAt) {
		t.Fatalf("legacy reason/time were not preserved: %+v", item.BlockedCause)
	}
	if item.Lease != nil {
		t.Fatalf("legacy lease released by the old reconciliation unexpectedly returned: %+v", item.Lease)
	}
	recovery, err := loop.Store.LegacyWorkerBlockRecoveryEvidence(*item)
	if err != nil || recovery.BaseSHA != "base-sha" || !reflect.DeepEqual(&recovery.Cause, item.BlockedCause) {
		t.Fatalf("typed legacy recovery evidence=%+v err=%v", recovery, err)
	}
}

func TestStartupReconciliationRejectsLegacyChainWithManualExclusion(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	legacyError := "issue: worker blocked: localhost listen denied"
	_, err := loop.Store.Update("issue_blocked", 1, "run_legacy", map[string]string{"error": legacyError, "failure_kind": "issue"}, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Status: "blocked", RunID: "run_legacy", FailureKind: "issue", LastError: legacyError,
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("github_state_synced", 1, "run_legacy", map[string]string{"state": "blocked"}, func(*state.Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	before, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked", "do-not-automate"}}}

	if err := loop.reconcileStartup(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	after, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := after.Issues["1"]
	if item.BlockedCause != nil || item.Status != "blocked" || !strings.Contains(item.LastError, "do not contain only the supervisor-owned blocked label") {
		t.Fatalf("manual exclusion was normalized as resumable provenance: %+v", item)
	}
}

func TestFaultWebhookReconciliationDoesNotOverwriteConcurrentEnvironmentResume(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_1", Slot: 0,
		DeclaredResources: []string{"scheduler"}, ResolvedResources: []string{"scheduler"}, BaseSHA: "base-sha", ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-test"
	_, err = loop.Store.Update("issue_blocked", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Status = "blocked"
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "network unavailable", BlockedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}}}
	inspection := worktree.Inspection{Exists: true, Valid: true, Branch: branch, LocalBranchExists: true, RemoteBranchExists: true}
	loop.Worktrees = fakeWorktree{path: loop.Config.RepoPath, inspection: &inspection}
	github.inspectHook = func() {
		github.inspectHook = nil
		_, updateErr := loop.Store.Update("environment_resume_requested", 1, owner.RunID, map[string]string{"resume_id": "resume_webhook", "base_sha": "base-sha"}, func(snapshot *state.Snapshot) error {
			item := snapshot.Issues["1"]
			item.Status = "environment_resume_pending"
			item.GitHubSync = "environment_resume"
			item.EnvironmentResume = &state.EnvironmentResume{ID: "resume_webhook", Status: "requested", ConfirmedAt: time.Now().UTC(), BaseSHA: "base-sha"}
			return nil
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	}
	handled, err := loop.reconcileTerminalWebhook(context.Background(), *stale.Issues["1"], webhook.Delivery{DeliveryID: "delivery-race", Event: "issues", Action: "labeled"})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("stale webhook reconciliation was acknowledged")
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := loaded.Issues["1"]
	if item.Status != "environment_resume_pending" || item.GitHubSync != "environment_resume" || item.EnvironmentResume == nil || item.EnvironmentResume.ID != "resume_webhook" || item.Lease == nil || item.Lease.Owner != owner {
		t.Fatalf("webhook reconciliation overwrote concurrent environment resume: %+v", item)
	}
}

func TestStartupReconciliationRetainsPRLeaseUntilMergeThenReleasesIt(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run_1", Slot: 0,
		DeclaredResources: []string{"git"}, ResolvedResources: []string{"git"}, BaseSHA: "base-sha", ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://example.test/pull/1"
	_, err = loop.Store.Update("awaiting_merge", 1, owner.RunID, nil, func(s *state.Snapshot) error {
		item := s.Issues["1"]
		item.Status = "awaiting_merge"
		item.Branch = "codex/issue-1-test"
		item.Worktree = loop.Config.RepoPath
		item.PullRequestURL = prURL
		item.ActualResources = []string{"git"}
		item.Lease.ActualResources = []string{"git"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: prURL, State: "OPEN", HeadRefName: "codex/issue-1-test"}},
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	retained, err := loop.Store.Load()
	if err != nil || retained.Issues["1"].Lease == nil || retained.Issues["1"].Status != "awaiting_merge" {
		t.Fatalf("open PR lease was not retained: issue=%+v err=%v", retained.Issues["1"], err)
	}
	now := time.Now().UTC()
	github.remote.PullRequests[0].State = "MERGED"
	github.remote.PullRequests[0].MergedAt = &now
	if err := loop.reconcileStartup(context.Background(), retained); err != nil {
		t.Fatal(err)
	}
	completed, err := loop.Store.Load()
	if err != nil || completed.Issues["1"].Lease != nil || completed.Issues["1"].Status != "completed" || !completed.Issues["1"].PullRequestMerged || !reflect.DeepEqual(completed.Issues["1"].ActualResources, []string{"git"}) {
		t.Fatalf("merged PR did not release lease atomically: issue=%+v err=%v", completed.Issues["1"], err)
	}
}

func timePointer() *time.Time {
	now := time.Now().UTC()
	return &now
}

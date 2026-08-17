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
		status     string
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
	if err != nil || !strings.Contains(string(events), "startup_reconciled") {
		t.Fatalf("events=%s err=%v", events, err)
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

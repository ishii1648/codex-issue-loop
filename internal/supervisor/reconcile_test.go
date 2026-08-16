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
		Issue: gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}, Comments: []string{"<!-- codex-issue-loop:failed:1 -->"}},
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
	if err != nil || !strings.Contains(string(events), "startup_reconciled") {
		t.Fatalf("events=%s err=%v", events, err)
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

package supervisor

import (
	"context"
	"os"
	"strings"
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

func timePointer() *time.Time {
	now := time.Now().UTC()
	return &now
}

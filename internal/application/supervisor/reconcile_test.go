package supervisor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/conflict"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type fakeProcesses map[int]bool

func (f fakeProcesses) Alive(pid int) bool { return f[pid] }

func TestFaultWorkerAndGitHubStateReconciliationDecisions(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	base := state.Issue{
		Number: 7, Status: issuedomain.StatusRunning, RunID: "run_1", Branch: "codex/issue-7-test",
		Worktree: "/tmp/worktree", WorkerPID: 123, UpdatedAt: time.Now().UTC(),
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
		effect     issuedomain.EffectKind
		prMerged   bool
		reason     string
	}{
		{
			name: "merged PR completes local state", current: base, inspection: worktree.Inspection{},
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED", MergedAt: timePointer(), HeadSHA: "head-11"}}},
			status: issuedomain.StatusCompleted, prURL: "https://example.test/pull/11", effect: issuedomain.EffectMarkDone, prMerged: true, reason: "merged Pull Request",
		},
		{
			name: "closed PR blocks", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED"}}},
			status: issuedomain.StatusBlocked, prURL: "https://example.test/pull/11", reason: "closed without merge",
		},
		{
			name: "open PR is discovered before retry", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", HeadRefName: base.Branch}}},
			status: issuedomain.StatusRetryWait, prURL: "https://example.test/pull/11", reason: "open Pull Request discovered",
		},
		{
			name: "deleted open PR branch blocks", current: base,
			inspection: worktree.Inspection{Exists: true, Valid: true, Branch: base.Branch, LocalBranchExists: true},
			remote:     gh.RemoteState{Issue: runningIssue, PullRequests: []gh.PullRequest{{Number: 11, State: "OPEN", HeadRefName: base.Branch}}},
			status:     issuedomain.StatusBlocked, reason: "head branch is missing",
		},
		{
			name: "manual exclusion label blocks", current: base, inspection: valid,
			remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{"blocked"}}},
			status: issuedomain.StatusBlocked, reason: "exclusion label",
		},
		{
			name: "live saved PID blocks duplicate worker", current: base, inspection: valid, alive: true,
			remote: gh.RemoteState{Issue: runningIssue}, status: issuedomain.StatusBlocked, reason: "still alive",
		},
		{
			name: "completed claim retries from durable work ahead", current: func() state.Issue {
				value := base
				value.Status = issuedomain.StatusClaiming
				value.Worktree = ""
				value.WorkerPID = 0
				return value
			}(),
			remote: gh.RemoteState{Issue: runningIssue}, status: issuedomain.StatusRetryWait, reason: "write-ahead claim",
		},
		{
			name: "done label preserves pending completion effect", current: base,
			remote: gh.RemoteState{Issue: gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}}},
			status: issuedomain.StatusCompleted, reason: "done label",
		},
		{
			name: "legacy completed draft returns to check monitoring", current: func() state.Issue {
				value := base
				value.Status = issuedomain.StatusCompleted
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue:        gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}},
				PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", IsDraft: true, HeadRefName: base.Branch, ChecksStatus: "success"}},
			},
			inspection: valid, status: issuedomain.StatusAwaitingChecks, prURL: "https://example.test/pull/11", reason: "legacy completed",
		},
		{
			name: "done label cannot release unmerged PR lease", current: func() state.Issue {
				value := base
				value.Status = issuedomain.StatusAwaitingMerge
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue:        gh.Issue{Number: 7, State: "OPEN", Labels: []string{cfg.GitHub.DoneLabel}},
				PullRequests: []gh.PullRequest{{Number: 11, URL: "https://example.test/pull/11", State: "OPEN", HeadRefName: base.Branch}},
			},
			inspection: valid, status: issuedomain.StatusAwaitingMerge, prURL: "https://example.test/pull/11", reason: "state already converged",
		},
		{
			name: "multiple Pull Requests block before merge authority", current: func() state.Issue {
				value := base
				value.Status = issuedomain.StatusAwaitingMerge
				value.PullRequestURL = "https://example.test/pull/11"
				return value
			}(),
			remote: gh.RemoteState{
				Issue: runningIssue,
				PullRequests: []gh.PullRequest{
					{Number: 12, URL: "https://example.test/pull/12", State: "OPEN", HeadRefName: base.Branch},
					{Number: 11, URL: "https://example.test/pull/11", State: "CLOSED", MergedAt: timePointer(), HeadRefName: base.Branch, HeadSHA: "head-11"},
				},
			},
			inspection: valid, status: issuedomain.StatusBlocked, prURL: "https://example.test/pull/11", reason: "multiple Pull Requests",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop := &Loop{Config: cfg, Processes: fakeProcesses{123: test.alive}}
			decision := loop.decideReconciliation(state.Snapshot{}, test.current, test.remote, test.inspection)
			if decision.status != test.status || decision.pullRequest != test.prURL || decision.effect != test.effect || decision.prMerged != test.prMerged || !strings.Contains(decision.reason, test.reason) {
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
		Number: 7, Status: issuedomain.StatusBlocked, RunID: "run_7", Branch: "codex/issue-7-test",
		PullRequestURL: "https://example.test/pull/7", FailureKind: "issue",
	}
	automationIssue := gh.Issue{
		Number: 7, State: "OPEN", Labels: []string{"blocked"},
		Comments: []string{"<!-- codex-issue-loop:failed:7 -->\nAutomation stopped."},
	}
	matching := gh.PullRequest{
		Number: 7, URL: base.PullRequestURL, State: "MERGED", MergedAt: merged, HeadSHA: "head-7",
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
			if !ok || (decision.status == issuedomain.StatusCompleted) != test.completed || !strings.Contains(decision.reason, test.reasonPart) {
				t.Fatalf("decision=%+v ok=%v", decision, ok)
			}
		})
	}
}

func TestPeriodicTerminalReconciliationCompletesAndIsIdempotent(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)}
	runID := "run_blocked"
	_, identity, err := loop.Store.StartExecution(state.ExecutionStart{
		IssueNumber: 1, Title: "Blocked", RunID: runID, StartedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("blocked_fixture", 1, runID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Branch = "codex/issue-1-test"
		item.PullRequestURL = "https://example.test/pull/1"
		item.FailureKind = "issue"
		item.LastError = "merge conflict"
		setSupervisorTestWorkspace(snapshot, item)
		if err := state.CaptureContinuation(snapshot, item.Number, identity, state.NewID("checkpoint"), loop.now()); err != nil {
			return err
		}
		item.Status = issuedomain.StatusBlocked
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.remote = &gh.RemoteState{
		Issue:        gh.Issue{Number: 1, State: "OPEN", Labels: []string{"blocked"}, Comments: []string{"<!-- codex-issue-loop:failed:1 -->"}},
		PullRequests: []gh.PullRequest{{Number: 1, URL: "https://example.test/pull/1", State: "MERGED", MergedAt: timePointer(), HeadRefName: "codex/issue-1-test", HeadSHA: "head-1"}},
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
	if item.Status != issuedomain.StatusCompleted || !item.PullRequestMerged || snapshot.ActiveExecution != nil || state.PendingEffect(&snapshot, 1) != nil || !github.done {
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
			Number: 1, Status: issuedomain.StatusCompleted, PullRequestURL: "https://example.test/pull/1", PullRequestNumber: 1, HeadSHA: "head-1", PullRequestMerged: true,
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

func TestStartupReconciliationStopsActiveOrphanBeforeInspectingIssue(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.issue = gh.Issue{State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	if _, _, err := loop.Store.StartExecution(state.ExecutionStart{IssueNumber: 1, RunID: "run_1", StartedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, err := loop.Store.Update("workers_running", 0, "", nil, func(snapshot *state.Snapshot) error {
		for _, number := range []int{1} {
			runID := fmt.Sprintf("run_%d", number)
			item := snapshot.Issues[strconv.Itoa(number)]
			item.RunID, item.Status = runID, issuedomain.StatusRunning
			item.Branch = "codex/issue-1-test"
			item.WorkerPID, item.WorkerPGID = 100+number, 100+number
			setSupervisorTestWorkspace(snapshot, item)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := &fakeProcessGroups{
		alive: map[int]bool{101: true}, owned: map[int]bool{101: true},
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
	if github.inspectCalls != 1 || len(groups.signals) != 1 {
		t.Fatalf("inspect calls=%d signals=%v", github.inspectCalls, groups.signals)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"1"} {
		if issue := loaded.Issues[key]; issue.Status != issuedomain.StatusRetryWait || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
			t.Fatalf("Issue %s=%+v", key, issue)
		}
	}
}

func TestFaultStartupReconciliationPersistsDiscoveredPullRequest(t *testing.T) {
	result := worker.Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Git: &worker.GitResult{}}
	loop, github := testLoop(t, result)
	now := time.Now().UTC()
	if _, _, err := loop.Store.StartExecution(state.ExecutionStart{IssueNumber: 1, Title: "Test", RunID: "run_1", StartedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, err := loop.Store.Update("worker_started", 1, "run_1", nil, func(s *state.Snapshot) error {
		item := s.Issues["1"]
		item.Status, item.Branch = issuedomain.StatusRunning, "codex/issue-1-test"
		item.Worktree, item.WorkerPID, item.WorkerPGID, item.UpdatedAt = loop.Config.RepoPath, 987, 987, now
		setSupervisorTestWorkspace(s, item)
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
	if item.Status != issuedomain.StatusRetryWait || item.PullRequestURL != "https://example.test/pull/2" || item.WorkerPID != 0 || item.RetryAfter == nil {
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
			Number: 1, Title: "Test", Status: issuedomain.StatusAwaitingChecks, RunID: "run_1", Generation: 1,
			Branch: branch, Worktree: loop.Config.RepoPath, Workspace: fixtureWorkspace(loop, loop.Config.RepoPath, branch),
			PullRequestURL: prURL, UpdatedAt: time.Now().UTC(),
			Continuation: &state.ContinuationCheckpoint{ID: "checkpoint_checks", CreatedAt: time.Now().UTC(), RunID: "run_1", Generation: 1, Stage: issuedomain.ContinuationStageChecks},
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
	if item := first.Issues["1"]; item.Status != issuedomain.StatusResolvingConflict || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 {
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
	if item := second.Issues["1"]; item.Status != issuedomain.StatusResolvingConflict || item.ConflictRecovery == nil || item.ConflictRecovery.Attempts != 1 || len(item.ConflictRecovery.History) != 1 {
		t.Fatalf("restart convergence=%+v", item)
	}
	if resolver.prepareCalls != 2 {
		t.Fatalf("restart created duplicate conflict work: prepare calls=%d", resolver.prepareCalls)
	}
}

func TestTerminalTransitionRetainsGenericCheckpointWithoutRecoveryInspection(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Processes = fakeProcesses{}
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	_, identity, err := loop.Store.StartExecution(state.ExecutionStart{
		IssueNumber: 1, Title: "Existing typed block", RunID: "run_typed",
		BaseSHA: "base-sha", StartedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-typed-block"
	_, err = loop.Store.Update("issue_blocked", 1, identity.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.Workspace = fixtureWorkspace(loop, loop.Config.RepoPath, branch)
		item.SessionID = "session-typed"
		item.LastError = "worker blocked: network unavailable"
		item.FailureKind = "issue"
		if err := state.CaptureContinuation(snapshot, item.Number, identity, state.NewID("checkpoint"), now); err != nil {
			return err
		}
		item.Status = issuedomain.StatusBlocked
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
	if after.ActiveExecution != nil || item.Continuation == nil || item.Continuation.Generation != identity.Generation {
		t.Fatalf("existing typed block lost its continuation: %+v", item)
	}
	if item.RunID != "run_typed" || item.Worktree != loop.Config.RepoPath || item.Branch != branch || item.SessionID != "session-typed" || item.Suspension == nil || item.Suspension.Reason != "worker blocked: network unavailable" {
		t.Fatalf("startup park changed continuation state: %+v", item)
	}
	if github.inspectCalls != 0 {
		t.Fatalf("terminal transition unexpectedly performed recovery inspection: %d", github.inspectCalls)
	}
}

func TestStartupReconciliationRetainsExistingNeedsInputCheckpoint(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Processes = fakeProcesses{}
	now := time.Date(2026, 8, 18, 7, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	_, identity, err := loop.Store.StartExecution(state.ExecutionStart{
		IssueNumber: 1, Title: "Existing input", RunID: "run_input",
		BaseSHA: "base-input", StartedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	branch := "codex/issue-1-input"
	_, err = loop.Store.Update("input_requested", 1, identity.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		item.Branch = branch
		item.Worktree = loop.Config.RepoPath
		item.Workspace = fixtureWorkspace(loop, loop.Config.RepoPath, branch)
		item.SessionID = "session-input"
		if err := state.CaptureContinuation(snapshot, item.Number, identity, state.NewID("checkpoint"), now); err != nil {
			return err
		}
		item.Status = issuedomain.StatusNeedsInput
		item.Continuation.Kind = state.ContinuationKindNeedsInput
		item.Continuation.RequestID = "req_input"
		snapshot.PendingRequests["req_input"] = &state.Request{
			ID: "req_input", IssueNumber: 1, Question: "Continue?", RunID: item.RunID,
			CheckpointID: item.Continuation.ID, ReleasedExecution: &identity,
			Status: issuedomain.RequestStatusPending, CreatedAt: now.Add(-time.Minute),
		}
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
	if item.Status != issuedomain.StatusNeedsInput || after.ActiveExecution != nil || item.Continuation == nil || item.Continuation.Kind != state.ContinuationKindNeedsInput || item.Continuation.RequestID != request.ID || item.Continuation.Generation != identity.Generation {
		t.Fatalf("needs-input continuation was not retained: item=%+v request=%+v", item, request)
	}
	if request.RunID != item.RunID || request.ResumeStatus != issuedomain.StatusUnset || request.CheckpointID != item.Continuation.ID ||
		request.ReleasedExecution == nil || *request.ReleasedExecution != identity || item.SessionID != "session-input" ||
		item.Worktree != loop.Config.RepoPath || item.Branch != branch {
		t.Fatalf("startup park changed request/continuation provenance: item=%+v request=%+v", item, request)
	}
}

func TestStartupReconciliationDoesNotHoldExecutionWhilePRWaits(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, identity, err := loop.Store.StartExecution(state.ExecutionStart{
		IssueNumber: 1, Title: "Test", RunID: "run_1", BaseSHA: "base-sha", StartedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	prURL := "https://example.test/pull/1"
	_, err = loop.Store.Update("awaiting_merge", 1, identity.RunID, nil, func(s *state.Snapshot) error {
		item := s.Issues["1"]
		item.Status = issuedomain.StatusAwaitingMerge
		item.Branch = "codex/issue-1-test"
		item.Worktree = loop.Config.RepoPath
		item.PullRequestURL = prURL
		setSupervisorTestWorkspace(s, item)
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
	if err != nil || retained.ActiveExecution != nil || retained.Issues["1"].Continuation == nil || retained.Issues["1"].Status != issuedomain.StatusAwaitingMerge {
		t.Fatalf("open PR wait retained execution authority: issue=%+v err=%v", retained.Issues["1"], err)
	}
	now := time.Now().UTC()
	github.remote.PullRequests[0].State = "MERGED"
	github.remote.PullRequests[0].MergedAt = &now
	github.remote.PullRequests[0].HeadSHA = "head-1"
	if err := loop.reconcileStartup(context.Background(), retained); err != nil {
		t.Fatal(err)
	}
	completed, err := loop.Store.Load()
	if err != nil || completed.ActiveExecution != nil || completed.Issues["1"].Status != issuedomain.StatusCompleted || !completed.Issues["1"].PullRequestMerged {
		t.Fatalf("merged PR did not converge atomically: issue=%+v err=%v", completed.Issues["1"], err)
	}
}

func timePointer() *time.Time {
	now := time.Now().UTC()
	return &now
}

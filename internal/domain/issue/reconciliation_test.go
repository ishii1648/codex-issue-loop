package issue

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDecideReconciliationOwnsLifecycleTargets(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		current  ReconciliationState
		observed ReconciliationObservation
		want     Status
		reason   string
	}{
		{
			name: "dead worker retries", current: ReconciliationState{Status: StatusRunning, WorkerPID: 42, WorktreeSaved: true},
			observed: ReconciliationObservation{Now: now, IssueOpen: true, Running: true, Workspace: ReconciliationWorkspace{Exists: true, Valid: true}},
			want:     StatusRetryWait, reason: "dead worker",
		},
		{
			name: "merged pull request completes", current: ReconciliationState{Status: StatusAwaitingMerge, Branch: "codex/issue-1", PullRequest: "pr", WorktreeSaved: true},
			observed: ReconciliationObservation{Now: now, IssueOpen: true, PullRequests: []ReconciliationPullRequest{{URL: "pr", HeadRefName: "codex/issue-1", Merged: true}}},
			want:     StatusCompleted, reason: "merged Pull Request",
		},
		{
			name: "ambiguous labels do not override claim", current: ReconciliationState{Status: StatusClaiming},
			observed: ReconciliationObservation{Now: now, IssueOpen: true, Ready: true, Running: true},
			want:     StatusClaiming, reason: "idempotently",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DecideReconciliation(tt.current, tt.observed)
			if got.Status != tt.want || !strings.Contains(got.Reason, tt.reason) {
				t.Fatalf("decision=%+v want status=%s reason containing %q", got, tt.want, tt.reason)
			}
		})
	}
}

func TestDecideReconciliationMergedPullRequestClearsWorkerIdentity(t *testing.T) {
	for _, status := range []Status{StatusRunning, StatusAwaitingMerge} {
		t.Run(string(status), func(t *testing.T) {
			current := ReconciliationState{Status: status, WorkerPID: 42, WorkerPGID: 42}
			got := DecideReconciliation(current, ReconciliationObservation{
				PullRequests: []ReconciliationPullRequest{{URL: "pr", Merged: true}},
			})
			if got.Status != StatusCompleted || got.WorkerPID != 0 || got.WorkerPGID != 0 {
				t.Fatalf("decision=%+v want completed with zero worker PID and PGID", got)
			}
		})
	}
}

func TestDecideReconciliationBlocksUnsafeRecovery(t *testing.T) {
	retryAt := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	openPR := ReconciliationPullRequest{URL: "pr", State: "OPEN", HeadRefName: "codex/issue-1"}
	validWorkspace := ReconciliationWorkspace{Exists: true, Valid: true, Branch: "codex/issue-1", LocalBranchExists: true, RemoteBranchExists: true}
	tests := []struct {
		name         string
		pullRequests []ReconciliationPullRequest
		workspace    ReconciliationWorkspace
		unsaved      bool
		workerAlive  bool
		wantPR       string
		reason       string
	}{
		{name: "multiple pull requests", pullRequests: []ReconciliationPullRequest{{Merged: true}, openPR}, workspace: validWorkspace, reason: "multiple Pull Requests target the saved branch"},
		{name: "multiple open pull requests", pullRequests: []ReconciliationPullRequest{openPR, openPR}, workspace: validWorkspace, reason: "multiple Pull Requests target the saved branch"},
		{name: "closed without merge", pullRequests: []ReconciliationPullRequest{{URL: "pr", State: "CLOSED"}}, workspace: validWorkspace, wantPR: "pr", reason: "Pull Request was closed without merge"},
		{name: "missing worktree", workspace: ReconciliationWorkspace{Valid: true}, reason: "saved worktree is missing or invalid"},
		{name: "invalid worktree", workspace: ReconciliationWorkspace{Exists: true}, reason: "saved worktree is missing or invalid"},
		{name: "changed branch", workspace: ReconciliationWorkspace{Exists: true, Valid: true, Branch: "other", LocalBranchExists: true}, reason: "worktree branch changed from codex/issue-1 to other"},
		{name: "missing local branch", workspace: ReconciliationWorkspace{Exists: true, Valid: true, Branch: "codex/issue-1", RemoteBranchExists: true}, reason: "saved local branch is missing"},
		{name: "missing remote branch", pullRequests: []ReconciliationPullRequest{openPR}, workspace: ReconciliationWorkspace{Exists: true, Valid: true, Branch: "codex/issue-1", LocalBranchExists: true}, wantPR: "pr", reason: "open Pull Request head branch is missing from origin"},
		{name: "unsaved worktree with open pull request", pullRequests: []ReconciliationPullRequest{openPR}, workspace: validWorkspace, unsaved: true, wantPR: "pr", reason: "open Pull Request exists but the saved worktree is missing"},
		{name: "live worker", workspace: validWorkspace, workerAlive: true, reason: "saved worker PID 42 is still alive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := ReconciliationState{Status: StatusRunning, Branch: "codex/issue-1", WorktreeSaved: !tt.unsaved, RetryAt: &retryAt, WorkerPID: 42, WorkerPGID: 43}
			got := DecideReconciliation(current, ReconciliationObservation{PullRequests: tt.pullRequests, Workspace: tt.workspace, WorkerAlive: tt.workerAlive})
			want := ReconciliationDecision{Status: StatusBlocked, Branch: current.Branch, PullRequest: tt.wantPR, LastError: "startup reconciliation blocked: " + tt.reason, Effect: EffectMarkBlocked, Reason: tt.reason}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decision=%+v want=%+v", got, want)
			}
		})
	}
}

func TestDecideReconciliationRetrySchedule(t *testing.T) {
	now := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	tests := []struct {
		name        string
		status      Status
		retryAt     *time.Time
		wantStatus  Status
		wantRetryAt *time.Time
		lastError   string
		reason      string
	}{
		{name: "interrupted launch", status: StatusLaunching, retryAt: &later, wantStatus: StatusRetryWait, wantRetryAt: &now, lastError: "worker launch interrupted before process identity was saved", reason: "dead worker launch scheduled for retry"},
		{name: "conflict without retry time", status: StatusResolvingConflict, wantStatus: StatusResolvingConflict, wantRetryAt: &now, reason: "durable Pull Request conflict recovery will resume in the saved worktree"},
		{name: "conflict preserves retry time", status: StatusResolvingConflict, retryAt: &later, wantStatus: StatusResolvingConflict, wantRetryAt: &later, reason: "durable Pull Request conflict recovery will resume in the saved worktree"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := ReconciliationState{Status: tt.status, RetryAt: tt.retryAt, WorkerPID: 42, WorkerPGID: 43}
			got := DecideReconciliation(current, ReconciliationObservation{Now: now})
			want := ReconciliationDecision{Status: tt.wantStatus, RetryAt: tt.wantRetryAt, LastError: tt.lastError, Reason: tt.reason}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decision=%+v want=%+v", got, want)
			}
		})
	}
}

func TestTerminalPullRequestReconciliationRequiresExactIdentity(t *testing.T) {
	retryAt := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		branch string
		pr     ReconciliationPullRequest
		merged bool
		reason string
	}{
		{name: "URL mismatch", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "other", HeadRefName: "codex/issue-1", Merged: true}, reason: "Pull Request for the saved branch does not match the saved Pull Request URL"},
		{name: "branch mismatch", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "pr", HeadRefName: "other", Merged: true}, reason: "Pull Request head does not match the saved branch"},
		{name: "missing saved branch", pr: ReconciliationPullRequest{URL: "pr", HeadRefName: "codex/issue-1", Merged: true}, reason: "Pull Request head does not match the saved branch"},
		{name: "missing head branch", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "pr", Merged: true}, reason: "Pull Request head does not match the saved branch"},
		{name: "open", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "pr", HeadRefName: "codex/issue-1", State: "OPEN"}, reason: "saved Pull Request is not merged"},
		{name: "closed", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "pr", HeadRefName: "codex/issue-1", State: "CLOSED"}, reason: "saved Pull Request was closed without merge"},
		{name: "merged", branch: "codex/issue-1", pr: ReconciliationPullRequest{URL: "pr", HeadRefName: "codex/issue-1", State: "CLOSED", Merged: true}, merged: true, reason: "saved Pull Request merge discovered for terminal Issue"},
	}
	for _, status := range []Status{StatusBlocked, StatusFailed} {
		for _, tt := range tests {
			t.Run(string(status)+"/"+tt.name, func(t *testing.T) {
				current := ReconciliationState{Status: status, Branch: tt.branch, PullRequest: "pr", LastError: "previous error", RetryAt: &retryAt, WorkerPID: 42, WorkerPGID: 43}
				observed := ReconciliationObservation{PullRequests: []ReconciliationPullRequest{tt.pr}}
				want := ReconciliationDecision{Status: status, Branch: tt.branch, PullRequest: "pr", LastError: "previous error", RetryAt: &retryAt, WorkerPID: 42, WorkerPGID: 43, Reason: tt.reason}
				if tt.merged {
					want = ReconciliationDecision{Status: StatusCompleted, Branch: tt.branch, PullRequest: "pr", Effect: EffectMarkDone, PullRequestMerged: true, Reason: tt.reason}
				}
				got, ok := DecideTerminalPullRequestReconciliation(current, observed)
				if !ok || !reflect.DeepEqual(got, want) {
					t.Fatalf("decision=%+v candidate=%v want=%+v", got, ok, want)
				}
				if got := DecideReconciliation(current, observed); !reflect.DeepEqual(got, want) {
					t.Fatalf("reconciliation decision=%+v want=%+v", got, want)
				}
			})
		}
	}
}

func TestManualGitHubStateCannotCancelManagedIssue(t *testing.T) {
	for _, status := range []Status{StatusBlocked, StatusFailed, StatusCompleted} {
		for _, reason := range []string{"NOT_PLANNED", "COMPLETED", ""} {
			current := ReconciliationState{Number: 93, Status: status, RunID: "run_93", Generation: 4}
			observed := ReconciliationObservation{IssueClosed: true, IssueStateReason: reason, Done: true, Failed: true, Excluded: true, Ready: true, Running: true}
			decision := DecideReconciliation(current, observed)
			if decision.Status != status {
				t.Fatalf("manual state changed %s: %+v", status, decision)
			}
		}
	}
}

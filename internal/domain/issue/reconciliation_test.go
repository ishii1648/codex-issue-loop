package issue

import (
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
			name: "ambiguous labels block", current: ReconciliationState{Status: StatusClaiming},
			observed: ReconciliationObservation{Now: now, IssueOpen: true, Ready: true, Running: true},
			want:     StatusBlocked, reason: "conflicting ready",
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

func TestTerminalPullRequestReconciliationRequiresExactIdentity(t *testing.T) {
	current := ReconciliationState{Status: StatusFailed, Branch: "codex/issue-1", PullRequest: "pr", Effect: EffectNone}
	decision, ok := DecideTerminalPullRequestReconciliation(current, ReconciliationObservation{
		PullRequests: []ReconciliationPullRequest{{URL: "other", HeadRefName: current.Branch, Merged: true}},
	})
	if !ok || decision.Status != StatusFailed || !strings.Contains(decision.Reason, "does not match") {
		t.Fatalf("decision=%+v candidate=%v", decision, ok)
	}
}

func TestNotPlannedCancellationRequiresInactiveUnambiguousTerminalBoundary(t *testing.T) {
	base := ReconciliationState{Number: 93, Status: StatusBlocked, RunID: "run_93", Generation: 4}
	closed := ReconciliationObservation{IssueClosed: true, IssueStateReason: "NOT_PLANNED"}
	tests := []struct {
		name       string
		current    ReconciliationState
		observed   ReconciliationObservation
		considered bool
		canceled   bool
	}{
		{name: "blocked", current: base, observed: closed, considered: true, canceled: true},
		{name: "failed", current: ReconciliationState{Status: StatusFailed}, observed: closed, considered: true, canceled: true},
		{name: "matching pull request", current: ReconciliationState{Status: StatusBlocked, Branch: "codex/issue-93", PullRequest: "pr", PullRequestNumber: 98, HeadSHA: "head"}, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "not_planned", PullRequests: []ReconciliationPullRequest{{Number: 98, URL: "pr", HeadRefName: "codex/issue-93", HeadSHA: "head"}}}, considered: true, canceled: true},
		{name: "completed close", current: base, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "COMPLETED"}},
		{name: "plain close", current: base, observed: ReconciliationObservation{IssueClosed: true}},
		{name: "unknown reason", current: base, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "UNKNOWN"}},
		{name: "open", current: base, observed: ReconciliationObservation{IssueOpen: true, IssueStateReason: "NOT_PLANNED"}},
		{name: "non terminal", current: ReconciliationState{Status: StatusRunning}, observed: closed},
		{name: "pid present", current: ReconciliationState{Status: StatusBlocked, WorkerPID: 42, WorkerPGID: 42}, observed: closed, considered: true},
		{name: "worker alive", current: base, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "NOT_PLANNED", WorkerAlive: true}, considered: true},
		{name: "pending request", current: ReconciliationState{Status: StatusBlocked, PendingRequest: true}, observed: closed, considered: true},
		{name: "target execution occupied", current: ReconciliationState{Number: 93, Status: StatusBlocked, RunID: "run_93", Generation: 4, ActiveExecutionIssueNumber: 93, ActiveExecutionRunID: "run_93", ActiveExecutionGeneration: 4}, observed: closed, considered: true},
		{name: "target execution owner mismatch", current: ReconciliationState{Number: 93, Status: StatusBlocked, RunID: "run_93", Generation: 4, ActiveExecutionIssueNumber: 93, ActiveExecutionRunID: "run_other", ActiveExecutionGeneration: 4}, observed: closed, considered: true},
		{name: "unrelated execution remains isolated", current: ReconciliationState{Number: 93, Status: StatusBlocked, RunID: "run_93", Generation: 4, ActiveExecutionIssueNumber: 94, ActiveExecutionRunID: "run_94", ActiveExecutionGeneration: 1}, observed: closed, considered: true, canceled: true},
		{name: "incompatible effect", current: ReconciliationState{Status: StatusBlocked, Effect: EffectMarkDone}, observed: closed, considered: true},
		{name: "multiple pull requests", current: ReconciliationState{Status: StatusBlocked, Branch: "branch", PullRequest: "pr"}, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "NOT_PLANNED", PullRequests: []ReconciliationPullRequest{{URL: "pr", HeadRefName: "branch"}, {URL: "other", HeadRefName: "branch"}}}, considered: true},
		{name: "pull request mismatch", current: ReconciliationState{Status: StatusBlocked, Branch: "branch", PullRequest: "pr"}, observed: ReconciliationObservation{IssueClosed: true, IssueStateReason: "NOT_PLANNED", PullRequests: []ReconciliationPullRequest{{URL: "other", HeadRefName: "branch"}}}, considered: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, considered := DecideNotPlannedCancellation(test.current, test.observed)
			if considered != test.considered || (decision.Status == StatusCanceled) != test.canceled {
				t.Fatalf("decision=%+v considered=%v", decision, considered)
			}
		})
	}
}

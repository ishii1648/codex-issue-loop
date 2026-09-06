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

func TestTerminalPullRequestReconciliationRequiresExactIdentity(t *testing.T) {
	current := ReconciliationState{Status: StatusFailed, Branch: "codex/issue-1", PullRequest: "pr", Effect: EffectNone}
	decision, ok := DecideTerminalPullRequestReconciliation(current, ReconciliationObservation{
		PullRequests: []ReconciliationPullRequest{{URL: "other", HeadRefName: current.Branch, Merged: true}},
	})
	if !ok || decision.Status != StatusFailed || !strings.Contains(decision.Reason, "does not match") {
		t.Fatalf("decision=%+v candidate=%v", decision, ok)
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

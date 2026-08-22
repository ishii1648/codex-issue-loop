package app

import (
	"testing"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestPullRequestReplacementChecksPolicy(t *testing.T) {
	failure := &state.PullRequestChecksFailure{HeadSHA: "old"}
	failedReplacement := gh.PullRequest{HeadSHA: "new", ChecksStatus: "failure"}
	if pullRequestReplacementChecksAllowed(failure, failedReplacement, true) {
		t.Fatal("legacy failure must be refused")
	}
	if !pullRequestReplacementChecksAllowed(failure, failedReplacement, false) {
		t.Fatal("typed failure must remain observable for an idempotent recovery record")
	}
}

func TestPullRequestChecksRecoveryProgressIncludesIdempotentStates(t *testing.T) {
	issue := &state.Issue{Status: issuedomain.StatusAwaitingChecks, PullRequestChecksRecovery: &state.PullRequestChecksRecovery{ID: "checks_1"}}
	if got := pullRequestChecksRecoveryProgress(issue); got != recoveryProgressIdempotent {
		t.Fatalf("progress=%v", got)
	}
}

func TestEnvironmentResumeProgress(t *testing.T) {
	tests := []struct {
		name  string
		issue state.Issue
		want  recoveryProgress
	}{
		{"pending sync", state.Issue{Status: issuedomain.StatusEnvironmentResumePending, GitHubSync: issuedomain.GitHubSyncEnvironmentResume, EnvironmentResume: &state.EnvironmentResume{ID: "resume_1", Status: issuedomain.EnvironmentResumeStatusRequested}}, recoveryProgressPendingSync},
		{"idempotent", state.Issue{Status: issuedomain.StatusEnvironmentResumePending, EnvironmentResume: &state.EnvironmentResume{ID: "resume_1", Status: issuedomain.EnvironmentResumeStatusGitHubSynced}}, recoveryProgressIdempotent},
		{"interrupted", state.Issue{Status: issuedomain.StatusBlocked, EnvironmentResume: &state.EnvironmentResume{ID: "resume_1", Status: issuedomain.EnvironmentResumeStatusRequested}}, recoveryProgressInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := environmentResumeProgress(&tt.issue); got != tt.want {
				t.Fatalf("progress=%v want %v", got, tt.want)
			}
		})
	}
}

package app

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

type recoveryProgress uint8

const (
	recoveryProgressInvalid recoveryProgress = iota
	recoveryProgressFresh
	recoveryProgressPendingSync
	recoveryProgressIdempotent
	recoveryProgressInterrupted
)

func pullRequestChecksRecoveryProgress(issue *state.Issue) recoveryProgress {
	if issue == nil {
		return recoveryProgressInvalid
	}
	if issue.PullRequestChecksRecovery != nil {
		switch issue.Status {
		case issuedomain.StatusChecksRecovery, issuedomain.StatusAwaitingChecks,
			issuedomain.StatusAwaitingMerge, issuedomain.StatusCompleted:
			return recoveryProgressIdempotent
		}
	}
	if issue.Status == issuedomain.StatusFailed && issue.GitHubSync == issuedomain.GitHubSyncNone {
		return recoveryProgressFresh
	}
	return recoveryProgressInvalid
}

func pullRequestReplacementChecksAllowed(failure *state.PullRequestChecksFailure, pr gh.PullRequest, legacy bool) bool {
	if failure == nil || pr.HeadSHA == failure.HeadSHA {
		return false
	}
	switch pr.ChecksStatus {
	case "pending", "success":
		return true
	case "failure":
		return !legacy
	default:
		return false
	}
}

func environmentResumeProgress(issue *state.Issue) recoveryProgress {
	if issue == nil {
		return recoveryProgressInvalid
	}
	resume := issue.EnvironmentResume
	if resume != nil && resume.ID != "" {
		if issue.Status == issuedomain.StatusEnvironmentResumePending {
			if issue.GitHubSync == issuedomain.GitHubSyncEnvironmentResume && resume.Status == issuedomain.EnvironmentResumeStatusRequested {
				return recoveryProgressPendingSync
			}
			if issue.GitHubSync == issuedomain.GitHubSyncNone && resume.Status == issuedomain.EnvironmentResumeStatusGitHubSynced {
				return recoveryProgressIdempotent
			}
			return recoveryProgressInvalid
		}
		if issue.Status == issuedomain.StatusBlocked && issue.GitHubSync == issuedomain.GitHubSyncNone &&
			(resume.Status == issuedomain.EnvironmentResumeStatusRequested || resume.Status == issuedomain.EnvironmentResumeStatusGitHubSynced) {
			return recoveryProgressInterrupted
		}
	}
	if issue.Status == issuedomain.StatusBlocked && issue.GitHubSync == issuedomain.GitHubSyncNone {
		return recoveryProgressFresh
	}
	return recoveryProgressInvalid
}

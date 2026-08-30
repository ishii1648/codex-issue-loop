package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) syncGitHub(ctx context.Context, issue state.Issue) error {
	var err error
	var checksTransition *issuedomain.Transition
	if issue.GitHubSync == issuedomain.GitHubSyncPullRequestChecksRecovery {
		decision, decisionErr := issuedomain.AwaitChecks(issue.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide checks recovery synchronization", decisionErr)
		}
		checksTransition = &decision.Transition
	}
	switch issue.GitHubSync {
	case issuedomain.GitHubSyncDone:
		if adoption := issue.MergedPullRequestAdoption; adoption != nil && adoption.Status == issuedomain.MergedPullRequestAdoptionStatusGitHubSyncPending {
			branch := adoption.Branch
			if branch == "" {
				branch = issue.Branch
			}
			remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, branch)
			if inspectErr != nil {
				return failure.Wrap(failure.Transient, "reinspect adopted merged Pull Request", inspectErr)
			}
			if _, validateErr := gh.ValidateMergedPullRequestAdoption(l.Config, remote, gh.MergedPullRequestAdoptionExpectation{
				IssueNumber: issue.Number, PreviousStatus: adoption.PreviousStatus, Branch: branch,
				BaseBranch: adoption.BaseBranch, HeadSHA: adoption.HeadSHA, PullRequestURL: adoption.PullRequestURL,
				PullRequestNumber: adoption.PullRequestNumber, MergeCommitSHA: adoption.MergeSHA, AllowDone: true,
			}); validateErr != nil {
				return validateErr
			}
		}
		err = l.GitHub.MarkDone(ctx, l.Config, issue.Number, issue.PullRequestURL)
	case issuedomain.GitHubSyncNeedsInput:
		snapshot, loadErr := l.Store.Load()
		if loadErr != nil {
			return loadErr
		}
		var pending *state.Request
		for _, request := range snapshot.PendingRequests {
			if request.IssueNumber == issue.Number && request.Status == issuedomain.RequestStatusPending {
				pending = request
				break
			}
		}
		if pending == nil {
			return fmt.Errorf("Issue #%d has no pending request to sync", issue.Number)
		}
		err = l.GitHub.MarkNeedsInput(ctx, l.Config, issue.Number, pending.ID, pending.Question)
	case issuedomain.GitHubSyncConflictRetry:
		recoveryID := issue.RunID
		if issue.ConflictRecovery != nil {
			if issue.ConflictRecovery.RetryID != "" {
				recoveryID = issue.ConflictRecovery.RetryID
			} else if issue.ConflictRecovery.TargetBaseSHA != "" {
				recoveryID = issue.ConflictRecovery.TargetBaseSHA
			}
		}
		err = l.GitHub.MarkConflictRetry(ctx, l.Config, issue.Number, recoveryID)
	case issuedomain.GitHubSyncEnvironmentResume:
		resumeID := issue.RunID
		if issue.EnvironmentResume != nil && issue.EnvironmentResume.ID != "" {
			resumeID = issue.EnvironmentResume.ID
		}
		if resumer, ok := l.GitHub.(interface {
			MarkEnvironmentResume(context.Context, config.Config, int, string) error
		}); ok {
			err = resumer.MarkEnvironmentResume(ctx, l.Config, issue.Number, resumeID)
		} else {
			err = l.GitHub.MarkRunning(ctx, l.Config, issue.Number)
		}
	case issuedomain.GitHubSyncAnsweredWorkspaceRecovery:
		if issue.AnsweredWorkspaceRecovery == nil || issue.AnsweredWorkspaceRecovery.ID == "" || issue.Status != issuedomain.StatusResumePending {
			return fmt.Errorf("Issue #%d answered workspace recovery metadata is missing", issue.Number)
		}
		remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, issue.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "reinspect answered workspace recovery", inspectErr)
		}
		alreadySynced, validateErr := l.validateAnsweredWorkspaceRecoverySync(issue, remote)
		if validateErr != nil {
			return validateErr
		}
		if !alreadySynced {
			resumer, ok := l.GitHub.(interface {
				MarkAnsweredWorkspaceRecovery(context.Context, config.Config, int, string) error
			})
			if !ok {
				return fmt.Errorf("GitHub client does not support answered workspace recovery synchronization")
			}
			err = resumer.MarkAnsweredWorkspaceRecovery(ctx, l.Config, issue.Number, issue.AnsweredWorkspaceRecovery.ID)
		}
	case issuedomain.GitHubSyncPublicationRecovery:
		recoveryID := issue.RunID
		if issue.PublicationRecovery != nil && issue.PublicationRecovery.ID != "" {
			recoveryID = issue.PublicationRecovery.ID
		}
		if resumer, ok := l.GitHub.(interface {
			MarkPublicationRecovery(context.Context, config.Config, int, string) error
		}); ok {
			err = resumer.MarkPublicationRecovery(ctx, l.Config, issue.Number, recoveryID)
		} else {
			err = l.GitHub.MarkRunning(ctx, l.Config, issue.Number)
		}
	case issuedomain.GitHubSyncPullRequestChecksRecovery:
		if issue.PullRequestChecksRecovery == nil {
			return fmt.Errorf("Issue #%d Pull Request checks recovery metadata is missing", issue.Number)
		}
		remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, issue.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "reinspect Pull Request checks recovery", inspectErr)
		}
		if validateErr := l.validatePullRequestChecksRecoverySync(issue, remote); validateErr != nil {
			return validateErr
		}
		err = l.GitHub.MarkPullRequestChecksRecovery(ctx, l.Config, issue.Number, issue.PullRequestChecksRecovery.ID)
	case issuedomain.GitHubSyncFailed, issuedomain.GitHubSyncBlocked:
		err = l.GitHub.MarkFailed(ctx, l.Config, issue.Number, issue.LastError, issue.GitHubSync == issuedomain.GitHubSyncBlocked)
	default:
		return fmt.Errorf("unknown GitHub sync state %q", issue.GitHubSync)
	}
	if err != nil {
		return failure.Wrap(failure.Transient, "sync GitHub Issue state", err)
	}
	_, err = l.Store.Update("github_state_synced", issue.Number, issue.RunID, map[string]any{"state": issue.GitHubSync}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub sync", issue.Number)
		}
		if issue.GitHubSync == issuedomain.GitHubSyncAnsweredWorkspaceRecovery && item.GitHubSync == issuedomain.GitHubSyncNone && item.AnsweredWorkspaceRecovery != nil &&
			issue.AnsweredWorkspaceRecovery != nil && item.AnsweredWorkspaceRecovery.ID == issue.AnsweredWorkspaceRecovery.ID &&
			item.AnsweredWorkspaceRecovery.Status == issuedomain.AnsweredWorkspaceRecoveryStatusGitHubSynced {
			return errAnsweredWorkspaceSyncConverged
		}
		if item.GitHubSync == issue.GitHubSync {
			item.GitHubSync = issuedomain.GitHubSyncNone
			if issue.GitHubSync == issuedomain.GitHubSyncDone && item.MergedPullRequestAdoption != nil && item.MergedPullRequestAdoption.Status == issuedomain.MergedPullRequestAdoptionStatusGitHubSyncPending {
				item.MergedPullRequestAdoption.Status = issuedomain.MergedPullRequestAdoptionStatusSynced
			}
			if issue.GitHubSync == issuedomain.GitHubSyncEnvironmentResume && item.EnvironmentResume != nil {
				item.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusGitHubSynced
			}
			if issue.GitHubSync == issuedomain.GitHubSyncAnsweredWorkspaceRecovery && item.AnsweredWorkspaceRecovery != nil {
				item.AnsweredWorkspaceRecovery.Status = issuedomain.AnsweredWorkspaceRecoveryStatusGitHubSynced
			}
			if issue.GitHubSync == issuedomain.GitHubSyncPublicationRecovery && item.PublicationRecovery != nil {
				item.PublicationRecovery.Status = issuedomain.PublicationRecoveryStatusGitHubSynced
			}
			if issue.GitHubSync == issuedomain.GitHubSyncPullRequestChecksRecovery && item.PullRequestChecksRecovery != nil {
				now := l.now()
				if checksTransition == nil {
					return fmt.Errorf("Issue #%d checks recovery is missing its lifecycle decision", issue.Number)
				}
				if err := state.ApplyIssueTransition(item, *checksTransition); err != nil {
					return err
				}
				item.FailureKind = ""
				item.LastError = ""
				item.PullRequestChecksRecovery.Status = issuedomain.PullRequestChecksRecoveryStatusResumed
				item.RetryAfter = &now
			}
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if errors.Is(err, errAnsweredWorkspaceSyncConverged) {
		return nil
	}
	return failure.Wrap(failure.Supervisor, "persist GitHub synchronization", err)
}

func (l *Loop) validateAnsweredWorkspaceRecoverySync(issue state.Issue, remote gh.RemoteState) (bool, error) {
	if issue.AnsweredWorkspaceRecovery == nil || !strings.EqualFold(remote.Issue.State, "open") || len(remote.PullRequests) != 0 {
		return false, fmt.Errorf("refuse answered workspace recovery synchronization: authoritative Issue identity changed")
	}
	labels := labelSet(remote.Issue.Labels)
	blockedLabel := ""
	for _, label := range l.Config.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = label
		} else if labels[label] {
			return false, fmt.Errorf("refuse answered workspace recovery synchronization: manual exclusion label changed")
		}
	}
	blocked, running := labels[blockedLabel], labels[l.Config.GitHub.RunningLabel]
	alreadySynced := !blocked && running
	if blocked == running || hasAnyLabel(labels, append(append([]string{l.Config.GitHub.NeedsInputLabel, l.Config.GitHub.DoneLabel, l.Config.GitHub.FailedLabel}, l.Config.GitHub.ReadyLabels...), "")) {
		return false, fmt.Errorf("refuse answered workspace recovery synchronization: authoritative Issue labels changed")
	}
	requestMarker := "<!-- codex-issue-loop:request:" + issue.AnsweredWorkspaceRecovery.RequestID + " -->"
	failedMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", issue.Number)
	failureDigest := sha256.Sum256([]byte(issue.LastError))
	failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", failureDigest[:8])
	recoveryMarker := "<!-- codex-issue-loop:answered-workspace-recovery:" + issue.AnsweredWorkspaceRecovery.ID + " -->"
	requestCount, failedCount := commentMarkerCount(remote.Issue.Comments, requestMarker), commentMarkerCount(remote.Issue.Comments, failedMarker)
	failureCount, recoveryCount := commentMarkerCount(remote.Issue.Comments, failureMarker), commentMarkerCount(remote.Issue.Comments, recoveryMarker)
	if requestCount != 1 || failedCount != 1 || failureCount != 1 || (alreadySynced && recoveryCount != 1) || (!alreadySynced && recoveryCount != 0) {
		return false, fmt.Errorf("refuse answered workspace recovery synchronization: authoritative comment markers changed")
	}
	return alreadySynced, nil
}

func commentMarkerCount(comments []string, marker string) int {
	count := 0
	for _, comment := range comments {
		if strings.Contains(comment, marker) {
			count++
		}
	}
	return count
}

func (l *Loop) validatePullRequestChecksRecoverySync(issue state.Issue, remote gh.RemoteState) error {
	labels := labelSet(remote.Issue.Labels)
	failed := labels[l.Config.GitHub.FailedLabel]
	running := labels[l.Config.GitHub.RunningLabel]
	if !strings.EqualFold(remote.Issue.State, "open") || failed == running ||
		hasAnyLabel(labels, append(append([]string{l.Config.GitHub.DoneLabel, l.Config.GitHub.NeedsInputLabel}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: authoritative Issue labels changed")
	}
	if len(remote.PullRequests) != 1 || issue.PullRequestChecksRecovery == nil {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: saved Pull Request count changed")
	}
	pr := remote.PullRequests[0]
	if pr.URL != issue.PullRequestURL || pr.Number != issue.PullRequestNumber || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil ||
		pr.HeadRefName != issue.Branch || pr.BaseRefName != l.Config.Git.BaseBranch || !strings.EqualFold(pr.HeadRepository, l.Config.GitHub.Repo) || pr.HeadSHA != issue.PullRequestChecksRecovery.NewHeadSHA ||
		(pr.ChecksStatus != "pending" && pr.ChecksStatus != "success") {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: authoritative Pull Request, head, or checks changed")
	}
	return nil
}

package supervisor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) syncGitHub(ctx context.Context, issue state.Issue) error {
	snapshot, loadErr := l.Store.Load()
	if loadErr != nil {
		return loadErr
	}
	effect := state.PendingEffect(&snapshot, issue.Number)
	if effect == nil {
		return nil
	}
	if effect.RunID != issue.RunID {
		return fmt.Errorf("Issue #%d effect belongs to a different run", issue.Number)
	}
	var err error
	switch effect.Kind {
	case issuedomain.EffectMarkDone:
		if pendingGenericAdoption(issue) {
			remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, issue.Branch)
			if inspectErr != nil {
				return failure.Wrap(failure.Transient, "reinspect adopted merged Pull Request", inspectErr)
			}
			previousStatus := issuedomain.StatusFailed
			for _, label := range remote.Issue.Labels {
				if strings.EqualFold(label, "blocked") {
					previousStatus = issuedomain.StatusBlocked
				}
			}
			if _, validateErr := gh.ValidateMergedPullRequest(l.Config, remote, gh.MergedPullRequestExpectation{
				IssueNumber: issue.Number, PreviousStatus: previousStatus, Branch: issue.Branch,
				BaseBranch: l.Config.Git.BaseBranch, HeadSHA: issue.HeadSHA, PullRequestURL: issue.PullRequestURL,
				PullRequestNumber: issue.PullRequestNumber, AllowDone: true,
			}); validateErr != nil {
				return validateErr
			}
			if l.Worktrees == nil {
				return fmt.Errorf("refuse adopted Pull Request synchronization: worktree inspector is unavailable")
			}
			inspection, worktreeErr := l.Worktrees.Inspect(ctx, l.Config, issue.Worktree, issue.Branch)
			if worktreeErr != nil || !inspection.Valid || inspection.Dirty || inspection.UnpushedCommits || inspection.Head != issue.HeadSHA || inspection.RemoteHead != issue.HeadSHA {
				return fmt.Errorf("refuse adopted Pull Request synchronization: saved worktree changed")
			}
		}
		err = l.GitHub.MarkDone(ctx, l.Config, issue.Number, issue.PullRequestURL)
	case issuedomain.EffectMarkNeedsInput:
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
	case issuedomain.EffectRetryConflict:
		recoveryID := issue.RunID
		if issue.ConflictRecovery != nil {
			if issue.ConflictRecovery.RetryID != "" {
				recoveryID = issue.ConflictRecovery.RetryID
			} else if issue.ConflictRecovery.TargetBaseSHA != "" {
				recoveryID = issue.ConflictRecovery.TargetBaseSHA
			}
		}
		err = l.GitHub.MarkConflictRetry(ctx, l.Config, issue.Number, recoveryID)
	case issuedomain.EffectApplyResolution:
		if issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionResolved ||
			(issue.Suspension.Resolution != issuedomain.ResolutionResume && issue.Suspension.Resolution != issuedomain.ResolutionRetryStage) {
			return fmt.Errorf("Issue #%d resolution synchronization has no resolved executable suspension", issue.Number)
		}
		remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, issue.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "reinspect Issue resolution", inspectErr)
		}
		if validateErr := l.validateIssueResolutionSync(issue, remote); validateErr != nil {
			return validateErr
		}
		if issue.Continuation.Stage == issuedomain.ContinuationStageChecks {
			if l.Worktrees == nil {
				return fmt.Errorf("refuse Issue resolution synchronization: worktree inspector is unavailable")
			}
			inspection, inspectWorktreeErr := l.Worktrees.Inspect(ctx, l.Config, issue.Worktree, issue.Branch)
			if inspectWorktreeErr != nil || !inspection.Valid || inspection.Dirty || inspection.UnpushedCommits || inspection.Head != issue.HeadSHA {
				return fmt.Errorf("refuse Issue resolution synchronization: repaired Pull Request worktree changed")
			}
		}
		err = l.GitHub.MarkRunning(ctx, l.Config, issue.Number)
	case issuedomain.EffectMarkFailed, issuedomain.EffectMarkBlocked:
		err = l.GitHub.MarkFailed(ctx, l.Config, issue.Number, issue.LastError, effect.Kind == issuedomain.EffectMarkBlocked)
	default:
		return fmt.Errorf("unknown lifecycle effect %q", effect.Kind)
	}
	if err != nil {
		return failure.Wrap(failure.Transient, "sync GitHub Issue state", err)
	}
	_, err = l.Store.Update("github_state_synced", issue.Number, issue.RunID, map[string]any{"effect_id": effect.ID, "kind": effect.Kind}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub sync", issue.Number)
		}
		if current := state.PendingEffect(s, issue.Number); current == nil || current.ID != effect.ID {
			return nil
		}
		if err := state.ClearEffect(s, issue.Number, effect.ID); err != nil {
			return err
		}
		if effect.Kind == issuedomain.EffectMarkDone && pendingGenericAdoption(*item) {
			item.Continuation = nil
			item.Suspension = nil
		}
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist GitHub synchronization", err)
}

func pendingGenericAdoption(issue state.Issue) bool {
	return issue.Status == issuedomain.StatusCompleted && issue.PullRequestMerged && issue.Continuation != nil && issue.Suspension != nil &&
		issue.Suspension.Status == issuedomain.SuspensionResolved && issue.Suspension.Resolution == issuedomain.ResolutionAdoptPR &&
		issue.Suspension.CheckpointID == issue.Continuation.ID
}

func (l *Loop) validateIssueResolutionSync(issue state.Issue, remote gh.RemoteState) error {
	checkpoint := issue.Continuation
	snapshot, err := l.Store.Load()
	identity := state.ExecutionIdentity{RunID: issue.RunID, Generation: issue.Generation}
	if err != nil || checkpoint == nil || !state.OwnsActiveExecution(&snapshot, issue.Number, identity) || !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("refuse Issue resolution synchronization: continuation authority changed")
	}
	labels := labelSet(remote.Issue.Labels)
	terminalLabels := 0
	if labels[l.Config.GitHub.FailedLabel] {
		terminalLabels++
	}
	for _, label := range l.Config.GitHub.ExcludeLabels {
		if labels[label] {
			terminalLabels++
		}
	}
	if terminalLabels != 1 || labels[l.Config.GitHub.RunningLabel] || labels[l.Config.GitHub.NeedsInputLabel] ||
		labels[l.Config.GitHub.DoneLabel] || hasAnyLabel(labels, l.Config.GitHub.ReadyLabels) {
		return fmt.Errorf("refuse Issue resolution synchronization: authoritative Issue labels changed")
	}
	if checkpoint.Kind == state.ContinuationKindNeedsInput {
		answerCount := 0
		for _, answer := range issue.Answers {
			if answer.RequestID == checkpoint.RequestID {
				answerCount++
			}
		}
		requestMarker := "<!-- codex-issue-loop:request:" + checkpoint.RequestID + " -->"
		failedMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", issue.Number)
		failureDigest := sha256.Sum256([]byte(issue.LastError))
		failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", failureDigest[:8])
		if checkpoint.RequestID == "" || answerCount != 1 || commentMarkerCount(remote.Issue.Comments, requestMarker) != 1 ||
			commentMarkerCount(remote.Issue.Comments, failedMarker) != 1 || commentMarkerCount(remote.Issue.Comments, failureMarker) != 1 {
			return fmt.Errorf("refuse Issue resolution synchronization: answered checkpoint evidence changed")
		}
	}
	if issue.PullRequestURL == "" {
		for _, pullRequest := range remote.PullRequests {
			if strings.EqualFold(pullRequest.State, "open") && pullRequest.MergedAt == nil {
				return fmt.Errorf("refuse Issue resolution synchronization: a Pull Request appeared after planning")
			}
		}
		return nil
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("refuse Issue resolution synchronization: saved Pull Request count changed")
	}
	pullRequest := remote.PullRequests[0]
	if pullRequest.URL != issue.PullRequestURL || pullRequest.Number != issue.PullRequestNumber ||
		pullRequest.HeadRefName != issue.Branch || !strings.EqualFold(pullRequest.State, "open") || pullRequest.MergedAt != nil {
		return fmt.Errorf("refuse Issue resolution synchronization: saved Pull Request identity changed")
	}
	if checkpoint.Stage == issuedomain.ContinuationStageChecks &&
		(pullRequest.HeadSHA == "" || pullRequest.HeadSHA != issue.HeadSHA || (pullRequest.ChecksStatus != "pending" && pullRequest.ChecksStatus != "success")) {
		return fmt.Errorf("refuse Issue resolution synchronization: repaired Pull Request head or checks changed")
	}
	return nil
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

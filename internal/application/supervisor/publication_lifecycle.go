package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) recordPublicationFailure(current state.Issue, cause error) error {
	provenance := publication.ClassifyFailure(cause, l.now())
	_, err := l.Store.Update("publication_failed", current.Number, current.RunID, &provenance, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID {
			return fmt.Errorf("Issue #%d run changed while recording publication failure", current.Number)
		}
		if item.Lease != nil {
			provenance.DeclaredResources = append([]string(nil), item.Lease.DeclaredResources...)
			provenance.ResolvedResources = append([]string(nil), item.Lease.ResolvedResources...)
			provenance.ActualResources = append([]string(nil), item.Lease.ActualResources...)
		}
		item.PublicationFailure = &provenance
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist publication failure provenance", err)
}

func (l *Loop) processPublicationRecovery(ctx context.Context, current state.Issue) error {
	recovery := current.PublicationRecovery
	if recovery == nil || recovery.ID == "" || !state.ValidID(current.RunID, "run_") || current.Lease == nil || current.Lease.BaseSHA == "" || l.Publisher == nil || l.Worktrees == nil {
		return l.failPublicationRecovery(ctx, current, "publication recovery metadata or durable base SHA is missing")
	}
	runningAttempt := publicationRecoveryAttemptRunning(recovery, recovery.Attempts)
	if (recovery.Status == issuedomain.PublicationRecoveryStatusPublishing) != runningAttempt {
		return l.failPublicationRecovery(ctx, current, "publication recovery attempt history is inconsistent")
	}
	if (issuedomain.AttemptBudget{Attempts: recovery.Attempts, MaxAttempts: recovery.MaxAttempts}).Exhausted() && !runningAttempt {
		return l.failPublicationRecovery(ctx, current, "publication recovery budget is exhausted")
	}
	inspection, err := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if err != nil {
		return l.failPublicationRecovery(ctx, current, "inspect saved publication worktree: "+err.Error())
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || inspection.Head == "" {
		return l.failPublicationRecovery(ctx, current, "saved publication worktree or branch is invalid")
	}
	if inspection.RemoteBranchExists && !inspection.RemoteConsistent {
		return l.failPublicationRecovery(ctx, current, "saved local and remote branch histories diverged")
	}
	if recovery.Attempts == 0 && inspection.Head != recovery.ExpectedHeadSHA {
		return l.failPublicationRecovery(ctx, current, "saved publication worktree HEAD changed after operator validation")
	}
	if recovery.Attempts == 0 {
		digest, digestErr := l.Worktrees.ContentDigest(ctx, current.Worktree)
		if digestErr != nil || digest != recovery.WorktreeSHA256 {
			return l.failPublicationRecovery(ctx, current, "saved publication worktree content changed after operator validation")
		}
	}
	result, resultBytes, err := worker.LoadLatestCompletedResult(filepath.Join(l.Store.Dir, "runs", current.RunID))
	if err != nil {
		return l.failPublicationRecovery(ctx, current, "saved completed worker result is unavailable: "+err.Error())
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(resultBytes))
	if digest != recovery.ResultSHA256 || result.Summary != recovery.Summary {
		return l.failPublicationRecovery(ctx, current, "saved completed worker result changed after operator validation")
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh Issue and Pull Requests for publication recovery", err)
	}
	if remoteErr := validatePublicationRecoveryRemote(l.Config, current, remote, inspection, recovery.Attempts > 0); remoteErr != nil {
		return l.failPublicationRecovery(ctx, current, remoteErr.Error())
	}
	issue := remote.Issue
	savedPRURL := current.PullRequestURL
	if savedPRURL == "" && len(remote.PullRequests) == 1 {
		savedPRURL = remote.PullRequests[0].URL
	}

	now := l.now()
	resumingAttempt := runningAttempt
	attemptNumber := recovery.Attempts + 1
	eventType := "publication_recovery_attempt_started"
	if resumingAttempt {
		attemptNumber = recovery.Attempts
		eventType = "publication_recovery_attempt_resumed"
	}
	_, err = l.Store.Update(eventType, current.Number, current.RunID, map[string]any{
		"recovery_id": recovery.ID, "generation": recovery.Generation, "attempt": attemptNumber,
		"resumed": resumingAttempt, "pull_request_url": savedPRURL,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.Status != issuedomain.StatusPublicationRecovery || item.GitHubSync != issuedomain.GitHubSyncNone || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recovery.ID || item.PublicationRecovery.Attempts != recovery.Attempts {
			return fmt.Errorf("Issue #%d publication recovery changed before attempt", current.Number)
		}
		if !resumingAttempt {
			item.PublicationRecovery.Attempts = attemptNumber
			item.PublicationRecovery.History = append(item.PublicationRecovery.History, state.PublicationRecoveryAttempt{
				Number: attemptNumber, Generation: recovery.Generation, Status: issuedomain.PublicationRecoveryAttemptStatusRunning, StartedAt: now,
			})
		}
		if item.PullRequestURL == "" && savedPRURL != "" {
			item.PullRequestURL = savedPRURL
		}
		item.PublicationRecovery.Status = issuedomain.PublicationRecoveryStatusPublishing
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery attempt", err)
	}
	declared := append([]string(nil), current.DeclaredResources...)
	l.publicationMu.Lock()
	published, audit, publishErr := l.Publisher.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, savedPRURL, recovery.Summary, current.Lease.BaseSHA, declared)
	l.publicationMu.Unlock()
	_, auditErr := l.Store.Update("publication_audited", current.Number, current.RunID, audit, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		auditCopy := audit
		item.PublicationAudit = &auditCopy
		item.ActualResources = append([]string(nil), audit.ActualResources...)
		if item.Lease != nil {
			item.Lease.ActualResources = append([]string(nil), audit.ActualResources...)
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if auditErr != nil {
		return failure.Wrap(failure.Supervisor, "persist recovered publication audit", auditErr)
	}
	if publishErr != nil {
		if ctx.Err() != nil {
			// Keep the write-ahead attempt running. A restarted supervisor resumes
			// this same attempt without consuming another budget entry.
			return nil
		}
		return l.finishPublicationRecoveryFailure(ctx, current, recovery.ID, attemptNumber, publishErr)
	}
	result.Git = &published
	successTransition := issuedomain.Transition{}
	pullRequestURL := published.PullRequestURL
	pullRequestMerged := false
	githubSync := issuedomain.GitHubSyncNone
	if published.PullRequestURL == "" {
		completionDecision, decisionErr := issuedomain.Complete(current.Status, "")
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide recovered publication completion", decisionErr)
		}
		successTransition = completionDecision.Transition
		pullRequestURL = completionDecision.PullRequestURL
		pullRequestMerged = completionDecision.PullRequestMerged
		githubSync = completionDecision.GitHubSync
	} else {
		checksDecision, decisionErr := issuedomain.AwaitChecks(current.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide recovered publication check wait", decisionErr)
		}
		successTransition = checksDecision.Transition
	}
	_, err = l.Store.Update("publication_recovery_succeeded", current.Number, current.RunID, result, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recovery.ID {
			return fmt.Errorf("Issue #%d publication recovery disappeared", current.Number)
		}
		if err := state.ApplyIssueTransition(item, successTransition); err != nil {
			return err
		}
		finishPublicationRecoveryAttempt(item, attemptNumber, issuedomain.PublicationRecoveryAttemptStatusSucceeded, "", l.now())
		item.PublicationRecovery.Status = issuedomain.PublicationRecoveryStatusSucceeded
		item.LastError = ""
		item.FailureKind = ""
		item.RetryAfter = nil
		if published.PullRequestURL == "" {
			if issuedomain.DecideLease(successTransition.To, pullRequestURL != "", false) == issuedomain.ReleaseLease && item.Lease != nil {
				if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
					return releaseErr
				}
			}
			item.PullRequestURL = pullRequestURL
			item.PullRequestMerged = pullRequestMerged
			item.SessionID = ""
			item.Session = nil
			item.GitHubSync = githubSync
		} else {
			item.PullRequestURL = pullRequestURL
			item.PullRequestMerged = pullRequestMerged
			item.GitHubSync = githubSync
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist recovered publication success", err)
	}
	if published.PullRequestURL == "" {
		updated, stateErr := l.issueState(current.Number)
		if stateErr != nil {
			return stateErr
		}
		return l.syncGitHub(ctx, updated)
	}
	return nil
}

func (l *Loop) finishPublicationRecoveryFailure(ctx context.Context, current state.Issue, recoveryID string, attempt int, cause error) error {
	provenance := publication.ClassifyFailure(cause, l.now())
	terminal := current.PublicationRecovery != nil && attempt >= current.PublicationRecovery.MaxAttempts
	var mismatch publication.PullRequestMismatchError
	var claimMismatch publication.ClaimMismatchError
	var formatter publication.FormatterError
	if errors.As(cause, &mismatch) || errors.As(cause, &claimMismatch) || (errors.As(cause, &formatter) && formatter.Code == "path_unsafe") {
		terminal = true
	}
	discoveredPRURL, discoveredOpenPRs, inspectedPRs := l.discoverOpenPublicationPullRequests(ctx, current)
	retainLease := current.PullRequestURL != "" || discoveredOpenPRs > 0 || !inspectedPRs
	retryAt := l.now().Add(l.retryDelay(attempt))
	decision, decisionErr := issuedomain.RecordPublicationRecoveryFailure(current.Status, "publication recovery: "+cause.Error(), string(failure.Issue), terminal, retryAt)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide publication recovery failure", decisionErr)
	}
	_, err := l.Store.Update("publication_recovery_attempt_failed", current.Number, current.RunID, map[string]any{
		"recovery_id": recoveryID, "attempt": attempt, "failure": provenance, "terminal": terminal,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recoveryID {
			return fmt.Errorf("Issue #%d publication recovery disappeared", current.Number)
		}
		finishPublicationRecoveryAttempt(item, attempt, issuedomain.PublicationRecoveryAttemptStatusFailed, cause.Error(), l.now())
		item.PublicationFailure = &provenance
		item.LastError = decision.Outcome.LastError
		item.FailureKind = decision.Outcome.FailureKind
		if item.PullRequestURL == "" && discoveredOpenPRs == 1 {
			item.PullRequestURL = discoveredPRURL
		}
		if terminal {
			if issuedomain.DecideLease(decision.Outcome.Transition.To, retainLease || item.PullRequestURL != "", false) == issuedomain.ReleaseLease && item.Lease != nil {
				if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
					return releaseErr
				}
			}
			if err := state.ApplyIssueTransition(item, decision.Outcome.Transition); err != nil {
				return err
			}
			item.GitHubSync = decision.Outcome.GitHubSync
			item.RetryAfter = decision.RetryAt
			item.PublicationRecovery.Status = decision.RecoveryStatus
		} else {
			if err := state.ApplyIssueTransition(item, decision.Outcome.Transition); err != nil {
				return err
			}
			item.GitHubSync = decision.Outcome.GitHubSync
			item.RetryAfter = decision.RetryAt
			item.PublicationRecovery.Status = decision.RecoveryStatus
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery failure", err)
	}
	if !terminal {
		return nil
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) failPublicationRecovery(ctx context.Context, current state.Issue, reason string) error {
	provenance := publication.FailureProvenance{
		Origin: publication.FailureOriginPublisher, Phase: publication.FailurePhasePublication,
		Code: "recovery_validation_failed", Recoverable: false, Reason: reason, FailedAt: l.now(),
	}
	discoveredPRURL, discoveredOpenPRs, inspectedPRs := l.discoverOpenPublicationPullRequests(ctx, current)
	retainLease := current.PullRequestURL != "" || discoveredOpenPRs > 0 || !inspectedPRs
	decision, decisionErr := issuedomain.RefusePublicationRecovery(current.Status, "publication recovery refused: "+reason, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide publication recovery refusal", decisionErr)
	}
	_, err := l.Store.Update("publication_recovery_refused", current.Number, current.RunID, provenance, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if issuedomain.DecideLease(decision.Transition.To, retainLease || item.PullRequestURL != "", false) == issuedomain.ReleaseLease && item.Lease != nil {
			if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
				return releaseErr
			}
		}
		if item.PullRequestURL == "" && discoveredOpenPRs == 1 {
			item.PullRequestURL = discoveredPRURL
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		item.GitHubSync = decision.GitHubSync
		item.PublicationFailure = &provenance
		item.RetryAfter = nil
		if item.PublicationRecovery != nil {
			item.PublicationRecovery.Status = issuedomain.PublicationRecoveryStatusFailed
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery refusal", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) discoverOpenPublicationPullRequests(ctx context.Context, current state.Issue) (string, int, bool) {
	if l.GitHub == nil {
		return "", 0, false
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return "", 0, false
	}
	url := ""
	count := 0
	for _, pr := range remote.PullRequests {
		if pr.MergedAt == nil && strings.EqualFold(pr.State, "open") {
			count++
			if count == 1 {
				url = pr.URL
			} else {
				url = ""
			}
		}
	}
	return url, count, true
}

func finishPublicationRecoveryAttempt(issue *state.Issue, number int, status issuedomain.PublicationRecoveryAttemptStatus, reason string, finished time.Time) {
	for index := len(issue.PublicationRecovery.History) - 1; index >= 0; index-- {
		attempt := &issue.PublicationRecovery.History[index]
		if attempt.Number == number && attempt.Status == issuedomain.PublicationRecoveryAttemptStatusRunning {
			attempt.Status = status
			attempt.Reason = reason
			attempt.FinishedAt = finished
			return
		}
	}
}

func publicationRecoveryAttemptRunning(recovery *state.PublicationRecovery, number int) bool {
	if recovery == nil || number < 1 {
		return false
	}
	for index := len(recovery.History) - 1; index >= 0; index-- {
		attempt := recovery.History[index]
		if attempt.Number == number {
			return attempt.Status == issuedomain.PublicationRecoveryAttemptStatusRunning && attempt.FinishedAt.IsZero()
		}
	}
	return false
}

func validatePublicationRecoveryRemote(cfg config.Config, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection, allowDiscoveredPR bool) error {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("GitHub Issue closed before recovered publication")
	}
	labels := map[string]bool{}
	for _, label := range remote.Issue.Labels {
		labels[strings.ToLower(label)] = true
	}
	if !labels[strings.ToLower(cfg.GitHub.RunningLabel)] || labels[strings.ToLower(cfg.GitHub.FailedLabel)] || labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return fmt.Errorf("GitHub labels changed after publication recovery confirmation")
	}
	for _, label := range append(append([]string(nil), cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("GitHub label %q excludes publication recovery", label)
		}
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) == 0 {
			return nil
		}
		if len(remote.PullRequests) == 1 && allowDiscoveredPR {
			pr := remote.PullRequests[0]
			if strings.EqualFold(pr.State, "open") && pr.MergedAt == nil && pr.HeadRefName == current.Branch && pr.BaseRefName == cfg.Git.BaseBranch && inspection.RemoteBranchExists {
				return nil
			}
		}
		return fmt.Errorf("a Pull Request appeared after publication recovery confirmation")
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("saved Pull Request count changed after publication recovery confirmation")
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRefName != current.Branch || pr.BaseRefName != cfg.Git.BaseBranch || !inspection.RemoteBranchExists {
		return fmt.Errorf("saved Pull Request changed after publication recovery confirmation")
	}
	return nil
}

func (l *Loop) requestResourceCorrection(ctx context.Context, current state.Issue, audit publication.Audit, detail string) error {
	requestID := state.NewID("req")
	question := fmt.Sprintf("Issue #%d has changes outside its declared resource claim. How should the existing worktree be corrected?", current.Number)
	decision, decisionErr := issuedomain.RequestResourceCorrection(current.Status, detail, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide resource correction request", decisionErr)
	}
	_, err := l.Store.Update("resource_claim_mismatch", current.Number, current.RunID, map[string]any{
		"reason": publication.ReasonResourceClaimMismatch, "audit": audit,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		item.GitHubSync = decision.GitHubSync
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		s.PendingRequests[requestID] = &state.Request{
			ID: requestID, IssueNumber: current.Number, Question: question,
			Reason:      "Publication was refused before commit or push because actual_resources is not a subset of declared_resources.",
			Recommended: "revise_diff",
			Options: []state.Option{
				{ID: "revise_diff", Label: "Revise the diff"},
				{ID: "abandon", Label: "Abandon this work"},
			},
			AllowFreeText: true, ResumeStatus: issuedomain.StatusResumePending, Status: issuedomain.RequestStatusPending, CreatedAt: l.now(),
		}
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist resource claim mismatch", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

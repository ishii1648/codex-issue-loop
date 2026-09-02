package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/application/conflict"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) beginConflictRecovery(ctx context.Context, current state.Issue, pr gh.PullRequest) error {
	if l.Conflicts == nil {
		return l.failIssue(ctx, current.Number, failure.Wrap(failure.Issue, "Pull Request conflict recovery", errors.New("conflict recovery manager is unavailable")), true)
	}
	// A newly observed dirty PR starts by fetching the latest base even when a
	// previous recovery record is retained for audit. Only resolving_conflict
	// resumes the exact recorded MERGE_HEAD.
	preparation, err := l.Conflicts.Prepare(ctx, l.Config, current.Worktree, current.Branch, nil)
	if err != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(err, &fatal) {
			return l.failConflictRecovery(ctx, current, err.Error())
		}
		return failure.Wrap(failure.Transient, "prepare Pull Request conflict recovery", err)
	}
	baseUpdates := 1
	attempts := 0
	history := []state.ConflictAttempt(nil)
	verification := []state.ConflictVerification(nil)
	startedAt := l.now()
	if previous := current.ConflictRecovery; previous != nil {
		baseUpdates = previous.BaseUpdates
		history = append(history, previous.History...)
		startedAt = previous.StartedAt
		if startedAt.IsZero() {
			startedAt = l.now()
		}
		if previous.TargetBaseSHA == preparation.TargetBaseSHA {
			attempts = previous.Attempts
			verification = append(verification, previous.Verification...)
		} else {
			baseUpdates++
		}
	}
	recovery := &state.ConflictRecovery{
		PullRequestURL: pr.URL, PreviousBaseSHA: preparation.PreviousBaseSHA,
		TargetBaseSHA: preparation.TargetBaseSHA, OriginalHeadSHA: preparation.OriginalHeadSHA,
		ConflictFiles: append([]string(nil), preparation.ConflictFiles...), AllowedPaths: append([]string(nil), preparation.AllowedPaths...),
		Attempts: attempts, BaseUpdates: baseUpdates, History: history,
		OriginalDiff: preparation.OriginalDiff, BaseCommits: preparation.BaseCommits,
		ConflictContent: preparation.ConflictContent, StartedAt: startedAt, UpdatedAt: l.now(),
		Verification: verification,
	}
	if baseUpdates > l.Config.ConflictRecovery.MaxBaseUpdates {
		updated, persistErr := l.Store.Update("conflict_recovery_base_budget_exceeded", current.Number, current.RunID, map[string]any{
			"target_base_sha": recovery.TargetBaseSHA, "base_updates": baseUpdates,
			"conflict_files": recovery.ConflictFiles,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.ConflictRecovery = recovery
			item.UpdatedAt = l.now()
			return nil
		})
		if persistErr != nil {
			return failure.Wrap(failure.Supervisor, "persist conflict recovery base budget", persistErr)
		}
		persisted := updated.Issues[strconv.Itoa(current.Number)]
		if persisted == nil {
			return failure.Wrap(failure.Supervisor, "reload conflict recovery base budget", fmt.Errorf("Issue #%d is missing after commit", current.Number))
		}
		current = *persisted
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("base update budget exceeded (%d > %d)", baseUpdates, l.Config.ConflictRecovery.MaxBaseUpdates))
	}
	conflictDecision, decisionErr := issuedomain.ResolveConflict(current.Status)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Pull Request conflict recovery", decisionErr)
	}
	_, err = l.Store.Update("conflict_recovery_prepared", current.Number, current.RunID, map[string]any{
		"pull_request_url": pr.URL, "previous_base_sha": recovery.PreviousBaseSHA,
		"target_base_sha": recovery.TargetBaseSHA, "conflict_files": recovery.ConflictFiles,
		"attempts": recovery.Attempts, "base_updates": recovery.BaseUpdates,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if err := state.ApplyIssueTransition(item, conflictDecision); err != nil {
			return err
		}
		item.ConflictRecovery = recovery
		item.PullRequestURL = pr.URL
		item.LastError = "Pull Request conflict recovery prepared"
		item.FailureKind = ""
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict recovery preparation", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	if preparation.Published {
		return l.finishConflictPublication(current, preparation.Commit)
	}
	if preparation.Resolved {
		issue, getErr := l.getIssue(ctx, current.Number)
		if getErr != nil {
			return failure.Wrap(failure.Transient, "refresh Issue for mechanical conflict recovery", getErr)
		}
		return l.publishConflictRecovery(ctx, issue, current, nil)
	}
	return l.processConflictRecovery(ctx, current)
}

func (l *Loop) processConflictRecovery(ctx context.Context, current state.Issue) error {
	if current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, "durable conflict recovery context is missing")
	}
	if l.Conflicts == nil {
		return l.failConflictRecovery(ctx, current, "conflict recovery manager is unavailable")
	}
	preparation, prepareErr := l.Conflicts.Prepare(ctx, l.Config, current.Worktree, current.Branch, current.ConflictRecovery)
	if prepareErr != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(prepareErr, &fatal) {
			return l.failConflictRecovery(ctx, current, prepareErr.Error())
		}
		return failure.Wrap(failure.Transient, "resume Pull Request conflict recovery", prepareErr)
	}
	if preparation.Published {
		return l.finishConflictPublication(current, preparation.Commit)
	}
	if preparation.Resolved && (len(current.ConflictRecovery.ConflictFiles) == 0 || conflictVerificationGreen(current.ConflictRecovery.Verification)) {
		issue, getErr := l.getIssue(ctx, current.Number)
		if getErr != nil {
			return failure.Wrap(failure.Transient, "refresh Issue for resolved conflict publication", getErr)
		}
		return l.publishConflictRecovery(ctx, issue, current, conflictTests(current.ConflictRecovery.Verification))
	}
	if (issuedomain.AttemptBudget{Attempts: current.ConflictRecovery.Attempts, MaxAttempts: l.Config.ConflictRecovery.MaxAttemptsPerBase}).Exhausted() {
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("recovery budget exhausted for base %s after %d attempts", current.ConflictRecovery.TargetBaseSHA, current.ConflictRecovery.Attempts))
	}
	issue, err := l.getIssue(ctx, current.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh Issue for conflict recovery", err)
	}
	runID := state.NewID("conflict")
	previousOwner := state.LeaseOwner{}
	if current.Lease != nil {
		previousOwner = current.Lease.Owner
	}
	attemptNumber := len(current.ConflictRecovery.History) + 1
	attemptTransition, decisionErr := issuedomain.StartConflictAttempt(current.Status)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide conflict recovery attempt", decisionErr)
	}
	payload := map[string]any{
		"attempt": current.ConflictRecovery.Attempts + 1, "base_sha": current.ConflictRecovery.TargetBaseSHA,
		"conflict_files": current.ConflictRecovery.ConflictFiles,
	}
	_, err = l.Store.Update("conflict_recovery_attempt_started", current.Number, runID, payload, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		owner, transferErr := state.TransferIssueLease(item, previousOwner, runID)
		if transferErr != nil {
			return transferErr
		}
		if owner != (state.LeaseOwner{}) {
			payload["lease_owner"] = owner
		}
		if err := state.ApplyIssueTransition(item, attemptTransition); err != nil {
			return err
		}
		item.RunID, item.WorkerPID, item.WorkerPGID = runID, 0, 0
		item.SessionID, item.Session = "", nil
		item.RetryAfter = nil
		item.WorkerIdentity = stateIdentity(l.WorkerIdentity)
		item.ConflictRecovery.Attempts++
		item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
			Number: attemptNumber, BaseSHA: item.ConflictRecovery.TargetBaseSHA, Status: issuedomain.ConflictAttemptStatusRunning,
			ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...), StartedAt: l.now(),
		})
		item.ConflictRecovery.UpdatedAt = l.now()
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict recovery attempt", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, worker.BuildConflictPrompt(current), l.recordWorkerPID(current))
	return l.handleConflictResult(ctx, issue, current, result, runErr)
}

func (l *Loop) handleConflictResult(ctx context.Context, issue gh.Issue, current state.Issue, result worker.Result, runErr error) error {
	var workspaceErr *workerWorkspaceError
	if errors.As(runErr, &workspaceErr) {
		return l.blockWorkerWorkspace(ctx, current, workspaceErr)
	}
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	if current.CapabilityRequirements != nil {
		profile = current.CapabilityRequirements.Profile
	}
	_, err := l.Store.Update("conflict_recovery_worker_completed", current.Number, current.RunID, map[string]any{
		"status": result.Status, "summary": result.Summary, "execution_profile": profile, "tests": result.Tests,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.ExecutionProfile = profile
		if result.Status == "completed" && item.ConflictRecovery != nil {
			item.ConflictRecovery.Verification = make([]state.ConflictVerification, 0, len(result.Tests))
			for _, test := range result.Tests {
				item.ConflictRecovery.Verification = append(item.ConflictRecovery.Verification, state.ConflictVerification{Command: test.Command, Result: test.Result})
			}
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict worker result", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	if runErr != nil {
		return l.scheduleConflictRetry(ctx, current, runErr.Error())
	}
	switch result.Status {
	case "completed":
		if result.Git != nil {
			return l.failConflictRecovery(ctx, current, "conflict worker crossed the publication boundary and returned Git publication data")
		}
		return l.publishConflictRecovery(ctx, issue, current, result.Tests)
	case "needs_input":
		inputDecision, decisionErr := issuedomain.RequestInput(current.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide conflict recovery input request", decisionErr)
		}
		requestID := state.NewID("req")
		_, err := l.Store.Update("input_requested", current.Number, current.RunID, result.Question, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if err := state.ApplyIssueTransition(item, inputDecision.Transition); err != nil {
				return err
			}
			item.GitHubSync, item.UpdatedAt = inputDecision.GitHubSync, l.now()
			finishConflictAttempt(item, issuedomain.ConflictAttemptStatusNeedsInput, result.Summary, l.now())
			q := result.Question
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: current.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				ResumeStatus: issuedomain.StatusResolvingConflict, Status: issuedomain.RequestStatusPending, CreatedAt: l.now(),
			}
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist conflict recovery input request", err)
		}
		updated, stateErr := l.issueState(current.Number)
		if stateErr != nil {
			return failure.Wrap(failure.Supervisor, "reload conflict recovery input request", stateErr)
		}
		return l.syncGitHub(ctx, updated)
	case "retryable_failure":
		reason := result.Summary
		if result.Retry != nil && result.Retry.Reason != "" {
			reason = result.Retry.Reason
		}
		return l.scheduleConflictRetry(ctx, current, reason)
	case "blocked":
		return l.failConflictRecovery(ctx, current, "worker reported a non-recoverable conflict: "+result.Summary)
	default:
		return l.scheduleConflictRetry(ctx, current, "conflict worker returned an unknown status")
	}
}

func (l *Loop) publishConflictRecovery(ctx context.Context, issue gh.Issue, current state.Issue, tests []worker.Test) error {
	if l.Conflicts == nil || current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, "conflict recovery publisher is unavailable")
	}
	l.publicationMu.Lock()
	published, err := l.Conflicts.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, *current.ConflictRecovery, tests)
	l.publicationMu.Unlock()
	if err != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(err, &fatal) {
			return l.failConflictRecovery(ctx, current, err.Error())
		}
		return l.scheduleConflictRetry(ctx, current, "publish resolved conflict: "+err.Error())
	}
	return l.finishConflictPublication(current, published.Commit)
}

func (l *Loop) finishConflictPublication(current state.Issue, commit string) error {
	retryAt := l.now().Add(l.Config.Queue.PollInterval.Duration)
	checksDecision, decisionErr := issuedomain.AwaitChecks(current.Status)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide post-conflict check wait", decisionErr)
	}
	_, err := l.Store.Update("conflict_recovery_published", current.Number, current.RunID, map[string]any{
		"pull_request_url": current.PullRequestURL, "commit": commit,
		"target_base_sha": current.ConflictRecovery.TargetBaseSHA,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if err := state.ApplyIssueTransition(item, checksDecision.Transition); err != nil {
			return err
		}
		item.LastError = ""
		item.FailureKind = ""
		item.RetryAfter = &retryAt
		item.ConflictRecovery.LastReason = "published; waiting for CI revalidation"
		item.ConflictRecovery.UpdatedAt = l.now()
		finishConflictAttempt(item, issuedomain.ConflictAttemptStatusCompleted, "published as "+commit, l.now())
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist conflict recovery publication", err)
}

func (l *Loop) scheduleConflictRetry(ctx context.Context, current state.Issue, reason string) error {
	if current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, reason)
	}
	hasRunningAttempt := len(current.ConflictRecovery.History) > 0 && current.ConflictRecovery.History[len(current.ConflictRecovery.History)-1].Status == issuedomain.ConflictAttemptStatusRunning
	budget := issuedomain.ConflictRetryBudget{
		Attempts: current.ConflictRecovery.Attempts, MaxAttempts: l.Config.ConflictRecovery.MaxAttemptsPerBase,
		HasRunningAttempt: hasRunningAttempt,
	}
	effectiveAttempts := budget.EffectiveAttempts()
	if !budget.Allowed() {
		if !hasRunningAttempt {
			updated, persistErr := l.Store.Update("conflict_recovery_budget_consumed", current.Number, current.RunID, map[string]any{"reason": reason, "attempts": effectiveAttempts}, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(current.Number)]
				item.ConflictRecovery.Attempts = effectiveAttempts
				item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
					Number: len(item.ConflictRecovery.History) + 1, BaseSHA: item.ConflictRecovery.TargetBaseSHA,
					Status: issuedomain.ConflictAttemptStatusRetryableFailure, Reason: reason, StartedAt: l.now(), FinishedAt: l.now(),
					ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...),
				})
				return nil
			})
			if persistErr != nil {
				return failure.Wrap(failure.Supervisor, "persist conflict recovery exhausted budget", persistErr)
			}
			persisted := updated.Issues[strconv.Itoa(current.Number)]
			if persisted == nil {
				return failure.Wrap(failure.Supervisor, "reload conflict recovery exhausted budget", fmt.Errorf("Issue #%d is missing after commit", current.Number))
			}
			current = *persisted
		}
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("%s; recovery budget exhausted for base %s after %d attempts", reason, current.ConflictRecovery.TargetBaseSHA, effectiveAttempts))
	}
	delay := l.retryDelay(effectiveAttempts)
	retryAt := l.now().Add(delay)
	retryTransition, decisionErr := issuedomain.ScheduleConflictRetry(current.Status)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide conflict recovery retry", decisionErr)
	}
	_, err := l.Store.Update("conflict_recovery_retry_scheduled", current.Number, current.RunID, map[string]any{
		"reason": reason, "base_sha": current.ConflictRecovery.TargetBaseSHA,
		"attempts": current.ConflictRecovery.Attempts, "retry_at": retryAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if err := state.ApplyIssueTransition(item, retryTransition); err != nil {
			return err
		}
		item.LastError, item.RetryAfter = reason, &retryAt
		item.FailureKind = string(failure.Transient)
		if !hasRunningAttempt {
			item.ConflictRecovery.Attempts = effectiveAttempts
			item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
				Number: len(item.ConflictRecovery.History) + 1, BaseSHA: item.ConflictRecovery.TargetBaseSHA,
				Status: issuedomain.ConflictAttemptStatusRetryableFailure, Reason: reason, StartedAt: l.now(), FinishedAt: l.now(),
				ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...),
			})
		}
		item.ConflictRecovery.LastReason = reason
		item.ConflictRecovery.UpdatedAt = l.now()
		finishConflictAttempt(item, issuedomain.ConflictAttemptStatusRetryableFailure, reason, l.now())
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist conflict recovery retry", err)
}

func (l *Loop) failConflictRecovery(ctx context.Context, current state.Issue, reason string) error {
	if current.ConflictRecovery != nil {
		updated, persistErr := l.Store.Update("conflict_recovery_exhausted", current.Number, current.RunID, map[string]any{
			"reason": reason, "attempts": current.ConflictRecovery.Attempts,
			"base_updates": current.ConflictRecovery.BaseUpdates, "conflict_files": current.ConflictRecovery.ConflictFiles,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.ConflictRecovery.LastReason = reason
			item.ConflictRecovery.UpdatedAt = l.now()
			finishConflictAttempt(item, issuedomain.ConflictAttemptStatusBlocked, reason, l.now())
			return nil
		})
		if persistErr != nil {
			return failure.Wrap(failure.Supervisor, "persist conflict recovery failure", persistErr)
		}
		persisted := updated.Issues[strconv.Itoa(current.Number)]
		if persisted == nil {
			return failure.Wrap(failure.Supervisor, "reload conflict recovery failure", fmt.Errorf("Issue #%d is missing after commit", current.Number))
		}
		current = *persisted
	}
	detail := conflictFailureDetail(l.Config.RepoPath, current, reason)
	return l.failIssue(ctx, current.Number, failure.Wrap(failure.Issue, "Pull Request conflict recovery", errors.New(detail)), true)
}

func finishConflictAttempt(item *state.Issue, status issuedomain.ConflictAttemptStatus, reason string, now time.Time) {
	if item == nil || item.ConflictRecovery == nil || len(item.ConflictRecovery.History) == 0 {
		return
	}
	attempt := &item.ConflictRecovery.History[len(item.ConflictRecovery.History)-1]
	if attempt.Status == issuedomain.ConflictAttemptStatusRunning {
		attempt.Status, attempt.Reason, attempt.FinishedAt = status, reason, now
	}
}

func conflictFailureDetail(repoPath string, current state.Issue, reason string) string {
	recovery := current.ConflictRecovery
	if recovery == nil {
		return fmt.Sprintf("%s. Recommended recovery: inspect agent-loop issue plan --repo %q --issue %d and select retry-stage after repairing the worktree.", reason, repoPath, current.Number)
	}
	baseHistory := make([]string, 0, len(recovery.History))
	for _, attempt := range recovery.History {
		baseHistory = append(baseHistory, attempt.BaseSHA)
	}
	baseHistory = append(baseHistory, recovery.TargetBaseSHA)
	return fmt.Sprintf("%s. Attempts: %d; base SHA history: %s; conflict files: %s; last reason: %s. Recommended recovery: inspect the saved worktree and run agent-loop issue resolve --repo %q --issue %d --action retry-stage.",
		reason, recovery.Attempts, strings.Join(uniqueStrings(baseHistory), ", "), strings.Join(recovery.ConflictFiles, ", "), recovery.LastReason, repoPath, current.Number)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func conflictTests(values []state.ConflictVerification) []worker.Test {
	result := make([]worker.Test, 0, len(values))
	for _, value := range values {
		result = append(result, worker.Test{Command: value.Command, Result: value.Result})
	}
	return result
}

func conflictVerificationGreen(values []state.ConflictVerification) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		result := strings.ToLower(strings.TrimSpace(value.Result))
		green := strings.TrimSpace(value.Command) != "" && (result == "ok" || strings.HasPrefix(result, "ok ") ||
			strings.HasPrefix(result, "pass") || strings.HasPrefix(result, "success") ||
			strings.HasPrefix(result, "green") || strings.Contains(result, "exit 0"))
		if !green {
			return false
		}
	}
	return true
}

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) processPullRequest(ctx context.Context, current state.Issue) error {
	remote, err := l.inspectIssue(ctx, current)
	if err != nil {
		return failure.Wrap(failure.Transient, "inspect Pull Request lifecycle", err)
	}
	var selected *gh.PullRequest
	for index := range remote.PullRequests {
		candidate := &remote.PullRequests[index]
		if candidate.MergedAt != nil && (candidate.URL == current.PullRequestURL || current.PullRequestURL == "") {
			return l.completeIssue(ctx, current, *candidate, nil)
		}
	}
	if len(remote.PullRequests) > 1 {
		return l.blockPullRequestLifecycle(ctx, current, remote.PullRequests[0].URL, "multiple Pull Requests exist for the saved branch")
	}
	for index := range remote.PullRequests {
		candidate := &remote.PullRequests[index]
		if candidate.URL == current.PullRequestURL || (current.PullRequestURL == "" && candidate.HeadRefName == current.Branch) {
			selected = candidate
			break
		}
	}
	if selected == nil {
		inspection, inspectErr := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "inspect Pull Request worktree", inspectErr)
		}
		if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
			return l.blockPullRequestLifecycle(ctx, current, current.PullRequestURL, "saved Pull Request branch or worktree disappeared")
		}
		return l.schedulePullRequestPoll(current, "Pull Request is not visible yet")
	}
	if selected.MergedAt != nil {
		return l.completeIssue(ctx, current, *selected, nil)
	}
	if selected.HeadSHA != "" && current.HeadSHA != selected.HeadSHA {
		_, err := l.Store.Update("pull_request_head_observed", current.Number, current.RunID, map[string]string{"head_sha": selected.HeadSHA}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.HeadSHA = selected.HeadSHA
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Pull Request head", err)
		}
		current.HeadSHA = selected.HeadSHA
	}
	if current.ReviewDecision != selected.ReviewDecision {
		_, err := l.Store.Update("pull_request_review_observed", current.Number, current.RunID, map[string]string{"review_decision": selected.ReviewDecision}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.ReviewDecision = selected.ReviewDecision
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Pull Request review decision", err)
		}
		current.ReviewDecision = selected.ReviewDecision
	}
	if !strings.EqualFold(selected.State, "open") {
		return l.blockPullRequestLifecycle(ctx, current, selected.URL, "Pull Request was closed without merge")
	}
	if selected.ReviewDecision == "CHANGES_REQUESTED" || selected.ReviewDecision == "REVIEW_REQUIRED" {
		return l.schedulePullRequestPoll(current, "waiting for required review to pass")
	}
	inspection, inspectErr := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if inspectErr != nil {
		return failure.Wrap(failure.Transient, "inspect Pull Request worktree", inspectErr)
	}
	if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
		return l.blockPullRequestLifecycle(ctx, current, selected.URL, "open Pull Request branch or worktree disappeared")
	}
	if current.Status == issuedomain.StatusAwaitingMerge && !l.Config.Completion.AutoMerge {
		return l.schedulePullRequestPoll(current, "waiting for Pull Request merge")
	}
	if l.Config.Completion.AutoMerge {
		// Merge-state mutations take precedence over checks: checks for a behind
		// head are stale, while a dirty head may never receive a check run at all.
		// Every mutation returns immediately so one poll cannot both update or
		// recover a branch and also ready or merge the Pull Request.
		switch strings.ToLower(selected.MergeStateStatus) {
		case "behind":
			if err := l.GitHub.UpdatePullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "update Pull Request branch", err)
			}
			return l.schedulePullRequestPoll(current, "Pull Request branch updated; waiting for checks")
		case "dirty":
			return l.beginConflictRecovery(ctx, current, *selected)
		case "unknown", "unstable":
			return l.schedulePullRequestPoll(current, "waiting for stable Pull Request merge state")
		}
	}
	switch selected.ChecksStatus {
	case "pending", "":
		return l.schedulePullRequestPoll(current, "waiting for Pull Request checks")
	case "failure":
		return l.schedulePullRequestChecksRetry(ctx, current, *selected)
	case "success":
		mergeDecision, decisionErr := issuedomain.AwaitMerge(current.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide Pull Request merge wait", decisionErr)
		}
		if selected.IsDraft {
			if err := l.GitHub.ReadyPullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "mark Pull Request ready", err)
			}
		}
		if l.Config.Completion.AutoMerge {
			if err := l.GitHub.MergePullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "merge Pull Request", err)
			}
		}
		_, err := l.Store.Update("pull_request_ready", current.Number, current.RunID, map[string]any{
			"pull_request_url": selected.URL, "auto_merge": l.Config.Completion.AutoMerge,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if err := state.ApplyIssueTransition(item, mergeDecision); err != nil {
				return err
			}
			item.PullRequestURL = selected.URL
			item.PullRequestNumber = selected.Number
			item.LastError = ""
			item.FailureKind = ""
			interval := l.Config.Queue.PollInterval.Duration
			if l.Config.Webhook.Enabled() {
				interval = l.Config.Webhook.SafetySweepInterval.Duration
			}
			item.RetryAfter = deadlinePointer(l.now().Add(interval))
			item.UpdatedAt = l.now()
			return nil
		})
		return failure.Wrap(failure.Supervisor, "persist Pull Request ready state", err)
	default:
		return failure.Wrap(failure.Issue, "inspect Pull Request checks", fmt.Errorf("unknown check status %q", selected.ChecksStatus))
	}
}

func (l *Loop) schedulePullRequestChecksRetry(ctx context.Context, issue state.Issue, pr gh.PullRequest) error {
	reason := "Pull Request checks failed: " + pr.URL
	if l.retryBudget(issue).Decide() != issuedomain.RetryExhausted {
		return l.scheduleRetry(ctx, issue, reason)
	}
	return l.failPullRequestChecks(ctx, issue, pr, reason)
}

func (l *Loop) failPullRequestChecks(ctx context.Context, current state.Issue, pr gh.PullRequest, reason string) error {
	cause := failure.Wrap(failure.Issue, "worker retry limit reached", errors.New(reason))
	if current.PullRequestURL == "" || current.Branch == "" || pr.URL != current.PullRequestURL ||
		pr.HeadRefName != current.Branch || pr.HeadSHA == "" {
		return l.failIssue(ctx, current.Number, cause, false)
	}
	now := l.now()
	decision, decisionErr := issuedomain.ExhaustPullRequestChecks(current.Status, cause.Error(), string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Pull Request checks exhaustion", decisionErr)
	}
	identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
	_, err := l.Store.Update("pull_request_checks_retry_exhausted", current.Number, current.RunID, map[string]any{
		"pull_request_url": pr.URL, "pull_request_number": pr.Number, "head_sha": pr.HeadSHA,
		"checks_status": pr.ChecksStatus, "attempts": current.Attempts, "continuations": current.Continuations,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID || item.PullRequestURL != pr.URL || item.Branch != pr.HeadRefName {
			return fmt.Errorf("Issue #%d changed while recording Pull Request checks exhaustion", current.Number)
		}
		if state.OwnsActiveExecution(s, current.Number, identity) {
			if err := state.CaptureContinuation(s, current.Number, identity, state.NewID("checkpoint"), now); err != nil {
				return err
			}
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		if err := state.SetEffect(s, item.Number, item.RunID, decision.Effect, now); err != nil {
			return err
		}
		item.RetryAfter = nil
		item.SessionID = ""
		item.Session = nil
		item.HeadSHA = pr.HeadSHA
		item.PullRequestNumber = pr.Number
		if item.Continuation == nil {
			return fmt.Errorf("Issue #%d checks exhaustion did not capture a continuation checkpoint", current.Number)
		}
		item.Continuation.HeadSHA = pr.HeadSHA
		item.Continuation.PullRequestURL = pr.URL
		item.Continuation.PullRequestNumber = pr.Number
		item.Continuation.Evidence = &state.ContinuationEvidence{
			Origin: "pull_request_lifecycle", Phase: "required_checks",
			Code: "checks_retry_exhausted", Status: "failure", ObservedAt: now,
		}
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Pull Request checks exhaustion", err)
	}
	updated, stateErr := l.issueState(current.Number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) blockPullRequestLifecycle(ctx context.Context, current state.Issue, prURL, reason string) error {
	cause := "Pull Request lifecycle: " + reason
	decision, decisionErr := issuedomain.BlockPullRequestLifecycle(current.Status, cause, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Pull Request lifecycle block", decisionErr)
	}
	_, err := l.Store.Update("pull_request_lifecycle_blocked", current.Number, current.RunID, map[string]any{
		"reason": reason, "pull_request_url": prURL,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.PullRequestURL = prURL
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		if err := state.SetEffect(s, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		item.RetryAfter = nil
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Pull Request lifecycle attention", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) schedulePullRequestPoll(current state.Issue, reason string) error {
	interval := l.Config.Queue.PollInterval.Duration
	if l.Config.Webhook.Enabled() {
		interval = l.Config.Webhook.SafetySweepInterval.Duration
	}
	retryAt := l.now().Add(interval)
	_, err := l.Store.Update("pull_request_poll_scheduled", current.Number, current.RunID, map[string]any{
		"status": current.Status, "reason": reason, "retry_at": retryAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.LastError = reason
		item.RetryAfter = &retryAt
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist Pull Request poll", err)
}

func (l *Loop) completeIssue(ctx context.Context, current state.Issue, pullRequest gh.PullRequest, payload any) error {
	decision, decisionErr := issuedomain.Complete(current.Status, pullRequest.URL)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Issue completion", decisionErr)
	}
	identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
	_, err := l.Store.Update("issue_completed", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if state.OwnsActiveExecution(s, current.Number, identity) {
			if err := state.ReleaseExecution(s, current.Number, identity); err != nil {
				return err
			}
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.PullRequestURL, item.LastError, item.SessionID = decision.PullRequestURL, "", ""
		if pullRequest.Number > 0 {
			item.PullRequestNumber = pullRequest.Number
		}
		if pullRequest.HeadSHA != "" {
			item.HeadSHA = pullRequest.HeadSHA
		}
		item.Session = nil
		item.PullRequestMerged = decision.PullRequestMerged
		item.FailureKind = ""
		if err := state.SetEffect(s, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		item.RetryAfter, item.UpdatedAt = nil, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Issue completion", err)
	}
	updated, stateErr := l.issueState(current.Number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
}

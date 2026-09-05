package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) suspendWorker(ctx context.Context, number int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "worker reported an unresolved environment prerequisite"
	}
	current, err := l.issueState(number)
	if err != nil {
		return err
	}
	cause := failure.Wrap(failure.Issue, "worker blocked", errors.New(reason))
	decision, decisionErr := issuedomain.SuspendWorker(current.Status, cause.Error(), string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide worker environment block", decisionErr)
	}
	checkpointID, suspendedAt := state.NewID("checkpoint"), l.now()
	identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
	worktreeSHA256, digestErr := l.continuationWorktreeDigest(ctx, current)
	_, err = l.Store.Update("issue_blocked", number, current.RunID, map[string]any{
		"error": cause.Error(), "failure_kind": string(failure.Issue), "blocked_origin": "worker", "blocked_kind": "environment",
		"checkpoint_id": checkpointID, "released_execution": identity, "suspended_at": suspendedAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil || item.RunID != current.RunID {
			return fmt.Errorf("Issue #%d run changed while recording worker block", number)
		}
		if err := state.CaptureContinuation(s, number, identity, checkpointID, suspendedAt); err != nil {
			return err
		}
		item.Continuation.WorktreeSHA256 = worktreeSHA256
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError, item.FailureKind = decision.LastError, decision.FailureKind
		if err := state.SetEffect(s, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		status, recoverability := issuedomain.SuspensionActive, issuedomain.RecoverabilityOperator
		actions, missing := []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionResume}, []string(nil)
		if digestErr != nil || worktreeSHA256 == "" {
			status, recoverability = issuedomain.SuspensionQuarantined, issuedomain.RecoverabilityAmbiguous
			actions, missing = []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}, []string{"worktree_sha256"}
		}
		item.Suspension = &state.Suspension{ID: state.NewID("suspension"), Origin: "worker", Status: status,
			ReasonCode: "environment", Recoverability: recoverability, Reason: reason, MissingEvidence: missing,
			AllowedActions: actions, CheckpointID: item.Continuation.ID, SuspendedAt: l.now()}
		item.RetryAfter, item.UpdatedAt = nil, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker environment block", err)
	}
	updated, err := l.issueState(number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) processPublicationCheckpoint(ctx context.Context, current state.Issue) error {
	checkpoint := current.Continuation
	snapshot, snapshotErr := l.Store.Load()
	identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
	if checkpoint == nil || checkpoint.Stage != issuedomain.ContinuationStagePublish || checkpoint.Summary == "" || checkpoint.ResultSHA256 == "" ||
		current.Status != issuedomain.StatusLaunching || current.LaunchSource != issuedomain.StatusResumePending || snapshotErr != nil || !state.OwnsActiveExecution(&snapshot, current.Number, identity) ||
		l.Publisher == nil || l.Worktrees == nil {
		return l.failCheckpointStage(ctx, current, "publication checkpoint is incomplete or no longer owns the active execution")
	}
	result, encoded, err := worker.LoadLatestCompletedResult(filepath.Join(l.Store.Dir, "runs", current.RunID))
	if err != nil || result.Summary != checkpoint.Summary || fmt.Sprintf("%x", sha256.Sum256(encoded)) != checkpoint.ResultSHA256 {
		return l.failCheckpointStage(ctx, current, "saved completed worker result differs from the resolved checkpoint")
	}
	inspection, err := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if err != nil || !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || inspection.Branch != current.Branch || inspection.Head == "" {
		return l.failCheckpointStage(ctx, current, "saved publication worktree is no longer valid")
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return failure.Wrap(failure.Transient, "inspect publication checkpoint GitHub state", err)
	}
	if err := validatePublicationCheckpointRemote(current, remote); err != nil {
		return l.failCheckpointStage(ctx, current, err.Error())
	}
	l.publicationMu.Lock()
	published, audit, publishErr := l.Publisher.Publish(ctx, l.Config, remote.Issue, current.Worktree, current.Branch, current.PullRequestURL,
		checkpoint.Summary, checkpoint.BaseSHA)
	l.publicationMu.Unlock()
	_, auditErr := l.Store.Update("publication_checkpoint_audited", current.Number, current.RunID, audit, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.Status != issuedomain.StatusLaunching || !state.OwnsActiveExecution(snapshot, current.Number, identity) ||
			item.Continuation == nil || item.Continuation.ID != checkpoint.ID || item.Continuation.ResultSHA256 != checkpoint.ResultSHA256 {
			return fmt.Errorf("Issue #%d publication checkpoint changed during execution", current.Number)
		}
		auditCopy := audit
		item.PublicationAudit = &auditCopy
		item.UpdatedAt = l.now()
		return nil
	})
	if auditErr != nil {
		return failure.Wrap(failure.Supervisor, "persist publication checkpoint audit", auditErr)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	if publishErr != nil {
		return l.failCheckpointStage(ctx, current, "publication checkpoint failed: "+publishErr.Error())
	}
	result.Git = &published
	if published.PullRequestURL == "" {
		return l.completeIssue(ctx, current, gh.PullRequest{}, result)
	}
	decision, err := issuedomain.AwaitChecks(current.Status)
	if err != nil {
		return failure.Wrap(failure.Issue, "decide publication checkpoint completion", err)
	}
	_, err = l.Store.Update("publication_checkpoint_completed", current.Number, current.RunID, result, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || !state.OwnsActiveExecution(snapshot, current.Number, identity) {
			return fmt.Errorf("Issue #%d publication checkpoint execution changed", current.Number)
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.PullRequestURL = published.PullRequestURL
		item.PullRequestNumber = pullRequestNumber(published.PullRequestURL)
		item.LastError, item.FailureKind, item.RetryAfter = "", "", nil
		if err := state.SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectNone, l.now()); err != nil {
			return err
		}
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist publication checkpoint completion", err)
}

func validatePublicationCheckpointRemote(current state.Issue, remote gh.RemoteState) error {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("GitHub Issue is no longer open")
	}
	open := make([]gh.PullRequest, 0, len(remote.PullRequests))
	for _, pullRequest := range remote.PullRequests {
		if strings.EqualFold(pullRequest.State, "open") && pullRequest.MergedAt == nil {
			open = append(open, pullRequest)
		}
	}
	if current.PullRequestURL == "" {
		if len(open) != 0 {
			return fmt.Errorf("a Pull Request appeared after publication checkpoint resolution")
		}
		return nil
	}
	if len(open) != 1 || open[0].URL != current.PullRequestURL || open[0].HeadRefName != current.Branch {
		return fmt.Errorf("saved Pull Request differs from the publication checkpoint")
	}
	return nil
}

func (l *Loop) failCheckpointStage(ctx context.Context, current state.Issue, reason string) error {
	decision, err := issuedomain.Fail(current.Status, reason, string(failure.Issue), false)
	if err != nil {
		return failure.Wrap(failure.Issue, "decide checkpoint stage failure", err)
	}
	worktreeSHA256, _ := l.continuationWorktreeDigest(ctx, current)
	_, err = l.Store.Update("continuation_stage_failed", current.Number, current.RunID, map[string]string{"reason": reason}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
		if item == nil || !state.OwnsActiveExecution(snapshot, current.Number, identity) {
			return fmt.Errorf("Issue #%d continuation stage changed", current.Number)
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		if item.Continuation != nil {
			item.Continuation.WorktreeSHA256 = worktreeSHA256
		}
		item.LastError, item.FailureKind, item.RetryAfter = decision.LastError, decision.FailureKind, nil
		if err := state.SetEffect(snapshot, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		item.WorkerPID, item.WorkerPGID = 0, 0
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist continuation stage failure", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) continuationWorktreeDigest(ctx context.Context, current state.Issue) (string, error) {
	if l.Worktrees == nil || strings.TrimSpace(current.Worktree) == "" {
		return "", fmt.Errorf("continuation worktree is unavailable")
	}
	digest, err := l.Worktrees.ContentDigest(ctx, current.Worktree)
	if err != nil {
		return "", fmt.Errorf("fingerprint continuation worktree: %w", err)
	}
	if strings.TrimSpace(digest) == "" {
		return "", fmt.Errorf("continuation worktree digest is empty")
	}
	return digest, nil
}

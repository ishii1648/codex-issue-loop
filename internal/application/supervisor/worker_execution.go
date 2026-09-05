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
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

type launchValidationError struct {
	cause    error
	terminal bool
}

func (e *launchValidationError) Error() string { return e.cause.Error() }

func (e *launchValidationError) Unwrap() error { return e.cause }

func (l *Loop) claimAndRun(ctx context.Context, issue gh.Issue, runID string) error {
	if err := l.GitHub.Claim(ctx, l.Config, issue, runID); err != nil {
		return failure.Wrap(failure.Transient, "claim GitHub Issue", err)
	}
	wt, err := l.Worktrees.Ensure(ctx, l.Config, l.Store.RepoID, issue.Number, issue.Title)
	if err != nil {
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "prepare Issue worktree", err), false)
	}
	launch, err := l.Worktrees.ValidateLaunch(ctx, l.Config, wt.Path, wt.Branch)
	if err != nil || !launch.Valid {
		if err == nil {
			err = fmt.Errorf("worktree validator did not establish a valid launch boundary")
		}
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "validate initial Issue worktree", err), false)
	}
	workspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch,
		RepoID: l.Store.RepoID, Repository: l.Config.GitHub.Repo, RepositoryID: l.Config.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout, CapturedAt: l.now(),
	}
	claimTransition, err := issuedomain.ConfirmClaim(issuedomain.StatusClaiming)
	if err != nil {
		return failure.Wrap(failure.Supervisor, "decide claimed Issue", err)
	}
	_, err = l.Store.Update("issue_claimed", issue.Number, runID, map[string]any{"title": issue.Title}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared while claiming", issue.Number)
		}
		if err := state.ApplyIssueTransition(item, claimTransition); err != nil {
			return err
		}
		item.Worktree, item.Branch, item.Workspace = launch.CanonicalCWD, wt.Branch, &workspace
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist claimed Issue", err)
	}
	return l.runClaimed(ctx, issue, runID, wt, launch, workspace)
}

func (l *Loop) runClaimed(ctx context.Context, issue gh.Issue, runID string, wt worktree.Result, launch worktree.LaunchValidation, workspace state.WorkerWorkspace) error {
	workerTransition, err := issuedomain.StartClaimedWorker(issuedomain.StatusClaimed)
	if err != nil {
		return failure.Wrap(failure.Supervisor, "decide initial worker start", err)
	}
	_, err = l.Store.Update("worker_started", issue.Number, runID, map[string]any{
		"worktree": wt.Path, "branch": wt.Branch, "identity": l.WorkerIdentity,
		"expected_cwd": launch.CanonicalCWD, "workspace_validation": launch,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.LaunchSource = item.Status
		if err := state.ApplyIssueTransition(item, workerTransition); err != nil {
			return err
		}
		if item.Workspace == nil || !item.Workspace.Matches(workspace.Path, workspace.Branch, workspace.RepoID, workspace.Repository, workspace.RepositoryID, workspace.GitCommonDir, workspace.MainCheckout) {
			return fmt.Errorf("Issue #%d claimed workspace provenance changed before worker start", issue.Number)
		}
		item.UpdatedAt = l.now()
		item.WorkerIdentity = stateIdentity(l.WorkerIdentity)
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker start", err)
	}
	current, err := l.issueState(issue.Number)
	if err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = workspace.Path
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, "", l.recordWorkerPID(current))
	return l.handleResult(ctx, issue, current, result, runErr)
}

func (l *Loop) handleResult(ctx context.Context, issue gh.Issue, current state.Issue, result worker.Result, runErr error) error {
	fresh, freshErr := l.issueState(current.Number)
	if freshErr == nil && fresh.RunID == current.RunID && fresh.Generation == current.Generation {
		current = fresh
	}
	var workspaceErr *workerWorkspaceError
	if errors.As(runErr, &workspaceErr) {
		if current.Status == issuedomain.StatusLaunching && !workspaceErr.terminal {
			return l.scheduleRetry(ctx, current, workspaceErr.Error())
		}
		return l.blockWorkerWorkspace(ctx, current, workspaceErr)
	}
	var launchErr *launchValidationError
	if errors.As(runErr, &launchErr) {
		return l.handleLaunchValidationFailure(ctx, current, launchErr)
	}
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	if current.ExecutionProfile == "extended" {
		profile = "extended"
	}
	_, err := l.Store.Update("worker_preflight_completed", issue.Number, current.RunID, map[string]string{"execution_profile": profile}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil || item.RunID != current.RunID || item.Status.TerminalForWebhook() {
			return errWorkerResultSuperseded
		}
		item.ExecutionProfile = profile
		// A fresh worker result supersedes any publisher provenance from an
		// earlier worker attempt. A new publication failure is recorded below
		// only if this completed result reaches that boundary again.
		if result.SessionID != "" {
			item.SessionID = result.SessionID
			backend := result.Identity.Backend
			if backend == "" {
				backend = l.Config.Worker.Backend
			}
			if backend == "" {
				backend = "codex"
			}
			item.Session = &state.WorkerSession{Backend: backend, ID: result.SessionID}
		}
		identity := result.Identity
		if identity.Backend == "" {
			identity = l.WorkerIdentity
		}
		item.WorkerIdentity = stateIdentity(identity)
		item.UpdatedAt = l.now()
		return nil
	})
	if errors.Is(err, errWorkerResultSuperseded) {
		return nil
	}
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker result", err)
	}
	current, err = l.issueState(issue.Number)
	if err != nil {
		return err
	}
	if runErr != nil {
		return l.scheduleRetry(ctx, current, runErr.Error())
	}
	switch result.Status {
	case "completed":
		if l.Publisher != nil {
			snapshot, loadErr := l.Store.Load()
			if loadErr != nil {
				return failure.Wrap(failure.Supervisor, "load active execution before publication", loadErr)
			}
			baseSHA := ""
			if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == current.Number {
				baseSHA = snapshot.ActiveExecution.BaseSHA
			}
			l.publicationMu.Lock()
			published, audit, publishErr := l.Publisher.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, current.PullRequestURL, result.Summary, baseSHA)
			l.publicationMu.Unlock()
			_, auditErr := l.Store.Update("publication_audited", issue.Number, current.RunID, audit, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(issue.Number)]
				auditCopy := audit
				item.PublicationAudit = &auditCopy
				item.UpdatedAt = l.now()
				return nil
			})
			if auditErr != nil {
				return failure.Wrap(failure.Supervisor, "persist publication resource audit", auditErr)
			}
			if publishErr != nil {
				return l.schedulePublicationRetry(ctx, current, fmt.Errorf("publish completed work: %w", publishErr), result.Summary)
			}
			result.Git = &published
		}
		if result.Git == nil {
			return l.scheduleRetry(ctx, current, "completed work has not been published")
		}
		prURL := ""
		if result.Git != nil {
			prURL = result.Git.PullRequestURL
		}
		if prURL == "" {
			return l.completeIssue(ctx, current, gh.PullRequest{}, result)
		}
		checksDecision, decisionErr := issuedomain.AwaitChecks(current.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide Pull Request check wait", decisionErr)
		}
		_, err := l.Store.Update("pull_request_checks_pending", issue.Number, current.RunID, result, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			if err := state.ApplyIssueTransition(item, checksDecision.Transition); err != nil {
				return err
			}
			item.PullRequestURL, item.LastError = prURL, ""
			item.PullRequestNumber = pullRequestNumber(prURL)
			item.PullRequestMerged = false
			item.FailureKind = ""
			if err := state.SetEffect(s, item.Number, item.RunID, issuedomain.EffectNone, l.now()); err != nil {
				return err
			}
			item.RetryAfter, item.UpdatedAt = nil, l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Pull Request check wait", err)
		}
		return nil
	case "needs_input":
		inputDecision, decisionErr := issuedomain.RequestInput(current.Status)
		if decisionErr != nil {
			return failure.Wrap(failure.Issue, "decide input request", decisionErr)
		}
		requestID := state.NewID("req")
		q := result.Question
		checkpointID := state.NewID("checkpoint")
		suspendedAt := l.now()
		identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
		_, err := l.Store.Update("input_requested", issue.Number, current.RunID, map[string]any{
			"question": q, "request_id": requestID, "checkpoint_id": checkpointID,
			"released_execution": identity, "suspended_at": suspendedAt,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			if item == nil || item.RunID != current.RunID ||
				!state.OwnsActiveExecution(s, issue.Number, identity) || item.ConflictRecovery != nil {
				return fmt.Errorf("Issue #%d no longer has an active needs-input worker boundary", issue.Number)
			}
			if err := state.CaptureContinuation(s, issue.Number, identity, checkpointID, suspendedAt); err != nil {
				return err
			}
			item.Continuation.Kind = state.ContinuationKindNeedsInput
			item.Continuation.RequestID = requestID
			if err := state.ApplyIssueTransition(item, inputDecision.Transition); err != nil {
				return err
			}
			item.UpdatedAt = l.now()
			item.FailureKind = ""
			if err := state.SetEffect(s, item.Number, item.RunID, inputDecision.Effect, l.now()); err != nil {
				return err
			}
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: issue.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				RunID: current.RunID, CheckpointID: checkpointID, ReleasedExecution: &identity,
				Status: issuedomain.RequestStatusPending, CreatedAt: l.now(),
			}
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist input request", err)
		}
		updated, stateErr := l.issueState(issue.Number)
		if stateErr != nil {
			return stateErr
		}
		return l.syncGitHub(ctx, updated)
	case "retryable_failure":
		reason := result.Summary
		if result.Retry != nil && result.Retry.Reason != "" {
			reason = result.Retry.Reason
		}
		return l.scheduleRetry(ctx, current, reason)
	case "blocked":
		return l.suspendWorker(ctx, issue.Number, result.Summary)
	default:
		return l.scheduleRetry(ctx, current, "worker returned an unknown status")
	}
}

func (l *Loop) answeredResumeRemoteMismatch(issue gh.Issue, current state.Issue) string {
	labels := labelSet(issue.Labels)
	needsInput := labels[l.Config.GitHub.NeedsInputLabel]
	running := labels[l.Config.GitHub.RunningLabel]
	if !strings.EqualFold(issue.State, "open") {
		return "GitHub Issue is no longer open"
	}
	if needsInput == running {
		return "GitHub needs-input/running labels are ambiguous"
	}
	if labels[l.Config.GitHub.DoneLabel] || labels[l.Config.GitHub.FailedLabel] ||
		hasAnyLabel(labels, append(append([]string{}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
		return "GitHub Issue has an incompatible manual, terminal, or ready label"
	}
	if current.PullRequestURL != "" {
		return "answered continuation unexpectedly has a saved Pull Request"
	}
	return ""
}

func (l *Loop) validateRemoteLaunch(ctx context.Context, current state.Issue, allowNeedsInput bool) (gh.RemoteState, error) {
	remote, err := l.inspectIssue(ctx, current)
	if err != nil {
		return gh.RemoteState{}, &launchValidationError{cause: fmt.Errorf("inspect GitHub launch boundary: %w", err)}
	}
	if current.Status != issuedomain.StatusLaunching {
		return gh.RemoteState{}, &launchValidationError{cause: fmt.Errorf("Issue #%d is not in launch state", current.Number), terminal: true}
	}
	if allowNeedsInput && current.LaunchSource == issuedomain.StatusResumePending && current.Continuation != nil && current.Continuation.Kind == state.ContinuationKindNeedsInput {
		if reason := l.answeredResumeRemoteMismatch(remote.Issue, current); reason != "" {
			return gh.RemoteState{}, &launchValidationError{cause: errors.New(reason), terminal: true}
		}
	} else {
		labels := labelSet(remote.Issue.Labels)
		if !strings.EqualFold(remote.Issue.State, "open") {
			return gh.RemoteState{}, &launchValidationError{cause: errors.New("GitHub Issue is no longer open"), terminal: true}
		}
		if !labels[l.Config.GitHub.RunningLabel] || labels[l.Config.GitHub.NeedsInputLabel] || labels[l.Config.GitHub.DoneLabel] || labels[l.Config.GitHub.FailedLabel] ||
			hasAnyLabel(labels, append(append([]string{}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
			return gh.RemoteState{}, &launchValidationError{cause: errors.New("GitHub labels no longer authorize worker launch"), terminal: true}
		}
	}
	if current.PullRequestURL != "" {
		if len(remote.PullRequests) != 1 || remote.PullRequests[0].URL != current.PullRequestURL ||
			(current.PullRequestNumber > 0 && remote.PullRequests[0].Number != current.PullRequestNumber) ||
			(current.HeadSHA != "" && remote.PullRequests[0].HeadSHA != current.HeadSHA) {
			return gh.RemoteState{}, &launchValidationError{cause: errors.New("saved Pull Request identity changed before worker launch"), terminal: true}
		}
	}
	return remote, nil
}

func (l *Loop) handleLaunchValidationFailure(ctx context.Context, current state.Issue, validationErr error) error {
	var launchErr *launchValidationError
	if errors.As(validationErr, &launchErr) && launchErr.terminal {
		if current.LaunchSource == issuedomain.StatusResumePending && current.Continuation != nil && current.Continuation.Kind == state.ContinuationKindNeedsInput {
			return l.rejectAnsweredContinuation(current, launchErr.Error())
		}
		return l.failIssue(ctx, current.Number, failure.Wrap(failure.Issue, "reject worker launch", launchErr), true)
	}
	if current.ConflictRecovery != nil {
		return l.scheduleConflictRetry(ctx, current, validationErr.Error())
	}
	return l.scheduleRetry(ctx, current, validationErr.Error())
}

func (l *Loop) rejectAnsweredContinuation(current state.Issue, reason string) error {
	now := l.now()
	decision, decisionErr := issuedomain.RejectAnsweredResume(current.Status, "answered continuation rejected: "+reason, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide answered continuation rejection", decisionErr)
	}
	_, err := l.Store.Update("answered_resume_rejected", current.Number, current.RunID, map[string]string{"reason": reason}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
		if item == nil || item.Status != issuedomain.StatusLaunching || item.RunID != current.RunID || !state.OwnsActiveExecution(snapshot, current.Number, identity) {
			return fmt.Errorf("Issue #%d answered continuation changed before rejection", current.Number)
		}
		if item.Continuation == nil || item.Continuation.Kind != state.ContinuationKindNeedsInput {
			return fmt.Errorf("Issue #%d answered continuation is inconsistent", current.Number)
		}
		if err := state.ReleaseExecution(snapshot, current.Number, identity); err != nil {
			return err
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		if err := state.SetEffect(snapshot, item.Number, item.RunID, decision.Effect, now); err != nil {
			return err
		}
		item.Suspension = &state.Suspension{ID: state.NewID("suspension"), Origin: "supervisor", Status: issuedomain.SuspensionActive,
			ReasonCode: "answer_resume", Recoverability: issuedomain.RecoverabilityNone, Reason: reason,
			AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}, CheckpointID: item.Continuation.ID, SuspendedAt: now}
		item.RetryAfter = nil
		item.UpdatedAt = now
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist rejected answered continuation", err)
}

func (l *Loop) blockWorkerWorkspace(ctx context.Context, expected state.Issue, validationErr *workerWorkspaceError) error {
	reason := validationErr.Error()
	decision, decisionErr := issuedomain.RejectWorkerWorkspace(expected.Status, reason, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide worker workspace rejection", decisionErr)
	}
	payload := map[string]any{
		"expected_cwd": validationErr.expected,
		"validation":   validationErr.validation,
		"error":        reason,
		"run_id":       expected.RunID,
	}
	_, err := l.Store.Update("worker_workspace_rejected", expected.Number, expected.RunID, payload, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(expected.Number)]
		if item == nil || item.RunID != expected.RunID {
			return fmt.Errorf("Issue #%d run changed while rejecting worker workspace", expected.Number)
		}
		identity := state.ExecutionIdentity{RunID: expected.RunID, Generation: expected.Generation}
		if state.OwnsActiveExecution(snapshot, expected.Number, identity) {
			if err := state.CaptureContinuation(snapshot, expected.Number, identity, state.NewID("checkpoint"), l.now()); err != nil {
				return err
			}
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		if err := state.SetEffect(snapshot, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.RetryAfter = nil
		checkpointID := ""
		if item.Continuation != nil {
			checkpointID = item.Continuation.ID
		}
		item.Suspension = &state.Suspension{ID: state.NewID("suspension"), Origin: "supervisor", Status: issuedomain.SuspensionActive,
			ReasonCode: "worker_workspace", Recoverability: issuedomain.RecoverabilityNone, Reason: reason,
			AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}, CheckpointID: checkpointID, SuspendedAt: l.now()}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist rejected worker workspace", err)
	}
	updated, err := l.issueState(expected.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) validateWorkerLaunch(ctx context.Context, cfg config.Config, expected state.Issue) (worktree.LaunchValidation, error) {
	validation := worktree.LaunchValidation{ExpectedCWD: cfg.RepoPath, Checks: map[string]bool{}}
	fresh, err := l.issueState(expected.Number)
	if err != nil {
		return validation, &workerWorkspaceError{expected: cfg.RepoPath, validation: validation, cause: err, terminal: true}
	}
	fail := func(cause error) (worktree.LaunchValidation, error) {
		return validation, &workerWorkspaceError{expected: cfg.RepoPath, validation: validation, cause: cause, terminal: true}
	}
	retry := func(cause error) (worktree.LaunchValidation, error) {
		return validation, &workerWorkspaceError{expected: cfg.RepoPath, validation: validation, cause: cause}
	}
	if fresh.RunID == "" || fresh.RunID != expected.RunID {
		return fail(fmt.Errorf("run changed from %q to %q", expected.RunID, fresh.RunID))
	}
	validation.Checks["run_id"] = true
	if fresh.Status != issuedomain.StatusLaunching || fresh.LaunchSource != expected.LaunchSource {
		return fail(fmt.Errorf("launch state changed before spawn"))
	}
	validation.Checks["launch_state"] = true
	if fresh.SessionID != expected.SessionID {
		return fail(fmt.Errorf("session changed before spawn"))
	}
	validation.Checks["session_id"] = true
	if fresh.Worktree == "" || fresh.Worktree != expected.Worktree || fresh.Worktree != cfg.RepoPath {
		return fail(fmt.Errorf("saved worktree path changed before spawn"))
	}
	validation.Checks["saved_path"] = true
	if fresh.Branch == "" || fresh.Branch != expected.Branch {
		return fail(fmt.Errorf("saved worktree branch changed before spawn"))
	}
	validation.Checks["saved_branch_state"] = true
	snapshot, loadErr := l.Store.Load()
	identity := state.ExecutionIdentity{RunID: fresh.RunID, Generation: fresh.Generation}
	if loadErr != nil || fresh.Generation == 0 || fresh.Generation != expected.Generation || !state.OwnsActiveExecution(&snapshot, fresh.Number, identity) {
		return fail(fmt.Errorf("active execution generation changed before spawn"))
	}
	validation.Checks["active_execution_generation"] = true

	local, inspectErr := l.Worktrees.ValidateLaunch(ctx, l.Config, fresh.Worktree, fresh.Branch)
	for name, passed := range local.Checks {
		validation.Checks[name] = passed
	}
	validation.CanonicalCWD = local.CanonicalCWD
	validation.TopLevel = local.TopLevel
	validation.Branch = local.Branch
	validation.CommonDir = local.CommonDir
	validation.MainCheckout = local.MainCheckout
	if inspectErr != nil {
		return retry(inspectErr)
	}
	validation.Valid = local.Valid
	if !validation.Valid {
		return retry(fmt.Errorf("worktree validator did not establish a valid launch boundary"))
	}

	workspace := fresh.Workspace
	if workspace == nil {
		return fail(fmt.Errorf("saved workspace provenance is missing"))
	} else if !workspace.Matches(validation.CanonicalCWD, validation.Branch, l.Store.RepoID, l.Config.GitHub.Repo,
		l.Config.GitHub.RepositoryID, validation.CommonDir, validation.MainCheckout) {
		return fail(fmt.Errorf("saved workspace provenance does not match the launch target"))
	}
	validation.Checks["saved_provenance"] = true

	payload := map[string]any{
		"expected_cwd": validation.CanonicalCWD, "validation": validation,
		"run_id": fresh.RunID, "session_present": fresh.SessionID != "", "execution_identity": identity,
	}
	_, err = l.Store.Update("worker_workspace_validated", fresh.Number, fresh.RunID, payload, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(fresh.Number)]
		if item == nil || item.RunID != fresh.RunID || item.Worktree != fresh.Worktree || item.Branch != fresh.Branch || item.SessionID != fresh.SessionID ||
			item.Generation != fresh.Generation || !state.OwnsActiveExecution(snapshot, fresh.Number, identity) {
			return fmt.Errorf("Issue #%d launch provenance changed during validation", fresh.Number)
		}
		if item.Workspace == nil || *item.Workspace != *workspace {
			return fmt.Errorf("Issue #%d workspace provenance changed during validation", fresh.Number)
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if _, remoteErr := l.validateRemoteLaunch(ctx, fresh, false); remoteErr != nil {
		return validation, remoteErr
	}
	return validation, nil
}

func (l *Loop) recordWorkerPID(expected state.Issue) worker.Started {
	return func(start worker.ProcessStart) error {
		number, runID := expected.Number, expected.RunID
		validation := worktree.LaunchValidation{
			ExpectedCWD: start.ExpectedCWD, CanonicalCWD: start.ActualCWD,
			Checks: map[string]bool{"spawn_cwd": start.ExpectedCWD != "" && start.ActualCWD == start.ExpectedCWD},
		}
		fail := func(cause error) error {
			return &workerWorkspaceError{expected: start.ExpectedCWD, validation: validation, cause: cause, terminal: true}
		}
		if start.PID <= 0 || start.PGID != start.PID {
			return fail(fmt.Errorf("Issue #%d run %s reported invalid worker process identity", number, runID))
		}
		if start.ExpectedCWD == "" || start.ActualCWD != start.ExpectedCWD {
			return fail(fmt.Errorf("Issue #%d run %s spawned with cwd %q, expected %q", number, runID, start.ActualCWD, start.ExpectedCWD))
		}
		_, err := l.Store.Update("worker_process_started", number, runID, start, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(number)]
			if item == nil || item.RunID != runID {
				return fmt.Errorf("Issue #%d run %s is no longer active", number, runID)
			}
			identity := state.ExecutionIdentity{RunID: expected.RunID, Generation: expected.Generation}
			if item.Generation != expected.Generation || !state.OwnsActiveExecution(s, number, identity) {
				return fmt.Errorf("Issue #%d run %s active execution generation changed before process audit", number, runID)
			}
			if item.Workspace == nil || item.Workspace.Path != start.ExpectedCWD {
				return fmt.Errorf("Issue #%d run %s has no matching saved workspace provenance", number, runID)
			}
			transition, transitionErr := issuedomain.ConfirmWorkerStarted(item.Status)
			if transitionErr != nil {
				return transitionErr
			}
			if err := state.ApplyIssueTransition(item, transition); err != nil {
				return err
			}
			item.WorkerPID = start.PID
			// All worker backends start their command with Setpgid=true, making
			// the process PID the process-group ID used for cancellation.
			item.WorkerPGID = start.PGID
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return fail(err)
		}
		return nil
	}
}

func (l *Loop) scheduleRetry(ctx context.Context, issue state.Issue, reason string) error {
	budget := l.retryBudget(issue)
	if budget.Decide() == issuedomain.RetryExhausted {
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "worker retry limit reached", errors.New(reason)), false)
	}
	delay := l.retryDelay(budget.DelayIndex())
	retryAt := l.now().Add(delay)
	decision, decisionErr := issuedomain.ScheduleRetry(issue.Status, reason, retryAt, string(failure.Transient))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Issue retry", decisionErr)
	}
	_, err := l.Store.Update("retry_scheduled", issue.Number, issue.RunID, map[string]any{
		"failure_kind": failure.Transient, "reason": reason, "retry_at": retryAt, "delay": delay.String(),
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError, item.RetryAfter = decision.Reason, &decision.RetryAt
		item.FailureKind = decision.FailureKind
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist Issue retry", err)
}

// schedulePublicationRetry deliberately ignores worker continuation budget.
// A publisher failure did not invalidate the completed worker result, and
// resuming that worker would blur implementation retries with publication
// retries. Before the worker-attempt budget is exhausted the existing retry
// path may start a fresh validation run; at the terminal boundary failIssue
// retains typed recoverable provenance and the completed session for the
// operator-only publication recovery transaction.
func (l *Loop) schedulePublicationRetry(ctx context.Context, issue state.Issue, cause error, summary string) error {
	reason := cause.Error()
	budget := issuedomain.PublicationRetryBudget{Attempts: issue.Attempts, MaxAttempts: l.Config.Queue.MaxAttempts}
	if !budget.Allowed() {
		_, encoded, loadErr := worker.LoadLatestCompletedResult(filepath.Join(l.Store.Dir, "runs", issue.RunID))
		if loadErr != nil {
			return l.failIssueAtStage(ctx, issue.Number, failure.Wrap(failure.Issue, "worker retry limit reached", cause), false,
				issuedomain.ContinuationStagePublish, summary, "")
		}
		return l.failIssueAtStage(ctx, issue.Number, failure.Wrap(failure.Issue, "worker retry limit reached", cause), false,
			issuedomain.ContinuationStagePublish, summary, fmt.Sprintf("%x", sha256.Sum256(encoded)))
	}
	delay := l.retryDelay(budget.DelayIndex())
	retryAt := l.now().Add(delay)
	decision, decisionErr := issuedomain.ScheduleRetry(issue.Status, reason, retryAt, string(failure.Transient))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide publication retry", decisionErr)
	}
	_, err := l.Store.Update("publication_retry_scheduled", issue.Number, issue.RunID, map[string]any{
		"failure_kind": failure.Transient, "reason": reason, "retry_at": retryAt, "delay": delay.String(),
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError, item.RetryAfter = decision.Reason, &decision.RetryAt
		item.FailureKind = decision.FailureKind
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist publication retry", err)
}

func (l *Loop) failIssue(ctx context.Context, number int, cause error, blocked bool) error {
	return l.failIssueAtStage(ctx, number, cause, blocked, issuedomain.ContinuationStageNone, "", "")
}

func (l *Loop) failIssueAtStage(ctx context.Context, number int, cause error, blocked bool, stage issuedomain.ContinuationStage, summary, resultSHA256 string) error {
	current, stateErr := l.issueState(number)
	if stateErr != nil {
		return failure.Wrap(failure.Supervisor, "load Issue before failure transition", stateErr)
	}
	identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
	kind := failure.KindOf(cause)
	decision, decisionErr := issuedomain.Fail(current.Status, cause.Error(), string(kind), blocked)
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide Issue failure", decisionErr)
	}
	worktreeSHA256, _ := l.continuationWorktreeDigest(ctx, current)
	_, err := l.Store.Update("issue_"+decision.Transition.To.String(), number, current.RunID, map[string]string{"error": cause.Error(), "failure_kind": string(kind)}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared before failure transition", number)
		}
		if state.OwnsActiveExecution(s, number, identity) {
			if err := state.CaptureContinuation(s, number, identity, state.NewID("checkpoint"), l.now()); err != nil {
				return err
			}
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		if stage != issuedomain.ContinuationStageNone {
			if item.Continuation == nil {
				return fmt.Errorf("Issue #%d terminal transition did not capture a continuation checkpoint", number)
			}
			item.Continuation.Stage = stage
			item.Continuation.Summary = summary
			item.Continuation.ResultSHA256 = resultSHA256
			evidence := state.ContinuationEvidence{Origin: "runtime", Phase: string(stage), Code: string(kind), Status: decision.Transition.To.String(), ObservedAt: l.now()}
			if stage == issuedomain.ContinuationStagePublish {
				provenance := publication.ClassifyFailure(cause, l.now())
				evidence.Origin, evidence.Phase, evidence.Code = provenance.Origin, provenance.Phase, provenance.Code
			}
			item.Continuation.Evidence = &evidence
		}
		if item.Continuation != nil {
			item.Continuation.WorktreeSHA256 = worktreeSHA256
		}
		item.LastError = decision.LastError
		item.SessionID = ""
		item.Session = nil
		item.FailureKind = decision.FailureKind
		if err := state.SetEffect(s, item.Number, item.RunID, decision.Effect, l.now()); err != nil {
			return err
		}
		item.RetryAfter, item.UpdatedAt = nil, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Issue failure", err)
	}
	updated, stateErr := l.issueState(number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
}

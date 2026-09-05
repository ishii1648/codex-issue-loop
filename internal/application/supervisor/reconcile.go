package supervisor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

var errReconciliationStateChanged = errors.New("durable Issue state changed during reconciliation")

type osProcessInspector struct{}

func (osProcessInspector) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

type reconciliationDecision struct {
	status      issuedomain.Status
	lastError   string
	branch      string
	pullRequest string
	prNumber    int
	headSHA     string
	effect      issuedomain.EffectKind
	retryAt     *time.Time
	workerPID   int
	workerPGID  int
	prMerged    bool
	markRunning bool
	reason      string
}

func (l *Loop) reconcileStartup(ctx context.Context, snapshot state.Snapshot) error {
	if controller, ok := l.Processes.(ProcessGroupController); ok {
		if _, err := StopWorkers(ctx, l.Store, l.Config.Worker.TimeoutGrace.Duration, "worker recovered after supervisor restart", controller); err != nil {
			return fmt.Errorf("reconcile orphan worker processes: %w", err)
		}
		var err error
		snapshot, err = l.Store.Load()
		if err != nil {
			return err
		}
	} else if l.Processes == nil {
		if _, err := StopWorkers(ctx, l.Store, l.Config.Worker.TimeoutGrace.Duration, "worker recovered after supervisor restart", OSProcessGroupController{}); err != nil {
			return fmt.Errorf("reconcile orphan worker processes: %w", err)
		}
		var err error
		snapshot, err = l.Store.Load()
		if err != nil {
			return err
		}
	}
	numbers := make([]int, 0, len(snapshot.Issues))
	for _, item := range snapshot.Issues {
		if !startupRemoteInspectionRequired(snapshot, item, l.now()) {
			continue
		}
		numbers = append(numbers, item.Number)
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		for {
			latest, err := l.Store.Load()
			if err != nil {
				return err
			}
			current := latest.Issues[strconv.Itoa(number)]
			if current == nil || !startupRemoteInspectionRequired(latest, current, l.now()) {
				break
			}
			remote, err := l.inspectReconciliationIssue(ctx, *current)
			if err != nil {
				return fmt.Errorf("reconcile GitHub state for Issue #%d: %w", number, err)
			}
			considered, canceled, cancelErr := l.reconcileNotPlannedCancellation(latest, *current, remote, "startup_reconciliation", nil)
			if cancelErr != nil {
				return fmt.Errorf("cancel not planned Issue #%d during startup reconciliation: %w", number, cancelErr)
			}
			if canceled {
				break
			}
			if terminalReconciliationCandidate(*current) && !terminalPullRequestCandidate(*current) {
				reason := "terminal Issue remains sticky"
				if considered {
					currentState, observation := l.reconciliationInputs(latest, *current, remote, worktree.Inspection{})
					decision, _ := issuedomain.DecideNotPlannedCancellation(currentState, observation)
					reason = decision.Reason
				}
				_, err = l.Store.Update("startup_reconciled", number, current.RunID, map[string]any{
					"previous_status": current.Status, "status": current.Status, "reason": reason,
					"github_state_reason": remote.Issue.StateReason,
				}, func(s *state.Snapshot) error {
					item := s.Issues[strconv.Itoa(number)]
					if !reflect.DeepEqual(item, current) {
						return errReconciliationStateChanged
					}
					item.GitHubStateReason = remote.Issue.StateReason
					item.UpdatedAt = l.now()
					return nil
				})
				if errors.Is(err, errReconciliationStateChanged) {
					continue
				}
				if err != nil {
					return err
				}
				break
			}
			inspection := worktree.Inspection{}
			if current.Worktree != "" {
				inspection, err = l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
				if err != nil {
					return fmt.Errorf("reconcile worktree for Issue #%d: %w", number, err)
				}
			}
			decision := l.decideReconciliation(latest, *current, remote, inspection)
			if decision.workerPID == 0 {
				decision.workerPGID = 0
			}
			lifecycleTransition, transitionErr := issuedomain.ReconcileObservation(current.Status, decision.status)
			if transitionErr != nil {
				return fmt.Errorf("decide startup lifecycle reconciliation for Issue #%d: %w", number, transitionErr)
			}
			predicateReport := startupReconciliationPredicateReport(number, decision)
			if decision.markRunning {
				if err := l.GitHub.MarkRunning(ctx, l.Config, number); err != nil {
					return fmt.Errorf("repair running label for Issue #%d: %w", number, err)
				}
			}
			request := singlePendingRequest(latest, number)
			captureInputContinuation := current.Status == issuedomain.StatusNeedsInput && state.PendingEffect(&latest, current.Number) == nil && current.Continuation == nil &&
				current.PullRequestURL == "" && current.WorkerPID == 0 && current.WorkerPGID == 0 && decision.status == issuedomain.StatusNeedsInput &&
				decision.effect == issuedomain.EffectNone && decision.pullRequest == "" && request != nil &&
				(request.ResumeStatus == issuedomain.StatusUnset || request.ResumeStatus == issuedomain.StatusResumePending) && current.ConflictRecovery == nil &&
				latest.ActiveExecution != nil && latest.ActiveExecution.IssueNumber == current.Number
			checkpointID := ""
			suspendedAt := time.Time{}
			if captureInputContinuation {
				checkpointID = state.NewID("checkpoint")
				suspendedAt = l.now()
			}
			_, err = l.Store.Update("startup_reconciled", number, current.RunID, map[string]any{
				"previous_status":    current.Status,
				"status":             decision.status,
				"reason":             decision.reason,
				"worktree":           inspection,
				"pull_requests":      remote.PullRequests,
				"predicate_report":   predicateReport,
				"checkpoint_id":      checkpointID,
				"checkpoint_created": captureInputContinuation,
			}, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(number)]
				if !reflect.DeepEqual(item, current) {
					return errReconciliationStateChanged
				}
				if captureInputContinuation {
					identity := state.ExecutionIdentity{RunID: latest.ActiveExecution.RunID, Generation: latest.ActiveExecution.Generation}
					if err := state.CaptureContinuation(s, number, identity, checkpointID, suspendedAt); err != nil {
						return err
					}
					latestRequest := s.PendingRequests[request.ID]
					if latestRequest == nil || !reflect.DeepEqual(latestRequest, request) {
						return errReconciliationStateChanged
					}
					item.Continuation.Kind = state.ContinuationKindNeedsInput
					item.Continuation.RequestID = request.ID
					latestRequest.RunID = item.RunID
					latestRequest.CheckpointID = checkpointID
					latestRequest.ReleasedExecution = &identity
					latestRequest.ResumeStatus = issuedomain.StatusUnset
				}
				if err := state.ApplyIssueTransition(item, lifecycleTransition); err != nil {
					return err
				}
				item.LastError = decision.lastError
				item.GitHubStateReason = remote.Issue.StateReason
				item.Branch = decision.branch
				item.PullRequestURL = decision.pullRequest
				item.PullRequestNumber = decision.prNumber
				if item.PullRequestNumber == 0 {
					item.PullRequestNumber = pullRequestNumber(decision.pullRequest)
				}
				if decision.headSHA != "" {
					item.HeadSHA = decision.headSHA
				}
				item.PullRequestMerged = decision.prMerged
				if err := state.SetEffect(s, item.Number, item.RunID, decision.effect, l.now()); err != nil {
					return err
				}
				item.RetryAfter = decision.retryAt
				item.WorkerPID = decision.workerPID
				item.WorkerPGID = decision.workerPGID
				item.UpdatedAt = l.now()
				return nil
			})
			if errors.Is(err, errReconciliationStateChanged) {
				continue
			}
			if err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func startupReconciliationPredicateReport(issueNumber int, decision reconciliationDecision) state.RecoveryPredicateReport {
	report := state.RecoveryPredicateReport{
		SchemaVersion: state.RecoveryPredicateReportSchemaVersion, Operation: "startup-reconciliation",
		IssueNumber: issueNumber, Eligible: true, Predicates: []state.RecoveryPredicate{},
	}
	blocked := strings.HasPrefix(decision.lastError, "startup reconciliation blocked:")
	code := state.RecoveryCodeStartupReconciliation
	source := "shared startup reconciliation decision"
	if blocked {
		reason := strings.ToLower(decision.reason)
		switch {
		case strings.Contains(reason, "worktree") || strings.Contains(reason, "branch"):
			code = state.RecoveryCodeWorkspace
		case strings.Contains(reason, "pull request"):
			code = state.RecoveryCodeGitHubIdentity
		case strings.Contains(reason, "label") || strings.Contains(reason, "github"):
			code = state.RecoveryCodeGitHubLabels
		case strings.Contains(reason, "worker pid"):
			code = state.RecoveryCodeWorkerProcess
		}
		report.AddPredicate(code, "fail", source, "authoritative state safely converges", "startup reconciliation refused the observed boundary", "operator", "inspect the named durable/GitHub/worktree boundary before retrying")
		return report
	}
	report.AddPredicate(code, "pass", source, "authoritative state safely converges", "startup reconciliation decision is safe", "automatic", "no operator action is required")
	return report
}

func startupRemoteInspectionRequired(snapshot state.Snapshot, item *state.Issue, now time.Time) bool {
	if item == nil {
		return false
	}
	if state.PendingEffect(&snapshot, item.Number) != nil {
		return true
	}
	switch item.Status {
	case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusLaunching, issuedomain.StatusRunning, issuedomain.StatusResumePending, issuedomain.StatusAwaitingChecks, issuedomain.StatusAwaitingMerge, issuedomain.StatusResolvingConflict:
		return true
	case issuedomain.StatusRetryWait:
		return item.RetryAfter == nil || !item.RetryAfter.After(now)
	case issuedomain.StatusBlocked, issuedomain.StatusFailed:
		return true
	case issuedomain.StatusNeedsInput:
		return false
	default:
		return false
	}
}

// reconcileTerminalWebhook performs a targeted, typed remote inspection for a
// stable local state. It deliberately accepts only terminal decisions: removal
// of a manual exclusion or failed label can therefore never restart a worker.
// Unsupported/non-authoritative transitions are considered safely inspected
// while preserving the local terminal state.
func (l *Loop) reconcileTerminalWebhook(ctx context.Context, current state.Issue, delivery webhook.Delivery) (bool, error) {
	if current.Status == issuedomain.StatusCanceled {
		return true, nil
	}
	remote, err := l.inspectReconciliationIssue(ctx, current)
	if err != nil {
		return false, fmt.Errorf("inspect terminal Issue #%d for webhook %s: %w", current.Number, delivery.DeliveryID, err)
	}
	return l.applyWebhookReconciliation(ctx, current, delivery, remote, false)
}

// reconcileCollectionExit verifies a sweep-derived ready-collection departure
// with an authoritative targeted read. A normal claim also removes the ready
// label, so an aligned running/needs-input label is not treated as exclusion.
func (l *Loop) reconcileCollectionExit(ctx context.Context, current state.Issue, delivery webhook.Delivery) (bool, error) {
	remote, err := l.inspectReconciliationIssue(ctx, current)
	if err != nil {
		return false, fmt.Errorf("inspect collection exit for Issue #%d from webhook %s: %w", current.Number, delivery.DeliveryID, err)
	}
	if !current.Status.TerminalForWebhook() && expectedActiveCollectionExit(current, remote.Issue, l.Config.GitHub) {
		return false, nil
	}
	return l.applyWebhookReconciliation(ctx, current, delivery, remote, !current.Status.TerminalForWebhook())
}

func (l *Loop) applyWebhookReconciliation(ctx context.Context, current state.Issue, delivery webhook.Delivery, remote gh.RemoteState, forceTerminal bool) (bool, error) {
	latest, err := l.Store.Load()
	if err != nil {
		return false, err
	}
	latestItem := latest.Issues[strconv.Itoa(current.Number)]
	if latestItem == nil || !reflect.DeepEqual(latestItem, &current) {
		return false, nil
	}
	source := "webhook_reconciliation"
	if delivery.Event == "issues" && delivery.Action == "collection_exited" {
		source = "safety_sweep_reconciliation"
	}
	_, canceled, cancelErr := l.reconcileNotPlannedCancellation(latest, current, remote, source, map[string]any{
		"delivery_id": delivery.DeliveryID, "event": delivery.Event, "action": delivery.Action,
	})
	if cancelErr != nil {
		return false, cancelErr
	}
	if canceled {
		return true, nil
	}
	inspection := worktree.Inspection{}
	if current.Worktree != "" {
		inspection, err = l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
		if err != nil {
			return false, fmt.Errorf("inspect terminal worktree for Issue #%d: %w", current.Number, err)
		}
	}
	decision := l.decideReconciliation(latest, current, remote, inspection)
	if forceTerminal && !decision.status.TerminalForWebhook() {
		decision = blockDecision(decision, "GitHub Issue left the configured ready collection")
	}
	if !decision.status.TerminalForWebhook() {
		// The event was read successfully, but the remote state does not carry
		// terminal authority. Preserve manual exclusions and failed/completed
		// states instead of turning a webhook into an implicit resume.
		return true, nil
	}
	if decision.workerPID == 0 {
		decision.workerPGID = 0
	}
	lifecycleTransition, transitionErr := issuedomain.ReconcileObservation(current.Status, decision.status)
	if transitionErr != nil {
		return false, fmt.Errorf("decide webhook lifecycle reconciliation for Issue #%d: %w", current.Number, transitionErr)
	}
	_, err = l.Store.Update("webhook_terminal_reconciled", current.Number, current.RunID, map[string]any{
		"delivery_id": delivery.DeliveryID,
		"event":       delivery.Event,
		"action":      delivery.Action,
		"status":      decision.status,
		"reason":      decision.reason,
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if !reflect.DeepEqual(item, &current) {
			return errReconciliationStateChanged
		}
		if err := state.ApplyIssueTransition(item, lifecycleTransition); err != nil {
			return err
		}
		item.LastError = decision.lastError
		item.GitHubStateReason = remote.Issue.StateReason
		item.Branch = decision.branch
		item.PullRequestURL = decision.pullRequest
		item.PullRequestNumber = decision.prNumber
		if item.PullRequestNumber == 0 {
			item.PullRequestNumber = pullRequestNumber(decision.pullRequest)
		}
		if decision.headSHA != "" {
			item.HeadSHA = decision.headSHA
		}
		item.PullRequestMerged = decision.prMerged
		if err := state.SetEffect(snapshot, item.Number, item.RunID, decision.effect, l.now()); err != nil {
			return err
		}
		item.RetryAfter = decision.retryAt
		item.WorkerPID = decision.workerPID
		item.WorkerPGID = decision.workerPGID
		item.UpdatedAt = l.now()
		return nil
	})
	if errors.Is(err, errReconciliationStateChanged) {
		return false, nil
	}
	return err == nil, err
}

func (l *Loop) reconcileNotPlannedCancellation(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, source string, extra map[string]any) (bool, bool, error) {
	currentState, observation := l.reconciliationInputs(snapshot, current, remote, worktree.Inspection{})
	decision, considered := issuedomain.DecideNotPlannedCancellation(currentState, observation)
	if !considered || decision.Status != issuedomain.StatusCanceled {
		return considered, false, nil
	}
	now := l.now()
	payload := map[string]any{
		"issue_number": current.Number, "run_id": current.RunID, "github_state_reason": remote.Issue.StateReason,
		"previous_status": current.Status, "execution_release_result": "not_present", "canceled_at": now,
		"source": source,
		"pull_request": map[string]any{
			"number": current.PullRequestNumber, "url": current.PullRequestURL, "head_sha": current.HeadSHA, "branch": current.Branch,
		},
	}
	for key, value := range extra {
		payload[key] = value
	}
	_, err := l.Store.Update("issue_canceled", current.Number, current.RunID, payload, func(latest *state.Snapshot) error {
		item := latest.Issues[strconv.Itoa(current.Number)]
		if item == nil || !reflect.DeepEqual(item, &current) {
			return errReconciliationStateChanged
		}
		item.GitHubStateReason = remote.Issue.StateReason
		releaseResult, applyErr := state.ApplyNotPlannedCancellation(latest, current.Number, &current, now)
		if applyErr == nil {
			payload["execution_release_result"] = releaseResult
		}
		return applyErr
	})
	if errors.Is(err, errReconciliationStateChanged) {
		return true, false, nil
	}
	return true, err == nil, err
}

func expectedActiveCollectionExit(current state.Issue, issue gh.Issue, cfg config.GitHub) bool {
	if !strings.EqualFold(issue.State, "open") {
		return false
	}
	labels := labelSet(issue.Labels)
	if hasAnyLabel(labels, cfg.ExcludeLabels) || labels[cfg.DoneLabel] || labels[cfg.FailedLabel] {
		return false
	}
	if cfg.Assignee != "" {
		matched := false
		for _, assignee := range issue.Assignees {
			matched = matched || strings.EqualFold(assignee, cfg.Assignee)
		}
		if !matched {
			return false
		}
	}
	if cfg.Milestone != "" && issue.Milestone != cfg.Milestone {
		return false
	}
	if labels[cfg.RunningLabel] {
		switch current.Status {
		case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusLaunching, issuedomain.StatusRunning, issuedomain.StatusRetryWait, issuedomain.StatusResumePending, issuedomain.StatusAwaitingChecks, issuedomain.StatusAwaitingMerge, issuedomain.StatusResolvingConflict:
			return true
		}
	}
	return labels[cfg.NeedsInputLabel] && (current.Status == issuedomain.StatusNeedsInput || current.Status == issuedomain.StatusResumePending)
}

func (l *Loop) decideReconciliation(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) reconciliationDecision {
	currentState, observation := l.reconciliationInputs(snapshot, current, remote, inspection)
	decision := issuedomain.DecideReconciliation(currentState, observation)
	prNumber, headSHA := reconciledPullRequestIdentity(remote.PullRequests, decision.PullRequest)
	return reconciliationDecision{
		status: decision.Status, lastError: decision.LastError, branch: decision.Branch,
		pullRequest: decision.PullRequest, prNumber: prNumber, headSHA: headSHA, effect: decision.Effect, retryAt: decision.RetryAt,
		workerPID: decision.WorkerPID, workerPGID: decision.WorkerPGID, prMerged: decision.PullRequestMerged,
		markRunning: decision.MarkRunning, reason: decision.Reason,
	}
}

func reconciledPullRequestIdentity(pullRequests []gh.PullRequest, url string) (int, string) {
	for _, pullRequest := range pullRequests {
		if pullRequest.URL == url {
			return pullRequest.Number, pullRequest.HeadSHA
		}
	}
	return 0, ""
}

func (l *Loop) reconciliationInputs(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) (issuedomain.ReconciliationState, issuedomain.ReconciliationObservation) {
	currentState := issuedomain.ReconciliationState{
		Number: current.Number, RunID: current.RunID, Generation: current.Generation,
		Status: current.Status, LastError: current.LastError, Branch: current.Branch,
		PullRequest: current.PullRequestURL, PullRequestNumber: current.PullRequestNumber, HeadSHA: current.HeadSHA,
		Effect: issuedomain.EffectNone, RetryAt: current.RetryAfter,
		WorkerPID: current.WorkerPID, WorkerPGID: current.WorkerPGID, PullRequestMerged: current.PullRequestMerged,
		WorktreeSaved: current.Worktree != "", PendingRequest: pendingRequest(snapshot, current.Number) != nil,
	}
	if active := snapshot.ActiveExecution; active != nil {
		currentState.ActiveExecutionIssueNumber = active.IssueNumber
		currentState.ActiveExecutionRunID = active.RunID
		currentState.ActiveExecutionGeneration = active.Generation
	}
	if effect := state.PendingEffect(&snapshot, current.Number); effect != nil {
		currentState.Effect = effect.Kind
	}
	labels := labelSet(remote.Issue.Labels)
	observation := issuedomain.ReconciliationObservation{
		Now: l.now(), IssueOpen: strings.EqualFold(remote.Issue.State, "open"), IssueClosed: strings.EqualFold(remote.Issue.State, "closed"),
		IssueStateReason: remote.Issue.StateReason,
		Ready:            hasAnyLabel(labels, l.Config.GitHub.ReadyLabels), Running: labels[l.Config.GitHub.RunningLabel],
		NeedsInput: labels[l.Config.GitHub.NeedsInputLabel], Done: labels[l.Config.GitHub.DoneLabel],
		Failed: labels[l.Config.GitHub.FailedLabel], Excluded: hasAnyLabel(labels, l.Config.GitHub.ExcludeLabels),
		OnlyBlockedExclusion: l.hasOnlyBlockedExclusion(labels), ManualExclusion: l.hasManualExclusion(remote.Issue, current),
		DoneMarker:   hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->"),
		FailedMarker: hasComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)),
		Workspace: issuedomain.ReconciliationWorkspace{
			Exists: inspection.Exists, Valid: inspection.Valid, Branch: inspection.Branch,
			LocalBranchExists: inspection.LocalBranchExists, RemoteBranchExists: inspection.RemoteBranchExists,
		},
	}
	if request := pendingRequest(snapshot, current.Number); request != nil {
		observation.PendingRequestMarker = observation.NeedsInput && hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:request:"+request.ID+" -->")
	}
	for _, pr := range remote.PullRequests {
		observation.PullRequests = append(observation.PullRequests, issuedomain.ReconciliationPullRequest{
			Number: pr.Number, URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, HeadSHA: pr.HeadSHA,
			Draft: pr.IsDraft, Merged: pr.MergedAt != nil,
		})
	}
	processes := l.Processes
	if processes == nil {
		processes = osProcessInspector{}
	}
	observation.WorkerAlive = current.WorkerPID > 0 && processes.Alive(current.WorkerPID)
	return currentState, observation
}

func (l *Loop) hasOnlyBlockedExclusion(labels map[string]bool) bool {
	if hasAnyLabel(labels, l.Config.GitHub.ReadyLabels) || labels[l.Config.GitHub.RunningLabel] ||
		labels[l.Config.GitHub.NeedsInputLabel] || labels[l.Config.GitHub.FailedLabel] || labels[l.Config.GitHub.DoneLabel] {
		return false
	}
	blocked := false
	for _, excluded := range l.Config.GitHub.ExcludeLabels {
		if !labels[excluded] {
			continue
		}
		if !strings.EqualFold(excluded, "blocked") {
			return false
		}
		blocked = true
	}
	return blocked
}

// decideTerminalPullRequestReconciliation is shared by startup and periodic
// reconciliation. A terminal Issue only converges from an authoritative merge
// when the single Pull Request returned for the saved branch is exactly the
// Pull Request recorded in durable state. An exclusion label remains sticky
// unless it is the automation-owned blocked label evidenced by our comment.
func (l *Loop) decideTerminalPullRequestReconciliation(current state.Issue, remote gh.RemoteState) (reconciliationDecision, bool) {
	currentState, observation := l.reconciliationInputs(state.Snapshot{}, current, remote, worktree.Inspection{})
	decision, ok := issuedomain.DecideTerminalPullRequestReconciliation(currentState, observation)
	prNumber, headSHA := reconciledPullRequestIdentity(remote.PullRequests, decision.PullRequest)
	return reconciliationDecision{
		status: decision.Status, lastError: decision.LastError, branch: decision.Branch,
		pullRequest: decision.PullRequest, prNumber: prNumber, headSHA: headSHA, effect: decision.Effect, retryAt: decision.RetryAt,
		workerPID: decision.WorkerPID, workerPGID: decision.WorkerPGID, prMerged: decision.PullRequestMerged,
		markRunning: decision.MarkRunning, reason: decision.Reason,
	}, ok
}

func (l *Loop) hasManualExclusion(issue gh.Issue, current state.Issue) bool {
	labels := labelSet(issue.Labels)
	automationBlocked := hasComment(issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number))
	for _, excluded := range l.Config.GitHub.ExcludeLabels {
		if !labels[excluded] {
			continue
		}
		if strings.EqualFold(excluded, "blocked") && automationBlocked {
			continue
		}
		return true
	}
	return false
}

// blockDecision is an adapter only; the lifecycle outcome is owned by the
// domain decision so webhook/startup callers cannot grow a parallel rule.
func blockDecision(decision reconciliationDecision, reason string) reconciliationDecision {
	domainDecision := issuedomain.BlockReconciliation(issuedomain.ReconciliationDecision{
		Status: decision.status, LastError: decision.lastError, Branch: decision.branch,
		PullRequest: decision.pullRequest, Effect: decision.effect, RetryAt: decision.retryAt,
		WorkerPID: decision.workerPID, WorkerPGID: decision.workerPGID, PullRequestMerged: decision.prMerged,
		MarkRunning: decision.markRunning, Reason: decision.reason,
	}, reason)
	decision.status, decision.lastError, decision.effect = domainDecision.Status, domainDecision.LastError, domainDecision.Effect
	decision.retryAt, decision.workerPID, decision.workerPGID, decision.reason = domainDecision.RetryAt, domainDecision.WorkerPID, domainDecision.WorkerPGID, domainDecision.Reason
	return decision
}

func terminalPullRequestCandidate(issue state.Issue) bool {
	return issuedomain.TerminalPullRequestCandidate(issuedomain.ReconciliationState{
		Status: issue.Status, PullRequest: issue.PullRequestURL, PullRequestMerged: issue.PullRequestMerged, Effect: issuedomain.EffectNone,
	})
}

func terminalReconciliationCandidate(issue state.Issue) bool {
	return issuedomain.TerminalReconciliationCandidate(issuedomain.ReconciliationState{Status: issue.Status})
}

func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		set[label] = true
	}
	return set
}

func hasAnyLabel(labels map[string]bool, candidates []string) bool {
	for _, candidate := range candidates {
		if labels[candidate] {
			return true
		}
	}
	return false
}

func hasComment(comments []string, marker string) bool {
	for _, comment := range comments {
		if strings.Contains(comment, marker) {
			return true
		}
	}
	return false
}

func pendingRequest(snapshot state.Snapshot, issueNumber int) *state.Request {
	for _, request := range snapshot.PendingRequests {
		if request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			return request
		}
	}
	return nil
}

func singlePendingRequest(snapshot state.Snapshot, issueNumber int) *state.Request {
	var found *state.Request
	for _, request := range snapshot.PendingRequests {
		if request == nil || request.IssueNumber != issueNumber || request.Status != issuedomain.RequestStatusPending {
			continue
		}
		if found != nil {
			return nil
		}
		copy := *request
		copy.Options = append([]state.Option(nil), request.Options...)
		found = &copy
	}
	return found
}

var _ ProcessInspector = osProcessInspector{}

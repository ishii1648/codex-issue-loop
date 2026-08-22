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

	"github.com/ishii1648/codex-issue-loop/internal/config"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
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
	status       issuedomain.Status
	lastError    string
	branch       string
	pullRequest  string
	githubSync   issuedomain.GitHubSync
	retryAt      *time.Time
	workerPID    int
	workerPGID   int
	prMerged     bool
	markRunning  bool
	reason       string
	blockedCause *state.BlockedCause
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
		if !startupRemoteInspectionRequired(item, l.now()) {
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
			if current == nil || !startupRemoteInspectionRequired(current, l.now()) {
				break
			}
			remote, err := l.inspectIssue(ctx, *current)
			if err != nil {
				return fmt.Errorf("reconcile GitHub state for Issue #%d: %w", number, err)
			}
			inspection := worktree.Inspection{}
			if current.Worktree != "" {
				inspection, err = l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
				if err != nil {
					return fmt.Errorf("reconcile worktree for Issue #%d: %w", number, err)
				}
			}
			evaluated := *current
			legacyProvenance := false
			onlyBlockedExclusion := l.hasOnlyBlockedExclusion(labelSet(remote.Issue.Labels))
			if state.MayHaveLegacyWorkerBlockProvenance(current) {
				if cause, provenanceErr := l.Store.LegacyWorkerBlockProvenance(*current); provenanceErr == nil {
					legacyProvenance = true
					if onlyBlockedExclusion {
						evaluated.BlockedCause = cause
						evaluated.LastError = "worker blocked: " + cause.Reason
					}
				}
			}
			decision := l.decideReconciliation(latest, evaluated, remote, inspection)
			if legacyProvenance && !onlyBlockedExclusion {
				decision = blockDecision(decision, "GitHub labels do not contain only the supervisor-owned blocked label")
			}
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
			parkEnvironmentLease := current.Status == issuedomain.StatusBlocked && current.GitHubSync == issuedomain.GitHubSyncNone && current.Lease != nil && current.ResourcePark == nil &&
				current.PullRequestURL == "" && current.WorkerPID == 0 && current.WorkerPGID == 0 && decision.status == issuedomain.StatusBlocked &&
				decision.githubSync == issuedomain.GitHubSyncNone && decision.pullRequest == "" && resumableWorkerBlock(decision.blockedCause) && onlyBlockedExclusion
			request := singlePendingRequest(latest, number)
			parkInputLease := current.Status == issuedomain.StatusNeedsInput && current.GitHubSync == issuedomain.GitHubSyncNone && current.Lease != nil && current.ResourcePark == nil &&
				current.PullRequestURL == "" && current.WorkerPID == 0 && current.WorkerPGID == 0 && decision.status == issuedomain.StatusNeedsInput &&
				decision.githubSync == issuedomain.GitHubSyncNone && decision.pullRequest == "" && request != nil && request.ResumeStatus == issuedomain.StatusUnset && current.ConflictRecovery == nil
			parkReconciledLease := parkEnvironmentLease || parkInputLease
			parkID := ""
			parkedAt := time.Time{}
			if parkReconciledLease {
				parkID = state.NewID("park")
				parkedAt = l.now()
			}
			_, err = l.Store.Update("startup_reconciled", number, current.RunID, map[string]any{
				"previous_status":       current.Status,
				"status":                decision.status,
				"reason":                decision.reason,
				"worktree":              inspection,
				"pull_requests":         remote.PullRequests,
				"predicate_report":      predicateReport,
				"resource_park_id":      parkID,
				"resource_park_created": parkReconciledLease,
			}, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(number)]
				if !reflect.DeepEqual(item, current) {
					return errReconciliationStateChanged
				}
				if parkReconciledLease {
					if err := state.ParkIssueLease(item, current.Lease.Owner, parkID, parkedAt); err != nil {
						return err
					}
					if parkInputLease {
						latestRequest := s.PendingRequests[request.ID]
						if latestRequest == nil || !reflect.DeepEqual(latestRequest, request) {
							return errReconciliationStateChanged
						}
						owner := item.ResourcePark.OriginalLease.Owner
						item.ResourcePark.Kind = state.ResourceParkKindNeedsInput
						item.ResourcePark.RequestID = request.ID
						latestRequest.RunID = item.RunID
						latestRequest.ResourceParkID = parkID
						latestRequest.ReleasedOwner = &owner
					} else {
						item.ResourcePark.Kind = state.ResourceParkKindEnvironmentBlock
					}
				}
				if issuedomain.DecideLease(decision.status, decision.pullRequest != "", retainsWorkerBoundary(decision.blockedCause)) == issuedomain.ReleaseLease && item.Lease != nil {
					if item.ResourcePark != nil && item.ResourcePark.Status == issuedomain.ResourceParkStatusResuming {
						item.ResourcePark.Status = issuedomain.ResourceParkStatusResumed
					}
					if err := state.ReleaseIssueLease(item, current.Lease.Owner); err != nil {
						return err
					}
				}
				if err := state.ApplyIssueTransition(item, lifecycleTransition); err != nil {
					return err
				}
				item.LastError = decision.lastError
				item.Branch = decision.branch
				item.PullRequestURL = decision.pullRequest
				item.PullRequestNumber = pullRequestNumber(decision.pullRequest)
				item.PullRequestMerged = decision.prMerged
				item.GitHubSync = decision.githubSync
				item.RetryAfter = decision.retryAt
				item.WorkerPID = decision.workerPID
				item.WorkerPGID = decision.workerPGID
				item.BlockedCause = decision.blockedCause
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

func resumableWorkerBlock(cause *state.BlockedCause) bool {
	return cause != nil && cause.Origin == "worker" && cause.Kind == "environment" && cause.Resumable
}

func workspaceSafetyBlock(cause *state.BlockedCause) bool {
	return cause != nil && cause.Origin == "supervisor" && cause.Kind == "worker_workspace" && !cause.Resumable
}

func retainsWorkerBoundary(cause *state.BlockedCause) bool {
	return resumableWorkerBlock(cause) || workspaceSafetyBlock(cause)
}

func startupRemoteInspectionRequired(item *state.Issue, now time.Time) bool {
	if item == nil {
		return false
	}
	if item.GitHubSync != issuedomain.GitHubSyncNone {
		return true
	}
	switch item.Status {
	case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusRunning, issuedomain.StatusAnswerClaimWaiting, issuedomain.StatusResumePending, issuedomain.StatusEnvironmentResumePending, issuedomain.StatusChecksRecovery, issuedomain.StatusAwaitingChecks, issuedomain.StatusAwaitingMerge, issuedomain.StatusResolvingConflict:
		return true
	case issuedomain.StatusRetryWait:
		return item.RetryAfter == nil || !item.RetryAfter.After(now)
	case issuedomain.StatusBlocked:
		return state.MayHaveLegacyWorkerBlockProvenance(item) ||
			(item.Lease != nil && item.ResourcePark == nil && item.PullRequestURL == "" && resumableWorkerBlock(item.BlockedCause))
	case issuedomain.StatusNeedsInput:
		return item.Lease != nil && item.ResourcePark == nil && item.PullRequestURL == "" && item.WorkerPID == 0 && item.WorkerPGID == 0
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
	remote, err := l.inspectIssue(ctx, current)
	if err != nil {
		return false, fmt.Errorf("inspect terminal Issue #%d for webhook %s: %w", current.Number, delivery.DeliveryID, err)
	}
	return l.applyWebhookReconciliation(ctx, current, delivery, remote, false)
}

// reconcileCollectionExit verifies a sweep-derived ready-collection departure
// with an authoritative targeted read. A normal claim also removes the ready
// label, so an aligned running/needs-input label is not treated as exclusion.
func (l *Loop) reconcileCollectionExit(ctx context.Context, current state.Issue, delivery webhook.Delivery) (bool, error) {
	remote, err := l.inspectIssue(ctx, current)
	if err != nil {
		return false, fmt.Errorf("inspect collection exit for Issue #%d from webhook %s: %w", current.Number, delivery.DeliveryID, err)
	}
	if !current.Status.TerminalForWebhook() && expectedActiveCollectionExit(current, remote.Issue, l.Config.GitHub) {
		return false, nil
	}
	return l.applyWebhookReconciliation(ctx, current, delivery, remote, !current.Status.TerminalForWebhook())
}

func (l *Loop) applyWebhookReconciliation(ctx context.Context, current state.Issue, delivery webhook.Delivery, remote gh.RemoteState, forceTerminal bool) (bool, error) {
	inspection := worktree.Inspection{}
	var err error
	if current.Worktree != "" {
		inspection, err = l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
		if err != nil {
			return false, fmt.Errorf("inspect terminal worktree for Issue #%d: %w", current.Number, err)
		}
	}
	decision := l.decideReconciliation(state.Snapshot{}, current, remote, inspection)
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
		if issuedomain.DecideLease(decision.status, decision.pullRequest != "", retainsWorkerBoundary(decision.blockedCause)) == issuedomain.ReleaseLease && item.Lease != nil {
			if err := state.ReleaseIssueLease(item, item.Lease.Owner); err != nil {
				return err
			}
		}
		if err := state.ApplyIssueTransition(item, lifecycleTransition); err != nil {
			return err
		}
		item.LastError = decision.lastError
		item.Branch = decision.branch
		item.PullRequestURL = decision.pullRequest
		item.PullRequestNumber = pullRequestNumber(decision.pullRequest)
		item.PullRequestMerged = decision.prMerged
		item.GitHubSync = decision.githubSync
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
		case issuedomain.StatusClaiming, issuedomain.StatusClaimed, issuedomain.StatusRunning, issuedomain.StatusRetryWait, issuedomain.StatusResumePending, issuedomain.StatusEnvironmentResumePending, issuedomain.StatusChecksRecovery, issuedomain.StatusAwaitingChecks, issuedomain.StatusAwaitingMerge, issuedomain.StatusResolvingConflict:
			return true
		}
	}
	return labels[cfg.NeedsInputLabel] && (current.Status == issuedomain.StatusNeedsInput || current.Status == issuedomain.StatusAnswerClaimWaiting || current.Status == issuedomain.StatusResumePending)
}

func (l *Loop) decideReconciliation(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) reconciliationDecision {
	currentState, observation := l.reconciliationInputs(snapshot, current, remote, inspection)
	decision := issuedomain.DecideReconciliation(currentState, observation)
	return reconciliationDecision{
		status: decision.Status, lastError: decision.LastError, branch: decision.Branch,
		pullRequest: decision.PullRequest, githubSync: decision.GitHubSync, retryAt: decision.RetryAt,
		workerPID: decision.WorkerPID, workerPGID: decision.WorkerPGID, prMerged: decision.PullRequestMerged,
		markRunning: decision.MarkRunning, reason: decision.Reason, blockedCause: current.BlockedCause,
	}
}

func (l *Loop) reconciliationInputs(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) (issuedomain.ReconciliationState, issuedomain.ReconciliationObservation) {
	currentState := issuedomain.ReconciliationState{
		Number: current.Number, Status: current.Status, LastError: current.LastError, Branch: current.Branch,
		PullRequest: current.PullRequestURL, GitHubSync: current.GitHubSync, RetryAt: current.RetryAfter,
		WorkerPID: current.WorkerPID, WorkerPGID: current.WorkerPGID, PullRequestMerged: current.PullRequestMerged,
		WorktreeSaved: current.Worktree != "",
	}
	if current.BlockedCause != nil {
		currentState.BlockedOrigin = current.BlockedCause.Origin
		currentState.BlockedKind = current.BlockedCause.Kind
		currentState.BlockedResumable = current.BlockedCause.Resumable
	}
	if current.PublicationRecovery != nil {
		currentState.PublicationRecoveryStatus = current.PublicationRecovery.Status
	}
	labels := labelSet(remote.Issue.Labels)
	observation := issuedomain.ReconciliationObservation{
		Now: l.now(), IssueOpen: strings.EqualFold(remote.Issue.State, "open"),
		Ready: hasAnyLabel(labels, l.Config.GitHub.ReadyLabels), Running: labels[l.Config.GitHub.RunningLabel],
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
			URL: pr.URL, State: pr.State, HeadRefName: pr.HeadRefName, Draft: pr.IsDraft, Merged: pr.MergedAt != nil,
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
	return reconciliationDecision{
		status: decision.Status, lastError: decision.LastError, branch: decision.Branch,
		pullRequest: decision.PullRequest, githubSync: decision.GitHubSync, retryAt: decision.RetryAt,
		workerPID: decision.WorkerPID, workerPGID: decision.WorkerPGID, prMerged: decision.PullRequestMerged,
		markRunning: decision.MarkRunning, reason: decision.Reason, blockedCause: current.BlockedCause,
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
		PullRequest: decision.pullRequest, GitHubSync: decision.githubSync, RetryAt: decision.retryAt,
		WorkerPID: decision.workerPID, WorkerPGID: decision.workerPGID, PullRequestMerged: decision.prMerged,
		MarkRunning: decision.markRunning, Reason: decision.reason,
	}, reason)
	decision.status, decision.lastError, decision.githubSync = domainDecision.Status, domainDecision.LastError, domainDecision.GitHubSync
	decision.retryAt, decision.workerPID, decision.workerPGID, decision.reason = domainDecision.RetryAt, domainDecision.WorkerPID, domainDecision.WorkerPGID, domainDecision.Reason
	return decision
}

func terminalPullRequestCandidate(issue state.Issue) bool {
	return issuedomain.TerminalPullRequestCandidate(issuedomain.ReconciliationState{
		Status: issue.Status, PullRequest: issue.PullRequestURL, PullRequestMerged: issue.PullRequestMerged, GitHubSync: issue.GitHubSync,
	})
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

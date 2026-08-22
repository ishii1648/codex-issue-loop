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
	githubSync   string
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
			predicateReport := startupReconciliationPredicateReport(number, decision)
			if decision.markRunning {
				if err := l.GitHub.MarkRunning(ctx, l.Config, number); err != nil {
					return fmt.Errorf("repair running label for Issue #%d: %w", number, err)
				}
			}
			parkEnvironmentLease := current.Status == "blocked" && current.GitHubSync == "" && current.Lease != nil && current.ResourcePark == nil &&
				current.PullRequestURL == "" && current.WorkerPID == 0 && current.WorkerPGID == 0 && decision.status == "blocked" &&
				decision.githubSync == "" && decision.pullRequest == "" && resumableWorkerBlock(decision.blockedCause) && onlyBlockedExclusion
			request := singlePendingRequest(latest, number)
			parkInputLease := current.Status == "needs_input" && current.GitHubSync == "" && current.Lease != nil && current.ResourcePark == nil &&
				current.PullRequestURL == "" && current.WorkerPID == 0 && current.WorkerPGID == 0 && decision.status == "needs_input" &&
				decision.githubSync == "" && decision.pullRequest == "" && request != nil && request.ResumeStatus == "" && current.ConflictRecovery == nil
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
				if decision.status == "completed" && item.Lease != nil {
					if item.ResourcePark != nil && item.ResourcePark.Status == "resuming" {
						item.ResourcePark.Status = "resumed"
					}
					if err := state.ReleaseIssueLease(item, current.Lease.Owner); err != nil {
						return err
					}
				}
				if (decision.status == "failed" || decision.status == "blocked") && decision.pullRequest == "" && item.Lease != nil && !retainsWorkerBoundary(decision.blockedCause) {
					if item.ResourcePark != nil && item.ResourcePark.Status == "resuming" {
						item.ResourcePark.Status = "resumed"
					}
					if err := state.ReleaseIssueLease(item, current.Lease.Owner); err != nil {
						return err
					}
				}
				if err := setIssueStatus(item, decision.status); err != nil {
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
	code := "RECOVERY_STARTUP_RECONCILIATION"
	source := "shared startup reconciliation decision"
	if blocked {
		reason := strings.ToLower(decision.reason)
		switch {
		case strings.Contains(reason, "worktree") || strings.Contains(reason, "branch"):
			code = "RECOVERY_WORKSPACE"
		case strings.Contains(reason, "pull request"):
			code = "RECOVERY_GITHUB_IDENTITY"
		case strings.Contains(reason, "label") || strings.Contains(reason, "github"):
			code = "RECOVERY_GITHUB_LABELS"
		case strings.Contains(reason, "worker pid"):
			code = "RECOVERY_WORKER_PROCESS"
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
	if item.GitHubSync != "" {
		return true
	}
	switch item.Status {
	case "claiming", "claimed", "running", "answer_claim_waiting", "resume_pending", "environment_resume_pending", "pull_request_checks_recovery_pending", "awaiting_checks", "awaiting_merge", "resolving_conflict":
		return true
	case "retry_wait":
		return item.RetryAfter == nil || !item.RetryAfter.After(now)
	case "blocked":
		return state.MayHaveLegacyWorkerBlockProvenance(item) ||
			(item.Lease != nil && item.ResourcePark == nil && item.PullRequestURL == "" && resumableWorkerBlock(item.BlockedCause))
	case "needs_input":
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
		if decision.status == "completed" && item.Lease != nil {
			if err := state.ReleaseIssueLease(item, item.Lease.Owner); err != nil {
				return err
			}
		}
		if (decision.status == "failed" || decision.status == "blocked") && decision.pullRequest == "" && item.Lease != nil && !retainsWorkerBoundary(decision.blockedCause) {
			if err := state.ReleaseIssueLease(item, item.Lease.Owner); err != nil {
				return err
			}
		}
		if err := setIssueStatus(item, decision.status); err != nil {
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
		case "claiming", "claimed", "running", "retry_wait", "resume_pending", "environment_resume_pending", "pull_request_checks_recovery_pending", "awaiting_checks", "awaiting_merge", "resolving_conflict":
			return true
		}
	}
	return labels[cfg.NeedsInputLabel] && (current.Status == "needs_input" || current.Status == "answer_claim_waiting" || current.Status == "resume_pending")
}

func (l *Loop) decideReconciliation(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) reconciliationDecision {
	decision := reconciliationDecision{
		status: current.Status, lastError: current.LastError, branch: current.Branch,
		pullRequest: current.PullRequestURL, githubSync: current.GitHubSync,
		retryAt: current.RetryAfter, workerPID: current.WorkerPID, workerPGID: current.WorkerPGID, prMerged: current.PullRequestMerged,
		reason: "state already converged", blockedCause: current.BlockedCause,
	}
	labels := labelSet(remote.Issue.Labels)
	ready := hasAnyLabel(labels, l.Config.GitHub.ReadyLabels)
	running := labels[l.Config.GitHub.RunningLabel]
	needsInput := labels[l.Config.GitHub.NeedsInputLabel]
	done := labels[l.Config.GitHub.DoneLabel]
	failed := labels[l.Config.GitHub.FailedLabel]
	excluded := hasAnyLabel(labels, l.Config.GitHub.ExcludeLabels)
	if terminalDecision, ok := l.decideTerminalPullRequestReconciliation(current, remote); ok && terminalDecision.status == "completed" && terminalDecision.prMerged {
		return terminalDecision
	}
	if current.Status == "completed" && strings.EqualFold(remote.Issue.State, "open") {
		open := []gh.PullRequest{}
		for _, pr := range remote.PullRequests {
			if pr.MergedAt == nil && strings.EqualFold(pr.State, "open") {
				open = append(open, pr)
			}
		}
		if len(open) == 1 {
			decision.status = "awaiting_merge"
			if open[0].IsDraft {
				decision.status = "awaiting_checks"
			}
			decision.pullRequest = open[0].URL
			decision.prMerged = false
			decision.githubSync = ""
			decision.retryAt = nil
			decision.workerPID = 0
			decision.markRunning = true
			decision.reason = "legacy completed Issue with open Pull Request returned to lifecycle monitoring"
			return decision
		}
	}

	// A done label cannot release an existing PR lease. Only an observed merge
	// below is authoritative once a Pull Request URL has been recorded.
	if done && current.PullRequestURL == "" && len(remote.PullRequests) == 0 {
		decision.status, decision.lastError = "completed", ""
		for _, pr := range remote.PullRequests {
			if pr.MergedAt != nil {
				decision.pullRequest = pr.URL
				decision.prMerged = true
				break
			}
		}
		if current.GitHubSync == "done" && !hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") {
			decision.githubSync = "done"
		} else {
			decision.githubSync = ""
		}
		decision.workerPID, decision.retryAt, decision.reason = 0, nil, "GitHub done label is authoritative"
		return decision
	}
	if excluded {
		if current.Status == "blocked" && workspaceSafetyBlock(current.BlockedCause) && l.hasOnlyBlockedExclusion(labels) {
			decision.status, decision.workerPID, decision.retryAt, decision.githubSync = "blocked", 0, nil, ""
			decision.reason = "supervisor-owned worker workspace safety block preserved"
			return decision
		} else if current.Status == "blocked" && current.BlockedCause != nil && current.BlockedCause.Origin == "worker" &&
			current.BlockedCause.Kind == "environment" && current.BlockedCause.Resumable && l.hasOnlyBlockedExclusion(labels) {
			decision.status, decision.workerPID, decision.retryAt, decision.githubSync = "blocked", 0, nil, ""
			decision.reason = "supervisor-owned worker environment block provenance preserved"
			return decision
		} else if current.GitHubSync == "environment_resume" && current.Status == "environment_resume_pending" {
			decision.reason = "explicit environment resume is waiting for GitHub label synchronization"
		} else if current.GitHubSync == "conflict_retry" && current.Status == "resolving_conflict" {
			decision.reason = "explicit conflict retry is waiting for GitHub label synchronization"
		} else if current.GitHubSync == "blocked" {
			decision.status, decision.workerPID, decision.retryAt = "blocked", 0, nil
			if hasComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)) {
				decision.githubSync = ""
			}
			decision.reason = "partially synchronized blocked state recovered"
			return decision
		} else {
			return blockDecision(decision, "GitHub exclusion label was applied manually")
		}
	}
	if failed {
		if current.GitHubSync == "pull_request_checks_recovery" && current.Status == "pull_request_checks_recovery_pending" {
			decision.reason = "explicit Pull Request checks recovery is waiting for GitHub label synchronization"
			return decision
		}
		if current.GitHubSync == "publication_recovery" && current.Status == "publication_recovery_pending" {
			decision.reason = "explicit publication recovery is waiting for GitHub label synchronization"
			return decision
		}
		if current.PublicationRecovery != nil {
			for _, pr := range remote.PullRequests {
				if pr.MergedAt != nil {
					decision.status, decision.lastError, decision.pullRequest = "completed", "", pr.URL
					decision.prMerged = true
					decision.workerPID, decision.retryAt, decision.githubSync = 0, nil, "done"
					decision.reason = "merged recovered Pull Request discovered"
					return decision
				}
			}
		}
		decision.status, decision.workerPID, decision.retryAt = "failed", 0, nil
		if current.PublicationRecovery != nil && current.PublicationRecovery.Status == "failed" && decision.pullRequest == "" {
			for _, pr := range remote.PullRequests {
				if pr.MergedAt == nil && strings.EqualFold(pr.State, "open") {
					decision.pullRequest = pr.URL
					break
				}
			}
		}
		if current.GitHubSync == "failed" && !hasComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)) {
			decision.githubSync = "failed"
		} else {
			decision.githubSync = ""
		}
		decision.reason = "GitHub failed label is authoritative"
		return decision
	}
	if ready && (running || needsInput) {
		return blockDecision(decision, "GitHub labels contain conflicting ready and active states")
	}

	openPRs := []gh.PullRequest{}
	var mergedPR *gh.PullRequest
	var closedPR *gh.PullRequest
	for index := range remote.PullRequests {
		pr := &remote.PullRequests[index]
		if pr.MergedAt != nil {
			if mergedPR == nil {
				mergedPR = pr
			}
			continue
		}
		if strings.EqualFold(pr.State, "open") {
			openPRs = append(openPRs, *pr)
		} else if closedPR == nil {
			closedPR = pr
		}
	}
	if len(remote.PullRequests) > 1 {
		return blockDecision(decision, "multiple Pull Requests target the saved branch")
	}
	if mergedPR != nil {
		decision.status, decision.lastError, decision.pullRequest = "completed", "", mergedPR.URL
		decision.prMerged = true
		decision.workerPID, decision.retryAt, decision.reason = 0, nil, "merged Pull Request discovered"
		if hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") && done {
			decision.githubSync = ""
		} else {
			decision.githubSync = "done"
		}
		return decision
	}
	if len(openPRs) > 1 {
		return blockDecision(decision, "multiple open Pull Requests target the saved branch")
	}
	if len(openPRs) == 1 {
		decision.pullRequest = openPRs[0].URL
		if decision.branch == "" {
			decision.branch = openPRs[0].HeadRefName
		}
	} else if closedPR != nil {
		decision.pullRequest = closedPR.URL
		return blockDecision(decision, "Pull Request was closed without merge")
	}
	if strings.EqualFold(remote.Issue.State, "closed") {
		return blockDecision(decision, "GitHub Issue was closed without a done label or merged Pull Request")
	}

	if current.Worktree != "" {
		if !inspection.Exists || !inspection.Valid {
			return blockDecision(decision, "saved worktree is missing or invalid")
		}
		if current.Branch != "" && inspection.Branch != current.Branch {
			return blockDecision(decision, fmt.Sprintf("worktree branch changed from %s to %s", current.Branch, inspection.Branch))
		}
		if current.Branch != "" && !inspection.LocalBranchExists {
			return blockDecision(decision, "saved local branch is missing")
		}
		if len(openPRs) == 1 && !inspection.RemoteBranchExists {
			return blockDecision(decision, "open Pull Request head branch is missing from origin")
		}
	} else if len(openPRs) == 1 {
		return blockDecision(decision, "open Pull Request exists but the saved worktree is missing")
	}

	processes := l.Processes
	if processes == nil {
		processes = osProcessInspector{}
	}
	if current.WorkerPID > 0 && processes.Alive(current.WorkerPID) {
		return blockDecision(decision, fmt.Sprintf("saved worker PID %d is still alive", current.WorkerPID))
	}
	decision.workerPID = 0
	decision.workerPGID = 0

	if current.GitHubSync == "needs_input" {
		if request := pendingRequest(snapshot, current.Number); request != nil && needsInput && hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:request:"+request.ID+" -->") {
			decision.githubSync = ""
		}
	}
	if current.GitHubSync == "done" && done && hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") {
		decision.githubSync = ""
	}

	switch current.Status {
	case "claiming":
		if running && !ready {
			now := l.now()
			decision.status, decision.retryAt = "retry_wait", &now
			decision.lastError, decision.reason = "claim completed before supervisor restart", "write-ahead claim converged from GitHub"
		} else if ready && !running {
			decision.reason = "claim did not reach GitHub and will be retried idempotently"
		} else if !ready && !running {
			return blockDecision(decision, "claim labels were removed manually")
		}
	case "running", "claimed":
		now := l.now()
		decision.status, decision.retryAt = "retry_wait", &now
		decision.lastError, decision.reason = "worker disappeared before supervisor restart", "dead worker scheduled for retry"
		if !running && inspection.Valid {
			decision.markRunning = true
		}
	case "retry_wait":
		decision.reason = "existing retry schedule preserved"
		if !running && inspection.Valid {
			decision.markRunning = true
		}
	case "resume_pending":
		decision.reason = "recorded answer remains pending for resume"
	case "answer_claim_waiting":
		decision.reason = "recorded answer is waiting for its parked resource claim"
	case "environment_resume_pending":
		decision.reason = "operator-confirmed environment resume remains pending in the saved worktree"
	case "publication_recovery_pending":
		decision.reason = "operator-confirmed publication recovery remains pending in the saved worktree"
		if !running {
			decision.githubSync = "publication_recovery"
		}
	case "pull_request_checks_recovery_pending":
		decision.reason = "operator-confirmed Pull Request checks recovery remains pending in the saved worktree"
		if !running {
			decision.githubSync = "pull_request_checks_recovery"
		}
	case "needs_input":
		decision.reason = "unanswered request remains sticky"
		if !needsInput {
			decision.githubSync = "needs_input"
		}
	case "resolving_conflict":
		decision.reason = "durable Pull Request conflict recovery will resume in the saved worktree"
		if decision.retryAt == nil {
			now := l.now()
			decision.retryAt = &now
		}
		if !running && inspection.Valid {
			decision.markRunning = true
		}
	}
	if len(openPRs) == 1 && (current.Status == "running" || current.Status == "claimed") {
		decision.reason = "open Pull Request discovered and dead worker scheduled for retry"
	}
	return decision
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
	decision := reconciliationDecision{
		status: current.Status, lastError: current.LastError, branch: current.Branch,
		pullRequest: current.PullRequestURL, githubSync: current.GitHubSync,
		retryAt: current.RetryAfter, workerPID: current.WorkerPID, workerPGID: current.WorkerPGID,
		prMerged: current.PullRequestMerged, reason: "terminal Issue remains sticky",
	}
	if !terminalPullRequestCandidate(current) {
		return decision, false
	}
	if len(remote.PullRequests) != 1 {
		if len(remote.PullRequests) > 1 {
			decision.reason = "multiple Pull Requests target the saved branch"
		} else {
			decision.reason = "saved Pull Request was not found for the saved branch"
		}
		return decision, true
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL {
		decision.reason = "Pull Request for the saved branch does not match the saved Pull Request URL"
		return decision, true
	}
	if current.Branch == "" || pr.HeadRefName == "" || pr.HeadRefName != current.Branch {
		decision.reason = "Pull Request head does not match the saved branch"
		return decision, true
	}
	if pr.MergedAt == nil {
		if strings.EqualFold(pr.State, "open") {
			decision.reason = "saved Pull Request is not merged"
		} else {
			decision.reason = "saved Pull Request was closed without merge"
		}
		return decision, true
	}
	if l.hasManualExclusion(remote.Issue, current) {
		decision.reason = "GitHub exclusion label was applied manually"
		return decision, true
	}
	decision.status = "completed"
	decision.lastError = ""
	decision.prMerged = true
	decision.githubSync = "done"
	decision.retryAt = nil
	decision.workerPID = 0
	decision.workerPGID = 0
	decision.reason = "saved Pull Request merge discovered for terminal Issue"
	return decision, true
}

func terminalPullRequestCandidate(issue state.Issue) bool {
	if issue.Status != "blocked" && issue.Status != "failed" {
		return false
	}
	return issue.PullRequestURL != "" && !issue.PullRequestMerged && issue.GitHubSync == ""
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

func blockDecision(decision reconciliationDecision, reason string) reconciliationDecision {
	decision.status = "blocked"
	decision.lastError = "startup reconciliation blocked: " + reason
	decision.githubSync = ""
	decision.retryAt = nil
	decision.workerPID = 0
	decision.workerPGID = 0
	decision.reason = reason
	return decision
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
		if request.IssueNumber == issueNumber && request.Status == "pending" {
			return request
		}
	}
	return nil
}

func singlePendingRequest(snapshot state.Snapshot, issueNumber int) *state.Request {
	var found *state.Request
	for _, request := range snapshot.PendingRequests {
		if request == nil || request.IssueNumber != issueNumber || request.Status != "pending" {
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

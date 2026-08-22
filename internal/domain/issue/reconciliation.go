package issue

import (
	"fmt"
	"strings"
	"time"
)

type ReconciliationState struct {
	Number                    int
	Status                    Status
	LastError                 string
	Branch                    string
	PullRequest               string
	GitHubSync                GitHubSync
	RetryAt                   *time.Time
	WorkerPID                 int
	WorkerPGID                int
	PullRequestMerged         bool
	WorktreeSaved             bool
	BlockedOrigin             string
	BlockedKind               string
	BlockedResumable          bool
	PublicationRecoveryStatus PublicationRecoveryStatus
}

type ReconciliationPullRequest struct {
	URL         string
	State       string
	HeadRefName string
	Draft       bool
	Merged      bool
}

type ReconciliationWorkspace struct {
	Exists             bool
	Valid              bool
	LocalBranchExists  bool
	RemoteBranchExists bool
	Branch             string
}

type ReconciliationObservation struct {
	Now                  time.Time
	IssueOpen            bool
	Ready                bool
	Running              bool
	NeedsInput           bool
	Done                 bool
	Failed               bool
	Excluded             bool
	OnlyBlockedExclusion bool
	ManualExclusion      bool
	DoneMarker           bool
	FailedMarker         bool
	PendingRequestMarker bool
	PullRequests         []ReconciliationPullRequest
	Workspace            ReconciliationWorkspace
	WorkerAlive          bool
}

type ReconciliationDecision struct {
	Status            Status
	LastError         string
	Branch            string
	PullRequest       string
	GitHubSync        GitHubSync
	RetryAt           *time.Time
	WorkerPID         int
	WorkerPGID        int
	PullRequestMerged bool
	MarkRunning       bool
	Reason            string
}

func initialReconciliationDecision(current ReconciliationState) ReconciliationDecision {
	return ReconciliationDecision{
		Status: current.Status, LastError: current.LastError, Branch: current.Branch,
		PullRequest: current.PullRequest, GitHubSync: current.GitHubSync, RetryAt: current.RetryAt,
		WorkerPID: current.WorkerPID, WorkerPGID: current.WorkerPGID, PullRequestMerged: current.PullRequestMerged,
		Reason: "state already converged",
	}
}

func BlockReconciliation(decision ReconciliationDecision, reason string) ReconciliationDecision {
	decision.Status = StatusBlocked
	decision.LastError = "startup reconciliation blocked: " + reason
	decision.GitHubSync = GitHubSyncNone
	decision.RetryAt = nil
	decision.WorkerPID = 0
	decision.WorkerPGID = 0
	decision.Reason = reason
	return decision
}

func TerminalPullRequestCandidate(current ReconciliationState) bool {
	return (current.Status == StatusBlocked || current.Status == StatusFailed) && current.PullRequest != "" &&
		!current.PullRequestMerged && current.GitHubSync == GitHubSyncNone
}

func DecideTerminalPullRequestReconciliation(current ReconciliationState, observed ReconciliationObservation) (ReconciliationDecision, bool) {
	decision := initialReconciliationDecision(current)
	decision.Reason = "terminal Issue remains sticky"
	if !TerminalPullRequestCandidate(current) {
		return decision, false
	}
	if len(observed.PullRequests) != 1 {
		if len(observed.PullRequests) > 1 {
			decision.Reason = "multiple Pull Requests target the saved branch"
		} else {
			decision.Reason = "saved Pull Request was not found for the saved branch"
		}
		return decision, true
	}
	pr := observed.PullRequests[0]
	if pr.URL != current.PullRequest {
		decision.Reason = "Pull Request for the saved branch does not match the saved Pull Request URL"
		return decision, true
	}
	if current.Branch == "" || pr.HeadRefName == "" || pr.HeadRefName != current.Branch {
		decision.Reason = "Pull Request head does not match the saved branch"
		return decision, true
	}
	if !pr.Merged {
		if strings.EqualFold(pr.State, "open") {
			decision.Reason = "saved Pull Request is not merged"
		} else {
			decision.Reason = "saved Pull Request was closed without merge"
		}
		return decision, true
	}
	if observed.ManualExclusion {
		decision.Reason = "GitHub exclusion label was applied manually"
		return decision, true
	}
	decision.Status, decision.LastError = StatusCompleted, ""
	decision.PullRequestMerged, decision.GitHubSync = true, GitHubSyncDone
	decision.RetryAt, decision.WorkerPID, decision.WorkerPGID = nil, 0, 0
	decision.Reason = "saved Pull Request merge discovered for terminal Issue"
	return decision, true
}

// DecideReconciliation owns lifecycle convergence. Adapters normalize GitHub,
// worktree, and process observations into this DTO and commit the result.
func DecideReconciliation(current ReconciliationState, observed ReconciliationObservation) ReconciliationDecision {
	decision := initialReconciliationDecision(current)

	// Terminal authority is intentionally checked before labels. An exact
	// observed merge may complete a sticky failed/blocked record.
	if terminal, ok := DecideTerminalPullRequestReconciliation(current, observed); ok && terminal.Status == StatusCompleted && terminal.PullRequestMerged {
		return terminal
	}
	if current.Status == StatusCompleted && observed.IssueOpen {
		open := []ReconciliationPullRequest{}
		for _, pr := range observed.PullRequests {
			if !pr.Merged && strings.EqualFold(pr.State, "open") {
				open = append(open, pr)
			}
		}
		if len(open) == 1 {
			decision.Status = StatusAwaitingMerge
			if open[0].Draft {
				decision.Status = StatusAwaitingChecks
			}
			decision.PullRequest, decision.PullRequestMerged = open[0].URL, false
			decision.GitHubSync, decision.RetryAt, decision.WorkerPID = GitHubSyncNone, nil, 0
			decision.MarkRunning = true
			decision.Reason = "legacy completed Issue with open Pull Request returned to lifecycle monitoring"
			return decision
		}
	}
	if observed.Done && current.PullRequest == "" && len(observed.PullRequests) == 0 {
		decision.Status, decision.LastError = StatusCompleted, ""
		if current.GitHubSync == GitHubSyncDone && !observed.DoneMarker {
			decision.GitHubSync = GitHubSyncDone
		} else {
			decision.GitHubSync = GitHubSyncNone
		}
		decision.WorkerPID, decision.RetryAt, decision.Reason = 0, nil, "GitHub done label is authoritative"
		return decision
	}

	// GitHub terminal and exclusion labels are shared operator state. Preserve
	// explicit recovery handshakes, otherwise converge or block deterministically.
	if observed.Excluded {
		workerSafety := current.Status == StatusBlocked && current.BlockedOrigin == "supervisor" && current.BlockedKind == "worker_workspace" && !current.BlockedResumable
		workerEnvironment := current.Status == StatusBlocked && current.BlockedOrigin == "worker" && current.BlockedKind == "environment" && current.BlockedResumable
		switch {
		case workerSafety && observed.OnlyBlockedExclusion:
			decision.Status, decision.WorkerPID, decision.RetryAt, decision.GitHubSync = StatusBlocked, 0, nil, GitHubSyncNone
			decision.Reason = "supervisor-owned worker workspace safety block preserved"
			return decision
		case workerEnvironment && observed.OnlyBlockedExclusion:
			decision.Status, decision.WorkerPID, decision.RetryAt, decision.GitHubSync = StatusBlocked, 0, nil, GitHubSyncNone
			decision.Reason = "supervisor-owned worker environment block provenance preserved"
			return decision
		case current.GitHubSync == GitHubSyncEnvironmentResume && current.Status == StatusEnvironmentResumePending:
			decision.Reason = "explicit environment resume is waiting for GitHub label synchronization"
		case current.GitHubSync == GitHubSyncConflictRetry && current.Status == StatusResolvingConflict:
			decision.Reason = "explicit conflict retry is waiting for GitHub label synchronization"
		case current.GitHubSync == GitHubSyncBlocked:
			decision.Status, decision.WorkerPID, decision.RetryAt = StatusBlocked, 0, nil
			if observed.FailedMarker {
				decision.GitHubSync = GitHubSyncNone
			}
			decision.Reason = "partially synchronized blocked state recovered"
			return decision
		default:
			return BlockReconciliation(decision, "GitHub exclusion label was applied manually")
		}
	}
	if observed.Failed {
		if current.GitHubSync == GitHubSyncPullRequestChecksRecovery && current.Status == StatusChecksRecovery {
			decision.Reason = "explicit Pull Request checks recovery is waiting for GitHub label synchronization"
			return decision
		}
		if current.GitHubSync == GitHubSyncPublicationRecovery && current.Status == StatusPublicationRecovery {
			decision.Reason = "explicit publication recovery is waiting for GitHub label synchronization"
			return decision
		}
		if current.PublicationRecoveryStatus != PublicationRecoveryStatusUnset {
			for _, pr := range observed.PullRequests {
				if pr.Merged {
					decision.Status, decision.LastError, decision.PullRequest = StatusCompleted, "", pr.URL
					decision.PullRequestMerged, decision.WorkerPID, decision.RetryAt, decision.GitHubSync = true, 0, nil, GitHubSyncDone
					decision.Reason = "merged recovered Pull Request discovered"
					return decision
				}
			}
		}
		decision.Status, decision.WorkerPID, decision.RetryAt = StatusFailed, 0, nil
		if current.PublicationRecoveryStatus == PublicationRecoveryStatusFailed && decision.PullRequest == "" {
			for _, pr := range observed.PullRequests {
				if !pr.Merged && strings.EqualFold(pr.State, "open") {
					decision.PullRequest = pr.URL
					break
				}
			}
		}
		if current.GitHubSync == GitHubSyncFailed && !observed.FailedMarker {
			decision.GitHubSync = GitHubSyncFailed
		} else {
			decision.GitHubSync = GitHubSyncNone
		}
		decision.Reason = "GitHub failed label is authoritative"
		return decision
	}
	if observed.Ready && (observed.Running || observed.NeedsInput) {
		return BlockReconciliation(decision, "GitHub labels contain conflicting ready and active states")
	}

	// Pull Request and workspace observations fence local restart recovery.
	open := []ReconciliationPullRequest{}
	var merged, closed *ReconciliationPullRequest
	for index := range observed.PullRequests {
		pr := &observed.PullRequests[index]
		if pr.Merged {
			if merged == nil {
				merged = pr
			}
			continue
		}
		if strings.EqualFold(pr.State, "open") {
			open = append(open, *pr)
		} else if closed == nil {
			closed = pr
		}
	}
	if len(observed.PullRequests) > 1 {
		return BlockReconciliation(decision, "multiple Pull Requests target the saved branch")
	}
	if merged != nil {
		decision.Status, decision.LastError, decision.PullRequest = StatusCompleted, "", merged.URL
		decision.PullRequestMerged, decision.WorkerPID, decision.RetryAt, decision.Reason = true, 0, nil, "merged Pull Request discovered"
		if observed.DoneMarker && observed.Done {
			decision.GitHubSync = GitHubSyncNone
		} else {
			decision.GitHubSync = GitHubSyncDone
		}
		return decision
	}
	if len(open) > 1 {
		return BlockReconciliation(decision, "multiple open Pull Requests target the saved branch")
	}
	if len(open) == 1 {
		decision.PullRequest = open[0].URL
		if decision.Branch == "" {
			decision.Branch = open[0].HeadRefName
		}
	} else if closed != nil {
		decision.PullRequest = closed.URL
		return BlockReconciliation(decision, "Pull Request was closed without merge")
	}
	if !observed.IssueOpen {
		return BlockReconciliation(decision, "GitHub Issue was closed without a done label or merged Pull Request")
	}

	if current.WorktreeSaved {
		if !observed.Workspace.Exists || !observed.Workspace.Valid {
			return BlockReconciliation(decision, "saved worktree is missing or invalid")
		}
		if current.Branch != "" && observed.Workspace.Branch != current.Branch {
			return BlockReconciliation(decision, fmt.Sprintf("worktree branch changed from %s to %s", current.Branch, observed.Workspace.Branch))
		}
		if current.Branch != "" && !observed.Workspace.LocalBranchExists {
			return BlockReconciliation(decision, "saved local branch is missing")
		}
		if len(open) == 1 && !observed.Workspace.RemoteBranchExists {
			return BlockReconciliation(decision, "open Pull Request head branch is missing from origin")
		}
	} else if len(open) == 1 {
		return BlockReconciliation(decision, "open Pull Request exists but the saved worktree is missing")
	}
	if observed.WorkerAlive {
		return BlockReconciliation(decision, fmt.Sprintf("saved worker PID %d is still alive", current.WorkerPID))
	}
	decision.WorkerPID, decision.WorkerPGID = 0, 0
	if current.GitHubSync == GitHubSyncNeedsInput && observed.PendingRequestMarker {
		decision.GitHubSync = GitHubSyncNone
	}
	if current.GitHubSync == GitHubSyncDone && observed.Done && observed.DoneMarker {
		decision.GitHubSync = GitHubSyncNone
	}

	// With external identity validated, choose the restart target for the saved
	// lifecycle state.
	switch current.Status {
	case StatusClaiming:
		if observed.Running && !observed.Ready {
			now := observed.Now
			decision.Status, decision.RetryAt = StatusRetryWait, &now
			decision.LastError, decision.Reason = "claim completed before supervisor restart", "write-ahead claim converged from GitHub"
		} else if observed.Ready && !observed.Running {
			decision.Reason = "claim did not reach GitHub and will be retried idempotently"
		} else if !observed.Ready && !observed.Running {
			return BlockReconciliation(decision, "claim labels were removed manually")
		}
	case StatusRunning, StatusClaimed:
		now := observed.Now
		decision.Status, decision.RetryAt = StatusRetryWait, &now
		decision.LastError, decision.Reason = "worker disappeared before supervisor restart", "dead worker scheduled for retry"
		if !observed.Running && observed.Workspace.Valid {
			decision.MarkRunning = true
		}
	case StatusRetryWait:
		decision.Reason = "existing retry schedule preserved"
		if !observed.Running && observed.Workspace.Valid {
			decision.MarkRunning = true
		}
	case StatusResumePending:
		decision.Reason = "recorded answer remains pending for resume"
	case StatusAnswerClaimWaiting:
		decision.Reason = "recorded answer is waiting for its parked resource claim"
	case StatusEnvironmentResumePending:
		decision.Reason = "operator-confirmed environment resume remains pending in the saved worktree"
	case StatusPublicationRecovery:
		decision.Reason = "operator-confirmed publication recovery remains pending in the saved worktree"
		if !observed.Running {
			decision.GitHubSync = GitHubSyncPublicationRecovery
		}
	case StatusChecksRecovery:
		decision.Reason = "operator-confirmed Pull Request checks recovery remains pending in the saved worktree"
		if !observed.Running {
			decision.GitHubSync = GitHubSyncPullRequestChecksRecovery
		}
	case StatusNeedsInput:
		decision.Reason = "unanswered request remains sticky"
		if !observed.NeedsInput {
			decision.GitHubSync = GitHubSyncNeedsInput
		}
	case StatusResolvingConflict:
		decision.Reason = "durable Pull Request conflict recovery will resume in the saved worktree"
		if decision.RetryAt == nil {
			now := observed.Now
			decision.RetryAt = &now
		}
		if !observed.Running && observed.Workspace.Valid {
			decision.MarkRunning = true
		}
	}
	if len(open) == 1 && (current.Status == StatusRunning || current.Status == StatusClaimed) {
		decision.Reason = "open Pull Request discovered and dead worker scheduled for retry"
	}
	return decision
}

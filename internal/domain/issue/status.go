// Package issue contains the deterministic lifecycle model for a queued Issue.
//
// The package deliberately has no filesystem, process, clock, or network
// dependencies. Application code observes the outside world, asks this package
// for a decision, and persists that decision through the state store.
package issue

import "fmt"

// Status is the durable lifecycle state of one queued Issue.
//
// Values are part of the persisted state contract and therefore must not be
// renamed without a schema/semantic migration.
type Status string

type WorktreeRetentionClass string

const (
	WorktreeRetainIndefinitely WorktreeRetentionClass = "retain_indefinitely"
	WorktreeRetainCompleted    WorktreeRetentionClass = "completed"
	WorktreeRetainFailed       WorktreeRetentionClass = "failed"
	WorktreeRetainBlocked      WorktreeRetentionClass = "blocked"
	WorktreeRetainAttention    WorktreeRetentionClass = "attention"
)

const (
	StatusUnset                    Status = ""
	StatusClaiming                 Status = "claiming"
	StatusClaimed                  Status = "claimed"
	StatusRunning                  Status = "running"
	StatusAnswerClaimWaiting       Status = "answer_claim_waiting"
	StatusResumePending            Status = "resume_pending"
	StatusEnvironmentResumePending Status = "environment_resume_pending"
	StatusPublicationRecovery      Status = "publication_recovery_pending"
	StatusChecksRecovery           Status = "pull_request_checks_recovery_pending"
	StatusRetryWait                Status = "retry_wait"
	StatusNeedsInput               Status = "needs_input"
	StatusAwaitingChecks           Status = "awaiting_checks"
	StatusAwaitingMerge            Status = "awaiting_merge"
	StatusResolvingConflict        Status = "resolving_conflict"
	StatusBlocked                  Status = "blocked"
	StatusFailed                   Status = "failed"
	StatusCompleted                Status = "completed"
)

var allStatuses = [...]Status{
	StatusUnset, StatusClaiming, StatusClaimed, StatusRunning,
	StatusAnswerClaimWaiting, StatusResumePending, StatusEnvironmentResumePending,
	StatusPublicationRecovery, StatusChecksRecovery, StatusRetryWait,
	StatusNeedsInput, StatusAwaitingChecks, StatusAwaitingMerge,
	StatusResolvingConflict, StatusBlocked, StatusFailed, StatusCompleted,
}

var knownStatuses = func() map[Status]struct{} {
	known := make(map[Status]struct{}, len(allStatuses))
	for _, status := range allStatuses {
		known[status] = struct{}{}
	}
	return known
}()

// AllStatuses returns a copy of every value in the durable lifecycle contract.
// Callers may derive secondary contracts without maintaining another status list.
func AllStatuses() []Status {
	return append([]Status(nil), allStatuses[:]...)
}

func (s Status) String() string { return string(s) }

// Validate rejects lifecycle values that are not part of the durable contract.
func (s Status) Validate() error {
	if _, ok := knownStatuses[s]; !ok {
		return fmt.Errorf("unknown Issue status %q", s)
	}
	return nil
}

func (s Status) Terminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusBlocked
}

func (s Status) WorktreeRetentionClass() WorktreeRetentionClass {
	switch s {
	case StatusCompleted:
		return WorktreeRetainCompleted
	case StatusFailed:
		return WorktreeRetainFailed
	case StatusBlocked:
		return WorktreeRetainBlocked
	case StatusNeedsInput, StatusAnswerClaimWaiting, StatusResumePending:
		return WorktreeRetainAttention
	default:
		return WorktreeRetainIndefinitely
	}
}

// RequiresWorkspaceProvenance reports whether execution or recovery may need
// the Issue's immutable worker workspace identity.
func (s Status) RequiresWorkspaceProvenance() bool {
	switch s {
	case StatusClaimed, StatusRunning, StatusAnswerClaimWaiting, StatusResumePending,
		StatusEnvironmentResumePending, StatusPublicationRecovery, StatusChecksRecovery,
		StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResolvingConflict, StatusBlocked, StatusFailed:
		return true
	default:
		return false
	}
}

// OccupiesWorkerSlot reports whether an Issue owns one of the bounded worker
// execution slots. Retained leases in waiting and attention states still fence
// resources, but do not consume a worker slot.
func (s Status) OccupiesWorkerSlot() bool {
	switch s {
	case StatusClaiming, StatusClaimed, StatusRunning, StatusResumePending,
		StatusEnvironmentResumePending, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

// RequiresCapabilityRecheck reports whether persisted capability requirements
// must be re-evaluated before the Issue can resume execution.
func (s Status) RequiresCapabilityRecheck() bool {
	switch s {
	case StatusClaiming, StatusAnswerClaimWaiting, StatusResumePending,
		StatusEnvironmentResumePending, StatusRetryWait:
		return true
	default:
		return false
	}
}

// TerminalForWebhook reports whether webhook reconciliation treats the Issue
// as an attention/terminal record rather than an active lifecycle owner.
func (s Status) TerminalForWebhook() bool {
	return s.Terminal() || s == StatusNeedsInput
}

// WebhookRoutable reports whether a webhook delivery may be routed directly
// to the persisted Issue lifecycle.
func (s Status) WebhookRoutable() bool {
	switch s {
	case StatusClaiming, StatusClaimed, StatusRunning, StatusAnswerClaimWaiting,
		StatusResumePending, StatusEnvironmentResumePending, StatusChecksRecovery,
		StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResolvingConflict:
		return true
	default:
		return false
	}
}

// PendingDispatch reports whether the scheduler may dispatch lifecycle work
// for this status once its retry deadline and resource admission allow it.
func (s Status) PendingDispatch() bool {
	switch s {
	case StatusClaiming, StatusAnswerClaimWaiting, StatusResumePending,
		StatusEnvironmentResumePending, StatusPublicationRecovery, StatusChecksRecovery,
		StatusRetryWait, StatusAwaitingChecks, StatusAwaitingMerge, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

// DispatchPending models the two durable lifecycle axes together. GitHub
// synchronization always takes precedence over status-specific dispatch.
func (s Status) DispatchPending(sync GitHubSync) bool {
	return sync.Pending() || s.PendingDispatch()
}

func (s Status) RetainsRunLogs() bool {
	return s.PendingDispatch() || s == StatusClaimed || s == StatusRunning || s == StatusNeedsInput
}

func (s Status) PreventsIdle() bool {
	return s.PendingDispatch() || s == StatusClaimed || s == StatusRunning
}

func (s Status) IneligibleForAdmission() bool {
	if s.Terminal() {
		return true
	}
	switch s {
	case StatusRunning, StatusClaimed, StatusNeedsInput, StatusAnswerClaimWaiting,
		StatusResumePending, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

func (s Status) UsesWorkerSlot() bool {
	switch s {
	case StatusAwaitingChecks, StatusAwaitingMerge, StatusPublicationRecovery, StatusChecksRecovery:
		return false
	default:
		return true
	}
}

func (s Status) UsesWorkerSlotWhile(sync GitHubSync) bool {
	return !sync.Pending() && s.UsesWorkerSlot()
}

func (s Status) BlocksQueueForPullRequest(autoMerge bool) bool {
	return s == StatusAwaitingChecks || s == StatusResolvingConflict || autoMerge && s == StatusAwaitingMerge
}

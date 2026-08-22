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

var knownStatuses = map[Status]struct{}{
	StatusUnset: {}, StatusClaiming: {}, StatusClaimed: {}, StatusRunning: {},
	StatusAnswerClaimWaiting: {}, StatusResumePending: {}, StatusEnvironmentResumePending: {},
	StatusPublicationRecovery: {}, StatusChecksRecovery: {}, StatusRetryWait: {},
	StatusNeedsInput: {}, StatusAwaitingChecks: {}, StatusAwaitingMerge: {},
	StatusResolvingConflict: {}, StatusBlocked: {}, StatusFailed: {}, StatusCompleted: {},
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
	switch s {
	case StatusBlocked, StatusFailed, StatusNeedsInput, StatusCompleted:
		return true
	default:
		return false
	}
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

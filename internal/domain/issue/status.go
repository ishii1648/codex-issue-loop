// Package issue contains the deterministic lifecycle model for a queued Issue.
//
// The package deliberately has no filesystem, process, clock, or network
// dependencies. Application code observes the outside world, asks this package
// for a decision, and persists that decision through the state store.
package issue

import "fmt"

// Status values are part of the persisted state contract and must not be
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
	StatusUnset             Status = ""
	StatusClaiming          Status = "claiming"
	StatusClaimed           Status = "claimed"
	StatusRunning           Status = "running"
	StatusResumePending     Status = "resume_pending"
	StatusRetryWait         Status = "retry_wait"
	StatusNeedsInput        Status = "needs_input"
	StatusAwaitingChecks    Status = "awaiting_checks"
	StatusAwaitingMerge     Status = "awaiting_merge"
	StatusResolvingConflict Status = "resolving_conflict"
	StatusBlocked           Status = "blocked"
	StatusFailed            Status = "failed"
	StatusCompleted         Status = "completed"
)

var allStatuses = [...]Status{
	StatusUnset, StatusClaiming, StatusClaimed, StatusRunning,
	StatusResumePending, StatusRetryWait,
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

// AllStatuses returns an independent slice so callers can derive secondary
// contracts without maintaining or mutating the canonical list.
func AllStatuses() []Status {
	return append([]Status(nil), allStatuses[:]...)
}

func (s Status) String() string { return string(s) }

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
	case StatusNeedsInput, StatusResumePending:
		return WorktreeRetainAttention
	default:
		return WorktreeRetainIndefinitely
	}
}

func (s Status) RequiresWorkspaceProvenance() bool {
	switch s {
	case StatusClaimed, StatusRunning, StatusResumePending,
		StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResolvingConflict:
		return true
	default:
		return false
	}
}

func (s Status) RequiresActiveExecution() bool {
	switch s {
	case StatusClaiming, StatusClaimed, StatusRunning, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

func (s Status) TerminalForWebhook() bool {
	return s.Terminal() || s == StatusNeedsInput
}

func (s Status) WebhookRoutable() bool {
	switch s {
	case StatusClaiming, StatusClaimed, StatusRunning, StatusResumePending,
		StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResolvingConflict:
		return true
	default:
		return false
	}
}

// PendingDispatch is only the lifecycle-status gate; dispatch also requires the
// repository's single active-execution scheduler gate.
func (s Status) PendingDispatch() bool {
	switch s {
	case StatusClaiming, StatusResumePending,
		StatusRetryWait, StatusAwaitingChecks, StatusAwaitingMerge, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

// Status and GitHub synchronization are separate durable lifecycle axes;
// pending synchronization requires dispatch independently of status policy.
func (s Status) DispatchPending(effectPending bool) bool {
	return effectPending || s.PendingDispatch()
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
	case StatusRunning, StatusClaimed, StatusNeedsInput,
		StatusResumePending, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

func (s Status) DispatchesWorker() bool {
	switch s {
	case StatusClaiming, StatusResumePending, StatusRetryWait, StatusResolvingConflict:
		return true
	default:
		return false
	}
}

func (s Status) DispatchesWorkerWhile(effectPending bool) bool {
	return !effectPending && s.DispatchesWorker()
}

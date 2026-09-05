package issue

import (
	"fmt"
	"time"
)

// Transition is a side-effect-free decision to move one Issue between durable
// lifecycle states. From is checked again when the application commits To, so
// a decision made from a stale snapshot cannot silently overwrite newer state.
type Transition struct {
	Name string
	From Status
	To   Status
}

func NewTransition(name string, from, to Status) (Transition, error) {
	if name == "" {
		return Transition{}, fmt.Errorf("Issue transition requires a name")
	}
	if err := from.Validate(); err != nil {
		return Transition{}, err
	}
	if err := to.Validate(); err != nil {
		return Transition{}, err
	}
	return Transition{Name: name, From: from, To: to}, nil
}

func newAllowedTransition(name string, from, to Status, allowed ...Status) (Transition, error) {
	transition, err := NewTransition(name, from, to)
	if err != nil {
		return Transition{}, err
	}
	for _, candidate := range allowed {
		if from == candidate {
			return transition, nil
		}
	}
	return Transition{}, fmt.Errorf("Issue transition %s does not allow status %q", name, from)
}

// ValidateCommit fences application of a decision to the state it observed.
func (t Transition) ValidateCommit(current Status) error {
	if err := current.Validate(); err != nil {
		return err
	}
	if current != t.From {
		return fmt.Errorf("Issue transition %s expected status %q, found %q", t.Name, t.From, current)
	}
	return t.To.Validate()
}

type RetryDecision struct {
	Transition  Transition
	Reason      string
	RetryAt     time.Time
	FailureKind string
}

func ScheduleRetry(from Status, reason string, retryAt time.Time, failureKind string) (RetryDecision, error) {
	transition, err := newAllowedTransition("schedule_retry", from, StatusRetryWait,
		StatusLaunching, StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge)
	if err != nil {
		return RetryDecision{}, err
	}
	if reason == "" || retryAt.IsZero() || failureKind == "" {
		return RetryDecision{}, fmt.Errorf("retry decision requires reason, retry time, and failure kind")
	}
	return RetryDecision{Transition: transition, Reason: reason, RetryAt: retryAt, FailureKind: failureKind}, nil
}

type CompletionDecision struct {
	Transition        Transition
	PullRequestURL    string
	PullRequestMerged bool
	Effect            EffectKind
}

func Complete(from Status, pullRequestURL string) (CompletionDecision, error) {
	transition, err := newAllowedTransition("complete", from, StatusCompleted,
		StatusLaunching, StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResumePending, StatusBlocked, StatusFailed)
	if err != nil {
		return CompletionDecision{}, err
	}
	return CompletionDecision{
		Transition: transition, PullRequestURL: pullRequestURL,
		PullRequestMerged: pullRequestURL != "", Effect: EffectMarkDone,
	}, nil
}

type InputDecision struct {
	Transition Transition
	Effect     EffectKind
}

func RequestInput(from Status) (InputDecision, error) {
	transition, err := newAllowedTransition("request_input", from, StatusNeedsInput,
		StatusRunning, StatusResolvingConflict)
	if err != nil {
		return InputDecision{}, err
	}
	return InputDecision{Transition: transition, Effect: EffectMarkNeedsInput}, nil
}

type ChecksDecision struct {
	Transition Transition
}

func AwaitChecks(from Status) (ChecksDecision, error) {
	transition, err := newAllowedTransition("await_pull_request_checks", from, StatusAwaitingChecks,
		StatusLaunching, StatusRunning, StatusResumePending, StatusResolvingConflict)
	return ChecksDecision{Transition: transition}, err
}

func AwaitMerge(from Status) (Transition, error) {
	return newAllowedTransition("await_pull_request_merge", from, StatusAwaitingMerge,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

func ResolveConflict(from Status) (Transition, error) {
	return newAllowedTransition("resolve_pull_request_conflict", from, StatusResolvingConflict,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

// StartClaim begins a new fenced run. StatusClaiming is accepted for an
// idempotent repeated start; failed records may be explicitly queued again.
func StartClaim(from Status) (Transition, error) {
	return newAllowedTransition("start_claim", from, StatusClaiming,
		StatusUnset, StatusClaiming, StatusFailed)
}

func ResumeAfterAnswer(from, target Status) (Transition, error) {
	switch target {
	case StatusResumePending, StatusResolvingConflict:
		return newAllowedTransition("resume_after_answer", from, target, StatusNeedsInput)
	default:
		return Transition{}, fmt.Errorf("resume after answer does not allow target status %q", target)
	}
}

func RetryConflict(from Status) (Transition, error) {
	return newAllowedTransition("retry_conflict", from, StatusResolvingConflict, StatusBlocked)
}

func ConfirmClaim(from Status) (Transition, error) {
	return newAllowedTransition("confirm_claim", from, StatusClaimed, StatusClaiming)
}

func StartClaimedWorker(from Status) (Transition, error) {
	return newAllowedTransition("start_claimed_worker", from, StatusLaunching, StatusClaimed)
}

func StartAnsweredResume(from Status) (Transition, error) {
	return newAllowedTransition("start_answered_resume", from, StatusLaunching, StatusResumePending)
}

func StartRetry(from Status) (Transition, error) {
	return newAllowedTransition("start_retry", from, StatusLaunching, StatusRetryWait)
}

func ConfirmWorkerStarted(from Status) (Transition, error) {
	return newAllowedTransition("confirm_worker_started", from, StatusRunning, StatusLaunching)
}

func RecoverUnstartedWorker(from, target Status) (Transition, error) {
	if target != StatusLaunching && target != StatusRetryWait {
		return Transition{}, fmt.Errorf("recover unstarted worker does not allow target status %q", target)
	}
	return newAllowedTransition("recover_unstarted_worker", from, target, StatusRunning)
}

func InterruptExecution(from Status) (Transition, error) {
	return newAllowedTransition("interrupt_execution", from, StatusRetryWait,
		StatusClaiming, StatusClaimed, StatusLaunching, StatusRunning)
}

type OutcomeDecision struct {
	Transition  Transition
	LastError   string
	FailureKind string
	Effect      EffectKind
}

func newOutcomeDecision(name string, from, to Status, reason, failureKind string, effect EffectKind, allowed ...Status) (OutcomeDecision, error) {
	transition, err := newAllowedTransition(name, from, to, allowed...)
	if err != nil {
		return OutcomeDecision{}, err
	}
	if reason == "" || failureKind == "" {
		return OutcomeDecision{}, fmt.Errorf("Issue outcome %s requires reason and failure kind", name)
	}
	return OutcomeDecision{Transition: transition, LastError: reason, FailureKind: failureKind, Effect: effect}, nil
}

func RejectAnsweredResume(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("reject_answered_resume", from, StatusBlocked, reason, failureKind, EffectNone, StatusLaunching)
}

func RejectWorkerWorkspace(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("reject_worker_workspace", from, StatusBlocked, reason, failureKind, EffectMarkBlocked,
		StatusLaunching, StatusRunning, StatusResolvingConflict)
}

func ExhaustPullRequestChecks(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("exhaust_pull_request_checks", from, StatusFailed, reason, failureKind, EffectMarkFailed,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

func BlockPullRequestLifecycle(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("block_pull_request_lifecycle", from, StatusBlocked, reason, failureKind, EffectMarkBlocked,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

func Fail(from Status, reason, failureKind string, blocked bool) (OutcomeDecision, error) {
	name, target := "fail_issue", StatusFailed
	if blocked {
		name, target = "block_issue", StatusBlocked
	}
	effect := EffectMarkFailed
	if blocked {
		effect = EffectMarkBlocked
	}
	return newOutcomeDecision(name, from, target, reason, failureKind, effect,
		StatusUnset, StatusClaimed, StatusLaunching, StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResumePending, StatusResolvingConflict, StatusRetryWait)
}

func SuspendWorker(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("suspend_worker", from, StatusBlocked, reason, failureKind, EffectMarkBlocked, StatusRunning)
}

func StartConflictAttempt(from Status) (Transition, error) {
	return newAllowedTransition("start_conflict_attempt", from, StatusLaunching, StatusResolvingConflict, StatusLaunching)
}

func ScheduleConflictRetry(from Status) (Transition, error) {
	return newAllowedTransition("schedule_conflict_retry", from, StatusResolvingConflict,
		StatusLaunching, StatusRunning, StatusResolvingConflict)
}

// ReconcileObservation accepts same-state transitions so repeated
// reconciliation remains idempotent.
func ReconcileObservation(from, to Status) (Transition, error) {
	if from == to {
		return NewTransition("reconcile_observation", from, to)
	}
	switch to {
	case StatusRetryWait:
		return newAllowedTransition("reconcile_observation", from, to, StatusClaiming, StatusClaimed, StatusLaunching, StatusRunning)
	case StatusAwaitingChecks, StatusAwaitingMerge:
		return newAllowedTransition("reconcile_observation", from, to, StatusCompleted)
	case StatusCompleted, StatusFailed, StatusBlocked:
		return newAllowedTransition("reconcile_observation", from, to,
			StatusClaiming, StatusClaimed, StatusLaunching, StatusRunning,
			StatusResumePending, StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks,
			StatusAwaitingMerge, StatusResolvingConflict, StatusBlocked, StatusFailed, StatusCompleted)
	default:
		return Transition{}, fmt.Errorf("reconciliation does not allow status %q to converge to %q", from, to)
	}
}

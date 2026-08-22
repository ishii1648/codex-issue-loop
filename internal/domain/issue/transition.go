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
		StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge)
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
	GitHubSync        GitHubSync
	Lease             LeaseDisposition
}

func Complete(from Status, pullRequestURL string) (CompletionDecision, error) {
	transition, err := newAllowedTransition("complete", from, StatusCompleted,
		StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge, StatusPublicationRecovery,
		StatusBlocked, StatusFailed)
	if err != nil {
		return CompletionDecision{}, err
	}
	return CompletionDecision{
		Transition: transition, PullRequestURL: pullRequestURL,
		PullRequestMerged: pullRequestURL != "", GitHubSync: GitHubSyncDone, Lease: ReleaseLease,
	}, nil
}

type InputDecision struct {
	Transition Transition
	GitHubSync GitHubSync
}

func RequestInput(from Status) (InputDecision, error) {
	transition, err := newAllowedTransition("request_input", from, StatusNeedsInput,
		StatusRunning, StatusResolvingConflict)
	if err != nil {
		return InputDecision{}, err
	}
	return InputDecision{Transition: transition, GitHubSync: GitHubSyncNeedsInput}, nil
}

type ChecksDecision struct {
	Transition Transition
}

func AwaitChecks(from Status) (ChecksDecision, error) {
	transition, err := newAllowedTransition("await_pull_request_checks", from, StatusAwaitingChecks,
		StatusRunning, StatusResolvingConflict, StatusPublicationRecovery, StatusChecksRecovery)
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

// StartClaim begins a new fenced run. StatusClaiming is accepted for the
// idempotent reserve-after-release path; failed records may be explicitly
// queued again after their GitHub labels have changed.
func StartClaim(from Status) (Transition, error) {
	return newAllowedTransition("start_claim", from, StatusClaiming,
		StatusUnset, StatusClaiming, StatusFailed)
}

// ResumeAfterAnswer selects the continuation requested by a recorded answer.
// The target depends on whether resources can be reacquired atomically and on
// the kind of question that was answered.
func ResumeAfterAnswer(from, target Status) (Transition, error) {
	switch target {
	case StatusAnswerClaimWaiting, StatusResumePending, StatusResolvingConflict:
		return newAllowedTransition("resume_after_answer", from, target, StatusNeedsInput)
	default:
		return Transition{}, fmt.Errorf("resume after answer does not allow target status %q", target)
	}
}

func RetryConflict(from Status) (Transition, error) {
	return newAllowedTransition("retry_conflict", from, StatusResolvingConflict, StatusBlocked)
}

func RequestEnvironmentResume(from Status) (Transition, error) {
	return newAllowedTransition("request_environment_resume", from, StatusEnvironmentResumePending, StatusBlocked)
}

func RecoverAnsweredWorkspace(from Status) (Transition, error) {
	return newAllowedTransition("recover_answered_workspace", from, StatusResumePending, StatusBlocked)
}

func RequestChecksRecovery(from Status) (Transition, error) {
	return newAllowedTransition("request_pull_request_checks_recovery", from, StatusChecksRecovery, StatusFailed)
}

func RequestPublicationRecovery(from Status) (Transition, error) {
	return newAllowedTransition("request_publication_recovery", from, StatusPublicationRecovery, StatusFailed)
}

func ConfirmClaim(from Status) (Transition, error) {
	return newAllowedTransition("confirm_claim", from, StatusClaimed, StatusClaiming)
}

func StartClaimedWorker(from Status) (Transition, error) {
	return newAllowedTransition("start_claimed_worker", from, StatusRunning, StatusClaimed)
}

func StartAnsweredResume(from Status) (Transition, error) {
	return newAllowedTransition("start_answered_resume", from, StatusRunning, StatusResumePending)
}

func StartEnvironmentResume(from Status) (Transition, error) {
	return newAllowedTransition("start_environment_resume", from, StatusRunning, StatusEnvironmentResumePending)
}

func StartRetry(from Status) (Transition, error) {
	return newAllowedTransition("start_retry", from, StatusRunning, StatusRetryWait)
}

func AcquireAnsweredClaim(from Status) (Transition, error) {
	return newAllowedTransition("acquire_answered_claim", from, StatusResumePending, StatusAnswerClaimWaiting)
}

func InterruptExecution(from Status) (Transition, error) {
	return newAllowedTransition("interrupt_execution", from, StatusRetryWait,
		StatusClaiming, StatusClaimed, StatusRunning)
}

type OutcomeDecision struct {
	Transition  Transition
	LastError   string
	FailureKind string
	GitHubSync  GitHubSync
}

func newOutcomeDecision(name string, from, to Status, reason, failureKind string, githubSync GitHubSync, allowed ...Status) (OutcomeDecision, error) {
	transition, err := newAllowedTransition(name, from, to, allowed...)
	if err != nil {
		return OutcomeDecision{}, err
	}
	if reason == "" || failureKind == "" {
		return OutcomeDecision{}, fmt.Errorf("Issue outcome %s requires reason and failure kind", name)
	}
	return OutcomeDecision{Transition: transition, LastError: reason, FailureKind: failureKind, GitHubSync: githubSync}, nil
}

func RejectAnsweredResume(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("reject_answered_resume", from, StatusBlocked, reason, failureKind, GitHubSyncNone, StatusResumePending)
}

func RejectWorkerWorkspace(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("reject_worker_workspace", from, StatusBlocked, reason, failureKind, GitHubSyncBlocked,
		StatusRunning, StatusResolvingConflict)
}

func RequestResourceCorrection(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("request_resource_correction", from, StatusNeedsInput, reason, failureKind, GitHubSyncNeedsInput, StatusRunning)
}

func ExhaustPullRequestChecks(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("exhaust_pull_request_checks", from, StatusFailed, reason, failureKind, GitHubSyncFailed,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

func BlockPullRequestLifecycle(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("block_pull_request_lifecycle", from, StatusBlocked, reason, failureKind, GitHubSyncBlocked,
		StatusAwaitingChecks, StatusAwaitingMerge)
}

func Fail(from Status, reason, failureKind string, blocked bool) (OutcomeDecision, error) {
	name, target := "fail_issue", StatusFailed
	if blocked {
		name, target = "block_issue", StatusBlocked
	}
	githubSync := GitHubSyncFailed
	if blocked {
		githubSync = GitHubSyncBlocked
	}
	return newOutcomeDecision(name, from, target, reason, failureKind, githubSync,
		StatusUnset, StatusClaimed, StatusRunning, StatusAwaitingChecks, StatusAwaitingMerge,
		StatusResolvingConflict, StatusPublicationRecovery, StatusRetryWait)
}

func BlockWorkerEnvironment(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("block_worker_environment", from, StatusBlocked, reason, failureKind, GitHubSyncBlocked, StatusRunning)
}

func StartConflictAttempt(from Status) (Transition, error) {
	return newAllowedTransition("start_conflict_attempt", from, StatusResolvingConflict, StatusResolvingConflict)
}

func ScheduleConflictRetry(from Status) (Transition, error) {
	return newAllowedTransition("schedule_conflict_retry", from, StatusResolvingConflict, StatusResolvingConflict)
}

type PublicationRecoveryFailureDecision struct {
	Outcome        OutcomeDecision
	RetryAt        *time.Time
	RecoveryStatus PublicationRecoveryStatus
}

func RecordPublicationRecoveryFailure(from Status, reason, failureKind string, terminal bool, retryAt time.Time) (PublicationRecoveryFailureDecision, error) {
	target, githubSync, recoveryStatus := StatusPublicationRecovery, GitHubSyncNone, PublicationRecoveryStatusRetryWait
	if terminal {
		target, githubSync, recoveryStatus = StatusFailed, GitHubSyncFailed, PublicationRecoveryStatusFailed
	} else if retryAt.IsZero() {
		return PublicationRecoveryFailureDecision{}, fmt.Errorf("non-terminal publication recovery failure requires retry time")
	}
	outcome, err := newOutcomeDecision("record_publication_recovery_failure", from, target, reason, failureKind, githubSync, StatusPublicationRecovery)
	if err != nil {
		return PublicationRecoveryFailureDecision{}, err
	}
	decision := PublicationRecoveryFailureDecision{Outcome: outcome, RecoveryStatus: recoveryStatus}
	if !terminal {
		retry := retryAt
		decision.RetryAt = &retry
	}
	return decision, nil
}

func RefusePublicationRecovery(from Status, reason, failureKind string) (OutcomeDecision, error) {
	return newOutcomeDecision("refuse_publication_recovery", from, StatusFailed, reason, failureKind, GitHubSyncFailed, StatusPublicationRecovery)
}

// ReconcileObservation validates lifecycle convergence derived from durable,
// GitHub, process, and worktree observations. Same-state convergence is
// explicit so reconciliation remains idempotent.
func ReconcileObservation(from, to Status) (Transition, error) {
	if from == to {
		return NewTransition("reconcile_observation", from, to)
	}
	switch to {
	case StatusRetryWait:
		return newAllowedTransition("reconcile_observation", from, to, StatusClaiming, StatusClaimed, StatusRunning)
	case StatusAwaitingChecks, StatusAwaitingMerge:
		return newAllowedTransition("reconcile_observation", from, to, StatusCompleted)
	case StatusCompleted, StatusFailed, StatusBlocked:
		return newAllowedTransition("reconcile_observation", from, to,
			StatusClaiming, StatusClaimed, StatusRunning, StatusAnswerClaimWaiting,
			StatusResumePending, StatusEnvironmentResumePending, StatusPublicationRecovery,
			StatusChecksRecovery, StatusRetryWait, StatusNeedsInput, StatusAwaitingChecks,
			StatusAwaitingMerge, StatusResolvingConflict, StatusBlocked, StatusFailed, StatusCompleted)
	default:
		return Transition{}, fmt.Errorf("reconciliation does not allow status %q to converge to %q", from, to)
	}
}

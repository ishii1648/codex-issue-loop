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
	GitHubSync        string
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
		PullRequestMerged: pullRequestURL != "", GitHubSync: "done",
	}, nil
}

type InputDecision struct {
	Transition Transition
	GitHubSync string
}

func RequestInput(from Status) (InputDecision, error) {
	transition, err := newAllowedTransition("request_input", from, StatusNeedsInput,
		StatusRunning, StatusResolvingConflict)
	if err != nil {
		return InputDecision{}, err
	}
	return InputDecision{Transition: transition, GitHubSync: "needs_input"}, nil
}

type ChecksDecision struct {
	Transition Transition
}

func AwaitChecks(from Status) (ChecksDecision, error) {
	transition, err := newAllowedTransition("await_pull_request_checks", from, StatusAwaitingChecks,
		StatusRunning, StatusResolvingConflict, StatusPublicationRecovery)
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

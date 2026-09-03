package issue

import "fmt"

type RequestStatus string

const (
	RequestStatusPending  RequestStatus = "pending"
	RequestStatusAnswered RequestStatus = "answered"
	RequestStatusCanceled RequestStatus = "canceled"
)

type ConflictAttemptStatus string

const (
	ConflictAttemptStatusRunning          ConflictAttemptStatus = "running"
	ConflictAttemptStatusCompleted        ConflictAttemptStatus = "completed"
	ConflictAttemptStatusNeedsInput       ConflictAttemptStatus = "needs_input"
	ConflictAttemptStatusRetryableFailure ConflictAttemptStatus = "retryable_failure"
	ConflictAttemptStatusBlocked          ConflictAttemptStatus = "blocked"
)

func validateVocabulary[T ~string](kind string, value T, allowed ...T) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("unknown %s %q", kind, value)
}

func (s RequestStatus) Validate() error {
	return validateVocabulary("request status", s, RequestStatusPending, RequestStatusAnswered, RequestStatusCanceled)
}

func (s ConflictAttemptStatus) Validate() error {
	return validateVocabulary("conflict attempt status", s, ConflictAttemptStatusRunning, ConflictAttemptStatusCompleted,
		ConflictAttemptStatusNeedsInput, ConflictAttemptStatusRetryableFailure, ConflictAttemptStatusBlocked)
}

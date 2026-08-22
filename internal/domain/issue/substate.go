package issue

import "fmt"

type ResourceParkStatus string

const (
	ResourceParkStatusParked   ResourceParkStatus = "parked"
	ResourceParkStatusResuming ResourceParkStatus = "resuming"
	ResourceParkStatusResumed  ResourceParkStatus = "resumed"
)

type RequestStatus string

const (
	RequestStatusPending  RequestStatus = "pending"
	RequestStatusAnswered RequestStatus = "answered"
)

type EnvironmentResumeStatus string

const (
	EnvironmentResumeStatusRequested    EnvironmentResumeStatus = "requested"
	EnvironmentResumeStatusGitHubSynced EnvironmentResumeStatus = "github_synced"
	EnvironmentResumeStatusRunning      EnvironmentResumeStatus = "running"
)

type PublicationRecoveryStatus string

const (
	PublicationRecoveryStatusUnset        PublicationRecoveryStatus = ""
	PublicationRecoveryStatusRequested    PublicationRecoveryStatus = "requested"
	PublicationRecoveryStatusGitHubSynced PublicationRecoveryStatus = "github_synced"
	PublicationRecoveryStatusPublishing   PublicationRecoveryStatus = "publishing"
	PublicationRecoveryStatusRetryWait    PublicationRecoveryStatus = "retry_wait"
	PublicationRecoveryStatusSucceeded    PublicationRecoveryStatus = "succeeded"
	PublicationRecoveryStatusFailed       PublicationRecoveryStatus = "failed"
)

type PublicationRecoveryAttemptStatus string

const (
	PublicationRecoveryAttemptStatusRunning   PublicationRecoveryAttemptStatus = "running"
	PublicationRecoveryAttemptStatusSucceeded PublicationRecoveryAttemptStatus = "succeeded"
	PublicationRecoveryAttemptStatusFailed    PublicationRecoveryAttemptStatus = "failed"
)

type ConflictAttemptStatus string

const (
	ConflictAttemptStatusRunning          ConflictAttemptStatus = "running"
	ConflictAttemptStatusCompleted        ConflictAttemptStatus = "completed"
	ConflictAttemptStatusNeedsInput       ConflictAttemptStatus = "needs_input"
	ConflictAttemptStatusRetryableFailure ConflictAttemptStatus = "retryable_failure"
	ConflictAttemptStatusBlocked          ConflictAttemptStatus = "blocked"
)

type PullRequestChecksRecoveryStatus string

const (
	PullRequestChecksRecoveryStatusRequested    PullRequestChecksRecoveryStatus = "requested"
	PullRequestChecksRecoveryStatusChecksFailed PullRequestChecksRecoveryStatus = "checks_failed"
	PullRequestChecksRecoveryStatusResumed      PullRequestChecksRecoveryStatus = "resumed"
)

type AnsweredWorkspaceRecoveryStatus string

const (
	AnsweredWorkspaceRecoveryStatusRequested    AnsweredWorkspaceRecoveryStatus = "requested"
	AnsweredWorkspaceRecoveryStatusGitHubSynced AnsweredWorkspaceRecoveryStatus = "github_synced"
)

type WorkspaceProvenanceRecoveryStatus string

const WorkspaceProvenanceRecoveryStatusVerified WorkspaceProvenanceRecoveryStatus = "verified"

type MergedPullRequestAdoptionStatus string

const (
	MergedPullRequestAdoptionStatusGitHubSyncPending MergedPullRequestAdoptionStatus = "github_sync_pending"
	MergedPullRequestAdoptionStatusSynced            MergedPullRequestAdoptionStatus = "synced"
)

func validateVocabulary[T ~string](kind string, value T, allowed ...T) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("unknown %s %q", kind, value)
}

func (s ResourceParkStatus) Validate() error {
	return validateVocabulary("resource park status", s, ResourceParkStatusParked, ResourceParkStatusResuming, ResourceParkStatusResumed)
}

func (s RequestStatus) Validate() error {
	return validateVocabulary("request status", s, RequestStatusPending, RequestStatusAnswered)
}

func (s EnvironmentResumeStatus) Validate() error {
	return validateVocabulary("environment resume status", s, EnvironmentResumeStatusRequested, EnvironmentResumeStatusGitHubSynced, EnvironmentResumeStatusRunning)
}

func (s PublicationRecoveryStatus) Validate() error {
	return validateVocabulary("publication recovery status", s, PublicationRecoveryStatusRequested, PublicationRecoveryStatusGitHubSynced,
		PublicationRecoveryStatusPublishing, PublicationRecoveryStatusRetryWait, PublicationRecoveryStatusSucceeded, PublicationRecoveryStatusFailed)
}

func (s PublicationRecoveryAttemptStatus) Validate() error {
	return validateVocabulary("publication recovery attempt status", s, PublicationRecoveryAttemptStatusRunning,
		PublicationRecoveryAttemptStatusSucceeded, PublicationRecoveryAttemptStatusFailed)
}

func (s ConflictAttemptStatus) Validate() error {
	return validateVocabulary("conflict attempt status", s, ConflictAttemptStatusRunning, ConflictAttemptStatusCompleted,
		ConflictAttemptStatusNeedsInput, ConflictAttemptStatusRetryableFailure, ConflictAttemptStatusBlocked)
}

func (s PullRequestChecksRecoveryStatus) Validate() error {
	return validateVocabulary("Pull Request checks recovery status", s, PullRequestChecksRecoveryStatusRequested,
		PullRequestChecksRecoveryStatusChecksFailed, PullRequestChecksRecoveryStatusResumed)
}

func (s AnsweredWorkspaceRecoveryStatus) Validate() error {
	return validateVocabulary("answered workspace recovery status", s, AnsweredWorkspaceRecoveryStatusRequested,
		AnsweredWorkspaceRecoveryStatusGitHubSynced)
}

func (s WorkspaceProvenanceRecoveryStatus) Validate() error {
	return validateVocabulary("workspace provenance recovery status", s, WorkspaceProvenanceRecoveryStatusVerified)
}

func (s MergedPullRequestAdoptionStatus) Validate() error {
	return validateVocabulary("merged Pull Request adoption status", s, MergedPullRequestAdoptionStatusGitHubSyncPending,
		MergedPullRequestAdoptionStatusSynced)
}

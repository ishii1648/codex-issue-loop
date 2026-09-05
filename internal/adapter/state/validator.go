package state

import (
	"fmt"
	"strconv"
	"strings"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

type SemanticContractVersionError struct {
	Version int
	Current int
}

func (e SemanticContractVersionError) Error() string {
	return fmt.Sprintf("snapshot semantic contract version %d does not match %d", e.Version, e.Current)
}

// Validate is the aggregate fail-closed boundary for every durable snapshot.
// Callers must run it before committing a snapshot and after completing any
// recovery, migration, or fixture reconstruction.
func (snapshot Snapshot) Validate() error {
	if snapshot.Version != CurrentVersion {
		return SchemaVersionError{Kind: "state", Version: snapshot.Version}
	}
	if snapshot.SemanticContractVersion != statecontract.CurrentVersion {
		return SemanticContractVersionError{Version: snapshot.SemanticContractVersion, Current: statecontract.CurrentVersion}
	}
	if snapshot.IssueLifecycleAPIVersion != issuedomain.LifecycleAPICurrent {
		return fmt.Errorf("snapshot Issue lifecycle API version %q does not match %q", snapshot.IssueLifecycleAPIVersion, issuedomain.LifecycleAPICurrent)
	}
	if strings.TrimSpace(snapshot.RepoID) == "" || strings.TrimSpace(snapshot.RepoPath) == "" {
		return fmt.Errorf("snapshot repository identity is incomplete")
	}
	if snapshot.Issues == nil || snapshot.PendingEffects == nil || snapshot.QuarantinedIssues == nil || snapshot.IntakeVerifications == nil || snapshot.PendingRequests == nil {
		return fmt.Errorf("snapshot aggregate maps must be initialized")
	}
	if err := validateExecutionState(snapshot); err != nil {
		return err
	}
	if err := validatePersistenceSemanticContract(snapshot); err != nil {
		return err
	}
	for key, issue := range snapshot.Issues {
		if issue == nil {
			return fmt.Errorf("Issue entry %q is null", key)
		}
		if issue.Number < 1 || key != strconv.Itoa(issue.Number) {
			return fmt.Errorf("Issue entry %q does not match Issue number %d", key, issue.Number)
		}
		if err := validateIssueAggregate(issue); err != nil {
			return fmt.Errorf("Issue #%d: %w", issue.Number, err)
		}
	}
	for key, record := range snapshot.QuarantinedIssues {
		if record == nil || record.IssueNumber < 1 || key != strconv.Itoa(record.IssueNumber) {
			return fmt.Errorf("quarantined Issue entry %q has invalid identity", key)
		}
		if snapshot.Issues[key] != nil {
			return fmt.Errorf("Issue #%d is both managed and quarantined", record.IssueNumber)
		}
		if record.ReasonCode == "" || strings.TrimSpace(record.Reason) == "" || record.QuarantinedAt.IsZero() {
			return fmt.Errorf("quarantined Issue #%d has incomplete evidence", record.IssueNumber)
		}
	}
	for key, effect := range snapshot.PendingEffects {
		if err := validateEffectIntent(snapshot, key, effect); err != nil {
			return err
		}
	}
	for key, verification := range snapshot.IntakeVerifications {
		if _, err := strconv.Atoi(key); err != nil || verification == nil || verification.Reason == "" || verification.VerifiedAt.IsZero() {
			return fmt.Errorf("Issue author verification entry %q is invalid", key)
		}
	}
	for id, request := range snapshot.PendingRequests {
		if request == nil {
			return fmt.Errorf("request entry %q is null", id)
		}
		if err := validateRequestAggregate(snapshot, id, request); err != nil {
			return err
		}
	}
	return nil
}

func validatePersistenceSemanticContract(snapshot Snapshot) error {
	violations := SemanticViolations(snapshot)
	remaining := make([]SemanticViolation, 0, len(violations))
	for _, violation := range violations {
		issue := snapshot.Issues[strconv.Itoa(violation.IssueNumber)]
		if violation.Code == SemanticCodeWorkspaceProvenanceMissing && issue != nil &&
			(issue.Status == issuedomain.StatusBlocked || issue.Status == issuedomain.StatusFailed) &&
			issue.WorkerPID == 0 && issue.WorkerPGID == 0 {
			continue
		}
		remaining = append(remaining, violation)
	}
	if len(remaining) > 0 {
		return SemanticCompatibilityError{Violations: remaining}
	}
	return nil
}

func validateIssueAggregate(issue *Issue) error {
	if err := issue.Status.Validate(); err != nil {
		return err
	}
	if issue.Attempts < 0 || issue.Continuations < 0 {
		return fmt.Errorf("attempt and continuation counters must not be negative")
	}
	if issue.WorkerPID < 0 || issue.WorkerPGID < 0 || (issue.WorkerPID == 0) != (issue.WorkerPGID == 0) {
		return fmt.Errorf("worker PID and PGID must be present or absent together")
	}
	if issue.WorkerPID > 0 && (issue.RunID == "" || !issue.Status.RequiresActiveExecution()) {
		return fmt.Errorf("worker process is not owned by an executing lifecycle")
	}
	if issue.Status == issuedomain.StatusRunning && issue.WorkerPID == 0 {
		return fmt.Errorf("running worker process identity is missing")
	}
	if issue.Status == issuedomain.StatusLaunching {
		switch issue.LaunchSource {
		case issuedomain.StatusClaimed, issuedomain.StatusResumePending, issuedomain.StatusRetryWait, issuedomain.StatusResolvingConflict:
		default:
			return fmt.Errorf("launch source %q is invalid", issue.LaunchSource)
		}
	} else if issue.LaunchSource != issuedomain.StatusUnset {
		return fmt.Errorf("launch source is retained outside launching state")
	}
	if issue.Session != nil {
		if strings.TrimSpace(issue.Session.Backend) == "" || strings.TrimSpace(issue.Session.ID) == "" || issue.SessionID != issue.Session.ID {
			return fmt.Errorf("worker session identity is incomplete or inconsistent")
		}
	} else if issue.SessionID != "" {
		return fmt.Errorf("legacy session ID is missing its typed session")
	}
	if issue.PullRequestNumber < 0 {
		return fmt.Errorf("Pull Request number must not be negative")
	}
	if issue.PullRequestNumber > 0 && issue.PullRequestURL == "" {
		return fmt.Errorf("Pull Request number has no URL")
	}
	if issue.PullRequestMerged && (issue.PullRequestNumber == 0 || issue.PullRequestURL == "" || issue.HeadSHA == "") {
		return fmt.Errorf("merged Pull Request identity is incomplete")
	}
	switch issue.ReviewDecision {
	case "", "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED":
	default:
		return fmt.Errorf("unsupported Pull Request review decision %q", issue.ReviewDecision)
	}
	if issue.RetryAfter != nil && issue.RetryAfter.IsZero() {
		return fmt.Errorf("retry deadline is zero")
	}
	if issue.Generation > 0 && issue.RunID == "" {
		return fmt.Errorf("execution generation has no run owner")
	}
	if issue.Workspace != nil && issue.Workspace.RepositoryID < 0 {
		return fmt.Errorf("workspace repository ID must not be negative")
	}
	if issue.ConflictRecovery != nil {
		for _, attempt := range issue.ConflictRecovery.History {
			if err := attempt.Status.Validate(); err != nil {
				return fmt.Errorf("conflict attempt %d: %w", attempt.Number, err)
			}
		}
	}
	if checkpoint := issue.Continuation; checkpoint != nil {
		if !ValidID(checkpoint.ID, "checkpoint_") && !ValidID(checkpoint.ID, "park_") {
			return fmt.Errorf("continuation checkpoint identity is invalid")
		}
		if checkpoint.RunID == "" || checkpoint.Generation == 0 || checkpoint.Generation > issue.Generation || checkpoint.CreatedAt.IsZero() {
			return fmt.Errorf("continuation checkpoint execution identity is incomplete")
		}
		if err := checkpoint.Stage.Validate(); err != nil {
			return fmt.Errorf("continuation checkpoint stage: %w", err)
		}
		if checkpoint.WorktreeSHA256 != "" && !validSHA256(checkpoint.WorktreeSHA256) {
			return fmt.Errorf("continuation checkpoint worktree digest is invalid")
		}
		if checkpoint.ResultSHA256 != "" && !validSHA256(checkpoint.ResultSHA256) {
			return fmt.Errorf("continuation checkpoint result digest is invalid")
		}
	}
	if err := validateSuspension(issue); err != nil {
		return err
	}
	return nil
}

func validateSuspension(issue *Issue) error {
	if issue.Suspension == nil {
		return nil
	}
	suspension := issue.Suspension
	if issue.Status == issuedomain.StatusCompleted {
		pendingAdoptionSync := suspension.Status == issuedomain.SuspensionResolved && suspension.Resolution == issuedomain.ResolutionAdoptPR &&
			issue.Continuation != nil && suspension.CheckpointID == issue.Continuation.ID
		if !pendingAdoptionSync {
			return fmt.Errorf("completed lifecycle retains a suspension outside pending adoption synchronization")
		}
	}
	if !issue.Status.Terminal() && suspension.Status != issuedomain.SuspensionResolved {
		return fmt.Errorf("active suspension is attached to executing lifecycle %q", issue.Status)
	}
	if !ValidID(suspension.ID, "suspension_") || strings.TrimSpace(suspension.ReasonCode) == "" ||
		strings.TrimSpace(suspension.Reason) == "" || suspension.SuspendedAt.IsZero() || len(suspension.AllowedActions) == 0 {
		return fmt.Errorf("suspension identity, reason, time, and actions must be complete")
	}
	switch suspension.Status {
	case issuedomain.SuspensionActive, issuedomain.SuspensionQuarantined:
		if !suspension.ResolvedAt.IsZero() || suspension.Resolution != issuedomain.ResolutionNone {
			return fmt.Errorf("active suspension contains a resolution")
		}
	case issuedomain.SuspensionResolved:
		if suspension.ResolvedAt.IsZero() || suspension.Resolution.Validate() != nil {
			return fmt.Errorf("resolved suspension has no valid resolution")
		}
	default:
		return fmt.Errorf("unknown suspension status %q", suspension.Status)
	}
	switch suspension.Recoverability {
	case issuedomain.RecoverabilityOperator, issuedomain.RecoverabilityAutomatic,
		issuedomain.RecoverabilityNone, issuedomain.RecoverabilityAmbiguous:
	default:
		return fmt.Errorf("unknown suspension recoverability %q", suspension.Recoverability)
	}
	seen := map[issuedomain.ResolutionAction]bool{}
	for _, action := range suspension.AllowedActions {
		if err := action.Validate(); err != nil || seen[action] {
			return fmt.Errorf("suspension contains an invalid or duplicate action %q", action)
		}
		seen[action] = true
	}
	if suspension.CheckpointID != "" {
		if issue.Continuation == nil || issue.Continuation.ID != suspension.CheckpointID {
			return fmt.Errorf("suspension checkpoint identity is inconsistent")
		}
	}
	return nil
}

func validateEffectIntent(snapshot Snapshot, key string, effect *EffectIntent) error {
	if effect == nil || key != strconv.Itoa(effect.IssueNumber) || !ValidID(effect.ID, "effect_") || effect.RunID == "" || effect.CreatedAt.IsZero() {
		return fmt.Errorf("pending effect entry %q has incomplete identity", key)
	}
	if err := effect.Kind.Validate(); err != nil {
		return err
	}
	issue := snapshot.Issues[key]
	if issue == nil || issue.RunID != effect.RunID {
		return fmt.Errorf("pending effect %s has no matching Issue run", effect.ID)
	}
	wantStatus := issuedomain.StatusUnset
	switch effect.Kind {
	case issuedomain.EffectMarkDone:
		wantStatus = issuedomain.StatusCompleted
	case issuedomain.EffectMarkNeedsInput:
		wantStatus = issuedomain.StatusNeedsInput
	case issuedomain.EffectMarkFailed:
		wantStatus = issuedomain.StatusFailed
	case issuedomain.EffectMarkBlocked:
		wantStatus = issuedomain.StatusBlocked
	case issuedomain.EffectRetryConflict:
		wantStatus = issuedomain.StatusResolvingConflict
	case issuedomain.EffectApplyResolution:
		if issue.Status.Terminal() || issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionResolved {
			return fmt.Errorf("Issue resolution effect has no resolved executable suspension")
		}
	}
	if wantStatus != issuedomain.StatusUnset && issue.Status != wantStatus {
		return fmt.Errorf("effect %q is incompatible with status %q", effect.Kind, issue.Status)
	}
	return nil
}

func validateRequestAggregate(snapshot Snapshot, id string, request *Request) error {
	if strings.TrimSpace(id) == "" || request.ID != id || !ValidID(id, "req_") {
		return fmt.Errorf("request entry %q has invalid identity", id)
	}
	issue := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
	if issue == nil {
		return fmt.Errorf("request %s refers to missing Issue #%d", id, request.IssueNumber)
	}
	if request.RunID != "" && request.RunID != issue.RunID {
		historicalAnsweredRun := request.Status == issuedomain.RequestStatusAnswered && request.CheckpointID != "" &&
			request.ReleasedExecution != nil && request.ReleasedExecution.RunID == request.RunID
		if !historicalAnsweredRun {
			return fmt.Errorf("request %s run does not match Issue #%d", id, issue.Number)
		}
	}
	if request.ResumeStatus != issuedomain.StatusUnset {
		if err := request.ResumeStatus.Validate(); err != nil {
			return fmt.Errorf("request %s: %w", id, err)
		}
	}
	switch request.Status {
	case issuedomain.RequestStatusPending:
		if request.Answer != "" || request.AnsweredAt != nil {
			return fmt.Errorf("pending request %s already contains an answer", id)
		}
	case issuedomain.RequestStatusAnswered:
		if strings.TrimSpace(request.Answer) == "" {
			return fmt.Errorf("answered request %s has no answer", id)
		}
	case issuedomain.RequestStatusCanceled:
		if request.Answer != "" || request.AnsweredAt != nil {
			return fmt.Errorf("canceled request %s contains an answer", id)
		}
	}
	if request.CheckpointID != "" {
		historicalCompletedRequest := issue.Status == issuedomain.StatusCompleted && issue.Continuation == nil &&
			request.Status == issuedomain.RequestStatusAnswered && request.ReleasedExecution != nil &&
			request.RunID != "" && request.ReleasedExecution.RunID == request.RunID
		if historicalCompletedRequest {
			return nil
		}
		if issue.Continuation == nil || issue.Continuation.ID != request.CheckpointID || issue.Continuation.RequestID != request.ID {
			issueCheckpointID, checkpointRequestID := "", ""
			if issue.Continuation != nil {
				issueCheckpointID, checkpointRequestID = issue.Continuation.ID, issue.Continuation.RequestID
			}
			return fmt.Errorf("request %s continuation identity is inconsistent: issue checkpoint=%q request checkpoint=%q checkpoint request=%q", id, issueCheckpointID, request.CheckpointID, checkpointRequestID)
		}
	}
	return nil
}

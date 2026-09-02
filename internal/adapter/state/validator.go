package state

import (
	"fmt"
	"strconv"
	"strings"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

// Validate is the aggregate fail-closed boundary for every durable snapshot.
// Callers must run it before committing a snapshot and after completing any
// recovery, migration, or fixture reconstruction.
func (snapshot Snapshot) Validate() error {
	if snapshot.Version != CurrentVersion {
		return SchemaVersionError{Kind: "state", Version: snapshot.Version}
	}
	if snapshot.SemanticContractVersion != statecontract.CurrentVersion {
		return fmt.Errorf("snapshot semantic contract version %d does not match %d", snapshot.SemanticContractVersion, statecontract.CurrentVersion)
	}
	if strings.TrimSpace(snapshot.RepoID) == "" || strings.TrimSpace(snapshot.RepoPath) == "" {
		return fmt.Errorf("snapshot repository identity is incomplete")
	}
	if snapshot.Issues == nil || snapshot.PendingRequests == nil {
		return fmt.Errorf("snapshot aggregate maps must be initialized")
	}
	if err := validateResourceLeases(snapshot); err != nil {
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
	if issue.Attempts < 0 || issue.Continuations < 0 {
		return fmt.Errorf("attempt and continuation counters must not be negative")
	}
	if issue.WorkerPID < 0 || issue.WorkerPGID < 0 || (issue.WorkerPID == 0) != (issue.WorkerPGID == 0) {
		return fmt.Errorf("worker PID and PGID must be present or absent together")
	}
	if issue.WorkerPID > 0 && (issue.RunID == "" || !issue.Status.OccupiesWorkerSlot()) {
		return fmt.Errorf("worker process is not owned by an executing lifecycle")
	}
	if issue.Status.Terminal() && issue.Lease != nil {
		return fmt.Errorf("terminal lifecycle must not retain an execution lease")
	}
	if issue.Status.RequiresExecutionLease() && issue.Lease == nil {
		return fmt.Errorf("executing lifecycle %q has no execution lease", issue.Status)
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
	if issue.LeaseGeneration > 0 && issue.RunID == "" {
		return fmt.Errorf("lease generation has no run owner")
	}
	if issue.Workspace != nil && issue.Workspace.RepositoryID < 0 {
		return fmt.Errorf("workspace repository ID must not be negative")
	}
	if err := validateSuspension(issue); err != nil {
		return err
	}
	return validateGitHubSyncSubstate(issue)
}

func validateSuspension(issue *Issue) error {
	if issue.Suspension == nil {
		return nil
	}
	suspension := issue.Suspension
	if issue.Status == issuedomain.StatusCompleted {
		pendingAdoptionSync := issue.GitHubSync == issuedomain.GitHubSyncDone && suspension.Status == issuedomain.SuspensionResolved &&
			suspension.Resolution == issuedomain.ResolutionAdoptPR && issue.ResourcePark != nil && suspension.CheckpointID == issue.ResourcePark.ID
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
		if issue.ResourcePark == nil || issue.ResourcePark.ID != suspension.CheckpointID {
			return fmt.Errorf("suspension checkpoint identity is inconsistent")
		}
	}
	return nil
}

func validateGitHubSyncSubstate(issue *Issue) error {
	wantStatus := issuedomain.StatusUnset
	switch issue.GitHubSync {
	case issuedomain.GitHubSyncDone:
		wantStatus = issuedomain.StatusCompleted
	case issuedomain.GitHubSyncNeedsInput:
		wantStatus = issuedomain.StatusNeedsInput
	case issuedomain.GitHubSyncFailed:
		wantStatus = issuedomain.StatusFailed
	case issuedomain.GitHubSyncBlocked:
		wantStatus = issuedomain.StatusBlocked
	case issuedomain.GitHubSyncConflictRetry:
		wantStatus = issuedomain.StatusResolvingConflict
	case issuedomain.GitHubSyncIssueResolution:
		if issue.Status.Terminal() || issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionResolved {
			return fmt.Errorf("Issue resolution synchronization has no resolved executable suspension")
		}
	}
	if wantStatus != issuedomain.StatusUnset && issue.Status != wantStatus {
		return fmt.Errorf("GitHub synchronization %q is incompatible with status %q", issue.GitHubSync, issue.Status)
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
		historicalAnsweredRun := request.Status == issuedomain.RequestStatusAnswered && request.ResourceParkID != "" &&
			request.ReleasedOwner != nil && request.ReleasedOwner.RunID == request.RunID
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
	if request.ResourceParkID != "" {
		historicalCompletedRequest := issue.Status == issuedomain.StatusCompleted && issue.ResourcePark == nil &&
			request.Status == issuedomain.RequestStatusAnswered && request.ReleasedOwner != nil &&
			request.RunID != "" && request.ReleasedOwner.RunID == request.RunID
		if historicalCompletedRequest {
			return nil
		}
		if issue.ResourcePark == nil || issue.ResourcePark.ID != request.ResourceParkID || issue.ResourcePark.RequestID != request.ID {
			issueParkID, parkRequestID := "", ""
			if issue.ResourcePark != nil {
				issueParkID, parkRequestID = issue.ResourcePark.ID, issue.ResourcePark.RequestID
			}
			return fmt.Errorf("request %s resource park identity is inconsistent: issue park=%q request park=%q park request=%q", id, issueParkID, request.ResourceParkID, parkRequestID)
		}
	}
	return nil
}

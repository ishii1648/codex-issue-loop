package state

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
)

const ContinuationKindNeedsInput = "needs_input"

const legacyLaunchAuthorityReason = "legacy running launch authority is ambiguous or inconsistent"

type ExecutionStart struct {
	IssueNumber        int
	Title              string
	RunID              string
	BaseSHA            string
	StartedAt          time.Time
	AuthorVerification *queuedomain.AuthorVerification
}

// IssueQuarantinedError reports that an Issue-local invariant boundary rejected
// execution without making the repository supervisor unhealthy.
type IssueQuarantinedError struct {
	IssueNumber int
	ReasonCode  string
	Reason      string
}

func (e IssueQuarantinedError) Error() string {
	return fmt.Sprintf("Issue #%d is quarantined (%s): %s", e.IssueNumber, e.ReasonCode, e.Reason)
}

func (s Store) StartExecution(start ExecutionStart) (Snapshot, ExecutionIdentity, error) {
	if start.IssueNumber < 1 || strings.TrimSpace(start.RunID) == "" {
		return Snapshot{}, ExecutionIdentity{}, fmt.Errorf("execution requires a positive Issue number and non-empty run ID")
	}
	if start.StartedAt.IsZero() {
		start.StartedAt = time.Now().UTC()
	} else {
		start.StartedAt = start.StartedAt.UTC()
	}
	var identity ExecutionIdentity
	payload := map[string]any{"base_sha": start.BaseSHA, "started_at": start.StartedAt}
	snapshot, err := s.Update("execution_started", start.IssueNumber, start.RunID, payload, func(snapshot *Snapshot) error {
		key := strconv.Itoa(start.IssueNumber)
		if record := snapshot.QuarantinedIssues[key]; record != nil {
			return IssueQuarantinedError{IssueNumber: start.IssueNumber, ReasonCode: record.ReasonCode, Reason: record.Reason}
		}
		if active := snapshot.ActiveExecution; active != nil {
			if active.IssueNumber != start.IssueNumber || active.RunID != start.RunID {
				return fmt.Errorf("repository active execution is owned by Issue #%d run %s generation %d", active.IssueNumber, active.RunID, active.Generation)
			}
			identity = ExecutionIdentity{RunID: active.RunID, Generation: active.Generation}
			return nil
		}
		issue := snapshot.Issues[key]
		if issue == nil {
			issue = &Issue{Number: start.IssueNumber}
			snapshot.Issues[key] = issue
		}
		transition, transitionErr := issuedomain.StartClaim(issue.Status)
		if transitionErr != nil {
			return transitionErr
		}
		if transitionErr := ApplyIssueTransition(issue, transition); transitionErr != nil {
			return transitionErr
		}
		issue.Generation++
		identity = ExecutionIdentity{RunID: start.RunID, Generation: issue.Generation}
		issue.Title, issue.RunID = start.Title, start.RunID
		if start.AuthorVerification != nil {
			verification := *start.AuthorVerification
			issue.AuthorVerification = &verification
			delete(snapshot.IntakeVerifications, key)
		}
		if issue.Attempts == 0 {
			issue.Attempts = 1
		}
		issue.UpdatedAt = start.StartedAt
		snapshot.ActiveExecution = &ActiveExecution{
			IssueNumber: issue.Number, RunID: start.RunID, Generation: issue.Generation,
			BaseSHA: start.BaseSHA, StartedAt: start.StartedAt,
		}
		snapshot.Supervisor.State = SupervisorStateRunning
		payload["identity"] = identity
		return nil
	})
	if err == nil {
		if record := snapshot.QuarantinedIssues[strconv.Itoa(start.IssueNumber)]; record != nil {
			return snapshot, ExecutionIdentity{}, IssueQuarantinedError{
				IssueNumber: start.IssueNumber, ReasonCode: record.ReasonCode, Reason: record.Reason,
			}
		}
	}
	return snapshot, identity, err
}

func ActiveIdentity(snapshot *Snapshot, issueNumber int) (ExecutionIdentity, bool) {
	if snapshot == nil || snapshot.ActiveExecution == nil || snapshot.ActiveExecution.IssueNumber != issueNumber {
		return ExecutionIdentity{}, false
	}
	active := snapshot.ActiveExecution
	return ExecutionIdentity{RunID: active.RunID, Generation: active.Generation}, true
}

func OwnsActiveExecution(snapshot *Snapshot, issueNumber int, identity ExecutionIdentity) bool {
	current, ok := ActiveIdentity(snapshot, issueNumber)
	return ok && current == identity
}

func ReleaseExecution(snapshot *Snapshot, issueNumber int, identity ExecutionIdentity) error {
	if !OwnsActiveExecution(snapshot, issueNumber, identity) {
		return fmt.Errorf("Issue #%d execution identity is stale", issueNumber)
	}
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || issue.RunID != identity.RunID || issue.Generation != identity.Generation {
		return fmt.Errorf("Issue #%d execution identity does not match its aggregate", issueNumber)
	}
	snapshot.ActiveExecution = nil
	issue.WorkerPID, issue.WorkerPGID = 0, 0
	return nil
}

func CaptureContinuation(snapshot *Snapshot, issueNumber int, identity ExecutionIdentity, checkpointID string, createdAt time.Time) error {
	if strings.TrimSpace(checkpointID) == "" || createdAt.IsZero() {
		return fmt.Errorf("continuation identity and timestamp are required")
	}
	if !OwnsActiveExecution(snapshot, issueNumber, identity) {
		return fmt.Errorf("Issue #%d execution identity is stale", issueNumber)
	}
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil {
		return fmt.Errorf("Issue #%d is missing", issueNumber)
	}
	active := snapshot.ActiveExecution
	issue.Continuation = &ContinuationCheckpoint{
		ID: checkpointID, CreatedAt: createdAt.UTC(), RunID: identity.RunID, Generation: identity.Generation,
		BaseSHA: active.BaseSHA, Workspace: cloneWorkspace(issue.Workspace), Session: cloneSession(issue.Session),
		HeadSHA: issue.HeadSHA, PullRequestURL: issue.PullRequestURL, PullRequestNumber: issue.PullRequestNumber,
		Stage: issuedomain.ContinuationStageForStatus(issue.Status),
	}
	return ReleaseExecution(snapshot, issueNumber, identity)
}

func ResumeContinuation(snapshot *Snapshot, issueNumber int, checkpointID string, resumedAt time.Time) (ExecutionIdentity, error) {
	if snapshot == nil || resumedAt.IsZero() {
		return ExecutionIdentity{}, fmt.Errorf("snapshot and resume timestamp are required")
	}
	if snapshot.ActiveExecution != nil {
		return ExecutionIdentity{}, fmt.Errorf("repository active execution is occupied by Issue #%d", snapshot.ActiveExecution.IssueNumber)
	}
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || issue.Continuation == nil || issue.Continuation.ID != checkpointID {
		return ExecutionIdentity{}, fmt.Errorf("Issue #%d continuation %s is missing", issueNumber, checkpointID)
	}
	checkpoint := issue.Continuation
	if checkpoint.RunID != issue.RunID || checkpoint.Generation == 0 || checkpoint.Generation > issue.Generation {
		return ExecutionIdentity{}, fmt.Errorf("Issue #%d continuation identity is inconsistent", issueNumber)
	}
	issue.Generation++
	identity := ExecutionIdentity{RunID: issue.RunID, Generation: issue.Generation}
	snapshot.ActiveExecution = &ActiveExecution{
		IssueNumber: issue.Number, RunID: identity.RunID, Generation: identity.Generation,
		BaseSHA: checkpoint.BaseSHA, StartedAt: resumedAt.UTC(),
	}
	return identity, nil
}

func TransferExecution(snapshot *Snapshot, issueNumber int, current ExecutionIdentity, newRunID string, startedAt time.Time) (ExecutionIdentity, error) {
	if strings.TrimSpace(newRunID) == "" || startedAt.IsZero() || !OwnsActiveExecution(snapshot, issueNumber, current) {
		return ExecutionIdentity{}, fmt.Errorf("execution transfer identity is invalid")
	}
	issue := snapshot.Issues[strconv.Itoa(issueNumber)]
	if issue == nil || issue.RunID != current.RunID || issue.Generation != current.Generation {
		return ExecutionIdentity{}, fmt.Errorf("Issue #%d execution transfer is stale", issueNumber)
	}
	issue.Generation++
	issue.RunID = newRunID
	identity := ExecutionIdentity{RunID: newRunID, Generation: issue.Generation}
	snapshot.ActiveExecution.RunID = newRunID
	snapshot.ActiveExecution.Generation = issue.Generation
	snapshot.ActiveExecution.StartedAt = startedAt.UTC()
	return identity, nil
}

func ValidateNeedsInputContinuation(issue *Issue, request *Request) error {
	if issue == nil || request == nil || issue.Continuation == nil {
		return fmt.Errorf("needs-input continuation is incomplete")
	}
	checkpoint := issue.Continuation
	if checkpoint.Kind != ContinuationKindNeedsInput || checkpoint.RequestID != request.ID ||
		request.CheckpointID != checkpoint.ID || request.IssueNumber != issue.Number || request.RunID != issue.RunID ||
		request.ReleasedExecution == nil || request.ReleasedExecution.RunID != checkpoint.RunID ||
		request.ReleasedExecution.Generation != checkpoint.Generation {
		return fmt.Errorf("Issue #%d needs-input continuation identity is inconsistent", issue.Number)
	}
	return nil
}

// RecoverUnstartedConflictLaunch releases only a conflict launch whose complete
// persisted identity chain proves that no worker process identity was recorded.
func (s Store) RecoverUnstartedConflictLaunch(now time.Time) (Snapshot, bool, error) {
	if now.IsZero() {
		return Snapshot{}, false, fmt.Errorf("unstarted conflict launch recovery requires an observation time")
	}
	snapshot, err := s.Load()
	if err != nil {
		return Snapshot{}, false, err
	}
	active := snapshot.ActiveExecution
	if active == nil {
		return snapshot, false, nil
	}
	issue := snapshot.Issues[strconv.Itoa(active.IssueNumber)]
	if issue == nil || issue.Status != issuedomain.StatusLaunching || issue.LaunchSource != issuedomain.StatusResolvingConflict || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
		return snapshot, false, nil
	}
	expected := *active
	if err := validateUnstartedConflictLaunch(&snapshot, issue); err != nil {
		return snapshot, false, err
	}
	payload := map[string]any{
		"launch_source": issue.LaunchSource, "run_id": issue.RunID, "generation": issue.Generation,
		"checkpoint_id": issue.Continuation.ID, "observed_at": now.UTC(),
	}
	updated, err := s.Update("unstarted_conflict_launch_recovered", issue.Number, issue.RunID, payload, func(latest *Snapshot) error {
		current := latest.Issues[strconv.Itoa(issue.Number)]
		if latest.ActiveExecution == nil || *latest.ActiveExecution != expected || current == nil {
			return fmt.Errorf("Issue #%d conflict launch changed before recovery", issue.Number)
		}
		if err := validateUnstartedConflictLaunch(latest, current); err != nil {
			return err
		}
		transition, err := issuedomain.AbortWorkerLaunch(current.Status, current.LaunchSource)
		if err != nil {
			return err
		}
		identity := ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
		if err := ReleaseExecution(latest, current.Number, identity); err != nil {
			return err
		}
		if err := ApplyIssueTransition(current, transition); err != nil {
			return err
		}
		current.UpdatedAt = now.UTC()
		return nil
	})
	if err != nil {
		return Snapshot{}, false, err
	}
	return updated, true, nil
}

func validateUnstartedConflictLaunch(snapshot *Snapshot, issue *Issue) error {
	if snapshot == nil || issue == nil || snapshot.ActiveExecution == nil {
		return fmt.Errorf("unstarted conflict launch evidence is incomplete")
	}
	active := snapshot.ActiveExecution
	if issue.Status != issuedomain.StatusLaunching || issue.LaunchSource != issuedomain.StatusResolvingConflict ||
		issue.WorkerPID != 0 || issue.WorkerPGID != 0 || active.IssueNumber != issue.Number ||
		active.RunID != issue.RunID || active.Generation != issue.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return fmt.Errorf("Issue #%d unstarted conflict launch execution identity is inconsistent", issue.Number)
	}
	checkpoint := issue.Continuation
	if checkpoint == nil || checkpoint.ID == "" || checkpoint.RunID != issue.RunID || checkpoint.Generation == 0 || checkpoint.Generation+1 != issue.Generation ||
		active.StartedAt.Before(checkpoint.CreatedAt) ||
		checkpoint.BaseSHA != active.BaseSHA || checkpoint.HeadSHA != issue.HeadSHA || checkpoint.PullRequestURL != issue.PullRequestURL ||
		checkpoint.PullRequestNumber != issue.PullRequestNumber || !reflect.DeepEqual(checkpoint.Workspace, issue.Workspace) {
		return fmt.Errorf("Issue #%d unstarted conflict launch continuation identity is inconsistent", issue.Number)
	}
	if checkpoint.Stage != issuedomain.ContinuationStageChecks && checkpoint.Stage != issuedomain.ContinuationStageConflict {
		return fmt.Errorf("Issue #%d unstarted conflict launch continuation stage is inconsistent", issue.Number)
	}
	recovery := issue.ConflictRecovery
	if recovery == nil || recovery.PullRequestURL == "" || recovery.PullRequestURL != issue.PullRequestURL ||
		recovery.PreviousBaseSHA != active.BaseSHA || recovery.TargetBaseSHA == "" || recovery.OriginalHeadSHA != issue.HeadSHA || len(recovery.ConflictFiles) == 0 {
		return fmt.Errorf("Issue #%d unstarted conflict launch recovery context is inconsistent", issue.Number)
	}
	return nil
}

// NormalizeLegacyWorkerLaunches recovers only launch authority that is proven
// by the complete persisted execution and continuation identity chain.
func NormalizeLegacyWorkerLaunches(snapshot *Snapshot) {
	if snapshot == nil {
		return
	}
	for _, issue := range snapshot.Issues {
		if issue == nil || issue.Status != issuedomain.StatusRunning || issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
			continue
		}
		source, reason, ok := legacyWorkerLaunchSource(snapshot, issue)
		if ok {
			transition, err := issuedomain.RecoverUnstartedWorker(issue.Status, issuedomain.StatusLaunching)
			if err == nil {
				_ = ApplyIssueTransition(issue, transition)
				issue.LaunchSource = source
			}
			continue
		}

		transition, err := issuedomain.RecoverUnstartedWorker(issue.Status, issuedomain.StatusBlocked)
		if err != nil {
			continue
		}
		if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issue.Number {
			snapshot.ActiveExecution = nil
		}
		_ = ApplyIssueTransition(issue, transition)
		issue.WorkerPID, issue.WorkerPGID = 0, 0
		issue.LastError = legacyLaunchAuthorityReason + ": " + reason
		issue.FailureKind = "issue"
		issue.RetryAfter = nil
		checkpointID := ""
		if issue.Continuation != nil {
			checkpointID = issue.Continuation.ID
		}
		issue.Suspension = &Suspension{
			ID: "suspension_legacy_launch_" + fmt.Sprint(issue.Number), Origin: "runtime", Status: issuedomain.SuspensionQuarantined,
			ReasonCode: "legacy_launch_authority", Recoverability: issuedomain.RecoverabilityAmbiguous,
			Reason: issue.LastError, AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel},
			CheckpointID: checkpointID, SuspendedAt: legacyLaunchObservedAt(snapshot, issue),
		}
	}
}

func legacyWorkerLaunchSource(snapshot *Snapshot, issue *Issue) (issuedomain.Status, string, bool) {
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != issue.Number || active.RunID != issue.RunID ||
		active.Generation != issue.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return issuedomain.StatusUnset, "active execution does not match the Issue run and generation", false
	}

	checkpoint := issue.Continuation
	answeredEvidence := len(issue.Answers) > 0
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issue.Number && request.Status == issuedomain.RequestStatusAnswered {
			answeredEvidence = true
		}
	}
	if checkpoint == nil {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence has no continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind == "" && checkpoint.RequestID == "" {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence is not bound to the continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind != ContinuationKindNeedsInput || checkpoint.RequestID == "" || checkpoint.RunID != issue.RunID ||
		checkpoint.Generation == 0 || checkpoint.Generation >= issue.Generation || checkpoint.Generation+1 != issue.Generation {
		return issuedomain.StatusUnset, "needs-input continuation identity does not match the Issue run and generation", false
	}
	request := snapshot.PendingRequests[checkpoint.RequestID]
	if request == nil || request.ID != checkpoint.RequestID || request.IssueNumber != issue.Number ||
		request.CheckpointID != checkpoint.ID || request.RunID != issue.RunID || request.ReleasedExecution == nil ||
		request.ReleasedExecution.RunID != checkpoint.RunID || request.ReleasedExecution.Generation != checkpoint.Generation ||
		request.Status != issuedomain.RequestStatusAnswered ||
		strings.TrimSpace(request.Answer) == "" || request.AnsweredAt == nil || request.AnsweredAt.IsZero() {
		return issuedomain.StatusUnset, "answered request does not match the continuation identity", false
	}
	answerCount := 0
	for _, answer := range issue.Answers {
		if answer.RequestID != checkpoint.RequestID {
			continue
		}
		answerCount++
		if answer.Question != request.Question || answer.Answer != request.Answer || answer.AnsweredAt.IsZero() || !answer.AnsweredAt.Equal(*request.AnsweredAt) {
			return issuedomain.StatusUnset, "recorded answer does not match the answered request", false
		}
	}
	if answerCount != 1 {
		return issuedomain.StatusUnset, "answered request does not have exactly one matching answer record", false
	}
	return issuedomain.StatusResumePending, "", true
}

func legacyLaunchObservedAt(snapshot *Snapshot, issue *Issue) time.Time {
	for _, observed := range []time.Time{issue.UpdatedAt, snapshot.Supervisor.UpdatedAt} {
		if !observed.IsZero() {
			return observed.UTC()
		}
	}
	if snapshot.ActiveExecution != nil && !snapshot.ActiveExecution.StartedAt.IsZero() {
		return snapshot.ActiveExecution.StartedAt.UTC()
	}
	if issue.Continuation != nil && !issue.Continuation.CreatedAt.IsZero() {
		return issue.Continuation.CreatedAt.UTC()
	}
	return time.Unix(1, 0).UTC()
}

func validateExecutionState(snapshot Snapshot) error {
	var executing *Issue
	for _, issue := range snapshot.Issues {
		if issue == nil || !issue.Status.RequiresActiveExecution() {
			continue
		}
		if executing != nil {
			return fmt.Errorf("Issues #%d and #%d both claim the single active execution", executing.Number, issue.Number)
		}
		executing = issue
	}
	if executing == nil {
		if snapshot.ActiveExecution != nil {
			return fmt.Errorf("active execution has no executing Issue")
		}
		return nil
	}
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != executing.Number || active.RunID != executing.RunID ||
		active.Generation != executing.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return fmt.Errorf("Issue #%d does not match repository active execution", executing.Number)
	}
	if executing.Status == issuedomain.StatusLaunching && executing.WorkerPID == 0 && executing.Continuation != nil &&
		executing.Continuation.Kind == ContinuationKindNeedsInput &&
		(executing.LaunchSource == issuedomain.StatusResumePending || executing.LaunchSource == issuedomain.StatusRetryWait) {
		source, reason, ok := legacyWorkerLaunchSource(&snapshot, executing)
		if !ok || source != executing.LaunchSource {
			if reason == "" {
				reason = fmt.Sprintf("evidence authorizes %s instead of %s", source, executing.LaunchSource)
			}
			return fmt.Errorf("Issue #%d launch source is inconsistent with answered continuation evidence: %s", executing.Number, reason)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

package state

import (
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"slices"
	"strconv"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// ApplyIssueTransition is the persistence commit boundary for a domain
// lifecycle decision. It fences the write to the status observed when the
// decision was made; ownership fields remain fenced by the surrounding
// transaction.
func ApplyIssueTransition(item *Issue, transition issuedomain.Transition) error {
	if item == nil {
		return fmt.Errorf("cannot apply Issue transition %s to a missing Issue", transition.Name)
	}
	if err := transition.ValidateCommit(item.Status); err != nil {
		return err
	}
	if transition.To.Terminal() && !transition.From.Terminal() && item.Suspension != nil && item.Suspension.Status == issuedomain.SuspensionResolved {
		item.Suspension = nil
	}
	item.Status = transition.To
	if transition.From == issuedomain.StatusLaunching && transition.To != issuedomain.StatusLaunching {
		item.LaunchSource = issuedomain.StatusUnset
	}
	if transition.To == issuedomain.StatusCompleted {
		item.Continuation = nil
		item.Suspension = nil
	}
	return nil
}

func ApplyNotPlannedCancellation(snapshot *Snapshot, issueNumber int, expected *Issue, now time.Time) (string, error) {
	if err := statecontract.ValidateNotPlannedCancellation(snapshot, issueNumber, expected, now); err != nil {
		return "", err
	}
	item := snapshot.Issues[strconv.Itoa(issueNumber)]
	effect := PendingEffect(snapshot, issueNumber)
	releaseResult := "not_present"
	previous := item.Status
	transition, err := issuedomain.ReconcileObservation(previous, issuedomain.StatusCanceled)
	if err != nil {
		return "", err
	}
	if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issueNumber {
		identity := ExecutionIdentity{RunID: item.RunID, Generation: item.Generation}
		if err := ReleaseExecution(snapshot, issueNumber, identity); err != nil {
			return "", err
		}
		releaseResult = "released"
	}
	if effect != nil {
		delete(snapshot.PendingEffects, strconv.Itoa(issueNumber))
	}
	item.GitHubStateReason = "NOT_PLANNED"
	if err := ApplyIssueTransition(item, transition); err != nil {
		return "", err
	}
	if item.Suspension != nil {
		item.Suspension.Status = issuedomain.SuspensionResolved
		item.Suspension.Resolution = issuedomain.ResolutionCancel
		item.Suspension.ResolvedAt = now.UTC()
	}
	item.Cancellation = &Cancellation{
		Source: "github_not_planned", GitHubStateReason: "NOT_PLANNED", PreviousStatus: previous,
		ExecutionReleaseResult: releaseResult, CanceledAt: now.UTC(),
	}
	item.LastError = ""
	item.RetryAfter = nil
	item.UpdatedAt = now.UTC()
	return releaseResult, nil
}

func CancelPendingRequests(snapshot *Snapshot, issueNumber int) {
	if snapshot == nil {
		return
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			request.Status = issuedomain.RequestStatusCanceled
		}
	}
}

func CancelQuarantinedIssue(snapshot *Snapshot, issueNumber int, now time.Time) error {
	key := strconv.Itoa(issueNumber)
	record := snapshot.QuarantinedIssues[key]
	if record == nil || snapshot.Issues[key] != nil || now.IsZero() {
		return fmt.Errorf("Issue #%d quarantine is unavailable", issueNumber)
	}
	if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issueNumber {
		return fmt.Errorf("Issue #%d quarantine retains execution authority", issueNumber)
	}
	item := &Issue{Number: issueNumber, RunID: record.RunID, Generation: record.Generation, Status: issuedomain.StatusBlocked}
	if saved := record.LastValid; saved != nil {
		item.Title, item.Branch, item.Worktree, item.Workspace = saved.Title, saved.Branch, saved.Worktree, saved.Workspace
		item.Session, item.SessionID, item.Answers = saved.Session, saved.SessionID, saved.Answers
		item.Attempts, item.Continuations, item.ExecutionProfile, item.WorkerIdentity = saved.Attempts, saved.Continuations, saved.ExecutionProfile, saved.WorkerIdentity
		item.PullRequestURL, item.PullRequestNumber, item.HeadSHA, item.PullRequestMerged = saved.PullRequestURL, saved.PullRequestNumber, saved.HeadSHA, saved.PullRequestMerged
	}
	transition, err := issuedomain.ResolveSuspension(item.Status, issuedomain.ResolutionCancel, issuedomain.ContinuationStageNone)
	if err != nil {
		return err
	}
	if err := ApplyIssueTransition(item, transition); err != nil {
		return err
	}
	item.Cancellation = &Cancellation{Source: "operator_quarantine_resolution", PreviousStatus: issuedomain.StatusBlocked, ExecutionReleaseResult: "not_present", CanceledAt: now}
	item.UpdatedAt = now
	snapshot.Issues[key] = item
	delete(snapshot.QuarantinedIssues, key)
	return nil
}

func ApplyRequestAnswer(request *Request, answer string, now time.Time) error {
	status, err := issuedomain.AnswerRequest(request.Status, request.Answer, answer)
	if err != nil {
		return err
	}
	if request.Status == status {
		return nil
	}
	at := now.UTC()
	request.Status, request.Answer, request.AnsweredAt = status, answer, &at
	return nil
}

func RestoreInput(snapshot *Snapshot, number int, head, digest string, now time.Time) error {
	q := snapshot.QuarantinedIssues[strconv.Itoa(number)]
	item, err := InputRecoveryCandidate(q)
	if err != nil {
		return err
	}
	if snapshot.Issues[strconv.Itoa(number)] != nil || snapshot.ActiveExecution != nil || head == "" || digest == "" || now.IsZero() {
		return fmt.Errorf("input recovery boundary changed")
	}
	r := q.Requests[0]
	if snapshot.PendingRequests[r.ID] != nil {
		return fmt.Errorf("input request ID already exists")
	}
	transition, err := issuedomain.RestoreRejectedInput(item.Status)
	if err != nil {
		return err
	}
	base := item.Continuation.BaseSHA
	item.Suspension = nil
	item.WorkerPID, item.WorkerPGID = 0, 0
	item.Continuation = &ContinuationCheckpoint{ID: r.CheckpointID, CreatedAt: r.CreatedAt, RunID: item.RunID, Generation: item.Generation, BaseSHA: base, Workspace: cloneWorkspace(item.Workspace), Session: cloneSession(item.Session), HeadSHA: head, WorktreeSHA256: digest, Kind: ContinuationKindNeedsInput, RequestID: r.ID, Stage: issuedomain.ContinuationStageResume}
	if err := ApplyIssueTransition(item, transition); err != nil {
		return err
	}
	item.LastError, item.FailureKind = "", ""
	item.RetryAfter, item.UpdatedAt = nil, now.UTC()
	snapshot.Issues[strconv.Itoa(number)] = item
	snapshot.PendingRequests[r.ID] = r
	delete(snapshot.QuarantinedIssues, strconv.Itoa(number))
	return nil
}

func RestoreAnsweredChecks(snapshot *Snapshot, number int, head, digest string, now time.Time) error {
	key := strconv.Itoa(number)
	q := snapshot.QuarantinedIssues[key]
	item, err := AnsweredChecksRecoveryCandidate(q)
	if err != nil {
		return err
	}
	if snapshot.Issues[key] != nil || snapshot.ActiveExecution != nil || head == "" || !validSHA256(digest) || now.IsZero() {
		return fmt.Errorf("checks recovery boundary changed")
	}
	for _, r := range q.Requests {
		if snapshot.PendingRequests[r.ID] != nil {
			return fmt.Errorf("checks request ID already exists")
		}
	}
	transition, err := issuedomain.RestoreRejectedChecks(item.Status)
	if err != nil {
		return err
	}
	base := item.Continuation.BaseSHA
	item.Continuation = &ContinuationCheckpoint{
		ID: NewID("checkpoint"), CreatedAt: now.UTC(), RunID: item.RunID, Generation: item.Generation,
		BaseSHA: base, Workspace: cloneWorkspace(item.Workspace), Session: cloneSession(item.Session),
		HeadSHA: head, WorktreeSHA256: digest, PullRequestURL: item.PullRequestURL, PullRequestNumber: item.PullRequestNumber,
		Stage: issuedomain.ContinuationStageChecks,
	}
	if err := ApplyIssueTransition(item, transition); err != nil {
		return err
	}
	item.RetryAfter, item.UpdatedAt = nil, now.UTC()
	snapshot.Issues[key] = item
	for _, r := range q.Requests {
		snapshot.PendingRequests[r.ID] = r
	}
	delete(snapshot.QuarantinedIssues, key)
	return nil
}

type OperatorResolutionObservation = statecontract.OperatorResolutionObservation

func CanAdoptHead(item *Issue) bool     { return statecontract.CanAdoptHead(item) }
func CanAdoptWorktree(item *Issue) bool { return statecontract.CanAdoptWorktree(item) }

func ResolveOperatorSuspension(snapshot *Snapshot, number int, action issuedomain.ResolutionAction, observed OperatorResolutionObservation, now time.Time) error {
	adoptedStage, err := statecontract.ValidateOperatorResolution(snapshot, number, action, observed, now)
	if err != nil {
		return err
	}
	item := snapshot.Issues[strconv.Itoa(number)]
	if action == issuedomain.ResolutionAdoptHead {
		item.Continuation.HeadSHA = observed.HeadSHA
	} else if action == issuedomain.ResolutionAdoptWorktree {
		item.Continuation.WorktreeSHA256 = observed.WorktreeSHA256
		item.Continuation.Stage = adoptedStage
		item.ConflictRecovery.AllowedPaths = append(slices.Clone(item.ConflictRecovery.AllowedPaths), observed.AllowedPaths...)
		slices.Sort(item.ConflictRecovery.AllowedPaths)
		item.ConflictRecovery.AllowedPaths = slices.Compact(item.ConflictRecovery.AllowedPaths)
		item.Suspension.Status = issuedomain.SuspensionActive
		item.Suspension.Recoverability = issuedomain.RecoverabilityOperator
		item.Suspension.MissingEvidence = nil
		item.Suspension.AllowedActions = []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage}
	} else if action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage {
		if action == issuedomain.ResolutionRetryStage && item.ConflictRecovery != nil {
			item.ConflictRecovery.Attempts = 0
			item.ConflictRecovery.UpdatedAt = now
		}
		if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStagePublish {
			if observed.RepairPublicationHead {
				item.Continuation.HeadSHA = observed.HeadSHA
			}
			item.Continuation.Summary = observed.ResultSummary
			item.Continuation.ResultSHA256 = observed.ResultSHA256
		}
		if _, err := ResumeContinuation(snapshot, item.Number, item.Continuation.ID, now); err != nil {
			return err
		}
		transition, err := issuedomain.ResolveSuspension(item.Status, action, item.Continuation.Stage)
		if err != nil {
			return err
		}
		if err := ApplyIssueTransition(item, transition); err != nil {
			return err
		}
		if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStageChecks {
			item.HeadSHA = observed.HeadSHA
			item.PullRequestNumber = observed.PullRequestNumber
		}
		if err := SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectApplyResolution, now); err != nil {
			return err
		}
	} else if action == issuedomain.ResolutionAdoptPR {
		transition, transitionErr := issuedomain.ResolveSuspension(item.Status, action, issuedomain.ContinuationStageNone)
		if transitionErr != nil {
			return transitionErr
		}
		item.PullRequestURL = observed.PullRequestURL
		item.PullRequestNumber = observed.PullRequestNumber
		item.HeadSHA = observed.HeadSHA
		item.PullRequestMerged = true
		item.Suspension.Status = issuedomain.SuspensionResolved
		item.Suspension.Resolution = action
		item.Suspension.ResolvedAt = now
		if err := SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectMarkDone, now); err != nil {
			return err
		}
		if err := ApplyIssueTransition(item, transition); err != nil {
			return err
		}
	} else {
		previous := item.Status
		transition, transitionErr := issuedomain.ResolveSuspension(item.Status, action, issuedomain.ContinuationStageNone)
		if transitionErr != nil {
			return transitionErr
		}
		if err := ApplyIssueTransition(item, transition); err != nil {
			return err
		}
		CancelPendingRequests(snapshot, item.Number)
		if err := SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectNone, now); err != nil {
			return err
		}
		item.Cancellation = &Cancellation{
			Source: "operator_resolution", GitHubStateReason: observed.GitHubStateReason,
			PreviousStatus: previous, ExecutionReleaseResult: "not_present", CanceledAt: now,
		}
		item.GitHubStateReason = observed.GitHubStateReason
	}
	if item.Suspension != nil && action != issuedomain.ResolutionAdoptWorktree && action != issuedomain.ResolutionAdoptHead {
		item.Suspension.Status = issuedomain.SuspensionResolved
		item.Suspension.Resolution = action
		item.Suspension.ResolvedAt = now
	}
	item.UpdatedAt = now
	return nil
}

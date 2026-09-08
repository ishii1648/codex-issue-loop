package state

import (
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
	if snapshot == nil || expected == nil || now.IsZero() {
		return "", fmt.Errorf("not planned cancellation requires snapshot, expected Issue, and time")
	}
	item := snapshot.Issues[strconv.Itoa(issueNumber)]
	if item == nil || expected.Number != issueNumber || item.Status != expected.Status || item.RunID != expected.RunID || item.Generation != expected.Generation {
		return "", fmt.Errorf("Issue #%d changed before not planned cancellation", issueNumber)
	}
	releaseResult := "not_present"
	if active := snapshot.ActiveExecution; active != nil && active.IssueNumber == issueNumber {
		if active.RunID != item.RunID || active.Generation != item.Generation {
			return "", fmt.Errorf("Issue #%d cannot be canceled because execution authority ownership is inconsistent", issueNumber)
		}
	}
	if item.WorkerPID != 0 || item.WorkerPGID != 0 {
		return "", fmt.Errorf("Issue #%d cannot be canceled while worker process identity is present", issueNumber)
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			return "", fmt.Errorf("Issue #%d cannot be canceled while an operator request is pending", issueNumber)
		}
	}
	effect := PendingEffect(snapshot, issueNumber)
	if effect != nil {
		if effect.RunID != item.RunID || effect.Kind != issuedomain.EffectMarkBlocked && effect.Kind != issuedomain.EffectMarkFailed {
			return "", fmt.Errorf("Issue #%d cannot be canceled with pending effect %q", issueNumber, effect.Kind)
		}
	}
	if !strings.EqualFold(item.GitHubStateReason, "NOT_PLANNED") {
		return "", fmt.Errorf("Issue #%d does not have authoritative NOT_PLANNED state reason", issueNumber)
	}
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

func CanAdoptHead(item *Issue) bool {
	if item == nil || item.Continuation == nil || item.Suspension == nil {
		return false
	}
	c, s := item.Continuation, item.Suspension
	return (item.Status == issuedomain.StatusBlocked || item.Status == issuedomain.StatusFailed) &&
		s.Status == issuedomain.SuspensionActive && s.Origin == "worker" && slices.Contains(s.AllowedActions, issuedomain.ResolutionResume) &&
		s.CheckpointID == c.ID && c.RunID == item.RunID && c.Generation == item.Generation &&
		c.Stage == issuedomain.ContinuationStageResume && c.HeadSHA == "" && c.WorktreeSHA256 != "" &&
		c.Session != nil && c.Session.ID != "" && c.Workspace != nil && reflect.DeepEqual(c.Workspace, item.Workspace) &&
		item.WorkerPID == 0 && item.WorkerPGID == 0 && item.ConflictRecovery == nil &&
		item.PullRequestURL == "" && item.PullRequestNumber == 0 && c.PullRequestURL == "" && c.PullRequestNumber == 0
}

func CanAdoptWorktree(item *Issue) bool {
	return item != nil && (item.Status == issuedomain.StatusBlocked || item.Status == issuedomain.StatusFailed) &&
		item.Continuation != nil && item.Continuation.WorktreeSHA256 == "" && item.ConflictRecovery != nil &&
		item.Suspension != nil && item.Suspension.Status == issuedomain.SuspensionQuarantined &&
		item.Suspension.Recoverability == issuedomain.RecoverabilityAmbiguous && item.Suspension.CheckpointID == item.Continuation.ID &&
		len(item.Suspension.MissingEvidence) == 1 && item.Suspension.MissingEvidence[0] == "worktree_sha256" &&
		len(item.Suspension.AllowedActions) == 1 && item.Suspension.AllowedActions[0] == issuedomain.ResolutionCancel
}

type OperatorResolutionObservation struct {
	HeadSHA               string
	WorktreeSHA256        string
	AllowedPaths          []string
	ResultSummary         string
	ResultSHA256          string
	RepairPublicationHead bool
	PullRequestURL        string
	PullRequestNumber     int
	GitHubStateReason     string
}

func ResolveOperatorSuspension(snapshot *Snapshot, number int, action issuedomain.ResolutionAction, observed OperatorResolutionObservation, now time.Time) error {
	if snapshot == nil || now.IsZero() {
		return fmt.Errorf("operator resolution requires snapshot and time")
	}
	item := snapshot.Issues[strconv.Itoa(number)]
	if item == nil || item.Number != number || item.RunID == "" || item.Suspension == nil || item.WorkerPID != 0 || item.WorkerPGID != 0 {
		return fmt.Errorf("Issue #%d suspension is unavailable or retains worker identity", number)
	}
	if snapshot.ActiveExecution != nil && (action != issuedomain.ResolutionCancel && action != issuedomain.ResolutionAdoptPR || snapshot.ActiveExecution.IssueNumber == number) {
		return fmt.Errorf("Issue #%d execution slot changed", number)
	}
	var adoptedStage issuedomain.ContinuationStage
	if action == issuedomain.ResolutionAdoptHead || action == issuedomain.ResolutionAdoptWorktree {
		if action == issuedomain.ResolutionAdoptHead && (!CanAdoptHead(item) || observed.HeadSHA == "") ||
			action == issuedomain.ResolutionAdoptWorktree && (!CanAdoptWorktree(item) || !validSHA256(observed.WorktreeSHA256)) {
			return fmt.Errorf("Issue #%d checkpoint adoption evidence is inconsistent", number)
		}
		if item.Continuation.RunID != item.RunID || item.Continuation.Generation == 0 || item.Continuation.Generation > item.Generation {
			return fmt.Errorf("Issue #%d continuation identity is inconsistent", number)
		}
		var err error
		adoptedStage, err = issuedomain.AdoptCheckpoint(item.Status, item.Suspension.Status, item.Continuation.Stage, action)
		if err != nil {
			return err
		}
	} else {
		if !slices.Contains(item.Suspension.AllowedActions, action) || (item.Suspension.Status != issuedomain.SuspensionActive && !(item.Suspension.Status == issuedomain.SuspensionQuarantined && action == issuedomain.ResolutionCancel)) {
			return fmt.Errorf("Issue #%d action is not allowed by suspension", number)
		}
		stage := issuedomain.ContinuationStageNone
		if item.Continuation != nil {
			stage = item.Continuation.Stage
		}
		if _, err := issuedomain.ResolveSuspension(item.Status, action, stage); err != nil {
			return err
		}
		if action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage {
			if item.Continuation == nil || item.Suspension.CheckpointID != item.Continuation.ID || item.Continuation.RunID != item.RunID || item.Continuation.Generation == 0 || item.Continuation.Generation > item.Generation {
				return fmt.Errorf("Issue #%d continuation identity is inconsistent", number)
			}
			if action == issuedomain.ResolutionRetryStage && stage == issuedomain.ContinuationStagePublish && (observed.ResultSummary == "" || !validSHA256(observed.ResultSHA256) || observed.RepairPublicationHead && observed.HeadSHA == "") {
				return fmt.Errorf("Issue #%d publication evidence is incomplete", number)
			}
			if action == issuedomain.ResolutionRetryStage && stage == issuedomain.ContinuationStageChecks && (observed.HeadSHA == "" || observed.PullRequestNumber <= 0) {
				return fmt.Errorf("Issue #%d checks Pull Request identity is incomplete", number)
			}
		}
	}
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
		if observed.PullRequestURL == "" || observed.PullRequestNumber <= 0 || observed.HeadSHA == "" {
			return fmt.Errorf("Issue #%d matching merged Pull Request changed after planning", number)
		}
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

package statecontract

import (
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"reflect"
	"slices"
	"strconv"
	"time"
)

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

func ValidateOperatorResolution(snapshot *Snapshot, number int, action issuedomain.ResolutionAction, observed OperatorResolutionObservation, now time.Time) (issuedomain.ContinuationStage, error) {
	if snapshot == nil || now.IsZero() {
		return issuedomain.ContinuationStageNone, fmt.Errorf("operator resolution requires snapshot and time")
	}
	item := snapshot.Issues[strconv.Itoa(number)]
	if item == nil || item.Number != number || item.RunID == "" || item.Suspension == nil || item.WorkerPID != 0 || item.WorkerPGID != 0 {
		return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d suspension is unavailable or retains worker identity", number)
	}
	if snapshot.ActiveExecution != nil && (action != issuedomain.ResolutionCancel && action != issuedomain.ResolutionAdoptPR || snapshot.ActiveExecution.IssueNumber == number) {
		return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d execution slot changed", number)
	}
	var adoptedStage issuedomain.ContinuationStage
	if action == issuedomain.ResolutionAdoptHead || action == issuedomain.ResolutionAdoptWorktree {
		if action == issuedomain.ResolutionAdoptHead && (!CanAdoptHead(item) || observed.HeadSHA == "") ||
			action == issuedomain.ResolutionAdoptWorktree && (!CanAdoptWorktree(item) || !ValidSHA256(observed.WorktreeSHA256)) {
			return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d checkpoint adoption evidence is inconsistent", number)
		}
		if item.Continuation.RunID != item.RunID || item.Continuation.Generation == 0 || item.Continuation.Generation > item.Generation {
			return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d continuation identity is inconsistent", number)
		}
		var err error
		adoptedStage, err = issuedomain.AdoptCheckpoint(item.Status, item.Suspension.Status, item.Continuation.Stage, action)
		if err != nil {
			return issuedomain.ContinuationStageNone, err
		}
	} else {
		if !slices.Contains(item.Suspension.AllowedActions, action) || (item.Suspension.Status != issuedomain.SuspensionActive && !(item.Suspension.Status == issuedomain.SuspensionQuarantined && action == issuedomain.ResolutionCancel)) {
			return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d action is not allowed by suspension", number)
		}
		stage := issuedomain.ContinuationStageNone
		if item.Continuation != nil {
			stage = item.Continuation.Stage
		}
		if _, err := issuedomain.ResolveSuspension(item.Status, action, stage); err != nil {
			return issuedomain.ContinuationStageNone, err
		}
		if action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage {
			if item.Continuation == nil || item.Suspension.CheckpointID != item.Continuation.ID || item.Continuation.RunID != item.RunID || item.Continuation.Generation == 0 || item.Continuation.Generation > item.Generation {
				return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d continuation identity is inconsistent", number)
			}
			if action == issuedomain.ResolutionRetryStage && stage == issuedomain.ContinuationStagePublish && (observed.ResultSummary == "" || !ValidSHA256(observed.ResultSHA256) || observed.RepairPublicationHead && observed.HeadSHA == "") {
				return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d publication evidence is incomplete", number)
			}
			if action == issuedomain.ResolutionRetryStage && stage == issuedomain.ContinuationStageChecks && (observed.HeadSHA == "" || observed.PullRequestNumber <= 0) {
				return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d checks Pull Request identity is incomplete", number)
			}
		}
	}
	if action == issuedomain.ResolutionAdoptPR {
		if observed.PullRequestURL == "" || observed.PullRequestNumber <= 0 || observed.HeadSHA == "" {
			return issuedomain.ContinuationStageNone, fmt.Errorf("Issue #%d matching merged Pull Request changed after planning", number)
		}

	}
	return adoptedStage, nil
}

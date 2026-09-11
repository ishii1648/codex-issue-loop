package issue

import (
	"fmt"
	"path"
	"strings"
)

type ResolutionAction string

const (
	ResolutionApproveConflictPaths ResolutionAction = "approve-conflict-paths"
	ResolutionNone                 ResolutionAction = ""
	ResolutionResume               ResolutionAction = "resume"
	ResolutionRetryStage           ResolutionAction = "retry-stage"
	ResolutionAdoptWorktree        ResolutionAction = "adopt-worktree"
	ResolutionAdoptHead            ResolutionAction = "adopt-head"
	ResolutionAdoptInput           ResolutionAction = "adopt-input"
	ResolutionAdoptPR              ResolutionAction = "adopt-pr"
	ResolutionCancel               ResolutionAction = "cancel"
)

func (a ResolutionAction) Validate() error {
	switch a {
	case ResolutionApproveConflictPaths, ResolutionResume, ResolutionRetryStage, ResolutionAdoptHead, ResolutionAdoptInput, ResolutionAdoptWorktree, ResolutionAdoptPR, ResolutionCancel:
		return nil
	default:
		return fmt.Errorf("unknown Issue resolution action %q", a)
	}
}

func ResolveSuspension(from Status, action ResolutionAction, checkpointStage ContinuationStage) (Transition, error) {
	if err := action.Validate(); err != nil {
		return Transition{}, err
	}
	if from != StatusBlocked && from != StatusFailed {
		return Transition{}, fmt.Errorf("Issue suspension cannot be resolved from status %q", from)
	}
	switch action {
	case ResolutionResume:
		return NewTransition("resolve_suspension_resume", from, StatusResumePending)
	case ResolutionRetryStage:
		target := StatusResumePending
		switch checkpointStage {
		case ContinuationStageChecks:
			target = StatusAwaitingChecks
		case ContinuationStageConflict:
			target = StatusResolvingConflict
		case ContinuationStageResume, ContinuationStagePublish:
		default:
			return Transition{}, fmt.Errorf("cannot retry unknown continuation stage %q", checkpointStage)
		}
		return NewTransition("resolve_suspension_retry_stage", from, target)
	case ResolutionAdoptPR:
		return NewTransition("resolve_suspension_adopt_pr", from, StatusCompleted)
	case ResolutionCancel:
		return NewTransition("resolve_suspension_cancel", from, StatusCanceled)
	default:
		return Transition{}, fmt.Errorf("unknown Issue resolution action %q", action)
	}
}

type SuspensionStatus string

const (
	SuspensionActive      SuspensionStatus = "active"
	SuspensionQuarantined SuspensionStatus = "quarantined"
	SuspensionResolved    SuspensionStatus = "resolved"
)

type Recoverability string

const (
	RecoverabilityOperator  Recoverability = "operator"
	RecoverabilityAutomatic Recoverability = "automatic"
	RecoverabilityNone      Recoverability = "none"
	RecoverabilityAmbiguous Recoverability = "ambiguous"
)

func AdoptCheckpoint(from Status, suspension SuspensionStatus, stage ContinuationStage, action ResolutionAction) (ContinuationStage, error) {
	if from != StatusBlocked && from != StatusFailed {
		return ContinuationStageNone, fmt.Errorf("checkpoint cannot be adopted from status %q", from)
	}
	switch action {
	case ResolutionAdoptHead:
		if suspension == SuspensionActive && stage == ContinuationStageResume {
			return stage, nil
		}
	case ResolutionAdoptWorktree:
		if suspension == SuspensionQuarantined && (stage == ContinuationStageResume || stage == ContinuationStageConflict) {
			return ContinuationStageConflict, nil
		}
	}
	return ContinuationStageNone, fmt.Errorf("cannot adopt checkpoint stage %q with suspension %q using %q", stage, suspension, action)
}

func ApproveConflictPaths(from Status, suspension SuspensionStatus, recoverability Recoverability, stage ContinuationStage, retryAllowed bool) error {
	if (from != StatusBlocked && from != StatusFailed) || suspension != SuspensionActive || recoverability != RecoverabilityOperator || !retryAllowed ||
		(stage != ContinuationStageResume && stage != ContinuationStageConflict) {
		return fmt.Errorf("conflict paths require an active retryable conflict checkpoint")
	}
	return nil
}

func ValidateConflictApprovalPaths(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("at least one explicit conflict path is required")
	}
	for _, value := range paths {
		if value == "" || value == "." || value == ".." || path.IsAbs(value) || path.Clean(value) != value || strings.HasPrefix(value, "../") || strings.ContainsAny(value, "\\\x00\r\n") {
			return fmt.Errorf("invalid conflict approval path %q", value)
		}
	}
	return nil
}

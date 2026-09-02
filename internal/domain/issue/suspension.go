package issue

import "fmt"

type ResolutionAction string

const (
	ResolutionNone       ResolutionAction = ""
	ResolutionResume     ResolutionAction = "resume"
	ResolutionRetryStage ResolutionAction = "retry-stage"
	ResolutionAdoptPR    ResolutionAction = "adopt-pr"
	ResolutionCancel     ResolutionAction = "cancel"
)

func (a ResolutionAction) Validate() error {
	switch a {
	case ResolutionResume, ResolutionRetryStage, ResolutionAdoptPR, ResolutionCancel:
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
		return NewTransition("resolve_suspension_cancel", from, from)
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

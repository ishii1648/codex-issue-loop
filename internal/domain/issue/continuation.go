package issue

import "fmt"

// ContinuationStage identifies the deterministic lifecycle boundary restored
// from one generic checkpoint.
type ContinuationStage string

const (
	ContinuationStageNone     ContinuationStage = ""
	ContinuationStageResume   ContinuationStage = "resume"
	ContinuationStagePublish  ContinuationStage = "publish"
	ContinuationStageChecks   ContinuationStage = "checks"
	ContinuationStageConflict ContinuationStage = "conflict"
)

func (s ContinuationStage) Validate() error {
	switch s {
	case ContinuationStageResume, ContinuationStagePublish, ContinuationStageChecks, ContinuationStageConflict:
		return nil
	default:
		return fmt.Errorf("unknown continuation stage %q", s)
	}
}

func ContinuationStageForStatus(status Status) ContinuationStage {
	switch status {
	case StatusAwaitingChecks, StatusAwaitingMerge:
		return ContinuationStageChecks
	case StatusResolvingConflict:
		return ContinuationStageConflict
	default:
		return ContinuationStageResume
	}
}

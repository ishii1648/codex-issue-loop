package issue

type HumanWait struct {
	Status           Status
	Unanswered       bool
	Quarantined      bool
	Recoverability   Recoverability
	SuspensionStatus SuspensionStatus
	ReviewDecision   string
	AutoMerge        bool
}

func (w HumanWait) Reason() string {
	if w.Status == StatusCompleted || w.Status == StatusCanceled {
		return ""
	}
	if w.Unanswered {
		return "answer_required"
	}
	if w.Quarantined {
		return "recovery_required"
	}
	if (w.Status == StatusBlocked || w.Status == StatusFailed) && w.SuspensionStatus != SuspensionResolved && (w.Recoverability == RecoverabilityOperator || w.Recoverability == RecoverabilityAmbiguous || w.Recoverability == RecoverabilityNone) {
		return "recovery_required"
	}
	if w.Status == StatusAwaitingChecks || w.Status == StatusAwaitingMerge {
		if w.ReviewDecision == "REVIEW_REQUIRED" || w.ReviewDecision == "CHANGES_REQUESTED" {
			return "review_required"
		}
		if w.Status == StatusAwaitingMerge && !w.AutoMerge {
			return "merge_required"
		}
	}
	return ""
}

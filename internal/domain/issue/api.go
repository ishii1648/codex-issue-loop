package issue

import (
	"fmt"
	"time"
)

const (
	LifecycleAPICurrent       = "2.1"
	LifecycleAPIPreviousMinor = "2.0"
	LifecycleAPIMinimum       = "1.0"
)

type IntentKind string

const (
	IntentStart           IntentKind = "start"
	IntentWorkerResult    IntentKind = "worker_result"
	IntentAnswer          IntentKind = "answer"
	IntentReconcile       IntentKind = "reconcile"
	IntentResolve         IntentKind = "resolve"
	IntentEffectCompleted IntentKind = "effect_completed"
)

type Intent struct {
	Kind       IntentKind
	Target     Status
	Reason     string
	RetryAt    *time.Time
	Resolution ResolutionAction
}

type Observation struct {
	RunID      string
	Generation uint64
	At         time.Time
}

type Decision struct {
	Transition Transition
	AuditCode  string
}

// Decide is the single deterministic lifecycle entry point. More detailed
// outcome data remains in typed decisions while call sites migrate to this
// common transition boundary.
func Decide(current Status, intent Intent, observation Observation) (Decision, error) {
	if intent.Kind == "" || observation.At.IsZero() {
		return Decision{}, fmt.Errorf("lifecycle decision requires intent and observation time")
	}
	var transition Transition
	var err error
	switch intent.Kind {
	case IntentStart:
		transition, err = StartClaim(current)
	case IntentWorkerResult, IntentReconcile, IntentEffectCompleted:
		transition, err = ReconcileObservation(current, intent.Target)
	case IntentAnswer:
		transition, err = ResumeAfterAnswer(current, intent.Target)
	case IntentResolve:
		transition, err = ResolveSuspension(current, intent.Resolution, ContinuationStageForStatus(current))
	default:
		err = fmt.Errorf("unknown lifecycle intent %q", intent.Kind)
	}
	if err != nil {
		return Decision{}, err
	}
	return Decision{Transition: transition, AuditCode: string(intent.Kind)}, nil
}

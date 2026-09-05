package issue

// RetryMode describes which execution resource a retry consumes. Keeping this
// decision in the domain prevents worker, checks, and scheduler paths from
// drifting on the same budget boundary.
type RetryMode uint8

const (
	RetryExhausted RetryMode = iota
	RetryContinuation
	RetryFreshAttempt
)

type RetryBudget struct {
	Extended         bool
	Resumable        bool
	Attempts         int
	MaxAttempts      int
	Continuations    int
	MaxContinuations int
}

func (b RetryBudget) Decide() RetryMode {
	if b.Extended && b.Resumable && b.Continuations < b.MaxContinuations {
		return RetryContinuation
	}
	if b.Attempts < b.MaxAttempts {
		return RetryFreshAttempt
	}
	return RetryExhausted
}

// DelayIndex is the already-consumed work count used by the retry backoff.
func (b RetryBudget) DelayIndex() int { return b.Attempts + b.Continuations }

type PublicationRetryBudget struct {
	Attempts    int
	MaxAttempts int
}

func (b PublicationRetryBudget) Allowed() bool   { return b.Attempts < b.MaxAttempts }
func (b PublicationRetryBudget) DelayIndex() int { return b.Attempts }

type AttemptBudget struct {
	Attempts    int
	MaxAttempts int
}

func (b AttemptBudget) Exhausted() bool { return b.Attempts >= b.MaxAttempts }

type ConflictRetryBudget struct {
	Attempts          int
	MaxAttempts       int
	HasRunningAttempt bool
}

func (b ConflictRetryBudget) EffectiveAttempts() int {
	if b.HasRunningAttempt {
		return b.Attempts
	}
	return b.Attempts + 1
}

func (b ConflictRetryBudget) Allowed() bool {
	return b.EffectiveAttempts() < b.MaxAttempts
}

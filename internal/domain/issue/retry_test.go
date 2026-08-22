package issue

import "testing"

func TestRetryBudgetDecision(t *testing.T) {
	tests := []struct {
		name   string
		budget RetryBudget
		want   RetryMode
	}{
		{"continuation first", RetryBudget{Extended: true, Resumable: true, Attempts: 3, MaxAttempts: 3, Continuations: 0, MaxContinuations: 1}, RetryContinuation},
		{"fresh attempt", RetryBudget{Attempts: 1, MaxAttempts: 2}, RetryFreshAttempt},
		{"exhausted", RetryBudget{Attempts: 2, MaxAttempts: 2}, RetryExhausted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.budget.Decide(); got != tt.want {
				t.Fatalf("Decide()=%v want %v", got, tt.want)
			}
		})
	}
}

func TestConflictRetryBudgetCountsNonWorkerFailure(t *testing.T) {
	running := ConflictRetryBudget{Attempts: 1, MaxAttempts: 2, HasRunningAttempt: true}
	if running.EffectiveAttempts() != 1 || !running.Allowed() {
		t.Fatalf("running attempt budget=%+v", running)
	}
	preparation := ConflictRetryBudget{Attempts: 1, MaxAttempts: 2}
	if preparation.EffectiveAttempts() != 2 || preparation.Allowed() {
		t.Fatalf("preparation failure budget=%+v", preparation)
	}
}

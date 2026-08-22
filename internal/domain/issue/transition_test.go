package issue

import (
	"testing"
	"time"
)

func TestTransitionRejectsStaleCommit(t *testing.T) {
	transition, err := NewTransition("complete", StatusRunning, StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if err := transition.ValidateCommit(StatusRetryWait); err == nil {
		t.Fatal("expected stale lifecycle decision to be rejected")
	}
}

func TestScheduleRetryIsPureDecision(t *testing.T) {
	retryAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	decision, err := ScheduleRetry(StatusRunning, "temporary failure", retryAt, "transient")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Transition.From != StatusRunning || decision.Transition.To != StatusRetryWait || decision.RetryAt != retryAt {
		t.Fatalf("unexpected retry decision: %+v", decision)
	}
}

func TestCompleteDerivesPublicationState(t *testing.T) {
	decision, err := Complete(StatusAwaitingMerge, "https://example.test/pull/7")
	if err != nil {
		t.Fatal(err)
	}
	if !decision.PullRequestMerged || decision.GitHubSync != "done" || decision.Transition.To != StatusCompleted {
		t.Fatalf("unexpected completion decision: %+v", decision)
	}
}

func TestLifecycleDecisionRejectsInvalidSourceState(t *testing.T) {
	if _, err := AwaitMerge(StatusRunning); err == nil {
		t.Fatal("expected running -> awaiting_merge to require the checks boundary")
	}
	if _, err := RequestInput(StatusCompleted); err == nil {
		t.Fatal("expected completed Issue to reject a new input request")
	}
}

func TestLifecycleRecoveryAndIdempotentTransitions(t *testing.T) {
	if _, err := Complete(StatusBlocked, "https://example.test/pull/7"); err != nil {
		t.Fatalf("merged Pull Request must complete a terminal local record: %v", err)
	}
	if _, err := AwaitMerge(StatusAwaitingMerge); err != nil {
		t.Fatalf("merge observation must be idempotent: %v", err)
	}
	if _, err := ScheduleRetry(StatusAwaitingChecks, "checks failed", time.Now().UTC(), "transient"); err != nil {
		t.Fatalf("checks failure must return to retry: %v", err)
	}
	if _, err := ScheduleRetry(StatusAwaitingMerge, "checks regressed", time.Now().UTC(), "transient"); err != nil {
		t.Fatalf("checks failure observed while awaiting merge must return to retry: %v", err)
	}
}

func TestRecoveryTransitions(t *testing.T) {
	tests := []struct {
		name string
		make func() (Transition, error)
		to   Status
	}{
		{name: "start fresh claim", make: func() (Transition, error) { return StartClaim(StatusUnset) }, to: StatusClaiming},
		{name: "resume answer", make: func() (Transition, error) { return ResumeAfterAnswer(StatusNeedsInput, StatusResumePending) }, to: StatusResumePending},
		{name: "wait for answer resources", make: func() (Transition, error) { return ResumeAfterAnswer(StatusNeedsInput, StatusAnswerClaimWaiting) }, to: StatusAnswerClaimWaiting},
		{name: "retry conflict", make: func() (Transition, error) { return RetryConflict(StatusBlocked) }, to: StatusResolvingConflict},
		{name: "resume environment", make: func() (Transition, error) { return RequestEnvironmentResume(StatusBlocked) }, to: StatusEnvironmentResumePending},
		{name: "recover workspace", make: func() (Transition, error) { return RecoverAnsweredWorkspace(StatusBlocked) }, to: StatusResumePending},
		{name: "recover checks", make: func() (Transition, error) { return RequestChecksRecovery(StatusFailed) }, to: StatusChecksRecovery},
		{name: "resume recovered checks", make: func() (Transition, error) {
			decision, err := AwaitChecks(StatusChecksRecovery)
			return decision.Transition, err
		}, to: StatusAwaitingChecks},
		{name: "recover publication", make: func() (Transition, error) { return RequestPublicationRecovery(StatusFailed) }, to: StatusPublicationRecovery},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transition, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			if transition.To != test.to {
				t.Fatalf("transition=%+v want target %q", transition, test.to)
			}
		})
	}
}

func TestRecoveryTransitionsRejectUnrelatedStatesAndTargets(t *testing.T) {
	if _, err := RequestChecksRecovery(StatusRunning); err == nil {
		t.Fatal("running Issue must not enter checks recovery")
	}
	if _, err := RetryConflict(StatusFailed); err == nil {
		t.Fatal("failed Issue must not enter conflict retry")
	}
	if _, err := StartClaim(StatusCompleted); err == nil {
		t.Fatal("completed Issue must not start another claim")
	}
	if _, err := ResumeAfterAnswer(StatusNeedsInput, StatusCompleted); err == nil {
		t.Fatal("answer must not select an unrelated lifecycle target")
	}
}

func TestExecutionTransitions(t *testing.T) {
	tests := []struct {
		name string
		make func() (Transition, error)
		from Status
		to   Status
	}{
		{name: "confirm claim", make: func() (Transition, error) { return ConfirmClaim(StatusClaiming) }, from: StatusClaiming, to: StatusClaimed},
		{name: "start claimed worker", make: func() (Transition, error) { return StartClaimedWorker(StatusClaimed) }, from: StatusClaimed, to: StatusRunning},
		{name: "start answered resume", make: func() (Transition, error) { return StartAnsweredResume(StatusResumePending) }, from: StatusResumePending, to: StatusRunning},
		{name: "start environment resume", make: func() (Transition, error) { return StartEnvironmentResume(StatusEnvironmentResumePending) }, from: StatusEnvironmentResumePending, to: StatusRunning},
		{name: "start retry", make: func() (Transition, error) { return StartRetry(StatusRetryWait) }, from: StatusRetryWait, to: StatusRunning},
		{name: "acquire answered claim", make: func() (Transition, error) { return AcquireAnsweredClaim(StatusAnswerClaimWaiting) }, from: StatusAnswerClaimWaiting, to: StatusResumePending},
		{name: "interrupt claim", make: func() (Transition, error) { return InterruptExecution(StatusClaiming) }, from: StatusClaiming, to: StatusRetryWait},
		{name: "interrupt running", make: func() (Transition, error) { return InterruptExecution(StatusRunning) }, from: StatusRunning, to: StatusRetryWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transition, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			if transition.From != test.from || transition.To != test.to {
				t.Fatalf("transition=%+v want %q -> %q", transition, test.from, test.to)
			}
		})
	}
}

func TestExecutionTransitionsRejectUnrelatedSources(t *testing.T) {
	if _, err := ConfirmClaim(StatusRunning); err == nil {
		t.Fatal("running Issue must not be confirmed as a claim")
	}
	if _, err := StartRetry(StatusClaimed); err == nil {
		t.Fatal("claimed Issue must not use the retry start transition")
	}
	if _, err := InterruptExecution(StatusAwaitingChecks); err == nil {
		t.Fatal("checks wait must not be interrupted as worker execution")
	}
}

func TestStatusOperationalPredicates(t *testing.T) {
	tests := []struct {
		status            Status
		occupiesSlot      bool
		capabilityRecheck bool
		webhookTerminal   bool
		webhookRoutable   bool
	}{
		{status: StatusClaiming, occupiesSlot: true, capabilityRecheck: true, webhookRoutable: true},
		{status: StatusRunning, occupiesSlot: true, webhookRoutable: true},
		{status: StatusRetryWait, capabilityRecheck: true, webhookRoutable: true},
		{status: StatusNeedsInput, webhookTerminal: true, webhookRoutable: true},
		{status: StatusPublicationRecovery},
		{status: StatusCompleted, webhookTerminal: true},
	}
	for _, test := range tests {
		if got := test.status.OccupiesWorkerSlot(); got != test.occupiesSlot {
			t.Errorf("%s OccupiesWorkerSlot=%v want %v", test.status, got, test.occupiesSlot)
		}
		if got := test.status.RequiresCapabilityRecheck(); got != test.capabilityRecheck {
			t.Errorf("%s RequiresCapabilityRecheck=%v want %v", test.status, got, test.capabilityRecheck)
		}
		if got := test.status.TerminalForWebhook(); got != test.webhookTerminal {
			t.Errorf("%s TerminalForWebhook=%v want %v", test.status, got, test.webhookTerminal)
		}
		if got := test.status.WebhookRoutable(); got != test.webhookRoutable {
			t.Errorf("%s WebhookRoutable=%v want %v", test.status, got, test.webhookRoutable)
		}
	}
}

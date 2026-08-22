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

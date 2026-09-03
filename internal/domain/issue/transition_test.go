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
	if !decision.PullRequestMerged || decision.GitHubSync != GitHubSyncDone || decision.Transition.To != StatusCompleted {
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
		{name: "recover workspace", make: func() (Transition, error) { return RecoverAnsweredWorkspace(StatusBlocked) }, to: StatusResumePending},
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

func TestResolveSuspensionUsesGenericActionsAndSavedStage(t *testing.T) {
	tests := []struct {
		action ResolutionAction
		stage  ContinuationStage
		want   Status
	}{
		{action: ResolutionResume, stage: ContinuationStageChecks, want: StatusResumePending},
		{action: ResolutionRetryStage, stage: ContinuationStageChecks, want: StatusAwaitingChecks},
		{action: ResolutionRetryStage, stage: ContinuationStagePublish, want: StatusResumePending},
		{action: ResolutionRetryStage, stage: ContinuationStageResume, want: StatusResumePending},
		{action: ResolutionRetryStage, stage: ContinuationStageConflict, want: StatusResolvingConflict},
		{action: ResolutionAdoptPR, want: StatusCompleted},
		{action: ResolutionCancel, want: StatusBlocked},
	}
	for _, test := range tests {
		transition, err := ResolveSuspension(StatusBlocked, test.action, test.stage)
		if err != nil || transition.To != test.want {
			t.Fatalf("action=%s stage=%s transition=%+v err=%v", test.action, test.stage, transition, err)
		}
	}
	if _, err := ResolveSuspension(StatusRunning, ResolutionResume, ContinuationStageResume); err == nil {
		t.Fatal("running Issue accepted terminal suspension resolution")
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

func TestOutcomeDecisionsDeriveCompanionState(t *testing.T) {
	tests := []struct {
		name   string
		make   func() (OutcomeDecision, error)
		to     Status
		sync   GitHubSync
		reason string
	}{
		{name: "workspace block", make: func() (OutcomeDecision, error) {
			return RejectWorkerWorkspace(StatusRunning, "unsafe workspace", "issue")
		}, to: StatusBlocked, sync: GitHubSyncBlocked, reason: "unsafe workspace"},
		{name: "resource correction", make: func() (OutcomeDecision, error) {
			return RequestResourceCorrection(StatusRunning, "claim mismatch", "issue")
		}, to: StatusNeedsInput, sync: GitHubSyncNeedsInput, reason: "claim mismatch"},
		{name: "checks exhausted", make: func() (OutcomeDecision, error) {
			return ExhaustPullRequestChecks(StatusAwaitingChecks, "checks failed", "issue")
		}, to: StatusFailed, sync: GitHubSyncFailed, reason: "checks failed"},
		{name: "generic failure", make: func() (OutcomeDecision, error) { return Fail(StatusRunning, "worker failed", "issue", false) }, to: StatusFailed, sync: GitHubSyncFailed, reason: "worker failed"},
		{name: "generic block", make: func() (OutcomeDecision, error) { return Fail(StatusResolvingConflict, "manual repair", "issue", true) }, to: StatusBlocked, sync: GitHubSyncBlocked, reason: "manual repair"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision, err := test.make()
			if err != nil {
				t.Fatal(err)
			}
			if decision.Transition.To != test.to || decision.GitHubSync != test.sync || decision.LastError != test.reason || decision.FailureKind != "issue" {
				t.Fatalf("unexpected outcome decision: %+v", decision)
			}
		})
	}
}

func TestOutcomeDecisionsRejectIncompleteOrUnrelatedInput(t *testing.T) {
	if _, err := RejectAnsweredResume(StatusRunning, "remote changed", "issue"); err == nil {
		t.Fatal("running Issue must not use answered resume rejection")
	}
	if _, err := RequestResourceCorrection(StatusRunning, "", "issue"); err == nil {
		t.Fatal("outcome decision must require a reason")
	}
	if _, err := Fail(StatusCompleted, "failed", "issue", false); err == nil {
		t.Fatal("completed Issue must not fail again")
	}
}

func TestReconcileObservationTransitions(t *testing.T) {
	tests := []struct{ from, to Status }{
		{from: StatusRunning, to: StatusRetryWait},
		{from: StatusCompleted, to: StatusAwaitingChecks},
		{from: StatusFailed, to: StatusCompleted},
		{from: StatusNeedsInput, to: StatusBlocked},
		{from: StatusRetryWait, to: StatusRetryWait},
	}
	for _, test := range tests {
		if _, err := ReconcileObservation(test.from, test.to); err != nil {
			t.Errorf("reconcile %q -> %q: %v", test.from, test.to, err)
		}
	}
	if _, err := ReconcileObservation(StatusRetryWait, StatusRunning); err == nil {
		t.Fatal("reconciliation must not implicitly start a worker")
	}
}

func TestStatusOperationalPredicates(t *testing.T) {
	tests := []struct {
		status          Status
		occupiesSlot    bool
		webhookTerminal bool
		webhookRoutable bool
	}{
		{status: StatusClaiming, occupiesSlot: true, webhookRoutable: true},
		{status: StatusRunning, occupiesSlot: true, webhookRoutable: true},
		{status: StatusRetryWait, webhookRoutable: true},
		{status: StatusNeedsInput, webhookTerminal: true, webhookRoutable: true},
		{status: StatusCompleted, webhookTerminal: true},
	}
	for _, test := range tests {
		if got := test.status.OccupiesWorkerSlot(); got != test.occupiesSlot {
			t.Errorf("%s OccupiesWorkerSlot=%v want %v", test.status, got, test.occupiesSlot)
		}
		if got := test.status.TerminalForWebhook(); got != test.webhookTerminal {
			t.Errorf("%s TerminalForWebhook=%v want %v", test.status, got, test.webhookTerminal)
		}
		if got := test.status.WebhookRoutable(); got != test.webhookRoutable {
			t.Errorf("%s WebhookRoutable=%v want %v", test.status, got, test.webhookRoutable)
		}
	}
}

func TestStatusSchedulingPredicates(t *testing.T) {
	tests := []struct {
		status        Status
		pending       bool
		retainsLogs   bool
		preventsIdle  bool
		ineligible    bool
		usesSlot      bool
		blocksPRQueue bool
	}{
		{status: StatusUnset, usesSlot: true},
		{status: StatusClaiming, pending: true, retainsLogs: true, preventsIdle: true, usesSlot: true},
		{status: StatusRunning, retainsLogs: true, preventsIdle: true, ineligible: true, usesSlot: true},
		{status: StatusNeedsInput, retainsLogs: true, ineligible: true, usesSlot: true},
		{status: StatusAwaitingChecks, pending: true, retainsLogs: true, preventsIdle: true, blocksPRQueue: true},
		{status: StatusAwaitingMerge, pending: true, retainsLogs: true, preventsIdle: true},
		{status: StatusCompleted, ineligible: true, usesSlot: true},
	}
	for _, test := range tests {
		if got := test.status.PendingDispatch(); got != test.pending {
			t.Errorf("%s PendingDispatch=%v want %v", test.status, got, test.pending)
		}
		if got := test.status.RetainsRunLogs(); got != test.retainsLogs {
			t.Errorf("%s RetainsRunLogs=%v want %v", test.status, got, test.retainsLogs)
		}
		if got := test.status.PreventsIdle(); got != test.preventsIdle {
			t.Errorf("%s PreventsIdle=%v want %v", test.status, got, test.preventsIdle)
		}
		if got := test.status.IneligibleForAdmission(); got != test.ineligible {
			t.Errorf("%s IneligibleForAdmission=%v want %v", test.status, got, test.ineligible)
		}
		if got := test.status.UsesWorkerSlot(); got != test.usesSlot {
			t.Errorf("%s UsesWorkerSlot=%v want %v", test.status, got, test.usesSlot)
		}
		if got := test.status.BlocksQueueForPullRequest(false); got != test.blocksPRQueue {
			t.Errorf("%s BlocksQueueForPullRequest(false)=%v want %v", test.status, got, test.blocksPRQueue)
		}
	}
	if !StatusAwaitingMerge.BlocksQueueForPullRequest(true) {
		t.Fatal("awaiting merge must block an auto-merge queue")
	}
}

func TestAllStatusesAreValidAndClassifiedForWorkspaceProvenance(t *testing.T) {
	statuses := AllStatuses()
	seen := make(map[Status]bool, len(statuses))
	for _, status := range statuses {
		if seen[status] {
			t.Fatalf("duplicate status %q", status)
		}
		seen[status] = true
		if err := status.Validate(); err != nil {
			t.Errorf("AllStatuses contains invalid value %q: %v", status, err)
		}
	}
	if len(statuses) != len(knownStatuses) {
		t.Fatalf("AllStatuses has %d values, validation knows %d", len(statuses), len(knownStatuses))
	}
	if StatusClaiming.RequiresWorkspaceProvenance() || StatusCompleted.RequiresWorkspaceProvenance() {
		t.Fatal("pre-execution and completed records must not require retained workspace provenance")
	}
	if !StatusRunning.RequiresWorkspaceProvenance() || StatusFailed.RequiresWorkspaceProvenance() {
		t.Fatal("only execution states require live workspace provenance; terminal continuation uses a checkpoint")
	}
}

func TestGitHubSyncVocabularyAndCombinedDispatch(t *testing.T) {
	for _, sync := range AllGitHubSyncs() {
		if err := sync.Validate(); err != nil {
			t.Errorf("AllGitHubSyncs contains invalid value %q: %v", sync, err)
		}
	}
	if StatusCompleted.DispatchPending(GitHubSyncNone) {
		t.Fatal("synchronized completed Issue must not be dispatched")
	}
	if !StatusCompleted.DispatchPending(GitHubSyncDone) {
		t.Fatal("completed Issue with pending done synchronization must be dispatched")
	}
	if StatusRunning.UsesWorkerSlotWhile(GitHubSyncFailed) {
		t.Fatal("GitHub synchronization must not consume a worker slot")
	}
}

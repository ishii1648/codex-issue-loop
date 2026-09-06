package issue

import "testing"

func TestHumanWaitDistinguishesAnswersRecoveryAndMachineWaits(t *testing.T) {
	for _, test := range []struct {
		name string
		wait HumanWait
		want string
	}{
		{"question", HumanWait{Status: StatusNeedsInput, Unanswered: true}, "answer_required"},
		{"answered", HumanWait{Status: StatusNeedsInput}, ""},
		{"quarantined", HumanWait{Quarantined: true}, "recovery_required"},
		{"operator recovery", HumanWait{Status: StatusBlocked, Recoverability: RecoverabilityOperator, SuspensionStatus: SuspensionActive}, "recovery_required"},
		{"automatic recovery", HumanWait{Status: StatusBlocked, Recoverability: RecoverabilityAutomatic, SuspensionStatus: SuspensionActive}, ""},
		{"resolved recovery", HumanWait{Status: StatusBlocked, Recoverability: RecoverabilityOperator, SuspensionStatus: SuspensionResolved}, ""},
		{"checks", HumanWait{Status: StatusAwaitingChecks}, ""},
		{"review", HumanWait{Status: StatusAwaitingChecks, ReviewDecision: "REVIEW_REQUIRED"}, "review_required"},
		{"requested changes", HumanWait{Status: StatusAwaitingMerge, ReviewDecision: "CHANGES_REQUESTED"}, "review_required"},
		{"manual merge", HumanWait{Status: StatusAwaitingMerge}, "merge_required"},
		{"auto merge", HumanWait{Status: StatusAwaitingMerge, AutoMerge: true}, ""},
		{"done", HumanWait{Status: StatusCompleted, Unanswered: true}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.wait.Reason(); got != test.want {
				t.Fatalf("got %q want %q", got, test.want)
			}
		})
	}
}

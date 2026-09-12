package issue

import "testing"

func TestApproveConflictPathsRequiresActiveRetryableCheckpoint(t *testing.T) {
	for _, status := range AllStatuses() {
		for _, suspension := range []SuspensionStatus{SuspensionActive, SuspensionQuarantined, SuspensionResolved} {
			for _, stage := range []ContinuationStage{ContinuationStageNone, ContinuationStageResume, ContinuationStageConflict, ContinuationStageChecks, ContinuationStagePublish} {
				for _, recoverability := range []Recoverability{RecoverabilityOperator, RecoverabilityAutomatic, RecoverabilityNone, RecoverabilityAmbiguous} {
					for _, retry := range []bool{false, true} {
						want := (status == StatusBlocked || status == StatusFailed) && suspension == SuspensionActive && recoverability == RecoverabilityOperator && retry && (stage == ContinuationStageResume || stage == ContinuationStageConflict)
						if err := ApproveConflictPaths(status, suspension, recoverability, stage, retry); (err == nil) != want {
							t.Fatalf("%s/%s/%s/%s/%t: %v", status, suspension, stage, recoverability, retry, err)
						}
					}
				}
			}
		}
	}
}

func TestConflictApprovalPathsAreIndividualRelativePaths(t *testing.T) {
	for _, value := range []string{"", ".", "..", "../outside", "/absolute", "a/../b", "a//b", "a/", "a\\b", "a\nb", "a\x00b"} {
		if err := ValidateConflictApprovalPaths([]string{value}); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := ValidateConflictApprovalPaths([]string{"internal/adapter/state/diagnosis.go", "internal/adapter/state/diagnosis_test.go"}); err != nil {
		t.Fatal(err)
	}
}

package issue

import "testing"

func TestSubstateVocabulariesRejectUnknownValues(t *testing.T) {
	const invalidSubstate = "typo"
	tests := []struct {
		name string
		err  error
	}{
		{"resource park", ResourceParkStatus(invalidSubstate).Validate()},
		{"request", RequestStatus(invalidSubstate).Validate()},
		{"environment resume", EnvironmentResumeStatus(invalidSubstate).Validate()},
		{"publication recovery", PublicationRecoveryStatus(invalidSubstate).Validate()},
		{"publication attempt", PublicationRecoveryAttemptStatus(invalidSubstate).Validate()},
		{"conflict attempt", ConflictAttemptStatus(invalidSubstate).Validate()},
		{"checks recovery", PullRequestChecksRecoveryStatus(invalidSubstate).Validate()},
		{"answered workspace", AnsweredWorkspaceRecoveryStatus(invalidSubstate).Validate()},
		{"workspace provenance", WorkspaceProvenanceRecoveryStatus(invalidSubstate).Validate()},
		{"merged adoption", MergedPullRequestAdoptionStatus(invalidSubstate).Validate()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatal("unknown vocabulary value was accepted")
			}
		})
	}
}

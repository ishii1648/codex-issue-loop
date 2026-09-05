package issue

import "testing"

func TestSubstateVocabulariesRejectUnknownValues(t *testing.T) {
	const invalidSubstate = "typo"
	tests := []struct {
		name string
		err  error
	}{
		{"request", RequestStatus(invalidSubstate).Validate()},
		{"conflict attempt", ConflictAttemptStatus(invalidSubstate).Validate()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.err == nil {
				t.Fatal("unknown vocabulary value was accepted")
			}
		})
	}
}

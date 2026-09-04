package state

import (
	"bytes"
	"os"
	"testing"
)

func TestRecoveryPredicateVocabularyExcludesRemovedExecutionPredicates(t *testing.T) {
	source, err := os.ReadFile("recovery_report.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{
		"RecoveryCode" + "Lea" + "sePark",
		"RecoveryCode" + "Lea" + "seIdentity",
		"RECOVERY_" + "LEA" + "SE_PARK",
		"RECOVERY_" + "LEA" + "SE_IDENTITY",
	} {
		if bytes.Contains(source, []byte(removed)) {
			t.Errorf("removed recovery predicate %q is still declared", removed)
		}
	}
}

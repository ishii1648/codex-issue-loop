package statecontract

import (
	"testing"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestCurrentContractHasMigrationRulesForEveryExecutionRequirement(t *testing.T) {
	if err := Validate(Current()); err != nil {
		t.Fatal(err)
	}
	field, ok := FieldByPath("issues[].workspace")
	if !ok || field.Class != ExecutionRequiredProvenance || !RequiredForStatus(field, issuedomain.StatusRetryWait) || !RequiredForStatus(field, issuedomain.StatusPublicationRecovery) {
		t.Fatalf("workspace contract=%+v ok=%v", field, ok)
	}
}

func TestContractRejectsRequiredFieldWithoutMigrationDecision(t *testing.T) {
	contract := Current()
	contract.Fields = append(contract.Fields, Field{Path: "issues[].future_identity", Class: ExecutionRequiredProvenance, Introduced: 2, RequiredStatuses: []issuedomain.Status{issuedomain.StatusRunning}})
	if err := Validate(contract); err == nil {
		t.Fatal("execution-required field without a migration rule was accepted")
	}
}

func TestWorkspaceRequirementIsDerivedFromDomainStatuses(t *testing.T) {
	field, ok := FieldByPath("issues[].workspace")
	if !ok {
		t.Fatal("workspace field missing")
	}
	for _, status := range issuedomain.AllStatuses() {
		if got, want := RequiredForStatus(field, status), status.RequiresWorkspaceProvenance(); got != want {
			t.Errorf("status %q required=%v want %v", status, got, want)
		}
	}
}

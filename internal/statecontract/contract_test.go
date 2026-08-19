package statecontract

import "testing"

func TestCurrentContractHasMigrationRulesForEveryExecutionRequirement(t *testing.T) {
	if err := Validate(Current()); err != nil {
		t.Fatal(err)
	}
	field, ok := FieldByPath("issues[].workspace")
	if !ok || field.Class != ExecutionRequiredProvenance || !RequiredForStatus(field, "retry_wait") || !RequiredForStatus(field, "publication_recovery_pending") {
		t.Fatalf("workspace contract=%+v ok=%v", field, ok)
	}
}

func TestContractRejectsRequiredFieldWithoutMigrationDecision(t *testing.T) {
	contract := Current()
	contract.Fields = append(contract.Fields, Field{Path: "issues[].future_identity", Class: ExecutionRequiredProvenance, Introduced: 2, RequiredStatuses: []string{"running"}})
	if err := Validate(contract); err == nil {
		t.Fatal("execution-required field without a migration rule was accepted")
	}
}

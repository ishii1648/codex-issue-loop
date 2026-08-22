package state

import (
	"errors"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/statecontract"
)

func TestEveryExecutionRequiredFieldHasRuntimeValidator(t *testing.T) {
	for _, field := range statecontract.Current().Fields {
		if field.Class == statecontract.ExecutionRequiredProvenance && !supportsExecutionRequiredField(field.Path) {
			t.Fatalf("execution-required field %s has no runtime validator", field.Path)
		}
	}
}

func TestSemanticContractValidatesSupportedRecoveryStates(t *testing.T) {
	statuses := []string{"running", "blocked", "needs_input", "retry_wait", "failed", "publication_recovery_pending", "pull_request_checks_recovery_pending"}
	for _, status := range statuses {
		t.Run(status, func(t *testing.T) {
			snapshot := semanticFixture(status)
			if err := ValidateSemanticContract(snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.Issues["442"].Workspace = nil
			err := ValidateSemanticContract(snapshot)
			var compatibility SemanticCompatibilityError
			if !errors.As(err, &compatibility) || len(compatibility.Violations) != 1 || compatibility.Violations[0].Code != SemanticCodeWorkspaceProvenanceMissing {
				t.Fatalf("err=%v violations=%+v", err, compatibility.Violations)
			}
		})
	}
}

func TestSemanticContractDoesNotRequireProvenanceBeforeWorkerBoundary(t *testing.T) {
	snapshot := Snapshot{RepoID: "repo-1", Issues: map[string]*Issue{"7": {Number: 7, Status: issuedomain.StatusBlocked}}}
	if err := ValidateSemanticContract(snapshot); err != nil {
		t.Fatal(err)
	}
}

func semanticFixture(status string) Snapshot {
	now := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	return Snapshot{RepoID: "repo-1", Issues: map[string]*Issue{"442": {
		Number: 442, Status: issuedomain.Status(status), RunID: "run-442", Worktree: "/state/worktrees/442", Branch: "codex/issue-442", Attempts: 1,
		Workspace: &WorkerWorkspace{Path: "/state/worktrees/442", Branch: "codex/issue-442", RepoID: "repo-1", Repository: "owner/repo", GitCommonDir: "/repo/.git", MainCheckout: "/repo", CapturedAt: now},
	}}}
}

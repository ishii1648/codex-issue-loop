package state

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

const (
	SemanticCodeCompatible                 = "SEMANTIC_COMPATIBLE"
	SemanticCodeContractVersionMismatch    = "SEMANTIC_CONTRACT_VERSION_MISMATCH"
	SemanticCodeWorkspaceProvenanceMissing = "EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_MISSING"
	SemanticCodeWorkspaceProvenanceInvalid = "EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_INVALID"
	SemanticCodeExecutionAuthorityMissing  = "EXECUTION_REQUIRED_ACTIVE_EXECUTION_MISSING"
	SemanticCodePreparedTransactionPresent = "PREPARED_TRANSACTION_REQUIRES_OLD_RUNTIME_RECOVERY"
)

type SemanticViolation struct {
	IssueNumber   int    `json:"issue_number"`
	Status        string `json:"status"`
	Field         string `json:"field"`
	Code          string `json:"code"`
	Migratable    bool   `json:"migratable"`
	Reason        string `json:"reason"`
	MigrationRule string `json:"migration_rule"`
	OperatorGuide string `json:"operator_guide,omitempty"`
}

type SemanticCompatibilityError struct {
	Violations []SemanticViolation
}

func (e SemanticCompatibilityError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, violation := range e.Violations {
		parts = append(parts, fmt.Sprintf("Issue #%d %s (%s)", violation.IssueNumber, violation.Reason, violation.Code))
	}
	return "durable state does not satisfy semantic contract: " + strings.Join(parts, "; ")
}

// ValidateSemanticContract never repairs or normalizes provenance.
func ValidateSemanticContract(snapshot Snapshot) error {
	violations := SemanticViolations(snapshot)
	if len(violations) == 0 {
		return nil
	}
	return SemanticCompatibilityError{Violations: violations}
}

func SemanticViolations(snapshot Snapshot) []SemanticViolation {
	if snapshot.Version != 0 && snapshot.SemanticContractVersion != statecontract.CurrentVersion {
		return []SemanticViolation{{Field: "semantic_contract_version", Code: SemanticCodeContractVersionMismatch, Migratable: false,
			Reason:        fmt.Sprintf("snapshot semantic contract is %d, current release requires %d", snapshot.SemanticContractVersion, statecontract.CurrentVersion),
			MigrationRule: "RUN_VERSIONED_MIGRATION", OperatorGuide: "stop all loops and run agent-loop migrate --json"}}
	}
	violations := []SemanticViolation{}
	for _, field := range statecontract.Current().Fields {
		if field.Class != statecontract.ExecutionRequiredProvenance {
			continue
		}
		if !supportsExecutionRequiredField(field.Path) {
			violations = append(violations, SemanticViolation{Field: field.Path, Code: "EXECUTION_REQUIRED_VALIDATOR_MISSING", Migratable: false,
				Reason: "execution-required contract field has no runtime validator", MigrationRule: field.Migration.Code})
		}
	}
	field, ok := statecontract.FieldByPath("issues[].workspace")
	if !ok {
		return append(violations, SemanticViolation{Field: "issues[].workspace", Code: SemanticCodeWorkspaceProvenanceInvalid, Reason: "workspace requirement is missing from the current contract"})
	}
	activeField, ok := statecontract.FieldByPath("active_execution")
	if !ok {
		return append(violations, SemanticViolation{Field: "active_execution", Code: SemanticCodeExecutionAuthorityMissing, Reason: "active execution requirement is missing from the current contract"})
	}
	keys := make([]string, 0, len(snapshot.Issues))
	for key := range snapshot.Issues {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, _ := strconv.Atoi(keys[i])
		right, _ := strconv.Atoi(keys[j])
		return left < right
	})
	for _, key := range keys {
		issue := snapshot.Issues[key]
		if issue == nil {
			continue
		}
		activeMatches := snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issue.Number && snapshot.ActiveExecution.RunID == issue.RunID && snapshot.ActiveExecution.Generation == issue.Generation
		if statecontract.RequiredForStatus(activeField, issue.Status) && !activeMatches {
			violations = append(violations, SemanticViolation{
				IssueNumber: issue.Number, Status: string(issue.Status), Field: activeField.Path,
				Code: SemanticCodeExecutionAuthorityMissing, Migratable: false,
				Reason:        "executing lifecycle has no matching repository active execution",
				MigrationRule: activeField.Migration.Code, OperatorGuide: activeField.Migration.OperatorGuide,
			})
		}
		if !statecontract.RequiredForStatus(field, issue.Status) || !crossedWorkerExecutionBoundary(issue) {
			continue
		}
		base := SemanticViolation{
			IssueNumber: issue.Number, Status: issue.Status.String(), Field: field.Path, Migratable: false,
			MigrationRule: field.Migration.Code, OperatorGuide: field.Migration.OperatorGuide,
		}
		if issue.Workspace == nil {
			base.Code = SemanticCodeWorkspaceProvenanceMissing
			base.Reason = "saved workspace provenance is required before this recovery state can execute"
			violations = append(violations, base)
			continue
		}
		workspace := issue.Workspace
		if workspace.Path == "" || workspace.Path != issue.Worktree || workspace.Branch == "" || workspace.Branch != issue.Branch ||
			workspace.RepoID == "" || workspace.RepoID != snapshot.RepoID || workspace.Repository == "" ||
			workspace.GitCommonDir == "" || workspace.MainCheckout == "" || workspace.CapturedAt.IsZero() {
			base.Code = SemanticCodeWorkspaceProvenanceInvalid
			base.Reason = "saved workspace provenance is incomplete or inconsistent with durable issue identity"
			violations = append(violations, base)
		}
	}
	return violations
}

func supportsExecutionRequiredField(path string) bool {
	switch path {
	case "issues[].workspace", "issues[].generation", "active_execution":
		return true
	default:
		return false
	}
}

func crossedWorkerExecutionBoundary(issue *Issue) bool {
	if issue.Workspace != nil || issue.Worktree != "" || issue.Branch != "" || issue.SessionID != "" || issue.Session != nil || issue.Attempts > 0 || issue.Continuations > 0 {
		return true
	}
	return issue.PublicationAudit != nil || issue.ConflictRecovery != nil || issue.Continuation != nil || issue.Suspension != nil
}

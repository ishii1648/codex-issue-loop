// Package statecontract is the versioned source of truth for durable state
// field classification, execution requirements, and migration policy.
package statecontract

import (
	"fmt"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

const (
	CurrentVersion       = 3
	MinimumVersion       = 1
	CurrentSchemaVersion = 5
	MigrationFromSchema  = 4
)

type Class string

const (
	Optional                    Class = "optional"
	Observational               Class = "observational"
	ExecutionRequiredProvenance Class = "execution_required_provenance"
)

type MigrationPolicy struct {
	Code          string `json:"code"`
	Kind          string `json:"kind"`
	OperatorGuide string `json:"operator_guide"`
}

type Field struct {
	Path             string               `json:"path"`
	Class            Class                `json:"class"`
	Introduced       int                  `json:"introduced_in_contract"`
	RequiredStatuses []issuedomain.Status `json:"required_statuses,omitempty"`
	Migration        MigrationPolicy      `json:"migration"`
}

type Contract struct {
	Version             int     `json:"version"`
	SchemaVersion       int     `json:"schema_version"`
	MigrationFromSchema int     `json:"migration_from_schema"`
	Fields              []Field `json:"fields"`
}

// Current is consumed by both the runtime validator and migration preview.
// An execution-required field without an explicit migration decision makes
// Validate fail, which is exercised by CI and the release check.
func Current() Contract {
	return Contract{
		Version: CurrentVersion, SchemaVersion: CurrentSchemaVersion, MigrationFromSchema: MigrationFromSchema,
		Fields: []Field{
			{Path: "issues[].workspace", Class: ExecutionRequiredProvenance, Introduced: 1,
				RequiredStatuses: workspaceRequiredStatuses(),
				Migration: MigrationPolicy{
					Code: "WORKSPACE_PROVENANCE_PRESERVED_OR_QUARANTINED", Kind: "migrate",
					OperatorGuide: "stopped v4 records are converted to a typed continuation checkpoint; ambiguous records are isolated",
				}},
			{Path: "issues[].execution_lease", Class: ExecutionRequiredProvenance, Introduced: 2,
				RequiredStatuses: executionLeaseStatuses(),
				Migration:        MigrationPolicy{Code: "RENAME_ACTIVE_EXECUTION_LEASE", Kind: "migrate"}},
			{Path: "issues[].continuation_checkpoint", Class: Optional, Introduced: 2,
				Migration: MigrationPolicy{Code: "FOLD_LEGACY_RECOVERY_TO_CHECKPOINT", Kind: "migrate"}},
			{Path: "issues[].continuation_checkpoint.stage", Class: Optional, Introduced: 3,
				Migration: MigrationPolicy{Code: "NORMALIZE_CONTINUATION_STAGE", Kind: "migrate"}},
			{Path: "issues[].continuation_checkpoint.evidence", Class: Optional, Introduced: 3,
				Migration: MigrationPolicy{Code: "PRESERVE_CONTINUATION_EVIDENCE", Kind: "preserve"}},
			{Path: "issues[].suspension", Class: Optional, Introduced: 2,
				Migration: MigrationPolicy{Code: "FOLD_TERMINAL_RECOVERY_TO_SUSPENSION", Kind: "migrate"}},
			{Path: "issues[].session", Class: Optional, Introduced: 1,
				Migration: MigrationPolicy{Code: "OPTIONAL_NO_MIGRATION", Kind: "compatible"}},
			{Path: "issues[].publication_audit", Class: Optional, Introduced: 1,
				Migration: MigrationPolicy{Code: "OPTIONAL_NO_MIGRATION", Kind: "compatible"}},
			{Path: "issues[].attempts", Class: Observational, Introduced: 1,
				Migration: MigrationPolicy{Code: "PRESERVE_OBSERVATION", Kind: "preserve"}},
			{Path: "issues[].updated_at", Class: Observational, Introduced: 1,
				Migration: MigrationPolicy{Code: "PRESERVE_OBSERVATION", Kind: "preserve"}},
		},
	}
}

func Validate(contract Contract) error {
	if contract.Version <= 0 || contract.SchemaVersion <= 0 || contract.MigrationFromSchema <= 0 {
		return fmt.Errorf("contract and schema versions must be positive")
	}
	seen := map[string]bool{}
	for _, field := range contract.Fields {
		if field.Path == "" || seen[field.Path] {
			return fmt.Errorf("state contract field path is empty or duplicated: %q", field.Path)
		}
		seen[field.Path] = true
		switch field.Class {
		case Optional, Observational, ExecutionRequiredProvenance:
		default:
			return fmt.Errorf("state contract field %s has unknown class %q", field.Path, field.Class)
		}
		if field.Migration.Code == "" || field.Migration.Kind == "" {
			return fmt.Errorf("state contract field %s has no migration/compatibility rule", field.Path)
		}
		if field.Class == ExecutionRequiredProvenance {
			if len(field.RequiredStatuses) == 0 {
				return fmt.Errorf("execution-required field %s has no required statuses", field.Path)
			}
			if field.Migration.Kind == "non_migratable" && field.Migration.OperatorGuide == "" {
				return fmt.Errorf("non-migratable field %s has no operator guide", field.Path)
			}
		}
	}
	return nil
}

func FieldByPath(path string) (Field, bool) {
	for _, field := range Current().Fields {
		if field.Path == path {
			return field, true
		}
	}
	return Field{}, false
}

func RequiredForStatus(field Field, status issuedomain.Status) bool {
	for _, candidate := range field.RequiredStatuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func workspaceRequiredStatuses() []issuedomain.Status {
	statuses := make([]issuedomain.Status, 0, len(issuedomain.AllStatuses()))
	for _, status := range issuedomain.AllStatuses() {
		if status.RequiresWorkspaceProvenance() {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

func executionLeaseStatuses() []issuedomain.Status {
	statuses := make([]issuedomain.Status, 0)
	for _, status := range issuedomain.AllStatuses() {
		if status.RequiresExecutionLease() {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

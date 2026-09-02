package migration

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

type QuarantinedSemanticReport struct {
	Plan             state.QuarantinedSemanticMigrationPlan `json:"plan"`
	SemanticFindings []SemanticFinding                      `json:"semantic_findings"`
	NonMigratable    []SemanticFinding                      `json:"non_migratable,omitempty"`
	ApplyAllowed     bool                                   `json:"apply_allowed"`
}

type QuarantinedSemanticResult struct {
	Changed              bool                                   `json:"changed"`
	RepositoryID         string                                 `json:"repository_id"`
	FromSemanticContract int                                    `json:"from_semantic_contract"`
	ToSemanticContract   int                                    `json:"to_semantic_contract"`
	SnapshotRevision     uint64                                 `json:"snapshot_revision"`
	IssueCount           int                                    `json:"issue_count"`
	PendingRequestCount  int                                    `json:"pending_request_count"`
	SourceBackup         string                                 `json:"source_backup"`
	RecoveryMarkerBackup string                                 `json:"recovery_marker_backup"`
	MigrationID          string                                 `json:"migration_id"`
	Plan                 state.QuarantinedSemanticMigrationPlan `json:"plan"`
}

type stagedQuarantinedSemantic struct {
	report QuarantinedSemanticReport
	state  []byte
	events []byte
	id     string
}

func InspectQuarantinedSemantic(l layout.Layout, repoPath, backup string) (QuarantinedSemanticReport, error) {
	return (Migrator{Layout: l}).inspectQuarantinedSemantic(repoPath, backup)
}

func (m Migrator) inspectQuarantinedSemantic(repoPath, backup string) (QuarantinedSemanticReport, error) {
	staged, err := m.stageQuarantinedSemantic(repoPath, backup)
	if err != nil {
		return QuarantinedSemanticReport{}, err
	}
	return staged.report, nil
}

func (m Migrator) ApplyQuarantinedSemantic(repoPath, backup string) (QuarantinedSemanticResult, error) {
	unlock, err := m.lock()
	if err != nil {
		return QuarantinedSemanticResult{}, err
	}
	defer unlock()
	staged, err := m.stageQuarantinedSemantic(repoPath, backup)
	if err != nil {
		return QuarantinedSemanticResult{}, err
	}
	if len(staged.report.NonMigratable) > 0 {
		return QuarantinedSemanticResult{}, nonMigratableError(staged.report.NonMigratable)
	}
	entry, err := (registry.Store{Path: m.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return QuarantinedSemanticResult{}, err
	}
	store := state.Store{Dir: m.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
	migrated, markerBackup, err := store.ApplyQuarantinedSemanticMigration(
		backup, staged.report.Plan.SourceStateSHA256, staged.report.Plan.SourceEventsSHA256, staged.state, staged.events,
	)
	if err != nil {
		return QuarantinedSemanticResult{}, err
	}
	return QuarantinedSemanticResult{
		Changed: true, RepositoryID: entry.RepoID,
		FromSemanticContract: staged.report.Plan.FromSemanticContract,
		ToSemanticContract:   staged.report.Plan.ToSemanticContract,
		SnapshotRevision:     migrated.StateRevision, IssueCount: len(migrated.Issues),
		PendingRequestCount: len(migrated.PendingRequests), SourceBackup: staged.report.Plan.Backup,
		RecoveryMarkerBackup: markerBackup, MigrationID: staged.id, Plan: staged.report.Plan,
	}, nil
}

func (m Migrator) stageQuarantinedSemantic(repoPath, backup string) (stagedQuarantinedSemantic, error) {
	entry, err := (registry.Store{Path: m.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	store := state.Store{Dir: m.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
	plan, err := store.PreviewQuarantinedSemanticMigration(backup)
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	findings, err := inspectSemanticState(filepath.Join(plan.Backup, "state.json"))
	if err != nil {
		return stagedQuarantinedSemantic{}, fmt.Errorf("inspect quarantined semantic state: %w", err)
	}
	nonMigratable := make([]SemanticFinding, 0)
	for _, finding := range findings {
		if !finding.Migratable {
			nonMigratable = append(nonMigratable, finding)
		}
	}
	report := QuarantinedSemanticReport{Plan: plan, SemanticFindings: findings, NonMigratable: nonMigratable, ApplyAllowed: len(nonMigratable) == 0}
	if len(nonMigratable) > 0 {
		return stagedQuarantinedSemantic{report: report}, nil
	}
	temporaryDir, err := os.MkdirTemp("", "agent-loop-quarantined-semantic.*")
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	defer os.RemoveAll(temporaryDir)
	statePath := filepath.Join(temporaryDir, "state.json")
	eventsPath := filepath.Join(temporaryDir, "events.jsonl")
	stateData, err := os.ReadFile(filepath.Join(plan.Backup, "state.json"))
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	eventsData, err := os.ReadFile(filepath.Join(plan.Backup, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		eventsData = nil
		err = nil
	}
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	if err := fsutil.WriteFile(statePath, stateData, 0o600); err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	if err := fsutil.WriteFile(eventsPath, eventsData, 0o600); err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	id := migrationID(plan.Backup)
	j := journal{Version: journalVersion, MigrationID: id, From: CurrentVersion,
		FromSemantic: plan.FromSemanticContract, To: CurrentVersion, Backup: plan.Backup,
		StartedAt: m.now(), Source: "agent-loop migrate --apply --quarantined-backup"}
	if err := migrateState(statePath, j); err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	if err := migrateEvents(eventsPath, j, CurrentVersion); err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	migratedState, err := os.ReadFile(statePath)
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	migratedEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		return stagedQuarantinedSemantic{}, err
	}
	probe := state.Store{Dir: temporaryDir, RepoID: entry.RepoID, RepoPath: entry.RepoPath}
	snapshot, err := probe.Load()
	if err != nil {
		return stagedQuarantinedSemantic{}, fmt.Errorf("validate staged quarantined semantic migration: %w", err)
	}
	if snapshot.StateRevision != plan.SnapshotRevision+1 || len(snapshot.Issues) != plan.IssueCount || len(snapshot.PendingRequests) != plan.PendingRequestCount {
		return stagedQuarantinedSemantic{}, errors.New("staged quarantined semantic migration changed aggregate counts or revision")
	}
	return stagedQuarantinedSemantic{report: report, state: migratedState, events: migratedEvents, id: id}, nil
}

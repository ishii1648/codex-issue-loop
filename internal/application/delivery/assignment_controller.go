package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/drain"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

type AssignmentController struct {
	Layout     layout.Layout
	ConfigPath string
	GH         string
	Runner     Runner
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error
}

type AssignmentMigrationReport struct {
	Version     int                    `json:"version"`
	From        int                    `json:"from"`
	To          int                    `json:"to"`
	Applied     bool                   `json:"applied"`
	AlreadyDone bool                   `json:"already_done"`
	Assignments []RepositoryAssignment `json:"assignments"`
}

type AssignmentRuntime struct {
	Program string         `json:"program,omitempty"`
	Digest  string         `json:"digest,omitempty"`
	Matches bool           `json:"matches"`
	Launchd launchd.Status `json:"launchd"`
}

type AssignmentReport struct {
	Version     int                    `json:"version"`
	Repository  string                 `json:"repository_id"`
	Assignment  RepositoryAssignment   `json:"assignment"`
	Runtime     AssignmentRuntime      `json:"runtime"`
	Transaction *AssignmentTransaction `json:"transaction,omitempty"`
	FenceActive bool                   `json:"fence_active"`
	Result      string                 `json:"result,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
}

type AssignmentPlan struct {
	Version            int           `json:"version"`
	RepositoryID       string        `json:"repository_id"`
	ExpectedGeneration uint64        `json:"expected_generation"`
	Current            AssignmentRef `json:"current"`
	Desired            AssignmentRef `json:"desired"`
	Allowed            bool          `json:"allowed"`
	Reason             string        `json:"reason,omitempty"`
}

func (c AssignmentController) EnsureRepositoryAssignment(entry registry.Entry) (RepositoryAssignment, bool, error) {
	if _, err := os.Lstat(c.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return RepositoryAssignment{}, false, nil
	} else if err != nil {
		return RepositoryAssignment{}, false, err
	}
	if _, err := LoadConfig(c.ConfigPath); err != nil {
		if _, legacyErr := LoadLegacyConfig(c.ConfigPath); legacyErr == nil {
			return RepositoryAssignment{}, false, nil
		}
		return RepositoryAssignment{}, false, err
	}
	lock, err := AcquireLock(RuntimePaths(c.Layout.Root).Lock)
	if err != nil {
		return RepositoryAssignment{}, false, err
	}
	defer lock.Close()
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return RepositoryAssignment{}, false, err
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return RepositoryAssignment{}, false, err
	}
	if registeredEntry, ok := registered.Repos[entry.RepoID]; !ok || registeredEntry.RepoPath != entry.RepoPath {
		return RepositoryAssignment{}, false, errors.New("repository must be registered before assignment initialization")
	}
	if current, ok := cfg.Assignments[entry.RepoID]; ok {
		if err := c.validateAssignmentSet(cfg, registered); err != nil {
			return RepositoryAssignment{}, false, err
		}
		return current, true, nil
	}
	if len(cfg.Assignments)+1 != len(registered.Repos) {
		return RepositoryAssignment{}, false, errors.New("assignment set differs from registry by more than the repository being initialized")
	}
	for id := range cfg.Assignments {
		if _, ok := registered.Repos[id]; !ok {
			return RepositoryAssignment{}, false, fmt.Errorf("assignment repository %s is not registered", id)
		}
	}
	installed, err := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	if err != nil {
		return RepositoryAssignment{}, false, err
	}
	binary := filepath.Join(c.Layout.BinDir, "agent-loop")
	digest, err := fileDigest(binary)
	if err != nil {
		return RepositoryAssignment{}, false, err
	}
	if installed.BinarySHA256 != "" && installed.BinarySHA256 != digest {
		return RepositoryAssignment{}, false, errors.New("installed binary digest does not match install manifest")
	}
	ref := SlotRef(c.Layout, installed.Version, installed.Commit, digest)
	if err := StageSlot(c.Layout, ref, binary); err != nil {
		return RepositoryAssignment{}, false, err
	}
	assignment := RepositoryAssignment{RepositoryID: entry.RepoID, AssignmentRef: ref, Generation: 1, UpdatedAt: c.now()}
	cfg.Assignments[entry.RepoID] = assignment
	if err := WriteConfig(c.ConfigPath, cfg); err != nil {
		return RepositoryAssignment{}, false, err
	}
	return assignment, true, nil
}

func (c AssignmentController) RemoveRepositoryAssignment(repoID string) (bool, error) {
	if _, err := os.Lstat(c.ConfigPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if _, err := LoadConfig(c.ConfigPath); err != nil {
		if _, legacyErr := LoadLegacyConfig(c.ConfigPath); legacyErr == nil {
			return false, nil
		}
		return false, err
	}
	lock, err := AcquireLock(RuntimePaths(c.Layout.Root).Lock)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return false, err
	}
	if _, ok := cfg.Assignments[repoID]; !ok {
		return false, nil
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return false, err
	}
	if _, stillRegistered := registered.Repos[repoID]; stillRegistered {
		return false, errors.New("repository must be removed from registry before deleting its assignment")
	}
	delete(cfg.Assignments, repoID)
	if err := c.validateAssignmentSet(cfg, registered); err != nil {
		return false, err
	}
	if err := WriteConfig(c.ConfigPath, cfg); err != nil {
		return false, err
	}
	return true, nil
}

func (c AssignmentController) MigrateConfig(ctx context.Context, apply bool) (AssignmentMigrationReport, error) {
	if cfg, err := LoadConfig(c.ConfigPath); err == nil {
		registered, loadErr := (registry.Store{Path: c.Layout.RegistryPath}).Load()
		if loadErr != nil {
			return AssignmentMigrationReport{}, loadErr
		}
		if validateErr := c.validateAssignmentSet(cfg, registered); validateErr != nil {
			return AssignmentMigrationReport{}, validateErr
		}
		assignments := sortedAssignments(cfg.Assignments)
		return AssignmentMigrationReport{Version: 1, From: ConfigVersion, To: ConfigVersion, Applied: false, AlreadyDone: true, Assignments: assignments}, nil
	}
	legacy, err := LoadLegacyConfig(c.ConfigPath)
	if err != nil {
		return AssignmentMigrationReport{}, err
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return AssignmentMigrationReport{}, err
	}
	installed, err := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	if err != nil {
		return AssignmentMigrationReport{}, err
	}
	binary := filepath.Join(c.Layout.BinDir, "agent-loop")
	digest, err := fileDigest(binary)
	if err != nil {
		return AssignmentMigrationReport{}, err
	}
	if installed.BinarySHA256 != "" && installed.BinarySHA256 != digest {
		return AssignmentMigrationReport{}, errors.New("installed binary digest does not match install manifest")
	}
	ref := SlotRef(c.Layout, installed.Version, installed.Commit, digest)
	assignments := make(map[string]RepositoryAssignment, len(registered.Repos))
	now := c.now()
	for id := range registered.Repos {
		assignments[id] = RepositoryAssignment{RepositoryID: id, AssignmentRef: ref, Generation: 1, UpdatedAt: now}
	}
	report := AssignmentMigrationReport{Version: 1, From: LegacyConfigVersion, To: ConfigVersion, Assignments: sortedAssignments(assignments)}
	if !apply {
		return report, nil
	}
	lock, err := AcquireLock(RuntimePaths(c.Layout.Root).Lock)
	if err != nil {
		return report, err
	}
	defer lock.Close()
	if currentConfig, err := LoadConfig(c.ConfigPath); err == nil {
		currentRegistry, loadErr := (registry.Store{Path: c.Layout.RegistryPath}).Load()
		if loadErr != nil {
			return report, loadErr
		}
		if validateErr := c.validateAssignmentSet(currentConfig, currentRegistry); validateErr != nil {
			return report, validateErr
		}
		report.AlreadyDone = true
		report.Assignments = sortedAssignments(currentConfig.Assignments)
		return report, nil
	}
	lockedLegacy, err := LoadLegacyConfig(c.ConfigPath)
	if err != nil {
		return report, err
	}
	lockedRegistry, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return report, err
	}
	lockedInstalled, err := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	if err != nil {
		return report, err
	}
	lockedDigest, err := fileDigest(binary)
	if err != nil {
		return report, err
	}
	lockedRef := SlotRef(c.Layout, lockedInstalled.Version, lockedInstalled.Commit, lockedDigest)
	if lockedLegacy != legacy || lockedRef != ref || len(lockedRegistry.Repos) != len(assignments) {
		return report, errors.New("delivery migration inputs changed after preview; run migration preview again")
	}
	for id := range assignments {
		if _, ok := lockedRegistry.Repos[id]; !ok {
			return report, errors.New("delivery migration registry changed after preview; run migration preview again")
		}
	}
	if err := StageSlot(c.Layout, ref, binary); err != nil {
		return report, err
	}
	if err := WriteConfig(c.ConfigPath, lockedLegacy.Migrated(assignments)); err != nil {
		return report, err
	}
	report.Applied = true
	return report, nil
}

func (c AssignmentController) Status(ctx context.Context, repoPath string) ([]AssignmentReport, error) {
	cfg, registered, err := c.loadAssignmentSet()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(cfg.Assignments))
	if repoPath != "" {
		entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
		if err != nil {
			return nil, err
		}
		ids = append(ids, entry.RepoID)
	} else {
		for id := range cfg.Assignments {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	reports := make([]AssignmentReport, 0, len(ids))
	for _, id := range ids {
		assignment, ok := cfg.Assignments[id]
		if !ok {
			return nil, fmt.Errorf("repository %s has no assignment", id)
		}
		entry, ok := registered.Repos[id]
		if !ok {
			return nil, fmt.Errorf("assignment repository %s is not registered", id)
		}
		report, err := c.assignmentReport(ctx, entry, assignment)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func (c AssignmentController) Verify(ctx context.Context, repoPath string) (AssignmentReport, error) {
	cfg, registered, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentReport{}, err
	}
	entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return AssignmentReport{}, err
	}
	assignment := cfg.Assignments[entry.RepoID]
	report, reportErr := c.assignmentReport(ctx, registered.Repos[entry.RepoID], assignment)
	if reportErr != nil {
		return report, reportErr
	}
	if !report.Runtime.Matches {
		return report, errors.New("repository runtime does not match its exact assignment")
	}
	if err := c.health(ctx, entry, assignment.AssignmentRef, report.Runtime.Launchd.Loaded); err != nil {
		return report, err
	}
	report.Result = "verified"
	return report, nil
}

func (c AssignmentController) Preview(ctx context.Context, repoPath, version string) (AssignmentPlan, error) {
	cfg, _, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentPlan{}, err
	}
	entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return AssignmentPlan{}, err
	}
	current, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return AssignmentPlan{}, errors.New("repository has no initialized assignment")
	}
	candidate, err := (Verifier{GH: c.GH, Runner: c.runner(), CacheDir: RuntimePaths(c.Layout.Root).Cache, ExpectedVersion: version}).Check(ctx, cfg)
	if err != nil {
		return AssignmentPlan{}, err
	}
	desired := SlotRef(c.Layout, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	plan := AssignmentPlan{Version: 1, RepositoryID: entry.RepoID, ExpectedGeneration: current.Generation, Current: current.AssignmentRef, Desired: desired}
	want, _ := ParseSemVer(desired.Version)
	have, _ := ParseSemVer(current.Version)
	if want.Compare(have) < 0 {
		plan.Reason = "downgrade requires assignment rollback"
		return plan, nil
	}
	if current.AssignmentRef == desired {
		plan.Reason = "already assigned"
		return plan, nil
	}
	plan.Allowed = true
	return plan, nil
}

func (c AssignmentController) Apply(ctx context.Context, repoPath, version string, expectedGeneration uint64) (AssignmentReport, error) {
	cfg, _, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentReport{}, err
	}
	candidate, err := (Verifier{GH: c.GH, Runner: c.runner(), CacheDir: RuntimePaths(c.Layout.Root).Cache, ExpectedVersion: version}).Check(ctx, cfg)
	if err != nil {
		return AssignmentReport{}, err
	}
	ref := SlotRef(c.Layout, candidate.Manifest.Version, candidate.Manifest.Commit, candidate.Digest)
	if err := StageSlot(c.Layout, ref, filepath.Join(candidate.Dir, BinaryAsset)); err != nil {
		return AssignmentReport{}, err
	}
	return c.switchTo(ctx, repoPath, ref, expectedGeneration, false, false)
}

func (c AssignmentController) Retry(ctx context.Context, repoPath string, expectedGeneration uint64) (AssignmentReport, error) {
	entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return AssignmentReport{}, err
	}
	tx, err := LoadAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID))
	if err != nil {
		return AssignmentReport{}, err
	}
	if tx.Phase != AssignmentRollbackFailed {
		return AssignmentReport{}, errors.New("assignment retry requires a rollback_failed transaction")
	}
	if tx.ExpectedGeneration != expectedGeneration {
		return AssignmentReport{}, fmt.Errorf("stale assignment retry: transaction generation is %d, expected %d", tx.ExpectedGeneration, expectedGeneration)
	}
	if err := VerifySlot(tx.Desired); err != nil {
		return AssignmentReport{}, fmt.Errorf("verify retained assignment target: %w", err)
	}
	return c.switchTo(ctx, repoPath, tx.Desired, expectedGeneration, tx.Operation == AssignmentOperationRollback, true)
}

func (c AssignmentController) Rollback(ctx context.Context, repoPath string, expectedGeneration uint64) (AssignmentReport, error) {
	cfg, _, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentReport{}, err
	}
	entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return AssignmentReport{}, err
	}
	assignment, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return AssignmentReport{}, errors.New("repository has no initialized assignment")
	}
	tx, err := LoadAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID))
	if err != nil {
		return AssignmentReport{}, err
	}
	activeRollback := tx.RepositoryID != "" && tx.Phase != AssignmentSucceeded && tx.Phase != AssignmentRolledBack && tx.Operation == AssignmentOperationRollback && tx.ExpectedGeneration == expectedGeneration
	if activeRollback {
		if err := VerifySlot(tx.Desired); err != nil {
			return AssignmentReport{}, fmt.Errorf("verify rollback transaction target: %w", err)
		}
		return c.switchTo(ctx, repoPath, tx.Desired, expectedGeneration, true, false)
	}
	if assignment.Previous == nil {
		return AssignmentReport{}, errors.New("repository assignment has no previous version")
	}
	if err := VerifySlot(*assignment.Previous); err != nil {
		return AssignmentReport{}, fmt.Errorf("verify previous assignment slot: %w", err)
	}
	return c.switchTo(ctx, repoPath, *assignment.Previous, expectedGeneration, true, false)
}

func (c AssignmentController) RetryRollback(ctx context.Context, repoPath string) (AssignmentReport, error) {
	lock, err := AcquireLock(RuntimePaths(c.Layout.Root).Lock)
	if err != nil {
		return AssignmentReport{}, err
	}
	defer lock.Close()
	cfg, _, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentReport{}, err
	}
	entry, err := (registry.Store{Path: c.Layout.RegistryPath}).Resolve(repoPath, "")
	if err != nil {
		return AssignmentReport{}, err
	}
	current, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return AssignmentReport{}, errors.New("repository has no initialized assignment")
	}
	txPath := c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID)
	tx, err := LoadAssignmentTransaction(txPath)
	if err != nil {
		return AssignmentReport{}, err
	}
	if tx.RepositoryID != entry.RepoID || tx.Phase != AssignmentRollbackFailed || tx.Result != "rollback_failed" {
		return AssignmentReport{}, errors.New("assignment rollback retry requires the exact active rollback_failed transaction")
	}
	if current.Generation != tx.ExpectedGeneration || current.AssignmentRef != tx.Current {
		return AssignmentReport{}, errors.New("assignment rollback retry no longer matches the committed assignment")
	}
	fencePath := c.Layout.DeliveryAssignmentFencePath(entry.RepoID)
	fence, err := LoadMaintenance(fencePath)
	if err != nil {
		return AssignmentReport{}, fmt.Errorf("load retained assignment fence: %w", err)
	}
	if fence.Generation != fmt.Sprintf("assignment-%d", tx.TargetGeneration) || fence.Desired.Version != tx.Desired.Version || fence.Desired.Commit != tx.Desired.Commit {
		return AssignmentReport{}, errors.New("retained assignment fence does not match the rollback_failed transaction")
	}
	manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
	status, err := manager.Status(ctx, entry)
	if err != nil {
		return AssignmentReport{}, err
	}
	if status.Loaded != tx.WasLoaded {
		return AssignmentReport{}, errors.New("repository loaded state changed since the failed assignment")
	}
	if status.Loaded {
		if err := manager.Stop(ctx, entry); err != nil {
			return AssignmentReport{}, err
		}
	}
	if err := VerifySlot(current.AssignmentRef); err != nil {
		return AssignmentReport{}, fmt.Errorf("verify rollback assignment slot: %w", err)
	}
	if err := manager.WritePlist(entry, current.Slot); err != nil {
		return AssignmentReport{}, err
	}
	if tx.WasLoaded {
		if err := manager.Start(ctx, entry); err != nil {
			return AssignmentReport{}, err
		}
	}
	if err := c.health(ctx, entry, current.AssignmentRef, tx.WasLoaded); err != nil {
		tx.Reason = "assignment rollback retry health failed: " + err.Error()
		_ = SaveAssignmentTransaction(txPath, tx)
		return c.reportWithTransaction(ctx, entry, current, tx, errors.New(tx.Reason))
	}
	if err := ClearMaintenance(fencePath); err != nil {
		return AssignmentReport{}, err
	}
	if err := c.wake(entry, "assignment-rollback-retry-cleared"); err != nil {
		return AssignmentReport{}, err
	}
	tx.Phase = AssignmentRolledBack
	tx.Result = "rolled_back"
	tx.Reason = ""
	if err := SaveAssignmentTransaction(txPath, tx); err != nil {
		return AssignmentReport{}, err
	}
	report, err := c.assignmentReport(ctx, entry, current)
	report.Result = "rolled_back"
	return report, err
}

func (c AssignmentController) switchTo(ctx context.Context, repoPath string, desired AssignmentRef, expectedGeneration uint64, rollback, retryRetainedFence bool) (AssignmentReport, error) {
	lock, err := AcquireLock(RuntimePaths(c.Layout.Root).Lock)
	if err != nil {
		return AssignmentReport{}, err
	}
	defer lock.Close()
	cfg, _, err := c.loadAssignmentSet()
	if err != nil {
		return AssignmentReport{}, err
	}
	store := registry.Store{Path: c.Layout.RegistryPath}
	entry, err := store.Resolve(repoPath, "")
	if err != nil {
		return AssignmentReport{}, err
	}
	current, ok := cfg.Assignments[entry.RepoID]
	if !ok {
		return AssignmentReport{}, errors.New("repository has no initialized assignment")
	}
	if err := VerifySlot(current.AssignmentRef); err != nil {
		return AssignmentReport{}, fmt.Errorf("verify current assignment slot: %w", err)
	}
	txPath := c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID)
	tx, err := LoadAssignmentTransaction(txPath)
	if err != nil {
		return AssignmentReport{}, err
	}
	active := tx.RepositoryID != "" && tx.Phase != AssignmentSucceeded && tx.Phase != AssignmentRolledBack
	operation := AssignmentOperationApply
	if rollback {
		operation = AssignmentOperationRollback
	}
	if active && tx.Operation != operation {
		return AssignmentReport{}, fmt.Errorf("assignment %s transaction is already in progress", tx.Operation)
	}
	resumingCommitted := active && tx.Desired == desired && tx.ExpectedGeneration == expectedGeneration && current.Generation == tx.TargetGeneration && current.AssignmentRef == desired
	if expectedGeneration == 0 || current.Generation != expectedGeneration && !resumingCommitted {
		return AssignmentReport{}, fmt.Errorf("stale assignment preview: generation is %d, expected %d", current.Generation, expectedGeneration)
	}
	if current.AssignmentRef == desired {
		if resumingCommitted {
			if err := c.health(ctx, entry, desired, tx.WasLoaded); err != nil {
				return AssignmentReport{}, err
			}
			if err := ClearMaintenance(c.Layout.DeliveryAssignmentFencePath(entry.RepoID)); err != nil {
				return AssignmentReport{}, err
			}
			_ = c.wake(entry, "assignment-resumed-cleared")
			tx.Phase, tx.Result, tx.Reason = AssignmentSucceeded, "succeeded", ""
			if tx.Operation == AssignmentOperationRollback {
				tx.Phase = AssignmentRolledBack
			}
			if err := SaveAssignmentTransaction(txPath, tx); err != nil {
				return AssignmentReport{}, err
			}
		}
		report, err := c.assignmentReport(ctx, entry, current)
		report.Result = map[bool]string{true: "succeeded", false: "current"}[resumingCommitted]
		return report, err
	}
	if err := VerifySlot(desired); err != nil {
		return AssignmentReport{}, err
	}
	if !rollback {
		want, _ := ParseSemVer(desired.Version)
		have, _ := ParseSemVer(current.Version)
		if want.Compare(have) < 0 {
			return AssignmentReport{}, errors.New("assignment apply does not allow downgrade; use assignment rollback")
		}
	}
	retrying := tx.Phase == AssignmentRollbackFailed
	if retrying {
		if !retryRetainedFence {
			return AssignmentReport{}, errors.New("previous assignment rollback failed; repository fence remains active")
		}
		if tx.RepositoryID != entry.RepoID || tx.Operation != operation || tx.ExpectedGeneration != expectedGeneration || tx.Current != current.AssignmentRef || tx.Desired != desired {
			return AssignmentReport{}, errors.New("retained assignment transaction does not match the requested retry")
		}
		fence, err := LoadMaintenance(c.Layout.DeliveryAssignmentFencePath(entry.RepoID))
		if err != nil {
			return AssignmentReport{}, fmt.Errorf("load retained assignment fence: %w", err)
		}
		if fence.Generation != fmt.Sprintf("assignment-%d", tx.TargetGeneration) || fence.Desired.Version != desired.Version || fence.Desired.Commit != desired.Commit {
			return AssignmentReport{}, errors.New("retained assignment fence does not match the failed transaction")
		}
	}
	if tx.RepositoryID != "" && tx.Phase != AssignmentSucceeded && tx.Phase != AssignmentRolledBack && tx.Desired != desired {
		return AssignmentReport{}, errors.New("another assignment target is already in progress")
	}
	if tx.Phase == AssignmentRollingBack {
		if tx.ExpectedGeneration != current.Generation || tx.Current != current.AssignmentRef {
			return AssignmentReport{}, errors.New("assignment rollback transaction no longer matches current generation")
		}
		cause := errors.New(tx.Reason)
		if strings.TrimSpace(tx.Reason) == "" {
			cause = errors.New("assignment process stopped during rollback")
		}
		return c.rollbackSwitch(ctx, cfg, entry, current, &tx, cause)
	}
	manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
	status, err := manager.Status(ctx, entry)
	if err != nil {
		return AssignmentReport{}, err
	}
	if retrying {
		stateStore := state.Store{Dir: c.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
		snapshot, loadErr := stateStore.Load()
		if loadErr != nil {
			return AssignmentReport{}, fmt.Errorf("inspect retained assignment state: %w", loadErr)
		}
		if snapshotHasWorker(snapshot) {
			return AssignmentReport{}, errors.New("retained assignment retry refuses to stop an active worker")
		}
		if status.Loaded {
			if err := manager.Stop(ctx, entry); err != nil {
				return AssignmentReport{}, fmt.Errorf("stop idle retained assignment runtime: %w", err)
			}
			status, err = manager.Status(ctx, entry)
			if err != nil {
				return AssignmentReport{}, err
			}
			if status.Loaded || status.Running {
				return AssignmentReport{}, errors.New("retained assignment runtime did not stop")
			}
		}
		if status.Running {
			return AssignmentReport{}, errors.New("retained assignment retry refuses a running unloaded runtime")
		}
		if !tx.WasLoaded {
			if err := reconcileStoppedAssignmentState(stateStore, c.now()); err != nil {
				return AssignmentReport{}, fmt.Errorf("reconcile stopped retained assignment state: %w", err)
			}
		}
		tx.Phase = AssignmentPlanned
		tx.Result = "retrying"
		if err := SaveAssignmentTransaction(txPath, tx); err != nil {
			return AssignmentReport{}, err
		}
	}
	legacyRuntime := false
	if status.Loaded {
		protocol, protocolErr := c.runtimeAssignmentProtocol(ctx, manager, entry, current.AssignmentRef)
		if protocolErr != nil {
			return AssignmentReport{}, protocolErr
		}
		legacyRuntime = protocol == 0
	}
	if tx.RepositoryID == "" || tx.Phase == AssignmentSucceeded || tx.Phase == AssignmentRolledBack {
		tx = AssignmentTransaction{RepositoryID: entry.RepoID, Operation: operation, Phase: AssignmentPlanned, ExpectedGeneration: current.Generation, TargetGeneration: current.Generation + 1, Current: current.AssignmentRef, Desired: desired, WasLoaded: status.Loaded, StartedAt: c.now()}
		if err := SaveAssignmentTransaction(txPath, tx); err != nil {
			return AssignmentReport{}, err
		}
	}
	fence := c.Layout.DeliveryAssignmentFencePath(entry.RepoID)
	if err := WriteMaintenance(fence, Maintenance{Generation: fmt.Sprintf("assignment-%d", tx.TargetGeneration), Desired: VersionRef{Version: desired.Version, Commit: desired.Commit}, RequestedAt: c.now()}); err != nil {
		return AssignmentReport{}, err
	}
	if err := c.wake(entry, fmt.Sprintf("assignment-%d", tx.TargetGeneration)); err != nil {
		return AssignmentReport{}, err
	}
	tx.Phase = AssignmentDraining
	tx.Result = "draining"
	if err := SaveAssignmentTransaction(txPath, tx); err != nil {
		return AssignmentReport{}, err
	}
	if status.Loaded {
		var drainErr error
		if legacyRuntime {
			drainErr = c.waitForLegacyIdleAndStop(ctx, cfg, entry, manager)
		} else {
			drainErr = c.waitForDrain(ctx, cfg, entry)
		}
		if drainErr != nil {
			tx.Result = "deferred"
			tx.Reason = drainErr.Error()
			_ = SaveAssignmentTransaction(txPath, tx)
			_ = ClearMaintenance(fence)
			return c.reportWithTransaction(ctx, entry, current, tx, drainErr)
		}
		if !legacyRuntime {
			drainErr = manager.Stop(ctx, entry)
		}
		if drainErr != nil {
			return c.rollbackSwitch(ctx, cfg, entry, current, &tx, drainErr)
		}
	}
	if !rollback {
		if err := c.ensureDeliveryController(ctx, cfg, manager, desired); err != nil {
			return c.rollbackSwitch(ctx, cfg, entry, current, &tx, fmt.Errorf("activate assignment-aware delivery controller: %w", err))
		}
	}
	tx.Phase = AssignmentApplying
	tx.Result = "applying"
	if err := SaveAssignmentTransaction(txPath, tx); err != nil {
		return AssignmentReport{}, err
	}
	if err := manager.WritePlist(entry, desired.Slot); err != nil {
		return c.rollbackSwitch(ctx, cfg, entry, current, &tx, err)
	}
	if tx.WasLoaded {
		if err := manager.Start(ctx, entry); err != nil {
			return c.rollbackSwitch(ctx, cfg, entry, current, &tx, err)
		}
	}
	tx.Phase = AssignmentValidating
	tx.Result = "validating"
	if err := SaveAssignmentTransaction(txPath, tx); err != nil {
		return AssignmentReport{}, err
	}
	if err := c.health(ctx, entry, desired, tx.WasLoaded); err != nil {
		return c.rollbackSwitch(ctx, cfg, entry, current, &tx, err)
	}
	original := current
	previous := original.AssignmentRef
	current.AssignmentRef = desired
	current.Previous = &previous
	current.Generation = tx.TargetGeneration
	current.UpdatedAt = c.now()
	cfg.Assignments[entry.RepoID] = current
	if err := WriteConfig(c.ConfigPath, cfg); err != nil {
		return c.rollbackSwitch(ctx, cfg, entry, original, &tx, err)
	}
	if err := ClearMaintenance(fence); err != nil {
		return AssignmentReport{}, err
	}
	if err := c.wake(entry, "assignment-cleared"); err != nil {
		return AssignmentReport{}, err
	}
	tx.Phase = AssignmentSucceeded
	if rollback {
		tx.Phase = AssignmentRolledBack
	}
	tx.Result = "succeeded"
	tx.Reason = ""
	if err := SaveAssignmentTransaction(txPath, tx); err != nil {
		return AssignmentReport{}, err
	}
	report, err := c.assignmentReport(ctx, entry, current)
	report.Result = "succeeded"
	return report, err
}

func reconcileStoppedAssignmentState(store state.Store, now time.Time) error {
	_, err := store.Update("assignment_stopped_state_reconciled", 0, "", map[string]string{"reason": "retained stopped assignment retry"}, func(snapshot *state.Snapshot) error {
		if snapshotHasWorker(*snapshot) {
			return errors.New("stopped assignment state retains a worker process identity")
		}
		if active := snapshot.ActiveExecution; active != nil {
			item := snapshot.Issues[fmt.Sprint(active.IssueNumber)]
			if item == nil || item.Status != issuedomain.StatusLaunching || item.WorkerPID != 0 || item.WorkerPGID != 0 ||
				item.RunID != active.RunID || item.Generation != active.Generation {
				return errors.New("stopped assignment active execution is not an unstarted worker launch")
			}
			transition, transitionErr := issuedomain.AbortWorkerLaunch(item.Status, item.LaunchSource)
			if transitionErr != nil {
				return transitionErr
			}
			identity := state.ExecutionIdentity{RunID: active.RunID, Generation: active.Generation}
			if releaseErr := state.ReleaseExecution(snapshot, item.Number, identity); releaseErr != nil {
				return releaseErr
			}
			if transitionErr := state.ApplyIssueTransition(item, transition); transitionErr != nil {
				return transitionErr
			}
			item.UpdatedAt = now.UTC()
		}
		snapshot.Supervisor.State = state.SupervisorStateStopped
		snapshot.Supervisor.PID = 0
		snapshot.Supervisor.Message = "retained stopped assignment retry"
		snapshot.Supervisor.UpdatedAt = now.UTC()
		return nil
	})
	return err
}

func (c AssignmentController) rollbackSwitch(ctx context.Context, cfg Config, entry registry.Entry, current RepositoryAssignment, tx *AssignmentTransaction, cause error) (AssignmentReport, error) {
	tx.Phase = AssignmentRollingBack
	tx.Result = "rolling_back"
	tx.Reason = cause.Error()
	_ = SaveAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID), *tx)
	manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
	_ = manager.Stop(ctx, entry)
	rollbackErr := VerifySlot(current.AssignmentRef)
	if rollbackErr == nil {
		rollbackErr = manager.WritePlist(entry, current.Slot)
	}
	if rollbackErr == nil && tx.WasLoaded {
		rollbackErr = manager.Start(ctx, entry)
	}
	if rollbackErr == nil {
		rollbackErr = c.health(ctx, entry, current.AssignmentRef, tx.WasLoaded)
	}
	if rollbackErr != nil {
		tx.Phase = AssignmentRollbackFailed
		tx.Result = "rollback_failed"
		tx.Reason = fmt.Sprintf("%v; rollback failed: %v", cause, rollbackErr)
		_ = SaveAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID), *tx)
		report, _ := c.reportWithTransaction(ctx, entry, current, *tx, errors.New(tx.Reason))
		return report, errors.New(tx.Reason)
	}
	_ = ClearMaintenance(c.Layout.DeliveryAssignmentFencePath(entry.RepoID))
	_ = c.wake(entry, "assignment-rollback-cleared")
	tx.Phase = AssignmentRolledBack
	tx.Result = "rolled_back"
	_ = SaveAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID), *tx)
	report, _ := c.reportWithTransaction(ctx, entry, current, *tx, cause)
	return report, fmt.Errorf("assignment failed and was rolled back: %w", cause)
}

func (c AssignmentController) waitForDrain(ctx context.Context, cfg Config, entry registry.Entry) error {
	deadline := c.now().Add(cfg.DrainDuration())
	for {
		snapshot, err := (state.Store{Dir: c.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}).Load()
		if err == nil {
			ready := drain.Ready(snapshot)
			if ready {
				return nil
			}
		}
		if !c.now().Before(deadline) {
			return errors.New("repository drain deadline reached without signaling or killing workers")
		}
		if err := c.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (c AssignmentController) waitForLegacyIdleAndStop(ctx context.Context, cfg Config, entry registry.Entry, manager launchd.Manager) error {
	deadline := c.now().Add(cfg.DrainDuration())
	store := state.Store{Dir: c.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}
	for {
		idle := false
		stopErr := store.InspectExclusive(func(snapshot state.Snapshot) error {
			if snapshotHasWorker(snapshot) {
				return nil
			}
			idle = true
			requestCtx, cancelRequest := context.WithTimeout(ctx, 5*time.Second)
			requested, err := manager.RequestStop(requestCtx, entry)
			cancelRequest()
			if err != nil || !requested {
				return err
			}
			waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
			defer cancelWait()
			return manager.WaitStopped(waitCtx, entry, 30*time.Second)
		})
		if stopErr != nil {
			return fmt.Errorf("stop legacy repository runtime under admission lock: %w", stopErr)
		}
		if idle {
			return nil
		}
		if !c.now().Before(deadline) {
			return errors.New("repository drain deadline reached without signaling or killing workers")
		}
		if err := c.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

func (c AssignmentController) runtimeAssignmentProtocol(ctx context.Context, manager launchd.Manager, entry registry.Entry, current AssignmentRef) (int, error) {
	program, err := manager.Program(entry)
	if err != nil {
		return 0, err
	}
	digest, err := fileDigest(program)
	if err != nil {
		return 0, err
	}
	if digest != current.ArtifactSHA256 {
		return 0, errors.New("loaded repository binary digest does not match its current assignment")
	}
	return c.assignmentProtocol(ctx, program, current)
}

func (c AssignmentController) assignmentProtocol(ctx context.Context, program string, ref AssignmentRef) (int, error) {
	out, err := c.runner().Run(ctx, program, "version", "--json")
	if err != nil {
		return 0, fmt.Errorf("inspect repository assignment protocol: %w", err)
	}
	var info BinaryInfo
	if err := decodeStrictJSON(out, &info); err != nil {
		return 0, fmt.Errorf("decode repository assignment protocol: %w", err)
	}
	if info.Version != ref.Version || info.Commit != ref.Commit {
		return 0, errors.New("loaded repository version and commit do not match its current assignment")
	}
	if info.AssignmentProtocol < 0 || info.AssignmentProtocol > AssignmentProtocolVersion {
		return 0, errors.New("loaded repository uses an unsupported assignment protocol")
	}
	return info.AssignmentProtocol, nil
}

func snapshotHasWorker(snapshot state.Snapshot) bool {
	return drain.HasWorker(snapshot)
}

func (c AssignmentController) health(ctx context.Context, entry registry.Entry, ref AssignmentRef, wasLoaded bool) error {
	if err := VerifySlot(ref); err != nil {
		return err
	}
	manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
	program, err := manager.Program(entry)
	if err != nil {
		return err
	}
	if filepath.Clean(program) != filepath.Clean(ref.Slot) {
		return errors.New("LaunchAgent program does not match the assigned immutable slot")
	}
	status, err := manager.Status(ctx, entry)
	if err != nil {
		return err
	}
	if wasLoaded && (!status.Loaded || !status.Running) {
		return errors.New("assigned repository LaunchAgent is not running")
	}
	protocol, err := c.assignmentProtocol(ctx, ref.Slot, ref)
	if err != nil {
		return err
	}
	if !wasLoaded {
		snapshot, err := (state.Store{Dir: c.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}).Load()
		if err != nil {
			return fmt.Errorf("validate stopped repository canonical snapshot: %w", err)
		}
		if snapshot.Supervisor.State != state.SupervisorStateStopped {
			return fmt.Errorf("stopped assignment requires supervisor state stopped, got %s", snapshot.Supervisor.State)
		}
		if snapshotHasWorker(snapshot) {
			return errors.New("stopped assignment canonical snapshot still owns a worker process")
		}
		return nil
	}
	doctorArgs := []string{"doctor", "--repo", entry.RepoPath}
	if protocol >= 1 {
		doctorArgs = append(doctorArgs, "--assignment-health")
	}
	doctorArgs = append(doctorArgs, "--json")
	out, err := c.runner().Run(ctx, ref.Slot, doctorArgs...)
	if err != nil {
		return fmt.Errorf("assigned repository doctor failed: %w", err)
	}
	var result struct {
		SchemaVersion int  `json:"schema_version"`
		OK            bool `json:"ok"`
	}
	if err := json.Unmarshal(out, &result); err != nil || result.SchemaVersion != 1 || !result.OK {
		return errors.New("assigned repository doctor did not return ok=true")
	}
	return nil
}

func (c AssignmentController) ensureDeliveryController(ctx context.Context, cfg Config, manager launchd.Manager, desired AssignmentRef) error {
	if err := VerifySlot(desired); err != nil {
		return err
	}
	if program, err := manager.DeliveryProgram(); err == nil && filepath.Clean(program) == filepath.Clean(desired.Slot) {
		return nil
	}
	plistPath := c.Layout.DeliveryPlistPath()
	previous, readErr := os.ReadFile(plistPath)
	previousExists := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	status, err := manager.DeliveryStatus(ctx)
	if err != nil {
		return err
	}
	if err := manager.WriteDeliveryPlist(desired.Slot, os.Getenv("PATH"), cfg.PollDuration()); err != nil {
		return err
	}
	restore := func() error {
		if previousExists {
			return fsutil.WriteFile(plistPath, previous, 0o600)
		}
		return os.Remove(plistPath)
	}
	if status.Loaded {
		if err := manager.StopDelivery(ctx); err != nil {
			_ = restore()
			return err
		}
		if err := manager.StartDelivery(ctx); err != nil {
			restoreErr := restore()
			restartErr := manager.StartDelivery(ctx)
			if restoreErr != nil || restartErr != nil {
				return fmt.Errorf("start assignment controller: %v; restore plist: %v; restart previous controller: %v", err, restoreErr, restartErr)
			}
			return fmt.Errorf("start assignment controller: %w; previous controller restored", err)
		}
	}
	return nil
}

func (c AssignmentController) assignmentReport(ctx context.Context, entry registry.Entry, assignment RepositoryAssignment) (AssignmentReport, error) {
	manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
	runtime := AssignmentRuntime{}
	program, programErr := manager.Program(entry)
	if programErr != nil {
		return AssignmentReport{Version: 1, Repository: entry.RepoID, Assignment: assignment}, fmt.Errorf("inspect repository LaunchAgent program: %w", programErr)
	}
	runtime.Program = program
	runtime.Digest, programErr = fileDigest(program)
	if programErr != nil {
		return AssignmentReport{Version: 1, Repository: entry.RepoID, Assignment: assignment, Runtime: runtime}, programErr
	}
	runtime.Matches = filepath.Clean(program) == filepath.Clean(assignment.Slot) && runtime.Digest == assignment.ArtifactSHA256
	runtime.Launchd, programErr = manager.Status(ctx, entry)
	if programErr != nil {
		return AssignmentReport{Version: 1, Repository: entry.RepoID, Assignment: assignment, Runtime: runtime}, programErr
	}
	report := AssignmentReport{Version: 1, Repository: entry.RepoID, Assignment: assignment, Runtime: runtime}
	tx, err := LoadAssignmentTransaction(c.Layout.DeliveryAssignmentTransactionPath(entry.RepoID))
	if err != nil {
		return report, err
	}
	if tx.RepositoryID != "" {
		report.Transaction = &tx
	}
	if _, fenceErr := os.Lstat(c.Layout.DeliveryAssignmentFencePath(entry.RepoID)); fenceErr == nil {
		report.FenceActive = true
	} else if !errors.Is(fenceErr, os.ErrNotExist) {
		return report, fenceErr
	}
	return report, nil
}

func (c AssignmentController) reportWithTransaction(ctx context.Context, entry registry.Entry, assignment RepositoryAssignment, tx AssignmentTransaction, cause error) (AssignmentReport, error) {
	report, _ := c.assignmentReport(ctx, entry, assignment)
	report.Transaction = &tx
	report.Result = tx.Result
	report.Reason = tx.Reason
	return report, cause
}

func (c AssignmentController) wake(entry registry.Entry, generation string) error {
	return fsutil.WriteFile(filepath.Join(c.Layout.RepoDir(entry.RepoID), "delivery-maintenance.wake"), []byte(generation+"\n"), 0o600)
}

func (c AssignmentController) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}

func (c AssignmentController) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c AssignmentController) sleep(ctx context.Context, delay time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sortedAssignments(values map[string]RepositoryAssignment) []RepositoryAssignment {
	result := make([]RepositoryAssignment, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return strings.Compare(result[i].RepositoryID, result[j].RepositoryID) < 0 })
	return result
}

func (c AssignmentController) loadAssignmentSet() (Config, registry.Registry, error) {
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return Config{}, registry.Registry{}, err
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return Config{}, registry.Registry{}, err
	}
	if err := c.validateAssignmentSet(cfg, registered); err != nil {
		return Config{}, registry.Registry{}, err
	}
	return cfg, registered, nil
}

func (c AssignmentController) validateAssignmentSet(cfg Config, registered registry.Registry) error {
	if len(cfg.Assignments) != len(registered.Repos) {
		return fmt.Errorf("assignment set has %d repositories but registry has %d", len(cfg.Assignments), len(registered.Repos))
	}
	for id, assignment := range cfg.Assignments {
		if _, ok := registered.Repos[id]; !ok {
			return fmt.Errorf("assignment repository %s is not registered", id)
		}
		want := SlotRef(c.Layout, assignment.Version, assignment.Commit, assignment.ArtifactSHA256)
		if assignment.AssignmentRef != want {
			return fmt.Errorf("assignment repository %s does not use its canonical immutable slot", id)
		}
		if assignment.Previous != nil {
			wantPrevious := SlotRef(c.Layout, assignment.Previous.Version, assignment.Previous.Commit, assignment.Previous.ArtifactSHA256)
			if *assignment.Previous != wantPrevious {
				return fmt.Errorf("assignment repository %s previous version does not use its canonical immutable slot", id)
			}
		}
	}
	for id := range registered.Repos {
		if _, ok := cfg.Assignments[id]; !ok {
			return fmt.Errorf("registered repository %s has no initialized assignment", id)
		}
	}
	return nil
}

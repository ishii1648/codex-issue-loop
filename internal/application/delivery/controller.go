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
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

type Controller struct {
	Layout          layout.Layout
	ConfigPath      string
	GH              string
	Runner          Runner
	ExpectedVersion string
	Now             func() time.Time
	Sleep           func(context.Context, time.Duration) error
	Soak            time.Duration
}

type installedManifest struct {
	Version                 string `json:"version"`
	Commit                  string `json:"commit"`
	SchemaVersion           int    `json:"schema_version"`
	SemanticContractVersion int    `json:"semantic_contract_version"`
}
type updateResult struct {
	Changed                 bool   `json:"changed"`
	Backup                  string `json:"backup"`
	SchemaMigrationRequired bool   `json:"schema_migration_required"`
}

type Report struct {
	Version            int                `json:"version"`
	Enabled            bool               `json:"enabled"`
	Current            VersionRef         `json:"current"`
	Desired            VersionRef         `json:"desired"`
	Previous           VersionRef         `json:"previous"`
	Phase              Phase              `json:"phase"`
	Result             string             `json:"result,omitempty"`
	Reason             string             `json:"reason,omitempty"`
	Plan               *CompatibilityPlan `json:"plan,omitempty"`
	LastCheckAt        time.Time          `json:"last_check_at,omitempty"`
	NextCheckAt        time.Time          `json:"next_check_at,omitempty"`
	Drain              DrainProgress      `json:"drain"`
	DrainStartedAt     time.Time          `json:"drain_started_at,omitempty"`
	DrainDeadline      time.Time          `json:"drain_deadline,omitempty"`
	LoadedRepositories []string           `json:"loaded_repositories,omitempty"`
	Backup             string             `json:"backup,omitempty"`
	Transaction        string             `json:"transaction"`
	Maintenance        string             `json:"maintenance_fence"`
}

type DrainProgress struct {
	Total   int      `json:"total"`
	Ready   int      `json:"ready"`
	Waiting []string `json:"waiting,omitempty"`
}

func (c Controller) Check(ctx context.Context) (Report, error) {
	paths, cfg, current, err := c.prepare()
	if err != nil {
		return Report{}, err
	}
	lock, err := AcquireLock(paths.Lock)
	if err != nil {
		return Report{}, err
	}
	defer lock.Close()
	report := c.baseReport(paths, cfg, current)
	candidate, err := (Verifier{GH: c.GH, Runner: c.runner(), CacheDir: paths.Cache}).Check(ctx, cfg)
	if err != nil {
		report.Result = verificationResult(err)
		report.Reason = err.Error()
		return report, err
	}
	plan := PlanCompatibility(current.ref(), current.SchemaVersion, current.SemanticContractVersion, candidate)
	report.Desired = plan.Desired
	report.Plan = &plan
	report.Result = plan.Result
	report.Reason = plan.Reason
	return report, nil
}

func (c Controller) Reconcile(ctx context.Context, force bool) (Report, error) {
	paths, cfg, current, err := c.prepare()
	if err != nil {
		return Report{}, err
	}
	lock, err := AcquireLock(paths.Lock)
	if err != nil {
		return Report{}, err
	}
	defer lock.Close()
	tx, err := LoadTransaction(paths.Transaction)
	if err != nil {
		return Report{Version: 1, Enabled: cfg.Enabled, Current: current.ref(), Phase: tx.Phase, Result: "blocked", Reason: err.Error(), Transaction: paths.Transaction, Maintenance: paths.Maintenance}, err
	}
	if tx.Current.Version == "" {
		tx.Current = current.ref()
	}
	if tx.Phase == PhaseSucceeded && tx.LastResult == "succeeded" {
		if _, fenceErr := os.Lstat(paths.Maintenance); fenceErr == nil {
			if err := c.clearFence(paths); err != nil {
				return Report{}, err
			}
		} else if !errors.Is(fenceErr, os.ErrNotExist) {
			return Report{}, fenceErr
		}
	}
	if !cfg.Enabled {
		if tx.Current.Version == "" {
			tx.Current = current.ref()
		}
		tx.LastResult = "paused"
		tx.Reason = "delivery is disabled"
		_ = SaveTransaction(paths.Transaction, tx)
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), nil
	}
	if transactionActive(tx) && tx.LastResult == "rollback_failed" {
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), errors.New(tx.Reason)
	}
	if transactionActive(tx) && tx.LastResult == "rolling_back" {
		if current.ref() == tx.Previous {
			if healthErr := c.health(ctx, tx.LoadedRepositories); healthErr == nil {
				tx.LastResult = "rolled_back"
				tx.Current = tx.Previous
				tx.Phase = PhaseVerified
				if err := SaveTransaction(paths.Transaction, tx); err != nil {
					return Report{}, err
				}
				if err := c.clearFence(paths); err != nil {
					return Report{}, err
				}
				return c.reportFrom(paths, cfg, tx, DrainProgress{}), nil
			}
		}
		return c.rollback(ctx, paths, cfg, &tx, DrainProgress{}, errors.New(tx.Reason))
	}
	holdRolledBack := false
	if tx.LastResult == "rolled_back" {
		if err := c.clearFence(paths); err != nil {
			return Report{}, err
		}
		if !force {
			holdRolledBack = true
		} else {
			tx.MaintenanceGeneration = ""
			tx.LoadedRepositories = nil
			tx.BackupPath = ""
			tx.Previous = current.ref()
			tx.Drain = DrainProgress{}
			tx.DrainStartedAt = time.Time{}
			tx.DrainDeadline = time.Time{}
		}
	}
	now := c.now()
	tx.Attempt++
	tx.LastCheckAt = now
	tx.NextCheckAt = now.Add(cfg.PollDuration())
	verifier := Verifier{GH: c.GH, Runner: c.runner(), CacheDir: paths.Cache}
	if holdRolledBack {
		discovered, discoverErr := verifier.Discover(ctx, cfg)
		if discoverErr != nil {
			_ = SaveTransaction(paths.Transaction, tx)
			return c.reportFrom(paths, cfg, tx, DrainProgress{}), discoverErr
		}
		if discovered == tx.Desired {
			_ = SaveTransaction(paths.Transaction, tx)
			return c.reportFrom(paths, cfg, tx, DrainProgress{}), nil
		}
	}
	wasActive := transactionActive(tx)
	initialDesired := tx.Desired
	if !wasActive {
		verifier.Progress = func(progress VerificationProgress) error {
			if tx.Desired != progress.Desired {
				tx.Previous = current.ref()
				tx.MaintenanceGeneration = ""
				tx.LoadedRepositories = nil
				tx.BackupPath = ""
				tx.Drain = DrainProgress{}
				tx.DrainStartedAt = time.Time{}
				tx.DrainDeadline = time.Time{}
				tx.StartedAt = now
				tx.ArtifactDigest = ""
				tx.CandidateDir = ""
			}
			tx.Current = current.ref()
			tx.Desired = progress.Desired
			tx.Phase = progress.Phase
			tx.LastResult = string(progress.Phase)
			tx.Reason = ""
			if progress.Candidate != "" {
				tx.CandidateDir = progress.Candidate
			}
			if progress.Digest != "" {
				tx.ArtifactDigest = progress.Digest
			}
			return SaveTransaction(paths.Transaction, tx)
		}
	}
	candidate, err := verifier.Check(ctx, cfg)
	if err != nil {
		if holdRolledBack {
			_ = SaveTransaction(paths.Transaction, tx)
			return c.reportFrom(paths, cfg, tx, DrainProgress{}), err
		}
		tx.LastResult = verificationResult(err)
		tx.Reason = err.Error()
		if tx.Phase == PhaseIdle {
			tx.Phase = PhaseIdle
		}
		_ = SaveTransaction(paths.Transaction, tx)
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), err
	}
	candidateRef := VersionRef{Version: candidate.Manifest.Version, Commit: candidate.Manifest.Commit}
	if c.ExpectedVersion != "" && candidateRef.Version != c.ExpectedVersion {
		tx.LastResult = "blocked"
		tx.Reason = fmt.Sprintf("verified latest production Release is %s, not explicitly requested %s", candidateRef.Version, c.ExpectedVersion)
		_ = SaveTransaction(paths.Transaction, tx)
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), errors.New(tx.Reason)
	}
	activeTransaction := wasActive
	newCandidate := !activeTransaction && initialDesired != candidateRef
	if activeTransaction && tx.Desired != candidateRef {
		tx.LastResult = "blocked"
		tx.Reason = "latest production Release changed during an active delivery transaction; maintenance remains enabled"
		_ = SaveTransaction(paths.Transaction, tx)
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), errors.New(tx.Reason)
	}
	if activeTransaction && (tx.Phase == PhaseApplying || tx.Phase == PhaseValidating) && current.ref() == tx.Desired {
		tx.Phase = PhaseValidating
		tx.LastResult = "validating"
		if err := SaveTransaction(paths.Transaction, tx); err != nil {
			return Report{}, err
		}
		if err := c.health(ctx, tx.LoadedRepositories); err != nil {
			return c.rollback(ctx, paths, cfg, &tx, DrainProgress{}, err)
		}
		if c.soak() > 0 {
			if err := c.sleep(ctx, c.soak()); err != nil {
				return c.rollback(ctx, paths, cfg, &tx, DrainProgress{}, err)
			}
			if err := c.health(ctx, tx.LoadedRepositories); err != nil {
				return c.rollback(ctx, paths, cfg, &tx, DrainProgress{}, err)
			}
		}
		tx.Phase = PhaseSucceeded
		tx.LastResult = "succeeded"
		tx.Reason = ""
		tx.Current = tx.Desired
		if err := SaveTransaction(paths.Transaction, tx); err != nil {
			return Report{}, err
		}
		if err := c.clearFence(paths); err != nil {
			return Report{}, err
		}
		return c.reportFrom(paths, cfg, tx, DrainProgress{}), nil
	}
	tx.Phase = PhaseDiscovered
	if !activeTransaction {
		if newCandidate {
			tx.MaintenanceGeneration = ""
			tx.LoadedRepositories = nil
			tx.Previous = current.ref()
			tx.BackupPath = ""
			tx.StartedAt = now
			tx.Drain = DrainProgress{}
			tx.DrainStartedAt = time.Time{}
			tx.DrainDeadline = time.Time{}
		}
		tx.Current = current.ref()
		tx.Desired = candidateRef
	}
	tx.ArtifactDigest = candidate.Digest
	tx.CandidateDir = candidate.Dir
	if tx.StartedAt.IsZero() {
		tx.StartedAt = now
	}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	tx.Phase = PhaseDownloaded
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	plan := PlanCompatibility(current.ref(), current.SchemaVersion, current.SemanticContractVersion, candidate)
	if !plan.Allowed {
		tx.LastResult = plan.Result
		tx.Reason = plan.Reason
		if plan.Result == "current" {
			tx.Phase = PhaseSucceeded
		} else {
			tx.Phase = PhaseVerified
		}
		if err := SaveTransaction(paths.Transaction, tx); err != nil {
			return Report{}, err
		}
		report := c.reportFrom(paths, cfg, tx, DrainProgress{})
		report.Plan = &plan
		return report, nil
	}
	tx.Phase = PhaseVerified
	tx.LastResult = "verified"
	tx.Reason = ""
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	if cfg.AutoApply == "never" && !force {
		tx.LastResult = "deferred"
		tx.Reason = "auto_apply is never"
		_ = SaveTransaction(paths.Transaction, tx)
		report := c.reportFrom(paths, cfg, tx, DrainProgress{})
		report.Plan = &plan
		return report, nil
	}

	entries, loaded, err := c.loadedEntries(ctx)
	if err != nil {
		return Report{}, c.failBeforeApply(paths, &tx, "deferred", err)
	}
	tx.LoadedRepositories = loaded
	sort.Strings(tx.LoadedRepositories)
	if tx.MaintenanceGeneration == "" {
		tx.MaintenanceGeneration = state.NewID("maintenance")
	}
	if tx.Previous.Version == "" {
		tx.Previous = tx.Current
	}
	if tx.BackupPath == "" {
		backupRoot := filepath.Join(c.Layout.Root, "backups")
		if err := os.MkdirAll(backupRoot, 0o700); err != nil {
			return Report{}, err
		}
		resolvedRoot, err := filepath.EvalSymlinks(backupRoot)
		if err != nil {
			return Report{}, err
		}
		tx.BackupPath = filepath.Join(resolvedRoot, "delivery-"+tx.MaintenanceGeneration+"-"+safeName(tx.Current.Version))
	}
	if err := WriteMaintenance(paths.Maintenance, Maintenance{Generation: tx.MaintenanceGeneration, Desired: tx.Desired, RequestedAt: now}); err != nil {
		return Report{}, err
	}
	if err := c.wakeRepositories(entries, tx.MaintenanceGeneration); err != nil {
		return Report{}, err
	}
	tx.Phase = PhaseDraining
	tx.LastResult = "draining"
	if tx.DrainStartedAt.IsZero() {
		tx.DrainStartedAt = now
	}
	if tx.DrainDeadline.IsZero() {
		tx.DrainDeadline = tx.DrainStartedAt.Add(cfg.DrainDuration())
	}
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	drain, err := c.waitForDrain(ctx, cfg, entries, paths.Transaction, &tx)
	if err != nil {
		tx.LastResult = "deferred"
		tx.Reason = err.Error()
		tx.Phase = PhaseVerified
		tx.DrainStartedAt = time.Time{}
		tx.DrainDeadline = time.Time{}
		_ = SaveTransaction(paths.Transaction, tx)
		_ = c.clearFence(paths)
		return c.reportFrom(paths, cfg, tx, drain), nil
	}
	// Draining can take hours. Re-verify the production Release immediately
	// before apply so a replaced tag, edited Release, network-truncated asset,
	// or modified cache cannot cross the maintenance boundary.
	fresh, err := (Verifier{GH: c.GH, Runner: c.runner(), CacheDir: paths.Cache}).Check(ctx, cfg)
	if err != nil {
		tx.LastResult = verificationResult(err)
		tx.Reason = fmt.Sprintf("pre-apply Release re-verification failed: %v", err)
		tx.Phase = PhaseVerified
		_ = SaveTransaction(paths.Transaction, tx)
		_ = c.clearFence(paths)
		return c.reportFrom(paths, cfg, tx, drain), errors.New(tx.Reason)
	}
	freshRef := VersionRef{Version: fresh.Manifest.Version, Commit: fresh.Manifest.Commit}
	if freshRef != tx.Desired || fresh.Digest != tx.ArtifactDigest {
		tx.LastResult = "blocked"
		tx.Reason = "production Release changed after drain; installed version was not modified"
		tx.Phase = PhaseVerified
		_ = SaveTransaction(paths.Transaction, tx)
		_ = c.clearFence(paths)
		return c.reportFrom(paths, cfg, tx, drain), errors.New(tx.Reason)
	}
	candidate = fresh
	tx.CandidateDir = fresh.Dir
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}

	tx.Phase = PhaseApplying
	tx.LastResult = "applying"
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	updateOut, err := c.runner().Run(ctx, filepath.Join(candidate.Dir, BinaryAsset), "update", "--delivery-backup", tx.BackupPath, "--json")
	if err != nil {
		return c.rollback(ctx, paths, cfg, &tx, drain, fmt.Errorf("apply verified candidate: %w", err))
	}
	var updated updateResult
	if err := json.Unmarshal(updateOut, &updated); err != nil {
		return c.rollback(ctx, paths, cfg, &tx, drain, fmt.Errorf("decode update result: %w", err))
	}
	if updated.SchemaMigrationRequired {
		return c.rollback(ctx, paths, cfg, &tx, drain, errors.New("candidate unexpectedly requires schema migration"))
	}
	if updated.Backup != "" && filepath.Clean(updated.Backup) != filepath.Clean(tx.BackupPath) {
		return c.rollback(ctx, paths, cfg, &tx, drain, errors.New("update returned an unexpected backup path"))
	}
	tx.Phase = PhaseValidating
	tx.LastResult = "validating"
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	if err := c.health(ctx, tx.LoadedRepositories); err != nil {
		return c.rollback(ctx, paths, cfg, &tx, drain, err)
	}
	if c.soak() > 0 {
		if err := c.sleep(ctx, c.soak()); err != nil {
			return c.rollback(ctx, paths, cfg, &tx, drain, fmt.Errorf("health soak interrupted: %w", err))
		}
		if err := c.health(ctx, tx.LoadedRepositories); err != nil {
			return c.rollback(ctx, paths, cfg, &tx, drain, err)
		}
	}
	tx.Phase = PhaseSucceeded
	tx.LastResult = "succeeded"
	tx.Reason = ""
	tx.Current = tx.Desired
	if err := SaveTransaction(paths.Transaction, tx); err != nil {
		return Report{}, err
	}
	if err := c.clearFence(paths); err != nil {
		return Report{}, err
	}
	report := c.reportFrom(paths, cfg, tx, drain)
	report.Plan = &plan
	return report, nil
}

func (c Controller) Status() (Report, error) {
	paths := RuntimePaths(c.Layout.Root)
	if err := paths.Ensure(); err != nil {
		return Report{}, err
	}
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return Report{}, err
	}
	current, installErr := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	tx, err := LoadTransaction(paths.Transaction)
	if err != nil {
		return Report{Version: 1, Enabled: cfg.Enabled, Current: current.ref(), Phase: tx.Phase, Result: "blocked", Reason: err.Error(), Transaction: paths.Transaction, Maintenance: paths.Maintenance}, err
	}
	if installErr == nil {
		tx.Current = current.ref()
	}
	if installErr != nil && tx.Reason == "" {
		tx.Reason = installErr.Error()
	}
	return c.reportFrom(paths, cfg, tx, DrainProgress{}), nil
}

func (c Controller) rollback(ctx context.Context, paths Paths, cfg Config, tx *Transaction, drain DrainProgress, cause error) (Report, error) {
	tx.LastResult = "rolling_back"
	tx.Reason = cause.Error()
	_ = SaveTransaction(paths.Transaction, *tx)
	binary := filepath.Join(c.Layout.BinDir, "agent-loop")
	_, rollbackErr := c.runner().Run(ctx, binary, "rollback", "--backup", tx.BackupPath, "--json")
	if rollbackErr == nil {
		rollbackErr = c.health(ctx, tx.LoadedRepositories)
	}
	if rollbackErr != nil {
		tx.LastResult = "rollback_failed"
		tx.Reason = fmt.Sprintf("%v; rollback failed: %v; keep maintenance fence and inspect backup %s", cause, rollbackErr, tx.BackupPath)
		_ = SaveTransaction(paths.Transaction, *tx)
		return c.reportFrom(paths, cfg, *tx, drain), errors.New(tx.Reason)
	}
	tx.LastResult = "rolled_back"
	tx.Reason = cause.Error()
	tx.Current = tx.Previous
	tx.Phase = PhaseVerified
	_ = SaveTransaction(paths.Transaction, *tx)
	_ = c.clearFence(paths)
	return c.reportFrom(paths, cfg, *tx, drain), cause
}

func (c Controller) health(ctx context.Context, expectedLoaded []string) error {
	if err := c.ensureExpectedStarted(ctx, expectedLoaded); err != nil {
		return err
	}
	_, err := c.runner().Run(ctx, filepath.Join(c.Layout.BinDir, "agent-loop"), "doctor", "--json")
	if err != nil {
		return fmt.Errorf("post-update doctor failed: %w", err)
	}
	if len(expectedLoaded) == 0 {
		return nil
	}
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		lastErr = c.repositoriesHealthy(ctx, expectedLoaded)
		if lastErr == nil {
			return nil
		}
		if err := c.sleep(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	return lastErr
}

func (c Controller) ensureExpectedStarted(ctx context.Context, expectedLoaded []string) error {
	if len(expectedLoaded) == 0 {
		return nil
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return err
	}
	for _, repoID := range expectedLoaded {
		entry, ok := registered.Repos[repoID]
		if !ok {
			return fmt.Errorf("previously loaded repository %s disappeared from registry", repoID)
		}
		manager := launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}
		status, statusErr := manager.Status(ctx, entry)
		if statusErr != nil {
			return statusErr
		}
		if !status.Running {
			if err := manager.Restart(ctx, entry); err != nil {
				return fmt.Errorf("restart previously loaded repository %s: %w", repoID, err)
			}
		}
	}
	return nil
}

func (c Controller) repositoriesHealthy(ctx context.Context, expectedLoaded []string) error {
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return err
	}
	for _, repoID := range expectedLoaded {
		entry, ok := registered.Repos[repoID]
		if !ok {
			return fmt.Errorf("previously loaded repository %s disappeared from registry", repoID)
		}
		status, statusErr := (launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
		if statusErr != nil {
			return statusErr
		}
		if !status.Loaded || !status.Running {
			return fmt.Errorf("previously loaded repository %s did not restart in maintenance (state=%s last_exit=%v)", repoID, status.State, status.LastExitStatus)
		}
		snapshot, loadErr := (state.Store{Dir: c.Layout.RepoDir(repoID), RepoID: repoID, RepoPath: entry.RepoPath}).Load()
		if loadErr != nil {
			return loadErr
		}
		if snapshot.Supervisor.State != state.SupervisorStateMaintenance {
			return fmt.Errorf("repository %s did not reach post-update maintenance state: %s", repoID, snapshot.Supervisor.State)
		}
	}
	return nil
}

func (c Controller) loadedEntries(ctx context.Context) ([]registry.Entry, []string, error) {
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return nil, nil, err
	}
	entries := make([]registry.Entry, 0)
	ids := make([]string, 0)
	for _, entry := range registered.Repos {
		status, err := (launchd.Manager{Layout: c.Layout, Launchctl: entry.Commands["launchctl"]}).Status(ctx, entry)
		if err != nil {
			return nil, nil, err
		}
		if status.Loaded {
			entries = append(entries, entry)
			ids = append(ids, entry.RepoID)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RepoID < entries[j].RepoID })
	sort.Strings(ids)
	return entries, ids, nil
}

func (c Controller) wakeRepositories(entries []registry.Entry, generation string) error {
	for _, entry := range entries {
		if err := fsutil.WriteFile(filepath.Join(c.Layout.RepoDir(entry.RepoID), "delivery-maintenance.wake"), []byte(generation+"\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}
func (c Controller) clearFence(paths Paths) error {
	if err := ClearMaintenance(paths.Maintenance); err != nil {
		return err
	}
	registered, err := (registry.Store{Path: c.Layout.RegistryPath}).Load()
	if err != nil {
		return err
	}
	entries := make([]registry.Entry, 0, len(registered.Repos))
	for _, entry := range registered.Repos {
		entries = append(entries, entry)
	}
	return c.wakeRepositories(entries, "cleared-"+c.now().Format(time.RFC3339Nano))
}

func (c Controller) waitForDrain(ctx context.Context, cfg Config, entries []registry.Entry, transactionPath string, tx *Transaction) (DrainProgress, error) {
	deadline := tx.DrainDeadline
	if deadline.IsZero() {
		deadline = c.now().Add(cfg.DrainDuration())
	}
	for {
		progress := DrainProgress{Total: len(entries)}
		for _, entry := range entries {
			snapshot, err := (state.Store{Dir: c.Layout.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath}).Load()
			if err != nil {
				progress.Waiting = append(progress.Waiting, entry.RepoID+":state_invalid")
				continue
			}
			ready := snapshot.Supervisor.State == state.SupervisorStateMaintenance
			for _, issue := range snapshot.Issues {
				if issue != nil && (issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
					ready = false
					break
				}
			}
			if ready {
				progress.Ready++
			} else {
				progress.Waiting = append(progress.Waiting, entry.RepoID)
			}
		}
		tx.Drain = progress
		if err := SaveTransaction(transactionPath, *tx); err != nil {
			return progress, err
		}
		if progress.Ready == progress.Total {
			return progress, nil
		}
		if !c.now().Before(deadline) {
			return progress, fmt.Errorf("drain timeout reached without signaling or killing active workers: %s", strings.Join(progress.Waiting, ", "))
		}
		if err := c.sleep(ctx, time.Second); err != nil {
			return progress, err
		}
	}
}

func (c Controller) prepare() (Paths, Config, installedManifest, error) {
	paths := RuntimePaths(c.Layout.Root)
	if err := paths.Ensure(); err != nil {
		return paths, Config{}, installedManifest{}, err
	}
	cfg, err := LoadConfig(c.ConfigPath)
	if err != nil {
		return paths, cfg, installedManifest{}, err
	}
	current, err := readInstalled(filepath.Join(c.Layout.Root, "install.json"))
	return paths, cfg, current, err
}
func readInstalled(path string) (installedManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return installedManifest{}, fmt.Errorf("read managed install manifest: %w", err)
	}
	var value installedManifest
	if err := json.Unmarshal(data, &value); err != nil {
		return value, err
	}
	if value.Version == "" || value.Commit == "" || value.SchemaVersion == 0 || value.SemanticContractVersion == 0 {
		return value, errors.New("managed install manifest is incomplete")
	}
	return value, nil
}
func (i installedManifest) ref() VersionRef { return VersionRef{Version: i.Version, Commit: i.Commit} }
func (c Controller) baseReport(paths Paths, cfg Config, current installedManifest) Report {
	return Report{Version: 1, Enabled: cfg.Enabled, Current: current.ref(), Phase: PhaseIdle, Transaction: paths.Transaction, Maintenance: paths.Maintenance}
}
func (c Controller) reportFrom(paths Paths, cfg Config, tx Transaction, drain DrainProgress) Report {
	if drain.Total == 0 && len(drain.Waiting) == 0 {
		drain = tx.Drain
	}
	return Report{Version: 1, Enabled: cfg.Enabled, Current: tx.Current, Desired: tx.Desired, Previous: tx.Previous, Phase: tx.Phase, Result: tx.LastResult, Reason: tx.Reason, LastCheckAt: tx.LastCheckAt, NextCheckAt: tx.NextCheckAt, Drain: drain, DrainStartedAt: tx.DrainStartedAt, DrainDeadline: tx.DrainDeadline, LoadedRepositories: append([]string(nil), tx.LoadedRepositories...), Backup: tx.BackupPath, Transaction: paths.Transaction, Maintenance: paths.Maintenance}
}
func (c Controller) runner() Runner {
	if c.Runner != nil {
		return c.Runner
	}
	return ExecRunner{}
}
func (c Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
func (c Controller) soak() time.Duration {
	if c.Soak < 0 {
		return 0
	}
	if c.Soak == 0 {
		return 5 * time.Second
	}
	return c.Soak
}
func (c Controller) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (c Controller) failBeforeApply(paths Paths, tx *Transaction, result string, err error) error {
	tx.LastResult = result
	tx.Reason = err.Error()
	_ = SaveTransaction(paths.Transaction, *tx)
	return err
}
func verificationResult(err error) string {
	value := strings.ToLower(err.Error())
	if strings.Contains(value, "download") || strings.Contains(value, "discover") || strings.Contains(value, "api failed") {
		return "deferred"
	}
	return "blocked"
}
func safeName(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func transactionActive(tx Transaction) bool {
	return tx.MaintenanceGeneration != "" && (tx.Phase == PhaseDraining || tx.Phase == PhaseApplying || tx.Phase == PhaseValidating)
}

func ClearMaintenance(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

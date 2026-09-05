package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

type LegacyMergedIdentityTarget struct {
	IssueNumber       int    `json:"issue_number"`
	RunID             string `json:"run_id"`
	Branch            string `json:"branch"`
	PullRequestURL    string `json:"pull_request_url"`
	PullRequestNumber int    `json:"pull_request_number,omitempty"`
	HeadSHA           string `json:"head_sha,omitempty"`
}

type LegacyMergedIdentityRepair struct {
	IssueNumber       int    `json:"issue_number"`
	Branch            string `json:"branch"`
	PullRequestURL    string `json:"pull_request_url"`
	PullRequestNumber int    `json:"pull_request_number"`
	HeadSHA           string `json:"head_sha"`
}

type QuarantinedSnapshotRecoveryPlan struct {
	Eligible             bool                         `json:"eligible"`
	ConfirmationRequired bool                         `json:"confirmation_required"`
	Backup               string                       `json:"backup"`
	RecoveryReason       string                       `json:"recovery_reason"`
	SnapshotRevision     uint64                       `json:"snapshot_revision"`
	Targets              []LegacyMergedIdentityTarget `json:"targets"`
	MutationScope        []string                     `json:"mutation_scope"`
}

type SemanticMismatchRecoveryPlan struct {
	Eligible                  bool     `json:"eligible"`
	ConfirmationRequired      bool     `json:"confirmation_required"`
	Backup                    string   `json:"backup"`
	RecoveryReason            string   `json:"recovery_reason"`
	CurrentSemanticContract   int      `json:"current_semantic_contract"`
	RestoredSemanticContract  int      `json:"restored_semantic_contract"`
	RestoredRevision          uint64   `json:"restored_revision"`
	RestoredIssueCount        int      `json:"restored_issue_count"`
	RestoredActiveWorkers     int      `json:"restored_active_workers"`
	RestoredActiveExecutions  int      `json:"restored_active_executions"`
	RestoredPendingRequests   int      `json:"restored_pending_requests"`
	RestoredRecoveryMarker    bool     `json:"restored_recovery_marker"`
	NextBackup                string   `json:"next_backup,omitempty"`
	StateSHA256               string   `json:"state_sha256"`
	EventsSHA256              string   `json:"events_sha256"`
	SemanticMigrationRequired bool     `json:"semantic_migration_required"`
	MutationScope             []string `json:"mutation_scope"`
}

type LifecycleMismatchRecoveryPlan struct {
	Eligible                 bool     `json:"eligible"`
	ConfirmationRequired     bool     `json:"confirmation_required"`
	Backup                   string   `json:"backup"`
	RecoveryReason           string   `json:"recovery_reason"`
	MarkerLifecycleAPI       string   `json:"marker_lifecycle_api"`
	RestoredLifecycleAPI     string   `json:"restored_lifecycle_api"`
	RestoredRevision         uint64   `json:"restored_revision"`
	RestoredIssueCount       int      `json:"restored_issue_count"`
	RestoredActiveWorkers    int      `json:"restored_active_workers"`
	RestoredActiveExecutions int      `json:"restored_active_executions"`
	RestoredPendingRequests  int      `json:"restored_pending_requests"`
	PreparedTransaction      bool     `json:"prepared_transaction"`
	StateSHA256              string   `json:"state_sha256"`
	EventsSHA256             string   `json:"events_sha256"`
	TransactionSHA256        string   `json:"transaction_sha256,omitempty"`
	MutationScope            []string `json:"mutation_scope"`
}

type quarantineRecoveryTransaction struct {
	Version           int    `json:"version"`
	RepoID            string `json:"repo_id"`
	StateFile         string `json:"state_file"`
	EventsFile        string `json:"events_file"`
	TransactionFile   string `json:"transaction_file,omitempty"`
	StateSHA256       string `json:"state_sha256"`
	EventsSHA256      string `json:"events_sha256"`
	TransactionSHA256 string `json:"transaction_sha256,omitempty"`
}

func (s Store) PreviewLegacyMergedIdentityRecovery(expectedBackup string) (QuarantinedSnapshotRecoveryPlan, error) {
	if err := s.ensureDir(); err != nil {
		return QuarantinedSnapshotRecoveryPlan{}, err
	}
	lock, err := s.lock(true)
	if err != nil {
		return QuarantinedSnapshotRecoveryPlan{}, err
	}
	defer unlock(lock)
	_, plan, err := s.legacyMergedIdentityRecoveryPlanUnlocked(expectedBackup)
	return plan, err
}

func (s Store) PreviewSemanticMismatchRecovery(expectedBackup string) (SemanticMismatchRecoveryPlan, error) {
	if err := s.ensureDir(); err != nil {
		return SemanticMismatchRecoveryPlan{}, err
	}
	lock, err := s.lock(true)
	if err != nil {
		return SemanticMismatchRecoveryPlan{}, err
	}
	defer unlock(lock)
	_, plan, err := s.semanticMismatchRecoveryPlanUnlocked(expectedBackup)
	return plan, err
}

func (s Store) PreviewLifecycleMismatchRecovery(expectedBackup string) (LifecycleMismatchRecoveryPlan, error) {
	if err := s.ensureDir(); err != nil {
		return LifecycleMismatchRecoveryPlan{}, err
	}
	lock, err := s.lock(true)
	if err != nil {
		return LifecycleMismatchRecoveryPlan{}, err
	}
	defer unlock(lock)
	_, plan, err := s.lifecycleMismatchRecoveryPlanUnlocked(expectedBackup)
	return plan, err
}

func semanticMismatchVersions(reason string) (int, int, bool) {
	var source, target int
	n, err := fmt.Sscanf(reason, "snapshot semantic contract version %d does not match %d", &source, &target)
	return source, target, err == nil && n == 2 && reason == fmt.Sprintf("snapshot semantic contract version %d does not match %d", source, target)
}

func (s Store) validateSemanticRecoveryMarker(snapshot Snapshot, events []Event) (int, error) {
	if snapshot.Recovery == nil || snapshot.Recovery.Status != RecoveryStateBlocked || snapshot.Supervisor.State != "blocked" ||
		snapshot.StateRevision != 1 || len(snapshot.Issues) != 0 || len(snapshot.PendingRequests) != 0 {
		return 0, errors.New("snapshot is not an exact semantic recovery marker")
	}
	if len(events) != 1 || events[0].Type != "recovery_blocked" || events[0].Sequence != 1 {
		return 0, errors.New("semantic recovery marker event chain is not exact")
	}
	source, target, ok := semanticMismatchVersions(snapshot.Recovery.Reason)
	if !ok || source == target || target != snapshot.SemanticContractVersion {
		return 0, fmt.Errorf("recovery reason is not an exact semantic contract mismatch: %s", snapshot.Recovery.Reason)
	}
	expectedMessage := fmt.Sprintf("durable state recovery blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)
	if snapshot.Supervisor.Message != expectedMessage {
		return 0, errors.New("semantic recovery marker supervisor message does not match its recovery record")
	}
	var payload map[string]string
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload["reason"] != snapshot.Recovery.Reason ||
		filepath.Clean(payload["backup_dir"]) != filepath.Clean(snapshot.Recovery.BackupDir) {
		return 0, errors.New("semantic recovery marker payload does not match its snapshot")
	}
	probe, err := cloneSnapshot(snapshot)
	if err != nil {
		return 0, err
	}
	probe.SemanticContractVersion = statecontract.CurrentVersion
	if err := s.validateConsistency(probe, events); err != nil {
		return 0, fmt.Errorf("validate semantic recovery marker: %w", err)
	}
	return source, nil
}

func (s Store) semanticMismatchRecoveryPlanUnlocked(expectedBackup string) (Snapshot, SemanticMismatchRecoveryPlan, error) {
	current, exists, err := s.loadSnapshotUnlocked()
	if err != nil || !exists {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("load semantic recovery marker: %w", err)
	}
	currentEvents, _, partial, err := s.readEventsUnlocked()
	if err != nil || partial {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("read semantic recovery marker events: partial=%t: %w", partial, err)
	}
	source, err := s.validateSemanticRecoveryMarker(current, currentEvents)
	if err != nil {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
	}
	backup, err := s.validateExactRecoveryBackup(expectedBackup, current.Recovery.BackupDir)
	if err != nil {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
	}
	for _, path := range []string{s.TransactionPath(), s.quarantineRecoveryTransactionPath()} {
		if _, err := os.Stat(path); err == nil {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("semantic recovery marker has an active transaction: %s", filepath.Base(path))
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
		}
	}
	if _, err := os.Stat(filepath.Join(backup, "state.txn.json")); err == nil {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, errors.New("semantic recovery backup contains a prepared transaction")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
	}
	backupStore := Store{Dir: backup, RepoID: s.RepoID, RepoPath: s.RepoPath, Secrets: s.Secrets}
	restored, exists, err := backupStore.loadSnapshotForSemanticRecoveryUnlocked()
	if err != nil || !exists {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("load semantic recovery backup: %w", err)
	}
	if restored.SemanticContractVersion != source {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("backup semantic contract version is %d, expected mismatch source %d", restored.SemanticContractVersion, source)
	}
	events, _, partial, err := backupStore.readEventsUnlocked()
	if err != nil || partial {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("read semantic recovery backup events: partial=%t: %w", partial, err)
	}
	restoredMarker := restored.Recovery != nil && restored.Recovery.Status == RecoveryStateBlocked
	nextBackup := ""
	activeWorkers := 0
	activeExecutions := 0
	pendingRequests := 0
	for _, issue := range restored.Issues {
		if issue != nil && (issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
			activeWorkers++
		}
	}
	if restored.ActiveExecution != nil {
		activeExecutions = 1
	}
	for _, request := range restored.PendingRequests {
		if request != nil && request.Status == issuedomain.RequestStatusPending {
			pendingRequests++
		}
	}
	if restoredMarker {
		if _, err := backupStore.validateSemanticRecoveryMarker(restored, events); err != nil {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("validate nested semantic recovery marker: %w", err)
		}
		if _, err := s.validateExactRecoveryBackup(restored.Recovery.BackupDir, restored.Recovery.BackupDir); err != nil {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("validate nested semantic recovery backup: %w", err)
		}
		nextBackup = restored.Recovery.BackupDir
	} else {
		if restored.SemanticContractVersion < statecontract.MinimumVersion || restored.SemanticContractVersion >= statecontract.CurrentVersion {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("restored semantic contract version %d is not a supported migration source", restored.SemanticContractVersion)
		}
		if restored.Supervisor.State != SupervisorStateStopped {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("restored snapshot supervisor is not stopped: %s", restored.Supervisor.State)
		}
		if pendingRequests != 0 {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("restored snapshot retains %d pending requests", pendingRequests)
		}
		if activeWorkers != 0 || activeExecutions != 0 {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("restored snapshot retains active workers=%d executions=%d", activeWorkers, activeExecutions)
		}
		if err := validateEventSequence(restored, events); err != nil {
			return Snapshot{}, SemanticMismatchRecoveryPlan{}, fmt.Errorf("semantic recovery backup event chain is invalid: %w", err)
		}
	}
	stateData, err := os.ReadFile(filepath.Join(backup, "state.json"))
	if err != nil {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
	}
	eventsData, err := os.ReadFile(filepath.Join(backup, "events.jsonl"))
	if err != nil {
		return Snapshot{}, SemanticMismatchRecoveryPlan{}, err
	}
	plan := SemanticMismatchRecoveryPlan{
		Eligible: true, ConfirmationRequired: true, Backup: backup, RecoveryReason: current.Recovery.Reason,
		CurrentSemanticContract: current.SemanticContractVersion, RestoredSemanticContract: restored.SemanticContractVersion,
		RestoredRevision: restored.StateRevision, RestoredIssueCount: len(restored.Issues), RestoredRecoveryMarker: restoredMarker,
		RestoredActiveWorkers: activeWorkers, RestoredActiveExecutions: activeExecutions, RestoredPendingRequests: pendingRequests,
		NextBackup: nextBackup, StateSHA256: fileSHA256(stateData), EventsSHA256: fileSHA256(eventsData),
		SemanticMigrationRequired: !restoredMarker && restored.SemanticContractVersion != statecontract.CurrentVersion,
		MutationScope:             []string{"current recovery marker backup", "exact state snapshot restore", "exact event log restore", "recovery journal"},
	}
	return restored, plan, nil
}

func lifecycleMismatchVersions(reason string) (string, string, bool) {
	var source, target string
	n, err := fmt.Sscanf(reason, "snapshot Issue lifecycle API version %q does not match %q", &source, &target)
	return source, target, err == nil && n == 2 && reason == (LifecycleAPIVersionError{Version: source, Current: target}).Error()
}

func rawLifecycleAPI(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var envelope struct {
		IssueLifecycleAPIVersion string `json:"issue_lifecycle_api_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", err
	}
	return envelope.IssueLifecycleAPIVersion, nil
}

func rawTransactionLifecycleAPI(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var envelope struct {
		Snapshot struct {
			IssueLifecycleAPIVersion string `json:"issue_lifecycle_api_version"`
		} `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", err
	}
	return envelope.Snapshot.IssueLifecycleAPIVersion, nil
}

func validateLifecycleBackupFiles(backup string) error {
	entries, err := os.ReadDir(backup)
	if err != nil {
		return err
	}
	allowed := map[string]bool{"state.json": true, "events.jsonl": true, "state.txn.json": true}
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			return fmt.Errorf("lifecycle recovery backup contains unexpected entry %s", entry.Name())
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("lifecycle recovery backup entry %s is not a regular file", entry.Name())
		}
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(backup, name)); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) validateLifecycleRecoveryMarker(snapshot Snapshot, rawLifecycle string, events []Event) (string, string, error) {
	if snapshot.Recovery == nil || snapshot.Recovery.Status != RecoveryStateBlocked || snapshot.Supervisor.State != SupervisorStateBlocked ||
		snapshot.StateRevision != 1 || len(snapshot.Issues) != 0 || len(snapshot.PendingEffects) != 0 ||
		len(snapshot.QuarantinedIssues) != 0 || len(snapshot.IntakeVerifications) != 0 || len(snapshot.PendingRequests) != 0 ||
		snapshot.ActiveExecution != nil || snapshot.Supervisor.PID != 0 || snapshot.Supervisor.UpdatedAt.IsZero() ||
		snapshot.Recovery.DetectedAt.IsZero() || !snapshot.Supervisor.UpdatedAt.Equal(snapshot.Recovery.DetectedAt) {
		return "", "", errors.New("snapshot is not an exact lifecycle recovery marker")
	}
	if len(events) != 1 || events[0].Type != "recovery_blocked" || events[0].Sequence != 1 || events[0].IssueNumber != 0 || events[0].RunID != "" ||
		!ValidID(events[0].EventID, "evt_") || !events[0].Timestamp.Equal(snapshot.Recovery.DetectedAt) {
		return "", "", errors.New("lifecycle recovery marker event chain is not exact")
	}
	source, target, ok := lifecycleMismatchVersions(snapshot.Recovery.Reason)
	if !ok || source != issuedomain.LifecycleAPICurrent || target != rawLifecycle || source == target {
		return "", "", fmt.Errorf("recovery reason is not the exact supported lifecycle mismatch: %s", snapshot.Recovery.Reason)
	}
	expectedMessage := fmt.Sprintf("durable state recovery blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)
	if snapshot.Supervisor.Message != expectedMessage {
		return "", "", errors.New("lifecycle recovery marker supervisor message does not match its recovery record")
	}
	var payload map[string]string
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || len(payload) != 2 || payload["reason"] != snapshot.Recovery.Reason ||
		filepath.Clean(payload["backup_dir"]) != filepath.Clean(snapshot.Recovery.BackupDir) {
		return "", "", errors.New("lifecycle recovery marker payload does not match its snapshot")
	}
	if err := s.validateConsistency(snapshot, events); err != nil {
		return "", "", fmt.Errorf("validate lifecycle recovery marker: %w", err)
	}
	return source, target, nil
}

func validatePreparedRecoveryState(store Store, snapshot Snapshot, events []Event, txn transaction, exists bool) (Snapshot, error) {
	if !exists {
		if err := store.validateConsistency(snapshot, events); err != nil {
			return Snapshot{}, err
		}
		return snapshot, nil
	}
	if err := store.validateTransaction(txn); err != nil {
		return Snapshot{}, err
	}
	last := uint64(0)
	if len(events) > 0 {
		last = events[len(events)-1].Sequence
	}
	switch {
	case last == txn.Event.Sequence:
		if len(events) == 0 || !sameEvent(events[len(events)-1], txn.Event) {
			return Snapshot{}, fmt.Errorf("prepared transaction conflicts with event sequence %d", last)
		}
	case last+1 == txn.Event.Sequence:
		events = append(append([]Event(nil), events...), txn.Event)
	default:
		return Snapshot{}, fmt.Errorf("prepared transaction event sequence %d does not follow event log sequence %d", txn.Event.Sequence, last)
	}
	final := snapshot
	switch {
	case snapshot.StateRevision == txn.Snapshot.StateRevision:
		if !reflect.DeepEqual(snapshot, txn.Snapshot) {
			return Snapshot{}, fmt.Errorf("prepared transaction snapshot conflicts at revision %d", snapshot.StateRevision)
		}
	case snapshot.StateRevision < txn.Snapshot.StateRevision:
		final = txn.Snapshot
	default:
		return Snapshot{}, fmt.Errorf("state revision %d is ahead of prepared transaction revision %d", snapshot.StateRevision, txn.Snapshot.StateRevision)
	}
	if err := store.validateConsistency(final, events); err != nil {
		return Snapshot{}, err
	}
	return final, nil
}

func (s Store) lifecycleMismatchRecoveryPlanUnlocked(expectedBackup string) (Snapshot, LifecycleMismatchRecoveryPlan, error) {
	current, exists, err := s.loadSnapshotUnlocked()
	if err != nil || !exists {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("load lifecycle recovery marker: %w", err)
	}
	rawMarkerLifecycle, err := rawLifecycleAPI(s.StatePath())
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("read lifecycle recovery marker version: %w", err)
	}
	currentEvents, _, partial, err := s.readEventsUnlocked()
	if err != nil || partial {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("read lifecycle recovery marker events: partial=%t: %w", partial, err)
	}
	source, target, err := s.validateLifecycleRecoveryMarker(current, rawMarkerLifecycle, currentEvents)
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, err
	}
	backup, err := s.validateExactRecoveryBackup(expectedBackup, current.Recovery.BackupDir)
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, err
	}
	if err := validateLifecycleBackupFiles(backup); err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, err
	}
	for _, path := range []string{s.TransactionPath(), s.quarantineRecoveryTransactionPath()} {
		if _, statErr := os.Stat(path); statErr == nil {
			return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("lifecycle recovery marker has an active transaction: %s", filepath.Base(path))
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return Snapshot{}, LifecycleMismatchRecoveryPlan{}, statErr
		}
	}
	backupStore := Store{Dir: backup, RepoID: s.RepoID, RepoPath: s.RepoPath, Secrets: s.Secrets}
	rawBackupLifecycle, err := rawLifecycleAPI(backupStore.StatePath())
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("read backup lifecycle API version: %w", err)
	}
	if rawBackupLifecycle != source {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("backup lifecycle API version is %q, expected mismatch source %q", rawBackupLifecycle, source)
	}
	restored, exists, err := backupStore.loadSnapshotUnlocked()
	if err != nil || !exists {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("load lifecycle recovery backup: %w", err)
	}
	if restored.Recovery != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, errors.New("lifecycle recovery backup contains a nested recovery marker")
	}
	events, _, partial, err := backupStore.readEventsUnlocked()
	if err != nil || partial {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("read lifecycle recovery backup events: partial=%t: %w", partial, err)
	}
	txn, txnExists, err := backupStore.loadTransactionUnlocked()
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("load lifecycle recovery backup transaction: %w", err)
	}
	if txnExists {
		rawTxnLifecycle, rawErr := rawTransactionLifecycleAPI(backupStore.TransactionPath())
		if rawErr != nil {
			return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("read backup transaction lifecycle API version: %w", rawErr)
		}
		if rawTxnLifecycle != source {
			return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("backup transaction lifecycle API version is %q, expected mismatch source %q", rawTxnLifecycle, source)
		}
	}
	final, err := validatePreparedRecoveryState(backupStore, restored, events, txn, txnExists)
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("validate lifecycle recovery backup: %w", err)
	}
	activeWorkers := 0
	for _, issue := range final.Issues {
		if issue != nil && (issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
			activeWorkers++
		}
	}
	pendingRequests := 0
	for _, request := range final.PendingRequests {
		if request != nil && request.Status == issuedomain.RequestStatusPending {
			pendingRequests++
		}
	}
	if activeWorkers != 0 {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, fmt.Errorf("restored snapshot retains %d active workers", activeWorkers)
	}
	stateData, err := os.ReadFile(backupStore.StatePath())
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, err
	}
	eventsData, err := os.ReadFile(backupStore.EventsPath())
	if err != nil {
		return Snapshot{}, LifecycleMismatchRecoveryPlan{}, err
	}
	transactionSHA := ""
	if txnExists {
		transactionData, readErr := os.ReadFile(backupStore.TransactionPath())
		if readErr != nil {
			return Snapshot{}, LifecycleMismatchRecoveryPlan{}, readErr
		}
		transactionSHA = fileSHA256(transactionData)
	}
	activeExecutions := 0
	if final.ActiveExecution != nil {
		activeExecutions = 1
	}
	return final, LifecycleMismatchRecoveryPlan{
		Eligible: true, ConfirmationRequired: true, Backup: backup, RecoveryReason: current.Recovery.Reason,
		MarkerLifecycleAPI: target, RestoredLifecycleAPI: source, RestoredRevision: final.StateRevision,
		RestoredIssueCount: len(final.Issues), RestoredActiveWorkers: activeWorkers,
		RestoredActiveExecutions: activeExecutions, RestoredPendingRequests: pendingRequests,
		PreparedTransaction: txnExists, StateSHA256: fileSHA256(stateData), EventsSHA256: fileSHA256(eventsData),
		TransactionSHA256: transactionSHA,
		MutationScope:     []string{"current recovery marker backup", "exact state snapshot restore", "exact event log restore", "exact prepared transaction restore", "recovery journal"},
	}, nil
}

func (s Store) loadSnapshotForSemanticRecoveryUnlocked() (Snapshot, bool, error) {
	data, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("read state: %w", err)
	}
	type snapshotAlias Snapshot
	var decoded snapshotAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return Snapshot{}, false, fmt.Errorf("decode state: %w", err)
	}
	snapshot := Snapshot(decoded)
	if snapshot.Version != CurrentVersion {
		return Snapshot{}, false, SchemaVersionError{Kind: "state", Version: snapshot.Version}
	}
	if snapshot.RepoID != s.RepoID || snapshot.RepoPath != s.RepoPath {
		return Snapshot{}, false, errors.New("semantic recovery backup repository identity does not match")
	}
	normalizeSnapshot(&snapshot)
	return snapshot, true, nil
}

func (s Store) legacyMergedIdentityRecoveryPlanUnlocked(expectedBackup string) (Snapshot, QuarantinedSnapshotRecoveryPlan, error) {
	current, exists, err := s.loadSnapshotUnlocked()
	if err != nil || !exists {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("load recovery marker snapshot: %w", err)
	}
	if current.Recovery == nil || current.Recovery.Status != RecoveryStateBlocked || current.Supervisor.State != "blocked" ||
		current.StateRevision != 1 || len(current.Issues) != 0 || len(current.PendingRequests) != 0 {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, errors.New("current snapshot is not the isolated recovery-blocked marker")
	}
	currentEvents, _, currentPartial, err := s.readEventsUnlocked()
	if err != nil || currentPartial {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("read recovery marker event log: partial=%t: %w", currentPartial, err)
	}
	if len(currentEvents) != 1 || currentEvents[0].Type != "recovery_blocked" {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, errors.New("current recovery marker event chain is not exact")
	}
	if err := s.validateConsistency(current, currentEvents); err != nil {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("validate recovery marker: %w", err)
	}
	backup, err := s.validateExactRecoveryBackup(expectedBackup, current.Recovery.BackupDir)
	if err != nil {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, err
	}
	if !strings.Contains(current.Recovery.Reason, "merged Pull Request identity is incomplete") {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("recovery reason is not the legacy merged identity invariant: %s", current.Recovery.Reason)
	}
	backupStore := Store{Dir: backup, RepoID: s.RepoID, RepoPath: s.RepoPath, Secrets: s.Secrets}
	restored, exists, err := backupStore.loadSnapshotUnlocked()
	if err != nil || !exists {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("load quarantined snapshot: %w", err)
	}
	if _, err := os.Stat(filepath.Join(backup, "state.txn.json")); err == nil {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, errors.New("quarantined snapshot contains a prepared transaction")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, err
	}
	if restored.Supervisor.State != "maintenance" {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("quarantined snapshot supervisor is not in delivery maintenance: %s", restored.Supervisor.State)
	}
	for _, request := range restored.PendingRequests {
		if request != nil && request.Status == issuedomain.RequestStatusPending {
			return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("Issue #%d retains a pending request %s", request.IssueNumber, request.ID)
		}
	}
	targets := make([]LegacyMergedIdentityTarget, 0)
	for _, issue := range restored.Issues {
		if issue == nil {
			continue
		}
		if issue.WorkerPID != 0 || issue.WorkerPGID != 0 {
			return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("Issue #%d retains an active worker identity", issue.Number)
		}
		if !issue.PullRequestMerged || (issue.PullRequestNumber > 0 && issue.PullRequestURL != "" && issue.HeadSHA != "") {
			continue
		}
		effect := PendingEffect(&restored, issue.Number)
		if issue.Status != issuedomain.StatusCompleted || issue.PullRequestURL == "" || issue.Branch == "" ||
			(effect != nil && effect.Kind != issuedomain.EffectMarkDone) {
			return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("Issue #%d is not an immutable completed legacy merged record", issue.Number)
		}
		targets = append(targets, LegacyMergedIdentityTarget{IssueNumber: issue.Number, RunID: issue.RunID, Branch: issue.Branch,
			PullRequestURL: issue.PullRequestURL, PullRequestNumber: issue.PullRequestNumber, HeadSHA: issue.HeadSHA})
	}
	if len(targets) == 0 {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, errors.New("quarantined snapshot has no incomplete legacy merged identities")
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].IssueNumber < targets[j].IssueNumber })

	probe, err := cloneSnapshot(restored)
	if err != nil {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, err
	}
	for _, target := range targets {
		issue := probe.Issues[strconv.Itoa(target.IssueNumber)]
		if issue.PullRequestNumber == 0 {
			issue.PullRequestNumber = 1
		}
		if issue.HeadSHA == "" {
			issue.HeadSHA = strings.Repeat("0", 40)
		}
	}
	events, _, partial, err := backupStore.readEventsUnlocked()
	if err != nil || partial {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("read quarantined event log: partial=%t: %w", partial, err)
	}
	if err := backupStore.validateConsistency(probe, events); err != nil {
		return Snapshot{}, QuarantinedSnapshotRecoveryPlan{}, fmt.Errorf("quarantined snapshot has an additional invariant violation: %w", err)
	}
	return restored, QuarantinedSnapshotRecoveryPlan{
		Eligible: true, ConfirmationRequired: true, Backup: backup, RecoveryReason: current.Recovery.Reason,
		SnapshotRevision: restored.StateRevision, Targets: targets,
		MutationScope: []string{"legacy completed Pull Request number/head identity", "restored snapshot", "recovery audit event"},
	}, nil
}

func cloneSnapshot(snapshot Snapshot) (Snapshot, error) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	var cloned Snapshot
	if err := json.Unmarshal(data, &cloned); err != nil {
		return Snapshot{}, err
	}
	normalizeSnapshot(&cloned)
	return cloned, nil
}

func (s Store) validateExactRecoveryBackup(expected, recorded string) (string, error) {
	if expected == "" || filepath.Clean(expected) != filepath.Clean(recorded) {
		return "", fmt.Errorf("recovery backup does not match marker backup %s", recorded)
	}
	root, err := filepath.EvalSymlinks(filepath.Join(s.Dir, "recovery"))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("recovery backup is outside the managed recovery root: %s", resolved)
	}
	return resolved, nil
}

func validCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (s Store) quarantineRecoveryTransactionPath() string {
	return filepath.Join(s.Dir, "quarantine-recovery.txn.json")
}

func fileSHA256(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s Store) validateManagedRecoveryFile(path string) error {
	root, err := filepath.EvalSymlinks(filepath.Join(s.Dir, "recovery"))
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("staged recovery file is outside the managed recovery root: %s", resolved)
	}
	return nil
}

func (s Store) completeQuarantineRecoveryUnlocked() error {
	data, err := os.ReadFile(s.quarantineRecoveryTransactionPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var txn quarantineRecoveryTransaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return err
	}
	if txn.Version != 1 || txn.RepoID != s.RepoID || txn.StateFile == "" || txn.EventsFile == "" {
		return errors.New("quarantined snapshot recovery transaction identity is invalid")
	}
	for _, path := range []string{txn.StateFile, txn.EventsFile} {
		if err := s.validateManagedRecoveryFile(path); err != nil {
			return err
		}
	}
	stateData, err := os.ReadFile(txn.StateFile)
	if err != nil {
		return err
	}
	eventsData, err := os.ReadFile(txn.EventsFile)
	if err != nil {
		return err
	}
	if fileSHA256(stateData) != txn.StateSHA256 || fileSHA256(eventsData) != txn.EventsSHA256 {
		return errors.New("staged quarantined snapshot recovery digest changed")
	}
	var transactionData []byte
	if txn.TransactionFile != "" {
		if err := s.validateManagedRecoveryFile(txn.TransactionFile); err != nil {
			return err
		}
		transactionData, err = os.ReadFile(txn.TransactionFile)
		if err != nil {
			return err
		}
		if fileSHA256(transactionData) != txn.TransactionSHA256 {
			return errors.New("staged quarantined snapshot recovery transaction digest changed")
		}
		if err := fsutil.WriteFile(s.TransactionPath(), transactionData, 0o600); err != nil {
			return err
		}
	}
	if err := fsutil.WriteFile(s.EventsPath(), eventsData, 0o600); err != nil {
		return err
	}
	if err := fsutil.WriteFile(s.StatePath(), stateData, 0o600); err != nil {
		return err
	}
	if err := os.Remove(s.quarantineRecoveryTransactionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(s.Dir)
}

func (s Store) ApplyLifecycleMismatchRecovery(expectedBackup string) (LifecycleMismatchRecoveryPlan, string, error) {
	if err := s.ensureDir(); err != nil {
		return LifecycleMismatchRecoveryPlan{}, "", err
	}
	lock, err := s.lock(true)
	if err != nil {
		return LifecycleMismatchRecoveryPlan{}, "", err
	}
	defer unlock(lock)
	_, plan, err := s.lifecycleMismatchRecoveryPlanUnlocked(expectedBackup)
	if err != nil {
		return LifecycleMismatchRecoveryPlan{}, "", err
	}
	markerBackup := filepath.Join(s.Dir, "recovery", time.Now().UTC().Format("20060102T150405.000000000Z")+"-lifecycle-recovery-marker_"+strings.TrimPrefix(NewID("marker"), "marker_"))
	if err := os.MkdirAll(markerBackup, 0o700); err != nil {
		return LifecycleMismatchRecoveryPlan{}, "", err
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		data, readErr := os.ReadFile(filepath.Join(s.Dir, name))
		if readErr != nil {
			return LifecycleMismatchRecoveryPlan{}, markerBackup, readErr
		}
		if writeErr := fsutil.WriteFile(filepath.Join(markerBackup, name), data, 0o600); writeErr != nil {
			return LifecycleMismatchRecoveryPlan{}, markerBackup, writeErr
		}
	}
	journal := map[string]any{
		"version": 1, "status": "prepared", "reason": plan.RecoveryReason, "backup": plan.Backup,
		"marker_backup": markerBackup, "state_sha256": plan.StateSHA256, "events_sha256": plan.EventsSHA256,
		"transaction_sha256":    plan.TransactionSHA256,
		"operator_confirmation": map[string]bool{"exact_lifecycle_mismatch_backup": true},
	}
	journalPath := filepath.Join(markerBackup, "restore-journal.json")
	if err := fsutil.WriteJSON(journalPath, journal, 0o600); err != nil {
		return LifecycleMismatchRecoveryPlan{}, markerBackup, err
	}
	txn := quarantineRecoveryTransaction{
		Version: 1, RepoID: s.RepoID, StateFile: filepath.Join(plan.Backup, "state.json"), EventsFile: filepath.Join(plan.Backup, "events.jsonl"),
		StateSHA256: plan.StateSHA256, EventsSHA256: plan.EventsSHA256,
	}
	if plan.PreparedTransaction {
		txn.TransactionFile = filepath.Join(plan.Backup, "state.txn.json")
		txn.TransactionSHA256 = plan.TransactionSHA256
	}
	if err := fsutil.WriteJSON(s.quarantineRecoveryTransactionPath(), txn, 0o600); err != nil {
		return LifecycleMismatchRecoveryPlan{}, markerBackup, err
	}
	if err := s.completeQuarantineRecoveryUnlocked(); err != nil {
		return LifecycleMismatchRecoveryPlan{}, markerBackup, err
	}
	journal["status"] = "completed"
	if err := fsutil.WriteJSON(journalPath, journal, 0o600); err != nil {
		return LifecycleMismatchRecoveryPlan{}, markerBackup, err
	}
	return plan, markerBackup, nil
}

func (s Store) ApplySemanticMismatchRecovery(expectedBackup string) (SemanticMismatchRecoveryPlan, string, error) {
	if err := s.ensureDir(); err != nil {
		return SemanticMismatchRecoveryPlan{}, "", err
	}
	lock, err := s.lock(true)
	if err != nil {
		return SemanticMismatchRecoveryPlan{}, "", err
	}
	defer unlock(lock)
	_, plan, err := s.semanticMismatchRecoveryPlanUnlocked(expectedBackup)
	if err != nil {
		return SemanticMismatchRecoveryPlan{}, "", err
	}
	markerBackup := filepath.Join(s.Dir, "recovery", time.Now().UTC().Format("20060102T150405.000000000Z")+"-semantic-recovery-marker_"+strings.TrimPrefix(NewID("marker"), "marker_"))
	if err := os.MkdirAll(markerBackup, 0o700); err != nil {
		return SemanticMismatchRecoveryPlan{}, "", err
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		data, readErr := os.ReadFile(filepath.Join(s.Dir, name))
		if readErr != nil {
			return SemanticMismatchRecoveryPlan{}, markerBackup, readErr
		}
		if writeErr := fsutil.WriteFile(filepath.Join(markerBackup, name), data, 0o600); writeErr != nil {
			return SemanticMismatchRecoveryPlan{}, markerBackup, writeErr
		}
	}
	j := map[string]any{
		"version": 1, "status": "prepared", "reason": plan.RecoveryReason, "backup": plan.Backup,
		"marker_backup": markerBackup, "state_sha256": plan.StateSHA256, "events_sha256": plan.EventsSHA256,
		"operator_confirmation": map[string]bool{"exact_semantic_mismatch_backup": true},
	}
	journalPath := filepath.Join(markerBackup, "restore-journal.json")
	if err := fsutil.WriteJSON(journalPath, j, 0o600); err != nil {
		return SemanticMismatchRecoveryPlan{}, markerBackup, err
	}
	txn := quarantineRecoveryTransaction{
		Version: 1, RepoID: s.RepoID,
		StateFile: filepath.Join(plan.Backup, "state.json"), EventsFile: filepath.Join(plan.Backup, "events.jsonl"),
		StateSHA256: plan.StateSHA256, EventsSHA256: plan.EventsSHA256,
	}
	if err := fsutil.WriteJSON(s.quarantineRecoveryTransactionPath(), txn, 0o600); err != nil {
		return SemanticMismatchRecoveryPlan{}, markerBackup, err
	}
	if err := s.completeQuarantineRecoveryUnlocked(); err != nil {
		return SemanticMismatchRecoveryPlan{}, markerBackup, err
	}
	j["status"] = "completed"
	if err := fsutil.WriteJSON(journalPath, j, 0o600); err != nil {
		return SemanticMismatchRecoveryPlan{}, markerBackup, err
	}
	return plan, markerBackup, nil
}

func (s Store) ApplyLegacyMergedIdentityRecovery(expectedBackup string, repairs []LegacyMergedIdentityRepair) (Snapshot, string, error) {
	if err := s.ensureDir(); err != nil {
		return Snapshot{}, "", err
	}
	lock, err := s.lock(true)
	if err != nil {
		return Snapshot{}, "", err
	}
	defer unlock(lock)
	restored, plan, err := s.legacyMergedIdentityRecoveryPlanUnlocked(expectedBackup)
	if err != nil {
		return Snapshot{}, "", err
	}
	byIssue := make(map[int]LegacyMergedIdentityRepair, len(repairs))
	for _, repair := range repairs {
		if repair.IssueNumber <= 0 || repair.PullRequestNumber <= 0 || repair.PullRequestURL == "" || repair.Branch == "" || !validCommitSHA(repair.HeadSHA) {
			return Snapshot{}, "", fmt.Errorf("Issue #%d repair identity is incomplete", repair.IssueNumber)
		}
		if _, duplicate := byIssue[repair.IssueNumber]; duplicate {
			return Snapshot{}, "", fmt.Errorf("Issue #%d repair is duplicated", repair.IssueNumber)
		}
		byIssue[repair.IssueNumber] = repair
	}
	if len(byIssue) != len(plan.Targets) {
		return Snapshot{}, "", fmt.Errorf("repair count %d does not match target count %d", len(byIssue), len(plan.Targets))
	}
	for _, target := range plan.Targets {
		repair, ok := byIssue[target.IssueNumber]
		if !ok || repair.PullRequestURL != target.PullRequestURL || repair.Branch != target.Branch ||
			(target.PullRequestNumber > 0 && repair.PullRequestNumber != target.PullRequestNumber) {
			return Snapshot{}, "", fmt.Errorf("Issue #%d repair changed retained Pull Request identity", target.IssueNumber)
		}
		issue := restored.Issues[strconv.Itoa(target.IssueNumber)]
		issue.PullRequestNumber = repair.PullRequestNumber
		issue.HeadSHA = repair.HeadSHA
	}
	if err := restored.Validate(); err != nil {
		return Snapshot{}, "", fmt.Errorf("validate repaired snapshot: %w", err)
	}

	markerBackup := filepath.Join(s.Dir, "recovery", time.Now().UTC().Format("20060102T150405.000000000Z")+"-recovery-marker_"+strings.TrimPrefix(NewID("marker"), "marker_"))
	if err := os.MkdirAll(markerBackup, 0o700); err != nil {
		return Snapshot{}, "", err
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		data, readErr := os.ReadFile(filepath.Join(s.Dir, name))
		if readErr != nil {
			return Snapshot{}, markerBackup, readErr
		}
		if writeErr := fsutil.WriteFile(filepath.Join(markerBackup, name), data, 0o600); writeErr != nil {
			return Snapshot{}, markerBackup, writeErr
		}
	}

	now := time.Now().UTC()
	restored.StateRevision++
	restored.Supervisor.UpdatedAt = now
	payload := map[string]any{"backup": plan.Backup, "recovery_marker_backup": markerBackup, "operator_confirmed": true, "repairs": repairs}
	payloadJSON, err := redact.Marshal(payload, s.Secrets)
	if err != nil {
		return Snapshot{}, markerBackup, err
	}
	event := Event{Version: CurrentVersion, EventID: NewID("evt"), Sequence: restored.StateRevision, Timestamp: now,
		RepoID: s.RepoID, Type: "legacy_merged_identity_quarantine_recovered", Payload: payloadJSON}
	eventLine, err := json.Marshal(event)
	if err != nil {
		return Snapshot{}, markerBackup, err
	}
	events, err := os.ReadFile(filepath.Join(plan.Backup, "events.jsonl"))
	if err != nil {
		return Snapshot{}, markerBackup, err
	}
	if len(events) > 0 && events[len(events)-1] != '\n' {
		return Snapshot{}, markerBackup, errors.New("quarantined event log has a partial tail")
	}
	events = append(events, append(eventLine, '\n')...)
	stateJSON, err := redact.Marshal(restored, s.Secrets)
	if err != nil {
		return Snapshot{}, markerBackup, err
	}
	stateJSON = append(stateJSON, '\n')
	stagedState := filepath.Join(markerBackup, "restored-state.json")
	stagedEvents := filepath.Join(markerBackup, "restored-events.jsonl")
	if err := fsutil.WriteFile(stagedState, stateJSON, 0o600); err != nil {
		return Snapshot{}, markerBackup, err
	}
	if err := fsutil.WriteFile(stagedEvents, events, 0o600); err != nil {
		return Snapshot{}, markerBackup, err
	}
	journal := map[string]any{"version": 1, "status": "prepared", "backup": plan.Backup, "marker_backup": markerBackup,
		"state_sha256": fileSHA256(stateJSON), "events_sha256": fileSHA256(events)}
	journalPath := filepath.Join(markerBackup, "restore-journal.json")
	if err := fsutil.WriteJSON(journalPath, journal, 0o600); err != nil {
		return Snapshot{}, markerBackup, err
	}
	txn := quarantineRecoveryTransaction{Version: 1, RepoID: s.RepoID, StateFile: stagedState, EventsFile: stagedEvents,
		StateSHA256: fileSHA256(stateJSON), EventsSHA256: fileSHA256(events)}
	if err := fsutil.WriteJSON(s.quarantineRecoveryTransactionPath(), txn, 0o600); err != nil {
		return Snapshot{}, markerBackup, err
	}
	if err := s.completeQuarantineRecoveryUnlocked(); err != nil {
		return Snapshot{}, markerBackup, err
	}
	loaded, err := s.recoverUnlocked()
	if err != nil {
		return Snapshot{}, markerBackup, err
	}
	journal["status"] = "completed"
	if err := fsutil.WriteJSON(journalPath, journal, 0o600); err != nil {
		return Snapshot{}, markerBackup, err
	}
	return loaded, markerBackup, nil
}

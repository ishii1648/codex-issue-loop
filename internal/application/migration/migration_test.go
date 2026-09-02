package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

const legacyPublicationCheckpointStage issuedomain.ContinuationStage = "publication_recovery_pending"

func TestApplyMigratesV4FixturesAndRestoreRecoversOriginalBytes(t *testing.T) {
	l, repo, original := writeV4Fixture(t, false)
	legacyCredential := filepath.Join(l.RepoDir("repo-1"), "notification-token")
	if err := os.WriteFile(legacyCredential, []byte("retained-legacy-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || !report.NeedsMigration || len(report.Unsupported) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	for path, want := range original {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("preview modified %s: err=%v", path, readErr)
		}
	}
	fixed := time.Date(2026, 8, 16, 1, 2, 3, 4, time.UTC)
	result, err := (Migrator{Layout: l, Now: func() time.Time { return fixed }}).Apply()
	if err != nil || !result.Changed || result.Backup == "" || result.From != 4 || result.To != CurrentVersion {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	manifest, err := readManifest(filepath.Join(result.Backup, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range manifest.Entries {
		if entry.Source == legacyCredential {
			t.Fatalf("legacy credential was included in migration backup: %+v", entry)
		}
		data, readErr := os.ReadFile(filepath.Join(result.Backup, entry.Backup))
		if readErr != nil || bytes.Contains(data, []byte("retained-legacy-credential")) {
			t.Fatalf("migration backup contains legacy credential material: path=%s err=%v", entry.Backup, readErr)
		}
	}
	report, err = Inspect(l)
	if err != nil || report.NeedsMigration || len(report.Unsupported) != 0 {
		t.Fatalf("post-migration report=%+v err=%v", report, err)
	}
	if _, err := config.Load(repo); err != nil {
		t.Fatalf("load migrated config: %v", err)
	}
	loadedRegistry, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil || loadedRegistry.Version != registry.CurrentVersion {
		t.Fatalf("registry=%+v err=%v", loadedRegistry, err)
	}
	snapshot, err := (state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}).Load()
	if err != nil || snapshot.Version != state.CurrentVersion || snapshot.SemanticContractVersion != statecontract.CurrentVersion || snapshot.StateRevision != 2 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	events, err := os.ReadFile(filepath.Join(l.RepoDir("repo-1"), "events.jsonl"))
	if err != nil || !bytes.Contains(events, []byte(`"type":"semantic_migration_applied"`)) ||
		!bytes.Contains(events, []byte(`"authority":"operator"`)) || !bytes.Contains(events, []byte(`"provenance_synthesized":false`)) {
		t.Fatalf("semantic migration audit is incomplete: err=%v events=%s", err, events)
	}
	for _, path := range []string{filepath.Join(repo, config.FileName), filepath.Join(l.RepoDir("repo-1"), "state.json"), filepath.Join(l.RepoDir("repo-1"), "events.jsonl")} {
		data, readErr := os.ReadFile(path)
		if readErr != nil || bytes.Contains(data, []byte(`"notifications"`)) || bytes.Contains(data, []byte(`notification_`)) {
			t.Fatalf("legacy delivery data remains in %s: err=%v data=%s", path, readErr, data)
		}
	}
	if data, readErr := os.ReadFile(legacyCredential); readErr != nil || string(data) != "retained-legacy-credential\n" {
		t.Fatalf("legacy credential was modified: err=%v data=%q", readErr, data)
	}

	restored, err := (Migrator{Layout: l, Now: func() time.Time { return fixed.Add(time.Minute) }}).Restore(result.Backup)
	if err != nil || !restored.Restored || restored.To != 4 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	for path, want := range original {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("restore %s mismatch: err=%v\ngot=%s\nwant=%s", path, readErr, got, want)
		}
	}
	if data, readErr := os.ReadFile(legacyCredential); readErr != nil || string(data) != "retained-legacy-credential\n" {
		t.Fatalf("rollback modified legacy credential: err=%v data=%q", readErr, data)
	}
}

func TestZeitreise442MissingWorkspaceMigratesToIsolatedQuarantine(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos")}
	dir := l.RepoDir("zeitreise")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	issueData, err := os.ReadFile(filepath.Join("..", "..", "adapter", "state", "testdata", "zeitreise-442-v0614-missing-workspace-resume-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	stateData := []byte(fmt.Sprintf(`{"version":4,"repo_id":"repo_zeitreise","repo_path":"/tmp/zeitreise","state_revision":3776,"supervisor":{"state":"stopped","updated_at":"2026-08-17T12:34:00Z"},"issues":{"442":%s},"pending_requests":{}}`+"\n", issueData))
	eventData, err := os.ReadFile(filepath.Join("..", "..", "adapter", "state", "testdata", "zeitreise-442-v0614-missing-workspace-resume-events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	eventsPath := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(statePath, stateData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, eventData, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || len(report.NonMigratable) != 0 || len(report.SemanticFindings) != 1 || !report.SemanticFindings[0].Migratable {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := (Migrator{Layout: l, Now: func() time.Time { return time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) }}).Apply(); err != nil {
		t.Fatal(err)
	}
	migrated, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(migrated, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["442"]
	if item == nil || item.Lease != nil || item.Suspension == nil || item.Suspension.Status != issuedomain.SuspensionQuarantined ||
		item.Suspension.Recoverability != issuedomain.RecoverabilityAmbiguous || len(item.Suspension.AllowedActions) != 1 ||
		item.Suspension.AllowedActions[0] != issuedomain.ResolutionCancel {
		t.Fatalf("migrated #442=%+v", item)
	}
}

func TestApplyMigratesV5SemanticV2CheckpointWithoutChangingEvidence(t *testing.T) {
	l, repo, _ := writeV4Fixture(t, false)
	first := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	if _, err := (Migrator{Layout: l, Now: func() time.Time { return first }}).Apply(); err != nil {
		t.Fatal(err)
	}
	repo = canonicalizeFixtureRegistry(t, l, repo)
	store := state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	originalEvidence := &state.ContinuationEvidence{Origin: "publisher", Phase: "pre_publication", Code: "durable_base_sha_missing", Status: "failed", ObservedAt: first}
	snapshot.SemanticContractVersion = 2
	workspace := &state.WorkerWorkspace{Path: filepath.Join(repo, "worktrees", "issue-7"), Branch: "codex/issue-7",
		RepoID: "repo-1", Repository: "owner/repo", GitCommonDir: filepath.Join(repo, ".git"), MainCheckout: repo, CapturedAt: first}
	snapshot.Issues["7"] = &state.Issue{
		Number: 7, Status: issuedomain.StatusFailed, RunID: "run_7", Attempts: 1, LeaseGeneration: 1,
		Branch: workspace.Branch, Worktree: workspace.Path, Workspace: workspace,
		LastError: "retained reason", FailureKind: "issue", UpdatedAt: first,
		ResourcePark: &state.ContinuationCheckpoint{
			ID: "checkpoint_7", Status: issuedomain.ResourceParkStatusParked,
			OriginalLease: state.ExecutionLease{Owner: state.LeaseOwner{RunID: "run_7", Generation: 1}, Slot: 0,
				DeclaredResources: []string{}, ResolvedResources: []string{state.RepositoryResource}, ReservedAt: first},
			ParkedAt: first, RunID: "run_7", Workspace: workspace, Stage: legacyPublicationCheckpointStage,
			Evidence: originalEvidence,
		},
		Suspension: &state.Suspension{ID: "suspension_7", Origin: "publisher", Status: issuedomain.SuspensionActive,
			ReasonCode: "durable_base_sha_missing", Recoverability: issuedomain.RecoverabilityOperator,
			Reason: "retained reason", AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage},
			CheckpointID: "checkpoint_7", SuspendedAt: first},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || !report.NeedsMigration || len(report.NonMigratable) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	second := first.Add(time.Hour)
	if _, err := (Migrator{Layout: l, Now: func() time.Time { return second }}).Apply(); err != nil {
		t.Fatal(err)
	}
	migrated, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := migrated.Issues["7"]
	if migrated.SemanticContractVersion != statecontract.CurrentVersion || item == nil || item.ResourcePark == nil ||
		item.ResourcePark.Stage != issuedomain.ContinuationStagePublish || !reflect.DeepEqual(item.ResourcePark.Evidence, originalEvidence) ||
		item.Suspension == nil || item.Suspension.Reason != "retained reason" || item.Lease != nil {
		t.Fatalf("migrated snapshot=%+v issue=%+v", migrated, item)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil || !bytes.Contains(events, []byte(`"before":{"semantic_contract_version":2,"state_schema_version":5}`)) {
		t.Fatalf("semantic v2 audit missing: err=%v events=%s", err, events)
	}
}

func TestApplyMigratesExactQuarantinedSemanticSnapshotWithoutChangingBackup(t *testing.T) {
	l, repo, _ := writeV4Fixture(t, false)
	first := time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)
	if _, err := (Migrator{Layout: l, Now: func() time.Time { return first }}).Apply(); err != nil {
		t.Fatal(err)
	}
	repo = canonicalizeFixtureRegistry(t, l, repo)
	store := state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.SemanticContractVersion = 2
	snapshot.RepoPath = repo
	snapshot.Supervisor = state.Supervisor{State: "stopped", UpdatedAt: first}
	snapshot.Issues["183"] = &state.Issue{Number: 183, Status: issuedomain.StatusCompleted, Attempts: 1, UpdatedAt: first}
	sourceState, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sourceState = append(sourceState, '\n')
	sourceEvents, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(store.Dir, "recovery", "20260903T000100.000000000Z-backup_semantic")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "state.json"), sourceState, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "events.jsonl"), sourceEvents, 0o600); err != nil {
		t.Fatal(err)
	}
	reason := (state.SemanticContractVersionError{Actual: 2, Expected: statecontract.CurrentVersion}).Error()
	marker := state.Snapshot{
		Version: state.CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion,
		RepoID: "repo-1", RepoPath: repo, StateRevision: 1,
		Supervisor: state.Supervisor{State: "blocked", UpdatedAt: first, Message: reason},
		Issues:     map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{},
		Recovery: &state.Recovery{Status: state.RecoveryStateBlocked, Reason: reason, BackupDir: backup, DetectedAt: first},
	}
	markerState, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	markerEvent, err := json.Marshal(state.Event{Version: state.CurrentVersion, EventID: "evt_recovery_marker", Sequence: 1,
		Timestamp: first, RepoID: "repo-1", Type: "recovery_blocked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), append(markerState, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.EventsPath(), append(markerEvent, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := InspectQuarantinedSemantic(l, repo, backup)
	if err != nil || !report.ApplyAllowed || !report.Plan.Eligible || report.Plan.FromSemanticContract != 2 ||
		report.Plan.ToSemanticContract != statecontract.CurrentVersion || report.Plan.IssueCount != 1 || len(report.NonMigratable) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	beforeState := append([]byte(nil), sourceState...)
	beforeEvents := append([]byte(nil), sourceEvents...)
	second := first.Add(time.Hour)
	result, err := (Migrator{Layout: l, Now: func() time.Time { return second }}).ApplyQuarantinedSemantic(repo, backup)
	if err != nil || !result.Changed || result.RepositoryID != "repo-1" || result.SnapshotRevision != snapshot.StateRevision+1 || result.IssueCount != 1 || result.RecoveryMarkerBackup == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for path, want := range map[string][]byte{
		filepath.Join(backup, "state.json"):   beforeState,
		filepath.Join(backup, "events.jsonl"): beforeEvents,
	} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("quarantine backup changed: path=%s err=%v", path, readErr)
		}
	}
	migrated, err := store.Load()
	if err != nil || migrated.SemanticContractVersion != statecontract.CurrentVersion || migrated.StateRevision != snapshot.StateRevision+1 ||
		len(migrated.Issues) != 1 || migrated.Issues["183"] == nil || migrated.Recovery != nil {
		t.Fatalf("migrated=%+v err=%v", migrated, err)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil || !bytes.Contains(events, []byte(`"source":"agent-loop migrate --apply --quarantined-backup"`)) ||
		!bytes.Contains(events, []byte(`"quarantined_backup":true`)) {
		t.Fatalf("quarantined migration audit missing: err=%v events=%s", err, events)
	}
	if _, err := os.Stat(filepath.Join(result.RecoveryMarkerBackup, "migration-journal.json")); err != nil {
		t.Fatalf("recovery marker backup was not retained: %v", err)
	}
	if _, err := InspectQuarantinedSemantic(l, repo, backup); err == nil || !strings.Contains(err.Error(), "isolated recovery-blocked marker") {
		t.Fatalf("completed migration remained replayable: %v", err)
	}
}

func TestQuarantinedSemanticMigrationRejectsUnrelatedRecoveryReason(t *testing.T) {
	l, repo, _ := writeV4Fixture(t, false)
	if _, err := (Migrator{Layout: l}).Apply(); err != nil {
		t.Fatal(err)
	}
	repo = canonicalizeFixtureRegistry(t, l, repo)
	store := state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}
	backup := filepath.Join(store.Dir, "recovery", "unrelated")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	source := fmt.Sprintf(`{"version":5,"semantic_contract_version":2,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"stopped"},"issues":{},"pending_requests":{}}`, repo)
	if err := os.WriteFile(filepath.Join(backup, "state.json"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := state.Snapshot{Version: state.CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion,
		RepoID: "repo-1", RepoPath: repo, StateRevision: 1, Supervisor: state.Supervisor{State: "blocked"},
		Issues: map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{},
		Recovery: &state.Recovery{Status: state.RecoveryStateBlocked, Reason: "state revision mismatch", BackupDir: backup}}
	markerData, _ := json.Marshal(marker)
	eventData, _ := json.Marshal(state.Event{Version: state.CurrentVersion, EventID: "evt_marker", Sequence: 1, RepoID: "repo-1", Type: "recovery_blocked"})
	if err := os.WriteFile(store.StatePath(), append(markerData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.EventsPath(), append(eventData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectQuarantinedSemantic(l, repo, backup); err == nil || !strings.Contains(err.Error(), "exact semantic contract mismatch") {
		t.Fatalf("unrelated recovery reason was accepted: %v", err)
	}
}

func canonicalizeFixtureRegistry(t *testing.T, l layout.Layout, repo string) string {
	t.Helper()
	canonical, err := config.CanonicalRepoPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	current, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil {
		t.Fatal(err)
	}
	entry := current.Repos["repo-1"]
	entry.RepoPath = canonical
	current.Repos["repo-1"] = entry
	data, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.RegistryPath, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestFaultMigrationCanRestorePreparedBackupWithoutTouchingWorktree(t *testing.T) {
	l, repo, original := writeV4Fixture(t, false)
	marker := filepath.Join(repo, "operator-owned-worktree-file")
	if err := os.WriteFile(marker, []byte("unchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("fault after durable artifact write")
	_, err := (Migrator{Layout: l, AfterWrite: func(string) error { return injected }}).Apply()
	if !errors.Is(err, injected) {
		t.Fatalf("fault=%v", err)
	}
	j, exists, err := (Migrator{Layout: l}).loadJournal()
	if err != nil || !exists || j.Status != "prepared" {
		t.Fatalf("journal=%+v exists=%v err=%v", j, exists, err)
	}
	if _, err := (Migrator{Layout: l}).Restore(j.Backup); err != nil {
		t.Fatal(err)
	}
	for path, want := range original {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("fault rollback mismatch for %s: err=%v", path, readErr)
		}
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "unchanged\n" {
		t.Fatalf("worktree marker changed: %q err=%v", got, err)
	}
}

func TestInterruptedApplyReusesJournalAndConvergesIdempotently(t *testing.T) {
	l, repo, _ := writeV4Fixture(t, false)
	fixed := time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC)
	writes := 0
	first := Migrator{Layout: l, Now: func() time.Time { return fixed }, AfterWrite: func(string) error {
		writes++
		if writes == 1 {
			return errors.New("injected stop")
		}
		return nil
	}}
	if _, err := first.Apply(); err == nil || !strings.Contains(err.Error(), "injected stop") {
		t.Fatalf("expected injected stop, got %v", err)
	}
	j, exists, err := (Migrator{Layout: l}).loadJournal()
	if err != nil || !exists || j.Status != "prepared" {
		t.Fatalf("journal=%+v exists=%v err=%v", j, exists, err)
	}
	backup := j.Backup
	manifest, err := readManifest(filepath.Join(backup, "manifest.json"))
	if err != nil || len(manifest.Entries) == 0 {
		t.Fatalf("manifest=%+v err=%v", manifest, err)
	}
	firstBackup := filepath.Join(backup, manifest.Entries[0].Backup)
	backupData, err := os.ReadFile(firstBackup)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firstBackup, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (Migrator{Layout: l}).Apply(); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("corrupt prepared backup was accepted: %v", err)
	}
	if err := os.WriteFile(firstBackup, backupData, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := (Migrator{Layout: l, Now: func() time.Time { return fixed.Add(time.Minute) }}).Apply()
	if err != nil || !result.Changed || result.Backup != backup {
		t.Fatalf("resume=%+v err=%v", result, err)
	}
	again, err := (Migrator{Layout: l}).Apply()
	if err != nil || again.Changed {
		t.Fatalf("idempotent apply=%+v err=%v", again, err)
	}
	snapshot, err := (state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}).Load()
	if err != nil || snapshot.StateRevision != 2 {
		t.Fatalf("prepared transaction was not recovered: snapshot=%+v err=%v", snapshot, err)
	}
}

func TestUnsupportedVersionIsRejectedWithoutBackup(t *testing.T) {
	l, _, _ := writeV4Fixture(t, false)
	if err := os.WriteFile(l.RegistryPath, []byte(`{"version":6,"repos":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || len(report.Unsupported) != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if _, err := (Migrator{Layout: l}).Apply(); err == nil || !strings.Contains(err.Error(), "unsupported schema version") {
		t.Fatalf("expected unsupported error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Root, "migrations")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsupported migration created backup: %v", err)
	}
}

func TestLegacyManifestEntryDefaultsToExistingSource(t *testing.T) {
	if !backupEntryExisted(backupEntry{Backup: "files/0001"}) {
		t.Fatal("legacy manifest entry would be treated as a migration-created file")
	}
	if backupEntryExisted(backupEntry{}) {
		t.Fatal("synthetic absent event entry was treated as an existing source")
	}
}

func TestV4ActiveLeaseAndParkedContinuationBlockRollback(t *testing.T) {
	l, repo, _ := writeV4Fixture(t, false)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	v4 := fmt.Sprintf(`{"version":4,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"running","updated_at":"2026-08-16T00:00:00Z"},"issues":{"63":{"number":63,"title":"active","status":"needs_input","run_id":"run_63","attempts":1,"continuations":0,"branch":"codex/issue-63","worktree":%q,"workspace":{"path":%q,"branch":"codex/issue-63","repo_id":"repo-1","repository":"owner/repo","git_common_dir":%q,"main_checkout":%q,"captured_at":"2026-08-16T00:01:00Z"},"updated_at":"2026-08-16T00:02:00Z","lease_generation":1,"lease":{"owner":{"run_id":"run_63","generation":1},"slot":0,"declared_resources":[],"resolved_resources":["repo:*"],"reserved_at":"2026-08-16T00:02:00Z"}}},"pending_requests":{}}`+"\n", repo, repo, repo, filepath.Join(repo, ".git"), repo)
	if err := os.WriteFile(statePath, []byte(v4), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || !report.NeedsMigration {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	result, err := (Migrator{Layout: l, Now: func() time.Time { return time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC) }}).Apply()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := (state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}).Load()
	if err != nil {
		t.Fatal(err)
	}
	issue := loaded.Issues["63"]
	if issue == nil || issue.Lease == nil || issue.Lease.Owner != (state.LeaseOwner{RunID: "run_63", Generation: 1}) ||
		len(issue.Lease.ResolvedResources) != 1 || issue.Lease.ResolvedResources[0] != state.RepositoryResource || issue.Status != issuedomain.StatusNeedsInput {
		t.Fatalf("migrated issue=%+v", issue)
	}
	if _, err := (Migrator{Layout: l}).Restore(result.Backup); err == nil || !strings.Contains(err.Error(), "active resource lease") {
		t.Fatalf("active lease rollback was accepted: %v", err)
	}
	store := state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}
	if _, err := store.Update("issue_blocked", 63, issue.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["63"]
		item.Status = issuedomain.StatusBlocked
		if err := state.CaptureContinuationLease(item, item.Lease.Owner, "park_63", time.Now().UTC()); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Migrator{Layout: l}).Restore(result.Backup); err == nil || !strings.Contains(err.Error(), "parked resource continuation") {
		t.Fatalf("parked continuation rollback was accepted: %v", err)
	}
	if _, err := store.Update("test_park_completed", 63, issue.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["63"].ResourcePark = nil
		snapshot.Issues["63"].Suspension = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := (Migrator{Layout: l}).Restore(result.Backup)
	if err != nil || !restored.Restored || restored.To != 4 {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, []byte(v4)) {
		t.Fatalf("v4 active state was not restored: %s err=%v", after, err)
	}
}

func TestV4PreparedTransactionMigratesItsSnapshotThroughTheSameV5Boundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.txn.json")
	startedAt := time.Date(2026, 9, 2, 1, 2, 3, 0, time.UTC)
	transaction := `{"version":4,"snapshot":{"version":4,"semantic_contract_version":1,"repo_id":"repo-1","repo_path":"/sanitized/repo","state_revision":2,"supervisor":{"state":"stopped","updated_at":"2026-09-02T00:00:00Z"},"issues":{"1":{"number":1,"status":"blocked","run_id":"run_1","lease_generation":1,"lease":{"owner":{"run_id":"run_1","generation":1},"slot":0,"declared_resources":[],"resolved_resources":["repo:*"],"reserved_at":"2026-09-02T00:00:00Z"},"blocked_cause":{"origin":"worker","kind":"environment","resumable":true,"reason":"offline","blocked_at":"2026-09-02T00:00:00Z"},"last_error":"offline","updated_at":"2026-09-02T00:00:00Z"}},"pending_requests":{}},"event":{"version":4,"event_id":"evt-2","sequence":2,"timestamp":"2026-09-02T00:00:00Z","repo_id":"repo-1","issue_number":1,"run_id":"run_1","type":"issue_blocked"}}`
	if err := os.WriteFile(path, []byte(transaction), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateTransaction(path, journal{StartedAt: startedAt}); err != nil {
		t.Fatal(err)
	}
	object, err := readRawObject(path)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(object["snapshot"], &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if snapshot.Version != 5 || snapshot.SemanticContractVersion != statecontract.CurrentVersion || item == nil || item.Lease != nil ||
		item.ResourcePark == nil || item.Suspension == nil || item.Suspension.Status != issuedomain.SuspensionQuarantined {
		t.Fatalf("migrated transaction snapshot=%+v Issue=%+v", snapshot, item)
	}
	var rawSnapshot struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(object["snapshot"], &rawSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(rawSnapshot.Issues["1"]["lease"]) != 0 || len(rawSnapshot.Issues["1"]["blocked_cause"]) != 0 {
		t.Fatalf("prepared transaction retained legacy runtime fields: %s", object["snapshot"])
	}
}

func writeV4Fixture(t *testing.T, withTransaction bool) (layout.Layout, string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "target")
	l := layout.Layout{
		Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"),
		BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills"), LaunchAgents: filepath.Join(root, "launch-agents"),
	}
	for _, dir := range []string{repo, l.ReposRoot, l.BinDir, l.SkillsDir, l.LaunchAgents, l.RepoDir("repo-1")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(repo, config.FileName)
	registryJSON := fmt.Sprintf(`{"version":4,"repos":{"repo-1":{"repo_id":"repo-1","repo_path":%q,"github_repo":"owner/repo","registered_at":"2026-08-16T00:00:00Z","commands":{}}}}`, repo)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	eventsPath := filepath.Join(l.RepoDir("repo-1"), "events.jsonl")
	files := map[string][]byte{
		configPath:     []byte("# retained comment\nversion: 4\ngithub:\n  repo: owner/repo\n"),
		l.RegistryPath: []byte(registryJSON + "\n"),
		statePath:      []byte(fmt.Sprintf(`{"version":4,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:00:00Z"},"issues":{},"pending_requests":{}}`+"\n", repo)),
		eventsPath:     []byte(`{"version":4,"event_id":"evt-1","sequence":1,"timestamp":"2026-08-16T00:00:00Z","repo_id":"repo-1","type":"supervisor_stopped"}` + "\n"),
	}
	if withTransaction {
		txnPath := filepath.Join(l.RepoDir("repo-1"), "state.txn.json")
		files[txnPath] = []byte(fmt.Sprintf(`{"version":4,"snapshot":{"version":4,"repo_id":"repo-1","repo_path":%q,"state_revision":2,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:01:00Z"},"issues":{},"pending_requests":{}},"event":{"version":4,"event_id":"evt-2","sequence":2,"timestamp":"2026-08-16T00:01:00Z","repo_id":"repo-1","type":"supervisor_controlled"}}`+"\n", repo))
	}
	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original := make(map[string][]byte, len(files))
	for path, data := range files {
		original[path] = append([]byte(nil), data...)
	}
	return l, repo, original
}

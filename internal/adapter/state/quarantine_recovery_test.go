package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

func TestSemanticMismatchRecoveryRestoresOneExactBackupWithoutRewritingIt(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("checkpoint", 0, "", nil, func(*Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var original Snapshot
	if err := json.Unmarshal(data, &original); err != nil {
		t.Fatal(err)
	}
	original.SemanticContractVersion--
	if err := fsutil.WriteJSON(store.StatePath(), original, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.quarantineUnlocked(SemanticContractVersionError{Version: original.SemanticContractVersion, Current: original.SemanticContractVersion + 1})
	if err != nil || blocked.Recovery == nil {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	backup := blocked.Recovery.BackupDir
	backupStateBefore, err := os.ReadFile(filepath.Join(backup, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	backupEventsBefore, err := os.ReadFile(filepath.Join(backup, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewSemanticMismatchRecovery(filepath.Join(store.Dir, "recovery", "wrong")); err == nil {
		t.Fatal("mismatched semantic recovery backup was accepted")
	}
	plan, err := store.PreviewSemanticMismatchRecovery(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Eligible || !plan.SemanticMigrationRequired || plan.RestoredRecoveryMarker || plan.RestoredRevision != original.StateRevision || plan.RestoredSemanticContract != original.SemanticContractVersion {
		t.Fatalf("plan=%+v", plan)
	}
	applied, markerBackup, err := store.ApplySemanticMismatchRecovery(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Eligible || markerBackup == "" {
		t.Fatalf("applied=%+v marker=%q", applied, markerBackup)
	}
	liveState, err := os.ReadFile(store.StatePath())
	if err != nil || !bytes.Equal(liveState, backupStateBefore) {
		t.Fatalf("restored state differs from backup: err=%v", err)
	}
	liveEvents, err := os.ReadFile(store.EventsPath())
	if err != nil || !bytes.Equal(liveEvents, backupEventsBefore) {
		t.Fatalf("restored events differ from backup: err=%v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("restored older semantic contract was accepted before migration")
	} else {
		var versionErr SemanticContractVersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("restored error=%T %v", err, err)
		}
	}
	for _, name := range []string{"state.json", "events.jsonl", "restore-journal.json"} {
		if _, err := os.Stat(filepath.Join(markerBackup, name)); err != nil {
			t.Fatalf("missing marker audit %s: %v", name, err)
		}
	}
	backupStateAfter, _ := os.ReadFile(filepath.Join(backup, "state.json"))
	backupEventsAfter, _ := os.ReadFile(filepath.Join(backup, "events.jsonl"))
	if !bytes.Equal(backupStateBefore, backupStateAfter) || !bytes.Equal(backupEventsBefore, backupEventsAfter) {
		t.Fatal("exact semantic recovery backup was modified")
	}
}

func TestLegacyMergedIdentityRecoveryRestoresExactQuarantine(t *testing.T) {
	store, backup := quarantinedLegacyMergedStore(t, false)
	plan, err := store.PreviewLegacyMergedIdentityRecovery(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Eligible || len(plan.Targets) != 1 || plan.Targets[0].IssueNumber != 67 || plan.SnapshotRevision != 1 {
		t.Fatalf("plan=%+v", plan)
	}
	result, markerBackup, err := store.ApplyLegacyMergedIdentityRecovery(backup, []LegacyMergedIdentityRepair{{
		IssueNumber: 67, Branch: "codex/issue-67", PullRequestURL: "https://github.com/owner/repo/pull/87",
		PullRequestNumber: 87, HeadSHA: strings.Repeat("a", 40),
	}})
	if err != nil {
		t.Fatal(err)
	}
	issue := result.Issues["67"]
	if result.Recovery != nil || result.Supervisor.State != "maintenance" || result.StateRevision != 2 ||
		issue == nil || issue.PullRequestNumber != 87 || issue.HeadSHA != strings.Repeat("a", 40) {
		t.Fatalf("result=%+v issue=%+v", result, issue)
	}
	for _, name := range []string{"state.json", "events.jsonl", "restore-journal.json"} {
		if _, err := os.Stat(filepath.Join(markerBackup, name)); err != nil {
			t.Fatalf("missing recovery marker audit %s: %v", name, err)
		}
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil || !strings.Contains(string(events), `"type":"legacy_merged_identity_quarantine_recovered"`) {
		t.Fatalf("events=%s err=%v", events, err)
	}
}

func TestLegacyMergedIdentityRecoveryRejectsAdditionalInvariantViolation(t *testing.T) {
	store, backup := quarantinedLegacyMergedStore(t, true)
	if _, err := store.PreviewLegacyMergedIdentityRecovery(backup); err == nil || !strings.Contains(err.Error(), "additional invariant violation") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := store.PreviewLegacyMergedIdentityRecovery(filepath.Join(store.Dir, "recovery", "wrong")); err == nil {
		t.Fatal("mismatched backup was accepted")
	}
}

func TestFaultPreparedQuarantineRecoveryCompletesBeforeNormalLoad(t *testing.T) {
	store, backup := quarantinedLegacyMergedStore(t, false)
	result, markerBackup, err := store.ApplyLegacyMergedIdentityRecovery(backup, []LegacyMergedIdentityRepair{{
		IssueNumber: 67, Branch: "codex/issue-67", PullRequestURL: "https://github.com/owner/repo/pull/87",
		PullRequestNumber: 87, HeadSHA: strings.Repeat("a", 40),
	}})
	if err != nil {
		t.Fatal(err)
	}
	stagedState := filepath.Join(markerBackup, "restored-state.json")
	stagedEvents := filepath.Join(markerBackup, "restored-events.jsonl")
	stateData, err := os.ReadFile(stagedState)
	if err != nil {
		t.Fatal(err)
	}
	eventsData, err := os.ReadFile(stagedEvents)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		markerData, readErr := os.ReadFile(filepath.Join(markerBackup, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := fsutil.WriteFile(filepath.Join(store.Dir, name), markerData, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	txn := quarantineRecoveryTransaction{Version: 1, RepoID: store.RepoID, StateFile: stagedState, EventsFile: stagedEvents,
		StateSHA256: fileSHA256(stateData), EventsSHA256: fileSHA256(eventsData)}
	if err := fsutil.WriteJSON(store.quarantineRecoveryTransactionPath(), txn, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.StateRevision != result.StateRevision || loaded.Recovery != nil {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err := os.Stat(store.quarantineRecoveryTransactionPath()); !os.IsNotExist(err) {
		t.Fatalf("recovery transaction remains: %v", err)
	}
}

func quarantinedLegacyMergedStore(t *testing.T, additionalViolation bool) (Store, string) {
	t.Helper()
	store := newStore(t)
	_, err := store.Update("legacy_completed", 67, "run_67", nil, func(snapshot *Snapshot) error {
		snapshot.Supervisor.State = "maintenance"
		snapshot.Issues["67"] = &Issue{Number: 67, Title: "legacy", Status: issuedomain.StatusCompleted, RunID: "run_67",
			Branch: "codex/issue-67", Attempts: 1, PullRequestURL: "https://github.com/owner/repo/pull/87",
			PullRequestNumber: 87, HeadSHA: strings.Repeat("a", 40), PullRequestMerged: true}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Issues["67"].PullRequestNumber = 0
	snapshot.Issues["67"].HeadSHA = strings.Repeat("b", 40)
	if additionalViolation {
		zero := time.Time{}
		snapshot.Issues["67"].RetryAfter = &zero
	}
	if err := fsutil.WriteJSON(store.StatePath(), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.Load()
	if err != nil || blocked.Recovery == nil {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	return store, blocked.Recovery.BackupDir
}

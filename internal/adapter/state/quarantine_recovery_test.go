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

func TestRecoveryValidatorsEnforceManagedRoot(t *testing.T) {
	store := newStore(t)
	root := filepath.Join(store.Dir, "recovery")
	backup := filepath.Join(root, "backup")
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(backup, "state.json")
	if err := os.WriteFile(staged, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(store.Dir, parentLink); err != nil {
		t.Fatal(err)
	}
	for _, validator := range []struct {
		name     string
		validate func(string) error
		valid    string
	}{
		{"backup", func(path string) error {
			resolved, err := store.validateExactRecoveryBackup(path, path)
			if err == nil {
				want, resolveErr := filepath.EvalSymlinks(path)
				if resolveErr != nil || resolved != want {
					t.Errorf("resolved=%q want=%q err=%v", resolved, want, resolveErr)
				}
			}
			return err
		}, backup},
		{"file", store.validateManagedRecoveryFile, staged},
	} {
		t.Run(validator.name, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				path string
			}{
				{"parent", store.Dir},
				{"root", root},
				{"parent_symlink", parentLink},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if err := validator.validate(tc.path); err == nil || !strings.Contains(err.Error(), "outside the managed recovery root") {
						t.Fatalf("path=%q err=%v", tc.path, err)
					}
				})
			}
			t.Run("managed", func(t *testing.T) {
				if err := validator.validate(validator.valid); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestLifecycleMismatchRecoveryRejectsChangedEvidence(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("checkpoint", 0, "", nil, func(*Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	blocked, err := store.quarantineUnlocked(LifecycleAPIVersionError{Version: issuedomain.LifecycleAPICurrent, Current: issuedomain.LifecycleAPIPreviousMinor})
	if err != nil {
		t.Fatal(err)
	}
	markerData, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	markerData = bytes.Replace(markerData,
		[]byte(`"issue_lifecycle_api_version": "`+issuedomain.LifecycleAPICurrent+`"`),
		[]byte(`"issue_lifecycle_api_version": "`+issuedomain.LifecycleAPIPreviousMinor+`"`), 1)
	if err := os.WriteFile(store.StatePath(), markerData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewLifecycleMismatchRecovery(filepath.Join(store.Dir, "recovery", "wrong")); err == nil {
		t.Fatal("unrecorded lifecycle backup was accepted")
	}
	backupState := filepath.Join(blocked.Recovery.BackupDir, "state.json")
	data, err := os.ReadFile(backupState)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data,
		[]byte(`"issue_lifecycle_api_version": "`+issuedomain.LifecycleAPICurrent+`"`),
		[]byte(`"issue_lifecycle_api_version": "99.0"`), 1)
	if err := os.WriteFile(backupState, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewLifecycleMismatchRecovery(blocked.Recovery.BackupDir); err == nil {
		t.Fatal("mutated lifecycle backup was accepted")
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

func TestLegacyMismatchRecoveryDoesNotRewriteStateBeforeMigration(t *testing.T) {
	for _, kind := range []string{"semantic", "lifecycle"} {
		t.Run(kind, func(t *testing.T) {
			store := newStore(t)
			snapshot := store.emptySnapshot()
			snapshot.Version, snapshot.SemanticContractVersion, snapshot.IssueLifecycleAPIVersion = 5, 4, "2.1"
			snapshot.StateRevision = 1
			backup := filepath.Join(store.Dir, "recovery", "legacy")
			snapshot.Supervisor.State = SupervisorStateBlocked
			snapshot.Recovery = &Recovery{Status: RecoveryStateBlocked, BackupDir: backup, Reason: "legacy version mismatch", DetectedAt: time.Now().UTC()}
			if err := fsutil.WriteJSON(store.StatePath(), snapshot, 0600); err != nil {
				t.Fatal(err)
			}
			event := Event{Version: 5, Sequence: 1, RepoID: store.RepoID, Type: "recovery_blocked"}
			if err := store.appendEventUnlocked(event); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			events, err := os.ReadFile(store.EventsPath())
			if err != nil {
				t.Fatal(err)
			}
			if kind == "semantic" {
				_, err = store.PreviewSemanticMismatchRecovery(backup)
			} else {
				_, err = store.PreviewLifecycleMismatchRecovery(backup)
			}
			var versionErr SchemaVersionError
			if !errors.As(err, &versionErr) {
				t.Fatalf("expected migration refusal, got %v", err)
			}
			if kind == "semantic" {
				_, _, err = store.ApplySemanticMismatchRecovery(backup)
			} else {
				_, _, err = store.ApplyLifecycleMismatchRecovery(backup)
			}
			if !errors.As(err, &versionErr) {
				t.Fatalf("expected migration refusal, got %v", err)
			}
			for path, want := range map[string][]byte{store.StatePath(): before, store.EventsPath(): events} {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("modified %s: %v", path, err)
				}
			}
		})
	}
}

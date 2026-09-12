package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

func diagnosticFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(data)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestFaultDiagnosticReadOnly(t *testing.T) {
	for _, scenario := range []string{"normal", "invalid", "unknown schema", "unknown lifecycle", "unknown semantic", "recovery", "prepared", "invalid transaction", "recovery transaction", "missing snapshot", "missing events", "partial events", "invalid events", "missing lock", "writer"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStore(t)
			snapshot, err := store.Update("fixture", 0, "", nil, func(*Snapshot) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			remove := func(path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "invalid":
				snapshot.Issues["1"] = &Issue{Number: 2}
			case "unknown schema":
				snapshot.Version = 999
			case "unknown lifecycle":
				snapshot.IssueLifecycleAPIVersion = "999.0"
				snapshot.Supervisor.State = "future-state"
			case "unknown semantic":
				snapshot.SemanticContractVersion = 999
				snapshot.Supervisor.State = "future-state"
			case "recovery":
				snapshot.Recovery = &Recovery{Status: RecoveryStateBlocked, Reason: "existing evidence"}
			case "prepared":
				events, _, _, err := store.readEventsUnlocked()
				if err != nil {
					t.Fatal(err)
				}
				if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: snapshot, Event: events[0]}, 0600); err != nil {
					t.Fatal(err)
				}
			case "invalid transaction":
				write(store.TransactionPath(), []byte("{"))
			case "recovery transaction":
				write(store.quarantineRecoveryTransactionPath(), []byte("{"))
			case "missing events":
				remove(store.EventsPath())
			case "partial events":
				write(store.EventsPath(), []byte("{"))
			case "invalid events":
				write(store.EventsPath(), []byte("{\n"))
			case "missing lock":
				remove(store.lockPath())
			case "writer":
				lock, err := store.lock(true)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock(lock)
			}
			data, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			write(store.StatePath(), data)
			if scenario == "missing snapshot" {
				remove(store.StatePath())
			}
			before := diagnosticFiles(t, store.Dir)
			_, _, err = store.ReadDiagnosticSnapshot()
			if (err == nil) != (scenario == "normal") {
				t.Fatalf("diagnosis=%v", err)
			}
			if strings.HasPrefix(scenario, "unknown") && !isVersionCompatibilityError(err) {
				t.Fatalf("version misclassified: %v", err)
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, store.Dir)) {
				t.Fatal("diagnosis changed durable files")
			}
		})
	}
}

func TestFaultDiagnosticConcurrentWriter(t *testing.T) {
	store := newStore(t)
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			if _, err := store.Update("concurrent", 0, "", nil, func(*Snapshot) error { return nil }); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for i := 0; i < 100; i++ {
		snapshot, events, err := store.ReadDiagnosticSnapshot()
		if err != nil {
			if !strings.Contains(err.Error(), "STATE_UNCONFIRMED") {
				t.Fatal(err)
			}
		} else if err := validateEventSequence(snapshot, events); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := store.ReadDiagnosticSnapshot()
	if err != nil || snapshot.StateRevision != 50 {
		t.Fatalf("revision=%d err=%v", snapshot.StateRevision, err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "recovery")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected recovery: %v", err)
	}
}

func TestDiagnosticIssue439AnsweredNextCheckpoint(t *testing.T) {
	data, err := os.ReadFile("testdata/issue-439-answered-next-checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Version = CurrentVersion
	snapshot.SemanticContractVersion = 0
	snapshot.IssueLifecycleAPIVersion = ""
	data, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	store.RepoID, store.RepoPath = snapshot.RepoID, snapshot.RepoPath
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.StatePath(), data, 0600); err != nil {
		t.Fatal(err)
	}
	before := diagnosticFiles(t, store.Dir)
	got, _, err := store.ReadDiagnosticSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	old := got.PendingRequests["req_44fe4448eeceeb3a"]
	current := got.Issues["439"].Continuation
	// v0.12.21 (961c523) required this equality even for an answer in Issue.Answers.
	if old.CheckpointID == current.ID || old.ID == current.RequestID {
		t.Fatal("fixture does not exercise the old validator mismatch")
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, store.Dir)) {
		t.Fatal("historical answer caused a durable mutation")
	}
	got.Issues["439"].Answers[0].Answer = "different"
	if err := fsutil.WriteJSON(store.StatePath(), got, 0600); err != nil {
		t.Fatal(err)
	}
	before = diagnosticFiles(t, store.Dir)
	if _, _, err := store.ReadDiagnosticSnapshot(); err == nil {
		t.Fatal("inconsistent answer hidden")
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, store.Dir)) {
		t.Fatal("invalid answer caused quarantine")
	}
}

package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
)

func TestStateAndEventsNeverPersistSecrets(t *testing.T) {
	secret := "configured-secret-value"
	store := Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo", Secrets: []string{secret}}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Update("unsafe_result", 1, "run_1", map[string]string{"stderr": "Bearer abcdefghijklmnopqrstuvwxyz", "custom": secret}, func(value *Snapshot) error {
		value.Issues["1"] = &Issue{Number: 1, Title: "contains " + secret, LastError: "ghp_abcdefghijklmnopqrstuvwxyz123456"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.Issues["1"].Title, secret) || strings.Contains(snapshot.Issues["1"].LastError, "ghp_") {
		t.Fatalf("returned snapshot contains secret: %+v", snapshot.Issues["1"])
	}
	for _, path := range []string{store.StatePath(), store.EventsPath()} {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), secret) || strings.Contains(string(data), "ghp_") || strings.Contains(string(data), "Bearer abc") {
			t.Fatalf("secret persisted in %s: %s", path, data)
		}
		if info, statErr := os.Stat(path); statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe mode for %s: info=%v err=%v", path, info, statErr)
		}
	}
	if info, err := os.Stat(store.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("unsafe state directory mode: info=%v err=%v", info, err)
	}
}

func newStore(t *testing.T) Store {
	t.Helper()
	store := Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestFaultAttentionRevisionPersistsSnapshotAndEvent(t *testing.T) {
	store := newStore(t)
	snapshot, err := store.Update("supervisor_started", 0, "", map[string]string{"ok": "yes"}, func(s *Snapshot) error {
		s.Supervisor.State = "polling"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateRevision != 1 {
		t.Fatalf("revision = %d", snapshot.StateRevision)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.State != "polling" || loaded.StateRevision != 1 {
		t.Fatalf("unexpected snapshot: %+v", loaded)
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"sequence":1`) || !strings.Contains(string(events), `"type":"supervisor_started"`) {
		t.Fatalf("unexpected events: %s", events)
	}
}

func TestLegacySessionIDIsNamespacedAsCodex(t *testing.T) {
	store := newStore(t)
	_, err := store.Update("legacy", 1, "run", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"] = &Issue{Number: 1, SessionID: "legacy-session"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	session := loaded.Issues["1"].Session
	if session == nil || session.Backend != "codex" || session.ID != "legacy-session" {
		t.Fatalf("session=%+v", session)
	}
}

func TestFaultAttentionRemainsStickyUntilAnswered(t *testing.T) {
	store := newStore(t)
	_, err := store.Update("input_requested", 7, "run", nil, func(s *Snapshot) error {
		s.Supervisor.State = "running"
		s.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 7, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Load()
	if reason, ok := snapshot.Attention(false); !ok || reason != "needs_input" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	_, err = store.Update("unrelated", 0, "", nil, func(s *Snapshot) error { s.Supervisor.State = "polling"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Load()
	if reason, ok := snapshot.Attention(false); !ok || reason != "needs_input" {
		t.Fatalf("request was not sticky")
	}
}

func TestUntilIdleWaitsForPullRequestLifecycle(t *testing.T) {
	for _, status := range []string{"awaiting_checks", "awaiting_merge"} {
		t.Run(status, func(t *testing.T) {
			snapshot := Snapshot{
				Supervisor: Supervisor{State: "polling"},
				Issues:     map[string]*Issue{"7": {Number: 7, Status: status}},
			}
			if reason, ok := snapshot.Attention(true); ok {
				t.Fatalf("reason=%q ok=%v", reason, ok)
			}
		})
	}
}

func TestFaultSnapshotWriteCrashRecoversEveryTransactionPoint(t *testing.T) {
	for _, crashPoint := range []string{"prepared", "event_appended", "snapshot_written"} {
		t.Run(crashPoint, func(t *testing.T) {
			store := newStore(t)
			base, err := store.Update("first", 0, "", nil, func(s *Snapshot) error {
				s.Supervisor.State = "polling"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			next.StateRevision++
			next.Supervisor.Message = "transaction completed"
			next.Supervisor.UpdatedAt = time.Now().UTC()
			event := Event{
				Version: CurrentVersion, EventID: "evt_transaction", Sequence: next.StateRevision,
				Timestamp: time.Now().UTC(), RepoID: store.RepoID, Type: "second",
			}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: next, Event: event}, 0o600); err != nil {
				t.Fatal(err)
			}
			if crashPoint == "event_appended" || crashPoint == "snapshot_written" {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			if crashPoint == "snapshot_written" {
				if err := fsutil.WriteJSON(store.StatePath(), next, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			loaded, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if loaded.StateRevision != 2 || loaded.Supervisor.Message != "transaction completed" {
				t.Fatalf("loaded=%+v", loaded)
			}
			if _, err := os.Stat(store.TransactionPath()); !os.IsNotExist(err) {
				t.Fatalf("transaction was not removed: %v", err)
			}
			events, _, partial, err := store.readEventsUnlocked()
			if err != nil || partial || len(events) != 2 || events[1].Type != "second" {
				t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
			}
		})
	}
}

func TestFaultPartialEventTailIsTruncatedAndRecorded(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("first", 0, "", nil, func(s *Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(store.EventsPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"version":2,"sequence":2`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.StateRevision != 2 || loaded.Recovery != nil {
		t.Fatalf("loaded=%+v", loaded)
	}
	events, _, partial, err := store.readEventsUnlocked()
	if err != nil || partial || len(events) != 2 || events[1].Type != "event_log_tail_truncated" {
		t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
	}
}

func TestFaultRevisionMismatchIsQuarantined(t *testing.T) {
	store := newStore(t)
	if _, err := store.Update("first", 0, "", nil, func(s *Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"cause": "missing transaction"})
	if err := store.appendEventUnlocked(Event{
		Version: CurrentVersion, EventID: "evt_orphan", Sequence: 2, Timestamp: time.Now().UTC(),
		RepoID: store.RepoID, Type: "orphan", Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recovery == nil || loaded.Recovery.Status != "blocked" || loaded.Supervisor.State != "blocked" {
		t.Fatalf("loaded=%+v", loaded)
	}
	for _, name := range []string{"state.json", "events.jsonl"} {
		if _, err := os.Stat(filepath.Join(loaded.Recovery.BackupDir, name)); err != nil {
			t.Fatalf("missing recovery backup %s: %v", name, err)
		}
	}
	if _, err := store.Update("must_not_run", 0, "", nil, func(s *Snapshot) error { return nil }); err == nil {
		t.Fatal("recovery-blocked state accepted an update")
	}
	second, err := store.Load()
	if err != nil || second.Recovery == nil || second.StateRevision != 1 {
		t.Fatalf("second load=%+v err=%v", second, err)
	}
}

func TestFaultCorruptSnapshotIsQuarantined(t *testing.T) {
	store := newStore(t)
	if err := os.WriteFile(store.StatePath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Recovery == nil || !strings.Contains(loaded.Recovery.Reason, "decode state") {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestUnsupportedSchemaVersionIsRejectedWithoutQuarantine(t *testing.T) {
	for _, version := range []int{1, CurrentVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			store := newStore(t)
			data, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			var snapshot Snapshot
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot.Version = version
			modified, err := json.MarshalIndent(snapshot, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			modified = append(modified, '\n')
			if err := os.WriteFile(store.StatePath(), modified, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); err == nil {
				t.Fatal("unsupported schema was accepted")
			}
			after, err := os.ReadFile(store.StatePath())
			if err != nil || !bytes.Equal(after, modified) {
				t.Fatalf("unsupported state was modified: err=%v", err)
			}
			if _, err := os.Stat(filepath.Join(store.Dir, "recovery")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported state was quarantined: %v", err)
			}
		})
	}
}

func TestFaultSecondSupervisorCannotAcquireLock(t *testing.T) {
	store := newStore(t)
	first, err := store.AcquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer ReleaseSupervisorLock(first)
	if second, err := store.AcquireSupervisorLock(); err == nil {
		ReleaseSupervisorLock(second)
		t.Fatal("second supervisor acquired the repository lock")
	}
	ReleaseSupervisorLock(first)
	first = nil
	third, err := store.AcquireSupervisorLock()
	if err != nil {
		t.Fatalf("lock was not reusable after release: %v", err)
	}
	ReleaseSupervisorLock(third)
}

func TestFaultEventRotationKeepsCheckpointAndRecoverySequence(t *testing.T) {
	store := Store{
		Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo",
		EventRetention: retention.Policy{MaxBytes: 1, MaxAge: time.Hour, Keep: 2},
	}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 4; index++ {
		if _, err := store.Update("tick", 0, "", map[string]int{"index": index}, func(*Snapshot) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateRevision != 4 {
		t.Fatalf("revision=%d", snapshot.StateRevision)
	}
	events, _, partial, err := store.readEventsUnlocked()
	if err != nil || partial || len(events) == 0 || events[0].Type != "event_log_checkpoint" {
		t.Fatalf("events=%+v partial=%v err=%v", events, partial, err)
	}
	archives, err := filepath.Glob(store.EventsPath() + ".*.gz")
	if err != nil || len(archives) == 0 || len(archives) > 2 {
		t.Fatalf("archives=%v err=%v", archives, err)
	}
}

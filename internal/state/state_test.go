package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

func TestLeaseReservationSurvivesRestartAndFencesStaleOwners(t *testing.T) {
	store := newStore(t)
	reservedAt := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	snapshot, owner, err := store.ReserveLease(LeaseReservation{
		IssueNumber: 7, Title: "durable", RunID: "run_7", Slot: 0,
		DeclaredResources: []string{"state", "docs"}, ResolvedResources: []string{"state", "docs"},
		BaseSHA: "abc123", ReservedAt: reservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := snapshot.Issues["7"].Lease
	if owner != (LeaseOwner{RunID: "run_7", Generation: 1}) || lease == nil || lease.BaseSHA != "abc123" || lease.ReservedAt != reservedAt {
		t.Fatalf("owner=%+v lease=%+v", owner, lease)
	}
	loaded, err := (Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}).Load()
	if err != nil || loaded.Issues["7"].Lease == nil || loaded.Issues["7"].Lease.Owner != owner {
		t.Fatalf("loaded=%+v err=%v", loaded.Issues["7"], err)
	}
	_, err = store.Update("publication_audited", 7, owner.RunID, nil, func(snapshot *Snapshot) error {
		issue := snapshot.Issues["7"]
		issue.ActualResources = []string{"docs", "state"}
		issue.Lease.ActualResources = []string{"docs", "state"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = (Store{Dir: store.Dir, RepoID: store.RepoID, RepoPath: store.RepoPath}).Load()
	if err != nil || !reflect.DeepEqual(loaded.Issues["7"].DeclaredResources, []string{"docs", "state"}) || !reflect.DeepEqual(loaded.Issues["7"].ActualResources, []string{"docs", "state"}) {
		t.Fatalf("resource audit did not survive restart: issue=%+v err=%v", loaded.Issues["7"], err)
	}
	if _, err := store.ReleaseLease(7, LeaseOwner{RunID: "run_other", Generation: 1}, "stale"); err == nil {
		t.Fatal("stale run released another run's lease")
	}
	if _, err := store.ReleaseLease(8, owner, "wrong Issue"); err == nil {
		t.Fatal("owner released another Issue's lease")
	}
	loaded, err = store.Load()
	if err != nil || loaded.Issues["7"].Lease == nil {
		t.Fatalf("stale release changed lease: issue=%+v err=%v", loaded.Issues["7"], err)
	}
	if _, err := store.ReleaseLease(7, owner, "completed"); err != nil {
		t.Fatal(err)
	}
	second, nextOwner, err := store.ReserveLease(LeaseReservation{IssueNumber: 7, RunID: "run_8", Slot: 0, ResolvedResources: []string{"state"}, ReservedAt: reservedAt.Add(time.Hour)})
	if err != nil || nextOwner.Generation != 2 || second.Issues["7"].LeaseGeneration != 2 {
		t.Fatalf("owner=%+v issue=%+v err=%v", nextOwner, second.Issues["7"], err)
	}
	if _, err := store.ReleaseLease(7, owner, "old generation"); err == nil {
		t.Fatal("old generation released replacement lease")
	}
}

func TestLeaseReservationAndExpansionAreExclusive(t *testing.T) {
	store := newStore(t)
	_, first, err := store.ReserveLease(LeaseReservation{IssueNumber: 1, RunID: "run_1", Slot: 0, ResolvedResources: []string{"state"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveLease(LeaseReservation{IssueNumber: 2, RunID: "run_2", Slot: 0, ResolvedResources: []string{"docs"}}); err == nil {
		t.Fatal("occupied slot was reserved twice")
	}
	if _, _, err := store.ReserveLease(LeaseReservation{IssueNumber: 2, RunID: "run_2", Slot: 1, ResolvedResources: []string{"state"}}); err == nil {
		t.Fatal("conflicting resource was reserved twice")
	}
	if _, _, err := store.ReserveLease(LeaseReservation{IssueNumber: 2, RunID: "run_2", Slot: 1, ResolvedResources: []string{"docs"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExpandLease(1, first, []string{"docs"}); err == nil {
		t.Fatal("lease expanded across another active lease")
	}
}

func TestFaultConcurrentLeaseReservationsNeverOverlapResources(t *testing.T) {
	store := newStore(t)
	const contenders = 16
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(contenders)
	results := make(chan error, contenders)
	for number := 1; number <= contenders; number++ {
		go func(number int) {
			defer wait.Done()
			<-start
			_, _, err := store.ReserveLease(LeaseReservation{
				IssueNumber: number, RunID: fmt.Sprintf("run_%d", number), Slot: number - 1,
				ResolvedResources: []string{"scheduler"}, ReservedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
			})
			results <- err
		}(number)
	}
	close(start)
	wait.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful reservations=%d want=1", successes)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	leases := 0
	for _, issue := range snapshot.Issues {
		if issue != nil && issue.Lease != nil {
			leases++
			if !reflect.DeepEqual(issue.Lease.ResolvedResources, []string{"scheduler"}) {
				t.Fatalf("unexpected lease=%+v", issue.Lease)
			}
		}
	}
	if leases != 1 {
		t.Fatalf("active leases=%d want=1", leases)
	}
}

func TestRetainedLeaseReleasesWorkerSlotButKeepsResourceConflict(t *testing.T) {
	store := newStore(t)
	reservedAt := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	_, owner, err := store.ReserveLease(LeaseReservation{
		IssueNumber: 1, RunID: "run_1", Slot: 0,
		ResolvedResources: []string{"docs"}, ReservedAt: reservedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("input_requested", 1, owner.RunID, nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"].Status = "needs_input"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReserveLease(LeaseReservation{
		IssueNumber: 2, RunID: "run_2", Slot: 0,
		ResolvedResources: []string{"scheduler"}, ReservedAt: reservedAt,
	}); err != nil {
		t.Fatalf("released worker slot was not reusable: %v", err)
	}
	if _, _, err := store.ReserveLease(LeaseReservation{
		IssueNumber: 3, RunID: "run_3", Slot: 1,
		ResolvedResources: []string{"docs"}, ReservedAt: reservedAt,
	}); err == nil {
		t.Fatal("retained resource lease stopped conflicting")
	}
}

func TestCrashPointsReplayPreparedLeaseTransaction(t *testing.T) {
	for _, appendEvent := range []bool{false, true} {
		t.Run(fmt.Sprintf("event_appended_%v", appendEvent), func(t *testing.T) {
			store := newStore(t)
			base, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			reservedAt := time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC)
			base.StateRevision++
			base.Issues["9"] = &Issue{
				Number: 9, Status: "claiming", RunID: "run_9", Attempts: 1, LeaseGeneration: 1, UpdatedAt: reservedAt,
				Lease: &ResourceLease{Owner: LeaseOwner{RunID: "run_9", Generation: 1}, Slot: 0, DeclaredResources: []string{}, ResolvedResources: []string{RepositoryResource}, ReservedAt: reservedAt},
			}
			event := Event{Version: CurrentVersion, EventID: "evt_lease", Sequence: base.StateRevision, Timestamp: reservedAt, RepoID: store.RepoID, IssueNumber: 9, RunID: "run_9", Type: "lease_reserved"}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: base, Event: event}, 0o600); err != nil {
				t.Fatal(err)
			}
			if appendEvent {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := store.Load()
			if err != nil || loaded.Issues["9"] == nil || loaded.Issues["9"].Lease == nil {
				t.Fatalf("loaded=%+v err=%v", loaded, err)
			}
			if _, err := os.Stat(store.TransactionPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prepared transaction remains: %v", err)
			}
		})
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
	for _, status := range []string{"awaiting_checks", "awaiting_merge", "resolving_conflict"} {
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

func TestAttentionReportsOneBlockedIssueWhileAnotherWorkerIsActive(t *testing.T) {
	snapshot := Snapshot{
		Supervisor: Supervisor{State: "running"},
		Issues: map[string]*Issue{
			"1": {Number: 1, Status: "running"},
			"2": {Number: 2, Status: "blocked"},
		},
	}
	if reason, ok := snapshot.Attention(false); !ok || reason != "blocked" {
		t.Fatalf("reason=%q ok=%v", reason, ok)
	}
	if reason, ok := snapshot.Attention(true); !ok || reason != "blocked" {
		t.Fatalf("until-idle reason=%q ok=%v", reason, ok)
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
	if _, err := f.WriteString(`{"version":4,"sequence":2`); err != nil {
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
	for _, version := range []int{CurrentVersion - 1, CurrentVersion + 1} {
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

func TestValidIDRejectsRunDirectoryTraversal(t *testing.T) {
	if !ValidID("run_abc-123", "run_") {
		t.Fatal("valid run ID was rejected")
	}
	for _, value := range []string{"run_", "../run_abc", "run_../../state", "resume_abc", "run_with space"} {
		if ValidID(value, "run_") {
			t.Fatalf("unsafe run ID was accepted: %q", value)
		}
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

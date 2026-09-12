package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func snapshotMigrationFixture(t *testing.T) (Migrator, state.Store, []byte) {
	t.Helper()
	root := t.TempDir()
	l := layout.Layout{Root: root, ReposRoot: filepath.Join(root, "repos"), RegistryPath: filepath.Join(root, "registry.json")}
	store := state.Store{Dir: l.RepoDir("repo-anonymous"), RepoID: "repo-anonymous", RepoPath: "/missing/repository"}
	if err := os.MkdirAll(store.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	original := legacyCancelFixture(t)
	if err := os.WriteFile(store.StatePath(), original, 0600); err != nil {
		t.Fatal(err)
	}
	return Migrator{Layout: l, Now: func() time.Time { return time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC) }}, store, original
}

func TestSnapshotMigrationStartupAndPairedRollback(t *testing.T) {
	m, store, original := snapshotMigrationFixture(t)
	report, err := InspectSnapshots(m.Layout)
	if err != nil || !report.NeedsMigration || report.TargetVersion != 6 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	if len(report.SemanticFindings) != 1 || report.SemanticFindings[0].IssueNumber != 17 || report.SemanticFindings[0].Code != "LEGACY_CANCEL_MIGRATABLE" {
		t.Fatalf("preview=%+v", report.SemanticFindings)
	}
	before, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(original, before) {
		t.Fatal("preview changed original")
	}
	result, err := m.ApplySnapshots("")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 6 || snapshot.StateRevision != 1 || snapshot.Issues["17"].Cancellation == nil {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	eventData, _ := os.ReadFile(store.EventsPath())
	if !bytes.Contains(eventData, []byte("snapshot_migration_applied")) || !bytes.Contains(eventData, []byte("2026-09-13")) {
		t.Fatalf("audit=%s", eventData)
	}
	after, _ := os.ReadFile(store.StatePath())
	repeat, err := m.ApplySnapshots("")
	if err != nil || repeat.Changed {
		t.Fatalf("repeat=%+v err=%v", repeat, err)
	}
	again, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(after, again) {
		t.Fatal("repeated migration changed snapshot")
	}
	if _, err := m.Restore(result.Backup); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(restored, original) {
		t.Fatal("rollback did not restore original")
	}
}

func TestFaultSnapshotMigrationRecoveryAtEveryWrite(t *testing.T) {
	for _, boundary := range []string{"migration.json", "events.jsonl", "state.json"} {
		t.Run(boundary, func(t *testing.T) {
			m, store, _ := snapshotMigrationFixture(t)
			fault := errors.New("injected migration interruption")
			m.AfterWrite = func(path string) error {
				if filepath.Base(path) == boundary {
					return fault
				}
				return nil
			}
			if _, err := m.ApplySnapshots(""); !errors.Is(err, fault) {
				t.Fatalf("err=%v", err)
			}
			journalBefore, _, err := m.loadJournal()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(); err == nil || !strings.Contains(err.Error(), "migration is prepared") {
				t.Fatalf("CLI crossed prepared migration: %v", err)
			}
			report, err := InspectSnapshots(m.Layout)
			if err != nil || !report.NeedsMigration {
				t.Fatalf("prepared migration cannot be previewed for CLI restart: report=%+v err=%v", report, err)
			}
			m.AfterWrite = nil
			result, err := m.ApplySnapshots("")
			if err != nil {
				t.Fatal(err)
			}
			if result.Backup != journalBefore.Backup {
				t.Fatal("recovery created another backup")
			}
			snapshot, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.StateRevision != 1 {
				t.Fatalf("revision=%d", snapshot.StateRevision)
			}
			events, _ := os.ReadFile(store.EventsPath())
			if bytes.Count(events, []byte("snapshot_migration_applied")) != 1 {
				t.Fatal("audit duplicated")
			}
			if !snapshot.Issues["17"].Cancellation.CanceledAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
				t.Fatal("historical cancellation time replaced")
			}
		})
	}
}

func TestSnapshotMigrationRefusesUnknownAndUnsafeInputsWithoutMutation(t *testing.T) {
	for _, replace := range [][2]string{{`"version":5`, `"version":7`}, {`"version":5`, `"version":4`}, {`"status":"blocked"`, `"status":"blocked","worker_pid":123,"worker_pgid":123`}, {`"resolved_at":"2026-09-01T00:00:00Z"`, `"resolved_at":"0001-01-01T00:00:00Z"`}} {
		m, store, original := snapshotMigrationFixture(t)
		original = bytes.Replace(original, []byte(replace[0]), []byte(replace[1]), 1)
		if err := os.WriteFile(store.StatePath(), original, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := m.ApplySnapshots(""); err == nil {
			t.Fatal("unsafe migration accepted")
		}
		after, _ := os.ReadFile(store.StatePath())
		if !bytes.Equal(original, after) {
			t.Fatal("refusal modified original")
		}
		if _, err := os.Stat(m.journalPath()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("refusal created a journal")
		}
	}
}

func TestSnapshotMigrationFencesSupervisorAndConcurrentCLI(t *testing.T) {
	m, store, _ := snapshotMigrationFixture(t)
	lock, err := store.AcquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplySnapshots(""); err == nil {
		t.Fatal("migration ignored supervisor lock")
	}
	state.ReleaseSupervisorLock(lock)
	entered, release := make(chan struct{}), make(chan struct{})
	m.AfterWrite = func(path string) error {
		if filepath.Base(path) == "migration.json" {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, err := m.ApplySnapshots(""); done <- err }()
	<-entered
	if _, err := (Migrator{Layout: m.Layout}).ApplySnapshots(""); err == nil {
		t.Fatal("double migration accepted")
	}
	cli := make(chan error, 1)
	go func() { _, err := store.Load(); cli <- err }()
	select {
	case err := <-cli:
		t.Fatalf("CLI bypassed state lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-cli; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotMigrationRollbackRefusesProgress(t *testing.T) {
	m, store, _ := snapshotMigrationFixture(t)
	result, err := m.ApplySnapshots("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("progress", 0, "", nil, func(*state.Snapshot) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.StatePath())
	if _, err := m.Restore(result.Backup); err == nil {
		t.Fatal("rollback discarded progress")
	}
	after, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(before, after) {
		t.Fatal("rollback changed progressed state")
	}
}

func TestSnapshotMigrationValidatesAllRepositoriesBeforeWriting(t *testing.T) {
	m, store, original := snapshotMigrationFixture(t)
	path := m.Layout.RepoDir("repo-invalid")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(original, &object); err != nil {
		t.Fatal(err)
	}
	object["repo_id"] = "repo-invalid"
	object["version"] = 99
	encoded, _ := json.Marshal(object)
	if err := os.WriteFile(filepath.Join(path, "state.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ApplySnapshots(""); err == nil {
		t.Fatal("mixed invalid repository accepted")
	}
	after, _ := os.ReadFile(store.StatePath())
	if !bytes.Equal(original, after) {
		t.Fatal("valid repository changed before whole migration validation")
	}
}

package runtimemetadata

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

func fixture(t *testing.T) (Store, Record, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	record := Record{1, "owner/one", "local-one", "1.8.2", 101, "boot-one", StartTime{now.Add(-time.Hour).Unix(), 123456}, now.Add(-time.Minute)}
	store := Store{Root: t.TempDir(), Inspect: func(pid int) (Process, error) {
		return Process{pid, uint32(os.Geteuid()), record.BootSessionID, record.ProcessStartedAt}, nil
	}}
	if err := os.Chmod(store.Root, 0700); err != nil {
		t.Fatal(err)
	}
	return store, record, now
}

func writeFixture(t *testing.T, store Store, record Record) string {
	t.Helper()
	key, err := repositoryKey(record.Repository)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Root, "runtime-metadata", key, record.RepoID+".json")
	if err := fsutil.WriteJSON(path, record, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepositoryMappingUpdateAndAmbiguity(t *testing.T) {
	store, first, now := fixture(t)
	writeFixture(t, store, first)
	second := first
	second.Repository = "owner/two"
	second.RepoID = "local-two"
	second.RuntimeVersion = "1.8.1"
	writeFixture(t, store, second)
	for _, record := range []Record{first, second} {
		got := store.Observe(record.Repository, now, 3*time.Minute)
		if got == nil || got.Version != record.RuntimeVersion || !got.ObservedAt.Equal(now) || !got.ExpiresAt.Equal(now.Add(3*time.Minute)) {
			t.Fatalf("observation=%+v", got)
		}
	}
	first.RuntimeVersion = "1.9.0"
	first.PID = 102
	writeFixture(t, store, first)
	if got := store.Observe("OWNER/ONE", now.Add(time.Hour), time.Minute); got == nil || got.Version != "1.9.0" {
		t.Fatalf("updated observation=%+v", got)
	}
	if store.Observe("owner/missing", now, time.Minute) != nil {
		t.Fatal("missing repository received a runtime")
	}
	duplicate := first
	duplicate.RepoID = "second-checkout"
	writeFixture(t, store, duplicate)
	if store.Observe(first.Repository, now, time.Minute) != nil {
		t.Fatal("ambiguous runtime accepted")
	}
	if store.Observe(second.Repository, now, time.Minute) == nil {
		t.Fatal("other repository affected")
	}
}

func TestMissingInvalidAndUnverifiableMetadata(t *testing.T) {
	for _, name := range []string{"schema", "repository", "repo_id", "version", "pid", "boot", "start", "written", "future", "permission", "symlink", "directory_symlink", "malformed", "missing_microseconds", "oversize", "probe_error", "pid_reuse", "reboot", "owner", "exited", "during_read", "record_replaced", "new_runtime"} {
		t.Run(name, func(t *testing.T) {
			store, record, now := fixture(t)
			path := writeFixture(t, store, record)
			probe := store.Inspect
			switch name {
			case "schema":
				record.SchemaVersion = 2
			case "repository":
				record.Repository = "owner/other"
			case "repo_id":
				record.RepoID = "other"
			case "version":
				record.RuntimeVersion = ""
			case "pid":
				record.PID = 0
			case "boot":
				record.BootSessionID = ""
			case "start":
				record.ProcessStartedAt = StartTime{}
			case "written":
				record.WrittenAt = time.Time{}
			case "future":
				record.WrittenAt = now.Add(time.Second)
			case "permission":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory_symlink":
				dir := filepath.Dir(path)
				target := dir + "-moved"
				if err := os.Rename(dir, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing_microseconds":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields["process_started_at"].(map[string]any), "microseconds")
				data, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, make([]byte, 4097), 0600); err != nil {
					t.Fatal(err)
				}
			case "probe_error":
				store.Inspect = func(int) (Process, error) { return Process{}, os.ErrPermission }
			case "pid_reuse":
				store.Inspect = func(pid int) (Process, error) { p, _ := probe(pid); p.StartedAt.Microseconds++; return p, nil }
			case "reboot":
				store.Inspect = func(pid int) (Process, error) { p, _ := probe(pid); p.BootSessionID = "next-boot"; return p, nil }
			case "owner":
				store.Inspect = func(pid int) (Process, error) { p, _ := probe(pid); p.UID++; return p, nil }
			case "exited":
				store.Inspect = func(int) (Process, error) { return Process{}, ErrNotRunning }
			case "during_read":
				calls := 0
				store.Inspect = func(pid int) (Process, error) {
					calls++
					if calls > 1 {
						return Process{}, ErrNotRunning
					}
					return probe(pid)
				}
			case "record_replaced":
				store.Inspect = func(pid int) (Process, error) {
					changed := record
					changed.RuntimeVersion = "1.9.0"
					writeFixture(t, store, changed)
					return probe(pid)
				}
			case "new_runtime":
				store.Inspect = func(pid int) (Process, error) {
					changed := record
					changed.RepoID = "second"
					writeFixture(t, store, changed)
					return probe(pid)
				}
			}
			switch name {
			case "schema", "repository", "repo_id", "version", "pid", "boot", "start", "written", "future":
				if err := fsutil.WriteJSON(path, record, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := store.Observe("owner/one", now, time.Minute); got != nil {
				t.Fatalf("unsafe observation=%+v", got)
			}
		})
	}
}

func TestStoppedRecordDoesNotMaskLiveRuntime(t *testing.T) {
	store, record, now := fixture(t)
	writeFixture(t, store, record)
	old := record
	old.RepoID = "old"
	old.PID = 100
	writeFixture(t, store, old)
	probe := store.Inspect
	store.Inspect = func(pid int) (Process, error) {
		if pid == old.PID {
			return Process{}, ErrNotRunning
		}
		return probe(pid)
	}
	if got := store.Observe(record.Repository, now, time.Minute); got == nil || got.Version != record.RuntimeVersion {
		t.Fatalf("live observation=%+v", got)
	}
}

func TestPublishAndCleanupOwnership(t *testing.T) {
	store, record, now := fixture(t)
	cleanup, err := store.Publish(record.Repository, record.RepoID, record.RuntimeVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	if store.Observe(record.Repository, now, time.Minute) == nil {
		t.Fatal("published runtime missing")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if store.Observe(record.Repository, now, time.Minute) != nil {
		t.Fatal("stopped runtime retained")
	}
	cleanup, err = store.Publish(record.Repository, record.RepoID, record.RuntimeVersion, now)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimeVersion = "1.9.0"
	writeFixture(t, store, record)
	if err := cleanup(); err == nil {
		t.Fatal("cleanup removed a different identity")
	}
	if store.Observe(record.Repository, now, time.Minute) == nil {
		t.Fatal("replacement removed")
	}
	store.Inspect = func(int) (Process, error) { return Process{}, errors.New("unavailable") }
	if _, err := store.Publish(record.Repository, record.RepoID, record.RuntimeVersion, now); err == nil {
		t.Fatal("published unverifiable identity")
	}
}

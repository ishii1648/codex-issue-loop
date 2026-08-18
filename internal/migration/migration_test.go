package migration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestApplyMigratesV3FixturesAndRestoreRecoversOriginalBytes(t *testing.T) {
	l, repo, original := writeV3Fixture(t, false)
	legacyCredential := filepath.Join(l.RepoDir("repo-1"), "notification-token")
	if err := os.WriteFile(legacyCredential, []byte("retained-legacy-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || !report.NeedsMigration || len(report.Unsupported) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	fixed := time.Date(2026, 8, 16, 1, 2, 3, 4, time.UTC)
	result, err := (Migrator{Layout: l, Now: func() time.Time { return fixed }}).Apply()
	if err != nil || !result.Changed || result.Backup == "" || result.From != 3 || result.To != CurrentVersion {
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
	if err != nil || snapshot.Version != state.CurrentVersion || snapshot.StateRevision != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
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
	if err != nil || !restored.Restored || restored.To != 3 {
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

func TestInterruptedApplyReusesJournalAndConvergesIdempotently(t *testing.T) {
	l, repo, _ := writeV3Fixture(t, true)
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
	l, _, _ := writeV3Fixture(t, false)
	if err := os.WriteFile(l.RegistryPath, []byte(`{"version":5,"repos":{}}`), 0o600); err != nil {
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

func TestV4ActiveLeaseAndParkedContinuationBlockRollback(t *testing.T) {
	l, repo, _ := writeV3Fixture(t, false)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	v3 := fmt.Sprintf(`{"version":3,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"running","updated_at":"2026-08-16T00:00:00Z"},"issues":{"63":{"number":63,"title":"active","status":"needs_input","run_id":"run_63","attempts":1,"continuations":0,"updated_at":"2026-08-16T00:02:00Z","lease_generation":1,"lease":{"owner":{"run_id":"run_63","generation":1},"slot":0,"declared_resources":[],"resolved_resources":["repo:*"],"reserved_at":"2026-08-16T00:02:00Z"}}},"pending_requests":{},"notifications":{"needs_input:req-1":{"id":"needs_input:req-1","status":"pending"}}}`+"\n", repo)
	if err := os.WriteFile(statePath, []byte(v3), 0o600); err != nil {
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
		len(issue.Lease.ResolvedResources) != 1 || issue.Lease.ResolvedResources[0] != state.RepositoryResource || issue.Status != "needs_input" {
		t.Fatalf("migrated issue=%+v", issue)
	}
	if _, err := (Migrator{Layout: l}).Restore(result.Backup); err == nil || !strings.Contains(err.Error(), "active resource lease") {
		t.Fatalf("active lease rollback was accepted: %v", err)
	}
	store := state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}
	if _, err := store.Update("issue_blocked", 63, issue.RunID, nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["63"]
		item.Status = "blocked"
		item.BlockedCause = &state.BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "test", BlockedAt: time.Now().UTC()}
		return state.ParkIssueLease(item, item.Lease.Owner, "park_63", time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := (Migrator{Layout: l}).Restore(result.Backup); err == nil || !strings.Contains(err.Error(), "parked resource continuation") {
		t.Fatalf("parked continuation rollback was accepted: %v", err)
	}
	if _, err := store.Update("test_park_completed", 63, issue.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["63"].ResourcePark = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := (Migrator{Layout: l}).Restore(result.Backup)
	if err != nil || !restored.Restored || restored.To != 3 {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, []byte(v3)) {
		t.Fatalf("v3 active state was not restored: %s err=%v", after, err)
	}
}

func writeV3Fixture(t *testing.T, withTransaction bool) (layout.Layout, string, map[string][]byte) {
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
	registryJSON := fmt.Sprintf(`{"version":3,"repos":{"repo-1":{"repo_id":"repo-1","repo_path":%q,"github_repo":"owner/repo","registered_at":"2026-08-16T00:00:00Z","commands":{}}}}`, repo)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	eventsPath := filepath.Join(l.RepoDir("repo-1"), "events.jsonl")
	files := map[string][]byte{
		configPath:     []byte("# retained comment\nversion: 3\ngithub:\n  repo: owner/repo\nnotifications:\n  enabled: true\n  provider: legacy\n  endpoint: https://push.invalid\n  topic: opaque-topic\n"),
		l.RegistryPath: []byte(registryJSON + "\n"),
		statePath:      []byte(fmt.Sprintf(`{"version":3,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:00:00Z"},"issues":{},"pending_requests":{},"notifications":{"needs_input:req-1":{"id":"needs_input:req-1","status":"sent"}}}`+"\n", repo)),
		eventsPath:     []byte(`{"version":3,"event_id":"evt-1","sequence":1,"timestamp":"2026-08-16T00:00:00Z","repo_id":"repo-1","type":"notification_sent","payload":{"notification_id":"needs_input:req-1"}}` + "\n"),
	}
	if withTransaction {
		txnPath := filepath.Join(l.RepoDir("repo-1"), "state.txn.json")
		files[txnPath] = []byte(fmt.Sprintf(`{"version":3,"snapshot":{"version":3,"repo_id":"repo-1","repo_path":%q,"state_revision":2,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:01:00Z"},"issues":{},"pending_requests":{},"notifications":{"needs_input:req-2":{"id":"needs_input:req-2","status":"pending"}}},"event":{"version":3,"event_id":"evt-2","sequence":2,"timestamp":"2026-08-16T00:01:00Z","repo_id":"repo-1","type":"notification_retry_scheduled","payload":{"notification_id":"needs_input:req-2"}}}`+"\n", repo))
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

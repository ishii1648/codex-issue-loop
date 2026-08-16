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

func TestApplyMigratesV2FixturesAndRestoreRecoversOriginalBytes(t *testing.T) {
	l, repo, original := writeV2Fixture(t, false)
	report, err := Inspect(l)
	if err != nil || !report.NeedsMigration || len(report.Unsupported) != 0 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	fixed := time.Date(2026, 8, 16, 1, 2, 3, 4, time.UTC)
	result, err := (Migrator{Layout: l, Now: func() time.Time { return fixed }}).Apply()
	if err != nil || !result.Changed || result.Backup == "" || result.From != 2 || result.To != CurrentVersion {
		t.Fatalf("result=%+v err=%v", result, err)
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

	restored, err := (Migrator{Layout: l, Now: func() time.Time { return fixed.Add(time.Minute) }}).Restore(result.Backup)
	if err != nil || !restored.Restored || restored.To != 2 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	for path, want := range original {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("restore %s mismatch: err=%v\ngot=%s\nwant=%s", path, readErr, got, want)
		}
	}
}

func TestInterruptedApplyReusesJournalAndConvergesIdempotently(t *testing.T) {
	l, repo, _ := writeV2Fixture(t, true)
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
	l, _, _ := writeV2Fixture(t, false)
	if err := os.WriteFile(l.RegistryPath, []byte(`{"version":4,"repos":{}}`), 0o600); err != nil {
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

func TestV2ActiveIssueMigratesToExclusiveFallbackAndBlocksRollback(t *testing.T) {
	l, repo, _ := writeV2Fixture(t, false)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	v2 := fmt.Sprintf(`{"version":2,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"running","updated_at":"2026-08-16T00:00:00Z"},"issues":{"63":{"number":63,"title":"active","status":"needs_input","run_id":"run_63","attempts":1,"continuations":0,"updated_at":"2026-08-16T00:02:00Z"}},"pending_requests":{}}`+"\n", repo)
	if err := os.WriteFile(statePath, []byte(v2), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(l)
	if err != nil || len(report.FallbackLeases) != 1 || report.FallbackLeases[0].IssueNumber != 63 || report.FallbackLeases[0].Resource != "repo:*" {
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
	if _, err := (state.Store{Dir: l.RepoDir("repo-1"), RepoID: "repo-1", RepoPath: repo}).ReleaseLease(63, issue.Lease.Owner, "test terminal transition"); err != nil {
		t.Fatal(err)
	}
	restored, err := (Migrator{Layout: l}).Restore(result.Backup)
	if err != nil || !restored.Restored || restored.To != 2 {
		t.Fatalf("restore=%+v err=%v", restored, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(after, []byte(v2)) {
		t.Fatalf("v2 active state was not restored: %s err=%v", after, err)
	}
}

func writeV2Fixture(t *testing.T, withTransaction bool) (layout.Layout, string, map[string][]byte) {
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
	registryJSON := fmt.Sprintf(`{"version":2,"repos":{"repo-1":{"repo_id":"repo-1","repo_path":%q,"github_repo":"owner/repo","registered_at":"2026-08-16T00:00:00Z","commands":{}}}}`, repo)
	statePath := filepath.Join(l.RepoDir("repo-1"), "state.json")
	eventsPath := filepath.Join(l.RepoDir("repo-1"), "events.jsonl")
	files := map[string][]byte{
		configPath:     []byte("# retained comment\nversion: 2\ngithub:\n  repo: owner/repo\n"),
		l.RegistryPath: []byte(registryJSON + "\n"),
		statePath:      []byte(fmt.Sprintf(`{"version":2,"repo_id":"repo-1","repo_path":%q,"state_revision":1,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:00:00Z"},"issues":{},"pending_requests":{}}`+"\n", repo)),
		eventsPath:     []byte(`{"version":2,"event_id":"evt-1","sequence":1,"timestamp":"2026-08-16T00:00:00Z","repo_id":"repo-1","type":"initialized"}` + "\n"),
	}
	if withTransaction {
		txnPath := filepath.Join(l.RepoDir("repo-1"), "state.txn.json")
		files[txnPath] = []byte(fmt.Sprintf(`{"version":2,"snapshot":{"version":2,"repo_id":"repo-1","repo_path":%q,"state_revision":2,"supervisor":{"state":"polling","updated_at":"2026-08-16T00:01:00Z"},"issues":{},"pending_requests":{}},"event":{"version":2,"event_id":"evt-2","sequence":2,"timestamp":"2026-08-16T00:01:00Z","repo_id":"repo-1","type":"poll"}}`+"\n", repo))
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

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	schema "github.com/ishii1648/codex-issue-loop/internal/migration"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func testEnvironment(t *testing.T) (string, layout.Layout) {
	t.Helper()
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENT_LOOP_HOME", filepath.Join(root, "home"))
	t.Setenv("AGENT_LOOP_SKILLS_DIR", filepath.Join(root, "skills"))
	t.Setenv("AGENT_LOOP_LAUNCH_AGENTS_DIR", filepath.Join(root, "launchagents"))
	l, err := layout.New()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	config := `version: 2
github:
  repo: owner/repo
watch:
  reconcile_interval: 20ms
`
	if err := os.WriteFile(filepath.Join(repo, ".agent-loop.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, l
}

func TestInstallAndRegister(t *testing.T) {
	repo, l := testEnvironment(t)
	var out, stderr bytes.Buffer
	a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
	if code := a.Run(context.Background(), []string{"install", "--json"}); code != 0 {
		t.Fatalf("install code=%d stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(l.BinDir, "agent-loop")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := a.Run(context.Background(), []string{"register", "--repo", repo, "--json"}); code != 0 {
		t.Fatalf("register code=%d stderr=%s", code, stderr.String())
	}
	r, err := (registry.Store{Path: l.RegistryPath}).Load()
	if err != nil || len(r.Repos) != 1 {
		t.Fatalf("registry=%+v err=%v", r, err)
	}
	for _, entry := range r.Repos {
		if _, err := os.Stat(l.PlistPath(entry.RepoID)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(l.RepoDir(entry.RepoID), "state.json")); err != nil {
			t.Fatal(err)
		}
	}
}

func mustConfig(t *testing.T, repo string) config.Config {
	t.Helper()
	cfg, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestAnswerIsRecordedAndIdempotent(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(mustConfig(t, repo))
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("input_requested", 4, "run", nil, func(s *state.Snapshot) error {
		s.Supervisor.State = "running"
		s.Issues["4"] = &state.Issue{Number: 4, Status: "needs_input"}
		s.PendingRequests["req_1"] = &state.Request{ID: "req_1", IssueNumber: 4, Question: "Choose", Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		var out, stderr bytes.Buffer
		a := App{In: bytes.NewBuffer(nil), Out: &out, Err: &stderr}
		code := a.Run(context.Background(), []string{"answer", "--repo", repo, "--request-id", "req_1", "--message", "A", "--json"})
		if code != 0 {
			t.Fatalf("answer %d code=%d stderr=%s", i, code, stderr.String())
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := store.Load()
	if snapshot.Issues["4"].Status != "resume_pending" || len(snapshot.Issues["4"].Answers) != 1 {
		t.Fatalf("issue=%+v", snapshot.Issues["4"])
	}
}

func TestEvaluateSleepSettings(t *testing.T) {
	output := `Battery Power:
 sleep 1
AC Power:
 sleep 0
 displaysleep 10
`
	ok, detail := evaluateSleepSettings(output, nil)
	if !ok || detail == "" {
		t.Fatalf("ok=%v detail=%q", ok, detail)
	}
	ok, _ = evaluateSleepSettings("AC Power:\n sleep 1\n", nil)
	if ok {
		t.Fatal("enabled sleep was accepted")
	}
}

func TestBootstrapLabelsCommandRequiresApplyToMutate(t *testing.T) {
	repo, _ := testEnvironment(t)
	binDir := filepath.Join(filepath.Dir(repo), "bin")
	logPath := filepath.Join(filepath.Dir(repo), "label-calls.log")
	fakeGH := filepath.Join(binDir, "gh")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n  \"label list\") printf '[]\\n' ;;\n  \"label create\") printf '%s\\n' \"$*\" >> \"$AGENT_LOOP_LABEL_LOG\" ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_LABEL_LOG", logPath)
	for _, test := range []struct {
		name  string
		apply bool
	}{
		{name: "preview"},
		{name: "apply", apply: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := []string{"bootstrap-labels", "--repo", repo, "--json"}
			if test.apply {
				args = append(args, "--apply")
			}
			if code := (App{Out: &out, Err: &stderr}).Run(context.Background(), args); code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			var result gh.LabelBootstrapResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Applied != test.apply {
				t.Fatalf("result=%+v", result)
			}
			if !test.apply {
				if _, err := os.Stat(logPath); !os.IsNotExist(err) {
					t.Fatalf("preview mutated labels: %v", err)
				}
			}
		})
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "label create") != len(gh.RequiredLabelSpecs(mustConfig(t, repo))) {
		t.Fatalf("calls=%s", calls)
	}
}

func TestRecordSupervisorControlReplacesStaleStoppedState(t *testing.T) {
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-id", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update("failed", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "stopped"
		snapshot.Supervisor.PID = 42
		snapshot.Supervisor.Message = "old failure"
		snapshot.Supervisor.FailureKind = "transient"
		snapshot.Supervisor.ConsecutiveFailures = 3
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recordSupervisorControl(store, "starting", "restart requested"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Supervisor
	if got.State != "starting" || got.PID != 0 || got.Message != "restart requested" || got.FailureKind != "" || got.ConsecutiveFailures != 0 {
		t.Fatalf("unexpected supervisor state: %+v", got)
	}
}

func TestInstallArtifactsAreIdempotentAndVersioned(t *testing.T) {
	_, l := testEnvironment(t)
	source := filepath.Join(t.TempDir(), "agent-loop")
	if err := os.WriteFile(source, []byte("release-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, changed, err := installArtifacts(l, source, "v1.2.3", "abc123")
	if err != nil || !changed {
		t.Fatalf("first=%+v changed=%v err=%v", first, changed, err)
	}
	second, changed, err := installArtifacts(l, source, "v1.2.3", "abc123")
	if err != nil || changed || second != first {
		t.Fatalf("second=%+v changed=%v err=%v", second, changed, err)
	}
	version, err := os.ReadFile(filepath.Join(l.SkillsDir, "agent-loop", "VERSION"))
	if err != nil || string(version) != "v1.2.3\n" {
		t.Fatalf("version=%q err=%v", version, err)
	}
	match, err := installationMatches(l, source, "v1.2.3", "abc123")
	if err != nil || !match {
		t.Fatalf("match=%v err=%v", match, err)
	}
}

func TestUpdateBackupCanRestoreBinarySkillAndManifest(t *testing.T) {
	_, l := testEnvironment(t)
	oldSource := filepath.Join(t.TempDir(), "old-agent-loop")
	newSource := filepath.Join(t.TempDir(), "new-agent-loop")
	if err := os.WriteFile(oldSource, []byte("old-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newSource, []byte("new-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldManifest, _, err := installArtifacts(l, oldSource, "v1.0.0", "old")
	if err != nil {
		t.Fatal(err)
	}
	backup, err := backupInstallation(l)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, newSource, "v1.1.0", "new"); err != nil {
		t.Fatal(err)
	}
	if err := restoreInstallation(l, backup); err != nil {
		t.Fatal(err)
	}
	restored, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil || restored != oldManifest {
		t.Fatalf("restored=%+v want=%+v err=%v", restored, oldManifest, err)
	}
	resolved, err := validateBackupPath(l, backup)
	expectedBackup, resolveErr := filepath.EvalSymlinks(backup)
	if err != nil || resolveErr != nil || resolved != expectedBackup {
		t.Fatalf("resolved=%q expected=%q err=%v resolveErr=%v", resolved, expectedBackup, err, resolveErr)
	}
	if _, err := validateBackupPath(l, filepath.Dir(l.Root)); err == nil {
		t.Fatal("outside backup path accepted")
	}
}

func TestSchemaChangingUpdateRequiresStoppedMigrationAndPairedRollback(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	oldSource := filepath.Join(t.TempDir(), "old-agent-loop")
	if err := os.WriteFile(oldSource, []byte("old-v1-release"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldManifest, _, err := installArtifacts(l, oldSource, "v0.1.0", "old")
	if err != nil {
		t.Fatal(err)
	}
	oldManifest.SchemaVersion = 1
	writeJSONFixture(t, filepath.Join(l.Root, "install.json"), oldManifest)
	writeLegacySchemas(t, repo, l)

	oldVersion, oldCommit := Version, Commit
	Version, Commit = "v0.2.0-test", "candidate"
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })
	var out, stderr bytes.Buffer
	a := App{Out: &out, Err: &stderr}
	if err := a.update(context.Background(), l, []string{"--json"}); err != nil {
		t.Fatalf("update: %v stderr=%s", err, stderr.String())
	}
	var updateResult struct {
		Backup                  string `json:"backup"`
		SchemaMigrationRequired bool   `json:"schema_migration_required"`
	}
	if err := json.Unmarshal(out.Bytes(), &updateResult); err != nil || updateResult.Backup == "" || !updateResult.SchemaMigrationRequired {
		t.Fatalf("update result=%+v err=%v output=%s", updateResult, err, out.String())
	}

	migrationResult, err := (schema.Migrator{Layout: l}).Apply()
	if err != nil || migrationResult.Backup == "" {
		t.Fatalf("migration=%+v err=%v", migrationResult, err)
	}
	if err := a.rollback(context.Background(), l, []string{"--backup", updateResult.Backup, "--json"}); err == nil || !strings.Contains(err.Error(), "restore the matching migration backup first") {
		t.Fatalf("installation rollback crossed schema boundary: %v", err)
	}
	if _, err := (schema.Migrator{Layout: l}).Restore(migrationResult.Backup); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := a.rollback(context.Background(), l, []string{"--backup", updateResult.Backup, "--json"}); err != nil {
		t.Fatalf("paired installation rollback: %v", err)
	}
	restored, err := readInstallManifest(filepath.Join(l.Root, "install.json"))
	if err != nil || restored.Version != "v0.1.0" || restored.SchemaVersion != 1 {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
}

func writeLegacySchemas(t *testing.T, repo string, l layout.Layout) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte("version: 1\ngithub:\n  repo: owner/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := registry.Entry{
		RepoID: "repo-v1", RepoPath: repo, GitHubRepo: "owner/repo",
		Commands: map[string]string{"launchctl": "/usr/bin/false"},
	}
	writeJSONFixture(t, l.RegistryPath, registry.Registry{Version: 1, Repos: map[string]registry.Entry{entry.RepoID: entry}})
	if err := os.MkdirAll(l.RepoDir(entry.RepoID), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, filepath.Join(l.RepoDir(entry.RepoID), "state.json"), state.Snapshot{
		Version: 1, RepoID: entry.RepoID, RepoPath: repo,
		Supervisor: state.Supervisor{State: "stopped"}, Issues: map[string]*state.Issue{}, PendingRequests: map[string]*state.Request{},
	})
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

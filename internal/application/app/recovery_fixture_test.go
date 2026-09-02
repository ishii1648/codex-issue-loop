package app

import (
	"bytes"
	"context"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/recoveryfixture"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestExportRecoveryFixtureIsReadOnlyAndImmediatelyReplayable(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	var err error
	repo, err = filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	runGitApp(t, repo, "config", "user.name", "Fixture Test")
	runGitApp(t, repo, "config", "user.email", "fixture@example.invalid")
	runGitApp(t, repo, "config", "commit.gpgsign", "false")
	runGitApp(t, repo, "add", ".agent-loop.yaml")
	runGitApp(t, repo, "commit", "-m", "fixture base")
	runGitApp(t, repo, "branch", "-M", "main")
	remotePath := filepath.Join(t.TempDir(), "fixture-origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", remotePath).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	runGitApp(t, repo, "remote", "add", "origin", remotePath)
	runGitApp(t, repo, "push", "-u", "origin", "main")
	configFile, err := os.OpenFile(filepath.Join(repo, config.FileName), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(configFile, "git:\n  worktree_root: %q\n", filepath.Dir(repo)); err != nil {
		t.Fatal(err)
	}
	if err := configFile.Close(); err != nil {
		t.Fatal(err)
	}

	binDir := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	ghPath := filepath.Join(binDir, "gh")
	ghLog := filepath.Join(t.TempDir(), "gh.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue view") printf '%%s\n' '{"number":166,"title":"private title","body":"private body","url":"https://example.test/issues/166","state":"OPEN","labels":[{"name":"blocked"}],"assignees":[],"milestone":null,"comments":[{"body":"private note"}]}' ;;
  "pr list") printf '%%s\n' '[]' ;;
  *) exit 91 ;;
esac
`, ghLog)
	if err := os.WriteFile(ghPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry, err := (registry.Store{Path: l.RegistryPath}).Add(cfg)
	if err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	baseSHA := runGitOutputApp(t, repo, "rev-parse", "HEAD")
	_, owner, err := store.ReserveLease(state.LeaseReservation{
		IssueNumber: 166, Title: "private title", RunID: "run_fixture_166", Slot: 0,
		DeclaredResources: []string{state.RepositoryResource}, ResolvedResources: []string{state.RepositoryResource},
		BaseSHA: baseSHA, ReservedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update("issue_blocked", 166, owner.RunID, map[string]any{"remote_head": nil}, func(snapshot *state.Snapshot) error {
		issue := snapshot.Issues["166"]
		issue.Status = issuedomain.StatusBlocked
		issue.Worktree = repo
		issue.Branch = "main"
		issue.FailureKind = "issue"
		issue.LastError = "worker blocked: private environment detail"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, _ := os.ReadFile(store.StatePath())
	eventsBefore, _ := os.ReadFile(store.EventsPath())
	statusBefore, err := exec.Command("git", "-C", repo, "status", "--porcelain=v1").Output()
	if err != nil {
		t.Fatal(err)
	}

	fixturePath := filepath.Join(t.TempDir(), "export.json")
	var out, stderr bytes.Buffer
	app := App{Out: &out, Err: &stderr}
	if code := app.Run(context.Background(), []string{"export-recovery-fixture", "--repo", repo, "--issue", "166", "--output", fixturePath, "--json"}); code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	stateAfter, _ := os.ReadFile(store.StatePath())
	eventsAfter, _ := os.ReadFile(store.EventsPath())
	statusAfter, err := exec.Command("git", "-C", repo, "status", "--porcelain=v1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(eventsBefore, eventsAfter) || !bytes.Equal(statusBefore, statusAfter) {
		t.Fatal("export changed durable state, events, or worktree")
	}
	calls, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) == "" || strings.Contains(string(calls), "issue edit") || strings.Contains(string(calls), "issue comment") {
		t.Fatalf("export made an unexpected GitHub call: %s", calls)
	}
	bundle, err := recoveryfixture.Load(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := bundle.Replay()
	if err != nil || replay.Snapshot.Issues["166"] == nil || len(replay.Events) != 2 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	data, _ := os.ReadFile(fixturePath)
	for _, private := range []string{"private title", "private body", "private note", filepath.Dir(repo)} {
		if bytes.Contains(data, []byte(private)) {
			t.Fatalf("export retained sensitive value %q", private)
		}
	}

	out.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"verify-recovery-fixture", "--fixture", fixturePath, "--json"}); code != 0 || !strings.Contains(out.String(), `"valid": true`) {
		t.Fatalf("verify code=%d stdout=%s stderr=%s", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := app.Run(context.Background(), []string{"export-recovery-fixture", "--repo", repo, "--issue", "166", "--output", fixturePath}); code == 0 {
		t.Fatal("export overwrote an existing fixture")
	}
}

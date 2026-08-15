package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
)

func TestPublishCommitsPushesAndCreatesDraftPullRequestIdempotently(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.name", "E2E Publisher")
	runGit(t, repo, "config", "user.email", "publisher@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	runGit(t, repo, "config", "commit.gpgsign", "true")
	runGit(t, repo, "config", "gpg.format", "ssh")
	runGit(t, repo, "config", "user.signingkey", filepath.Join(root, "missing-signing-key"))
	branch := "codex/issue-1-publish"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.MkdirAll(filepath.Join(repo, "results"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "results", "one.txt"), []byte("done\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "pr-created")
	fakeGH := filepath.Join(root, "gh")
	script := `#!/bin/sh
case "$1 $2" in
  "pr list")
    if test -f "$PUBLISH_TEST_MARKER"; then
      printf '%s\n' '[{"url":"https://github.example/owner/repo/pull/1"}]'
    else
      printf '%s\n' '[]'
    fi
    ;;
  "pr create")
    : > "$PUBLISH_TEST_MARKER"
    printf '%s\n' 'https://github.example/owner/repo/pull/1'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLISH_TEST_MARKER", marker)
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	manager := Manager{GitPath: "git", GHPath: fakeGH}
	issue := gh.Issue{Number: 1, Title: "Create marker"}

	first, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "implemented")
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != branch || first.Commit == "" || first.PullRequestURL != "https://github.example/owner/repo/pull/1" {
		t.Fatalf("unexpected first publish result: %+v", first)
	}
	if status := runGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("published repository is dirty: %q", status)
	}
	if remoteCommit := runGit(t, repo, "rev-parse", "origin/"+branch); remoteCommit != first.Commit {
		t.Fatalf("remote commit=%s, want %s", remoteCommit, first.Commit)
	}

	second, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "implemented")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent publish result=%+v, want %+v", second, first)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

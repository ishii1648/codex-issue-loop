package publish

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
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
	baseSHA := runGit(t, repo, "rev-parse", "HEAD")
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
	printf '%s\n' 'Warning: 1 uncommitted change'
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

	first, audit, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "implemented", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if first.Branch != branch || first.Commit == "" || first.PullRequestURL != "https://github.example/owner/repo/pull/1" {
		t.Fatalf("unexpected first publish result: %+v", first)
	}
	if len(audit.ChangedPaths) != 1 || audit.ChangedPaths[0] != "results/one.txt" || len(audit.ActualResources) != 1 || audit.ActualResources[0] != admission.RepositoryResource {
		t.Fatalf("unexpected publication audit: %+v", audit)
	}
	if status := runGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("published repository is dirty: %q", status)
	}
	if remoteCommit := runGit(t, repo, "rev-parse", "origin/"+branch); remoteCommit != first.Commit {
		t.Fatalf("remote commit=%s, want %s", remoteCommit, first.Commit)
	}

	second, _, err := manager.Publish(context.Background(), cfg, issue, repo, branch, "implemented", baseSHA, []string{admission.RepositoryResource})
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent publish result=%+v, want %+v", second, first)
	}
}

func TestPublishRefusesTrackedAndUntrackedResourcesOutsideClaimBeforeMutation(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", repo)
	runGit(t, repo, "config", "user.name", "Audit Publisher")
	runGit(t, repo, "config", "user.email", "publisher@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "branch", "-M", "main")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	baseSHA := runGit(t, repo, "rev-parse", "HEAD")
	branch := "codex/issue-2-audit"
	runGit(t, repo, "switch", "-c", branch)
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "tracked.md"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "docs/tracked.md")
	runGit(t, repo, "commit", "-m", "worker committed docs")
	workerHead := runGit(t, repo, "rev-parse", "HEAD")
	if err := os.MkdirAll(filepath.Join(repo, "internal", "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "internal", "config", "untracked.go"), []byte("package config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Resources.Definitions = []config.ResourceDefinition{
		{Name: "config", Paths: []string{"internal/config/**"}},
		{Name: "docs", Paths: []string{"docs/**"}},
	}
	manager := Manager{GitPath: "git", GHPath: filepath.Join(root, "gh-must-not-run")}
	_, audit, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 2, Title: "Audit"}, repo, branch, "implemented", baseSHA, []string{"config"})
	var mismatch publication.ClaimMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("claim mismatch not returned: audit=%+v err=%v", audit, err)
	}
	if strings.Join(audit.ChangedPaths, ",") != "docs/tracked.md,internal/config/untracked.go" || strings.Join(audit.ActualResources, ",") != "config,docs" || audit.Reason != publication.ReasonResourceClaimMismatch {
		t.Fatalf("unexpected audit: %+v", audit)
	}
	if got := runGit(t, repo, "rev-parse", "HEAD"); got != workerHead {
		t.Fatalf("publisher changed HEAD before refusal: got=%s want=%s", got, workerHead)
	}
	if got := runGit(t, repo, "status", "--porcelain", "--untracked-files=all"); !strings.Contains(got, "?? internal/config/untracked.go") {
		t.Fatalf("publisher staged or removed untracked work: %q", got)
	}
	if out, commandErr := exec.Command("git", "-C", repo, "rev-parse", "--verify", "origin/"+branch).CombinedOutput(); commandErr == nil {
		t.Fatalf("publisher pushed refused branch: %s", out)
	}
}

func TestExtractPullRequestURLRejectsAmbiguousOutput(t *testing.T) {
	_, err := extractPullRequestURL("https://github.example/owner/repo/pull/1\nhttps://github.example/owner/repo/pull/2\n")
	if err == nil || !strings.Contains(err.Error(), "multiple Pull Request URLs") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if len(args) > 0 && args[0] == "commit" {
		args = append([]string{"-c", "commit.gpgsign=false"}, args...)
	}
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

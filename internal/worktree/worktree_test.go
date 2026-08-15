package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func gitRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestEnsureCreatesIsolatedWorktree(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	gitRun(t, "init", "--bare", "-q", remote)
	gitRun(t, "clone", "-q", remote, repo)
	gitRun(t, "-C", repo, "config", "user.name", "Test")
	gitRun(t, "-C", repo, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", repo, "add", "README.md")
	gitRun(t, "-C", repo, "commit", "-q", "-m", "initial")
	gitRun(t, "-C", repo, "branch", "-M", "main")
	gitRun(t, "-C", repo, "push", "-q", "-u", "origin", "main")
	cfg := config.Defaults()
	cfg.RepoPath = repo
	cfg.GitHub.Repo = "owner/repo"
	cfg.Git.WorktreeRoot = filepath.Join(root, "worktrees")
	result, err := (Manager{StateRoot: root}).Ensure(context.Background(), cfg, "repo-id", 12, "Add useful feature")
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "codex/issue-12-add-useful-feature" {
		t.Fatalf("branch=%s", result.Branch)
	}
	if _, err := os.Stat(filepath.Join(result.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	second, err := (Manager{StateRoot: root}).Ensure(context.Background(), cfg, "repo-id", 12, "Add useful feature")
	if err != nil || second != result {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	inspection, err := (Manager{StateRoot: root}).Inspect(context.Background(), cfg, result.Path, result.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || inspection.RemoteBranchExists || inspection.Branch != result.Branch || inspection.Dirty {
		t.Fatalf("inspection=%+v", inspection)
	}
	if err := os.WriteFile(filepath.Join(result.Path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err = (Manager{StateRoot: root}).Inspect(context.Background(), cfg, result.Path, result.Branch)
	if err != nil || !inspection.Dirty {
		t.Fatalf("dirty inspection=%+v err=%v", inspection, err)
	}
}

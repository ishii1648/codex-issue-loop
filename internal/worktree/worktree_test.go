package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/gitops"
)

func gitRun(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMultipleRealWorktreesUseOneImmutableDispatchBase(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	gitRun(t, "init", "--bare", "-q", remote)
	gitRun(t, "clone", "-q", remote, repo)
	gitRun(t, "-C", repo, "config", "user.name", "Test")
	gitRun(t, "-C", repo, "config", "user.email", "test@example.test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", repo, "add", "base.txt")
	gitRun(t, "-C", repo, "commit", "-q", "-m", "base one")
	gitRun(t, "-C", repo, "branch", "-M", "main")
	gitRun(t, "-C", repo, "push", "-q", "-u", "origin", "main")

	cfg := config.Defaults()
	cfg.RepoPath = repo
	cfg.Git.WorktreeRoot = filepath.Join(root, "worktrees")
	manager := Manager{StateRoot: root, Gate: gitops.NewGate()}
	baseSHA, err := manager.ResolveBase(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, "-C", repo, "add", "base.txt")
	gitRun(t, "-C", repo, "commit", "-q", "-m", "base two")
	gitRun(t, "-C", repo, "push", "-q", "origin", "main")
	advancedSHA := gitOutput(t, "-C", repo, "rev-parse", "HEAD")
	if advancedSHA == baseSHA {
		t.Fatal("test base branch did not advance")
	}

	type outcome struct {
		result Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	var start sync.WaitGroup
	start.Add(1)
	for number := 21; number <= 22; number++ {
		number := number
		go func() {
			start.Wait()
			result, ensureErr := manager.Ensure(context.Background(), cfg, "repo-id", number, "Shared dispatch", baseSHA)
			outcomes <- outcome{result: result, err: ensureErr}
		}()
	}
	start.Done()
	for index := 0; index < 2; index++ {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		head := gitOutput(t, "-C", outcome.result.Path, "rev-parse", "HEAD")
		if head != baseSHA {
			t.Fatalf("worktree %s HEAD=%s, want immutable base %s (latest=%s)", outcome.result.Path, head, baseSHA, advancedSHA)
		}
	}
}

func TestFaultWorktreeCreateReuseAndPartialCreation(t *testing.T) {
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
	manager := Manager{StateRoot: root}
	baseSHA, err := manager.ResolveBase(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	result, err := manager.Ensure(context.Background(), cfg, "repo-id", 12, "Add useful feature", baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if result.Branch != "codex/issue-12-add-useful-feature" {
		t.Fatalf("branch=%s", result.Branch)
	}
	if _, err := os.Stat(filepath.Join(result.Path, ".git")); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Ensure(context.Background(), cfg, "repo-id", 12, "Add useful feature", baseSHA)
	if err != nil || second != result {
		t.Fatalf("second=%+v err=%v", second, err)
	}
	inspection, err := manager.Inspect(context.Background(), cfg, result.Path, result.Branch)
	if err != nil {
		t.Fatal(err)
	}
	if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || inspection.RemoteBranchExists || inspection.Branch != result.Branch || inspection.Dirty {
		t.Fatalf("inspection=%+v", inspection)
	}
	if err := os.WriteFile(filepath.Join(result.Path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err = manager.Inspect(context.Background(), cfg, result.Path, result.Branch)
	if err != nil || !inspection.Dirty {
		t.Fatalf("dirty inspection=%+v err=%v", inspection, err)
	}

	partialPath := filepath.Join(cfg.Git.WorktreeRoot, "repo-id", "issue-13")
	if err := os.MkdirAll(partialPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), cfg, "repo-id", 13, "Interrupted creation", baseSHA); err == nil {
		t.Fatal("partially created directory was treated as a reusable worktree")
	}
}

func TestWorktreeRejectsTraversalAndSymbolicLink(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Git.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.RepoPath = root
	manager := Manager{StateRoot: root}
	missingSHA := strings.Repeat("0", 40)
	if _, err := manager.Ensure(context.Background(), cfg, "../outside", 1, "test", missingSHA); err == nil {
		t.Fatal("repository ID traversal was accepted")
	}
	repoRoot := filepath.Join(cfg.Git.WorktreeRoot, "repo-id")
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repoRoot, "issue-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), cfg, "repo-id", 1, "test", missingSHA); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatal("symbolic-link worktree was accepted")
	}
}

package conflict

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestConflictRecoveryPreservesBothChangeIntentsAndPublishesWithoutForce(t *testing.T) {
	tests := []struct {
		name       string
		initial    string
		prChange   string
		baseChange string
		resolved   string
	}{
		{
			name: "table rows", initial: "| id |\n| --- |\n| seed |\n",
			prChange:   "| id |\n| --- |\n| seed |\n| pr-row |\n",
			baseChange: "| id |\n| --- |\n| seed |\n| base-row |\n",
			resolved:   "| id |\n| --- |\n| seed |\n| base-row |\n| pr-row |\n",
		},
		{
			name: "semantic function", initial: "export function label() {\n  return 'legacy'\n}\n",
			prChange:   "export function label() {\n  return outerFrame('legacy')\n}\n",
			baseChange: "export function label() {\n  return buildHierarchicalLabel()\n}\n",
			resolved:   "export function label() {\n  return outerFrame(buildHierarchicalLabel())\n}\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, branch, file, cfg := conflictRepository(t, test.initial, test.prChange, test.baseChange)
			manager := Manager{}
			prepared, err := manager.Prepare(context.Background(), cfg, repo, branch, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(prepared.ConflictFiles) != 1 || prepared.ConflictFiles[0] != file || prepared.TargetBaseSHA == "" || prepared.OriginalHeadSHA == "" {
				t.Fatalf("preparation=%+v", prepared)
			}
			if !strings.Contains(prepared.ConflictContent, "<<<<<<<") || prepared.OriginalDiff == "" {
				t.Fatalf("recovery context missing conflict/diff: %+v", prepared)
			}
			if err := os.WriteFile(filepath.Join(repo, file), []byte(test.resolved), 0o600); err != nil {
				t.Fatal(err)
			}
			recovery := state.ConflictRecovery{
				PullRequestURL: "https://example.test/pull/1", PreviousBaseSHA: prepared.PreviousBaseSHA,
				TargetBaseSHA: prepared.TargetBaseSHA, OriginalHeadSHA: prepared.OriginalHeadSHA,
				ConflictFiles: prepared.ConflictFiles, AllowedPaths: prepared.AllowedPaths,
			}
			published, err := manager.Publish(context.Background(), cfg, gh.Issue{Number: 1}, repo, branch, recovery, []worker.Test{{Command: "test", Result: "passed"}})
			if err != nil {
				t.Fatal(err)
			}
			if published.PullRequestURL != recovery.PullRequestURL || published.Commit == prepared.OriginalHeadSHA {
				t.Fatalf("published=%+v", published)
			}
			parents := gitOutput(t, repo, "rev-list", "--parents", "-n", "1", "HEAD")
			if !strings.Contains(parents, prepared.TargetBaseSHA) || !strings.Contains(parents, prepared.OriginalHeadSHA) {
				t.Fatalf("merge parents=%s", parents)
			}
			if remote := strings.Fields(gitOutput(t, repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch))[0]; remote != published.Commit {
				t.Fatalf("remote=%s commit=%s", remote, published.Commit)
			}
			if status := gitOutput(t, repo, "status", "--porcelain"); status != "" {
				t.Fatalf("worktree remains dirty: %q", status)
			}
			restarted, err := manager.Prepare(context.Background(), cfg, repo, branch, &recovery)
			if err != nil || !restarted.Published || restarted.Commit != published.Commit {
				t.Fatalf("restart preparation=%+v err=%v", restarted, err)
			}
		})
	}
}

func TestConflictRecoveryDescribesDeletedConflictResolutionOnRetry(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "pr")
	actor := filepath.Join(root, "base")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "-b", "main", repo)
	configureGit(t, repo)
	file := "shared.txt"
	if err := os.WriteFile(filepath.Join(repo, file), []byte("initial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", file)
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	branch := "codex/issue-1-delete"
	runGit(t, repo, "checkout", "-b", branch)
	runGit(t, repo, "rm", file)
	runGit(t, repo, "commit", "-m", "delete file")
	runGit(t, repo, "push", "-u", "origin", branch)
	runGit(t, root, "clone", "-b", "main", remote, actor)
	configureGit(t, actor)
	if err := os.WriteFile(filepath.Join(actor, file), []byte("base change\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, actor, "add", file)
	runGit(t, actor, "commit", "-m", "modify file")
	runGit(t, actor, "push", "origin", "main")
	cfg := config.Defaults()
	cfg.Git.BaseBranch = "main"
	cfg.RepoPath = repo

	manager := Manager{}
	prepared, err := manager.Prepare(context.Background(), cfg, repo, branch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, file)); err != nil {
		t.Fatal(err)
	}
	recovery := state.ConflictRecovery{
		PreviousBaseSHA: prepared.PreviousBaseSHA, TargetBaseSHA: prepared.TargetBaseSHA,
		OriginalHeadSHA: prepared.OriginalHeadSHA, ConflictFiles: prepared.ConflictFiles,
		AllowedPaths: prepared.AllowedPaths,
	}
	retried, err := manager.Prepare(context.Background(), cfg, repo, branch, &recovery)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.ConflictFiles) != 1 || retried.ConflictFiles[0] != file || !strings.Contains(retried.ConflictContent, "[absent from worktree]") {
		t.Fatalf("preparation=%+v", retried)
	}
}

func TestConflictPublicationRejectsPathOutsideRecordedScope(t *testing.T) {
	repo, branch, file, cfg := conflictRepository(t, "one\n", "one\npr\n", "one\nbase\n")
	manager := Manager{}
	prepared, err := manager.Prepare(context.Background(), cfg, repo, branch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, file), []byte("one\nbase\npr\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("not allowed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery := state.ConflictRecovery{PullRequestURL: "https://example.test/pull/1", TargetBaseSHA: prepared.TargetBaseSHA, OriginalHeadSHA: prepared.OriginalHeadSHA, ConflictFiles: prepared.ConflictFiles, AllowedPaths: prepared.AllowedPaths}
	_, err = manager.Publish(context.Background(), cfg, gh.Issue{Number: 1}, repo, branch, recovery, []worker.Test{{Command: "test", Result: "passed"}})
	var fatal NonRecoverableError
	if err == nil || !strings.Contains(err.Error(), "outside the recorded scope") || !strings.Contains(err.Error(), "outside.txt") || !errors.As(err, &fatal) {
		t.Fatalf("err=%v", err)
	}
}

func TestConflictPublicationUsesExactVerificationOutcome(t *testing.T) {
	recovery := state.ConflictRecovery{TargetBaseSHA: "base", OriginalHeadSHA: "head", ConflictFiles: []string{"shared.txt"}}
	manager := Manager{}
	for _, result := range []string{"failed", "pass", "ok", "success", "exit 0", "成功"} {
		_, err := manager.Publish(context.Background(), config.Config{}, gh.Issue{}, t.TempDir(), "branch", recovery, []worker.Test{{Command: "go test ./...", Result: result}})
		if err == nil || !strings.Contains(err.Error(), "not explicitly green") {
			t.Fatalf("result %q: err=%v", result, err)
		}
	}
}

func conflictRepository(t *testing.T, initial, prChange, baseChange string) (string, string, string, config.Config) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "pr")
	actor := filepath.Join(root, "base")
	runGit(t, root, "init", "--bare", remote)
	runGit(t, root, "init", "-b", "main", repo)
	configureGit(t, repo)
	file := "shared.txt"
	if err := os.WriteFile(filepath.Join(repo, file), []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", file)
	runGit(t, repo, "commit", "-m", "initial")
	runGit(t, repo, "remote", "add", "origin", remote)
	runGit(t, repo, "push", "-u", "origin", "main")
	branch := "codex/issue-1-conflict"
	runGit(t, repo, "checkout", "-b", branch)
	if err := os.WriteFile(filepath.Join(repo, file), []byte(prChange), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", file)
	runGit(t, repo, "commit", "-m", "PR change")
	runGit(t, repo, "push", "-u", "origin", branch)
	runGit(t, root, "clone", "-b", "main", remote, actor)
	configureGit(t, actor)
	if err := os.WriteFile(filepath.Join(actor, file), []byte(baseChange), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, actor, "add", file)
	runGit(t, actor, "commit", "-m", "base change")
	runGit(t, actor, "push", "origin", "main")
	cfg := config.Defaults()
	cfg.Git.BaseBranch = "main"
	cfg.RepoPath = repo
	return repo, branch, file, cfg
}

func configureGit(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "commit.gpgsign", "false")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

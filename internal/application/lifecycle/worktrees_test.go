package lifecycle

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type fakeRemote struct {
	open map[int]string
}

func (f fakeRemote) Inspect(_ context.Context, _ config.Config, number int, _ string) (gh.RemoteState, error) {
	result := gh.RemoteState{}
	if url := f.open[number]; url != "" {
		result.PullRequests = []gh.PullRequest{{Number: number, URL: url, State: "OPEN"}}
	}
	return result, nil
}

func TestCleanupRetainsUnsafeWorktreesAndAuditsSafeRemoval(t *testing.T) {
	ctx := context.Background()
	cfg, stateRoot := lifecycleRepository(t)
	worktrees := worktree.Manager{StateRoot: stateRoot, GitPath: "git"}
	now := time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC)
	issues := map[string]*state.Issue{}
	for number, status := range map[int]string{1: "completed", 2: "completed", 3: "failed", 4: "completed", 5: "needs_input", 6: "resume_pending"} {
		result, err := worktrees.Ensure(ctx, cfg, "repo-id", number, fmt.Sprintf("Issue %d", number))
		if err != nil {
			t.Fatalf("ensure #%d: %v", number, err)
		}
		issues[fmt.Sprint(number)] = &state.Issue{
			Number: number, Status: issuedomain.Status(status), Branch: result.Branch, Worktree: result.Path,
			UpdatedAt: now.Add(-48 * time.Hour),
		}
		issues[fmt.Sprint(number)].Workspace = &state.WorkerWorkspace{
			Path: result.Path, Branch: result.Branch, RepoID: "repo-id", Repository: cfg.GitHub.Repo,
			GitCommonDir: filepath.Join(cfg.RepoPath, ".git"), MainCheckout: cfg.RepoPath, CapturedAt: now,
		}
	}
	if err := os.WriteFile(filepath.Join(issues["2"].Worktree, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(issues["3"].Worktree, "committed.txt"), []byte("local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, issues["3"].Worktree, "add", "committed.txt")
	gitRun(t, issues["3"].Worktree, "commit", "-m", "local only")

	store := state.Store{Dir: filepath.Join(stateRoot, "repos", "repo-id"), RepoID: "repo-id", RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Update("fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues = issues
		snapshot.PendingRequests["req_5"] = &state.Request{ID: "req_5", IssueNumber: 5, Status: issuedomain.RequestStatusPending}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{
		Worktrees: worktrees,
		Remote:    fakeRemote{open: map[int]string{4: "https://example.test/pull/4"}},
		Now:       func() time.Time { return now },
	}
	preview, err := manager.Plan(ctx, cfg, "repo-id", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertPlan(t, preview, 1, true, "retention_period_expired")
	assertPlan(t, preview, 2, false, "dirty_worktree")
	assertPlan(t, preview, 3, false, "unpushed_commits")
	assertPlan(t, preview, 4, false, "open_pull_request")
	assertPlan(t, preview, 5, false, "status_retained_indefinitely")
	assertPlan(t, preview, 6, false, "status_retained_indefinitely")
	if preview.Applied {
		t.Fatal("preview unexpectedly applied")
	}
	for _, issue := range issues {
		if _, err := os.Stat(issue.Worktree); err != nil {
			t.Fatalf("preview removed #%d: %v", issue.Number, err)
		}
	}

	applied, err := manager.Cleanup(ctx, cfg, "repo-id", store, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertPlan(t, applied, 1, true, "retention_period_expired")
	if _, err := os.Stat(issues["1"].Worktree); !os.IsNotExist(err) {
		t.Fatalf("eligible worktree remains: %v", err)
	}
	for _, number := range []string{"2", "3", "4", "5"} {
		if _, err := os.Stat(issues[number].Worktree); err != nil {
			t.Fatalf("unsafe worktree #%s removed: %v", number, err)
		}
	}
	updated, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if updated.Issues["1"].Worktree != "" || updated.Issues["1"].Branch == "" {
		t.Fatalf("cleanup did not preserve branch recovery: %+v", updated.Issues["1"])
	}
	events, err := os.ReadFile(store.EventsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), `"type":"worktree_cleanup_started"`) || !strings.Contains(string(events), `"type":"worktree_cleaned"`) {
		t.Fatalf("cleanup audit events missing: %s", events)
	}
	listed := gitRun(t, cfg.RepoPath, "worktree", "list", "--porcelain")
	if strings.Contains(listed, issues["1"].Worktree) {
		t.Fatalf("stale worktree metadata remains: %s", listed)
	}
}

func TestPurgeRequiresExactConfirmationAndCanRemoveDirtyWorktree(t *testing.T) {
	ctx := context.Background()
	cfg, stateRoot := lifecycleRepository(t)
	worktrees := worktree.Manager{StateRoot: stateRoot, GitPath: "git"}
	created, err := worktrees.Ensure(ctx, cfg, "repo-id", 9, "dirty")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "dirty.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: filepath.Join(stateRoot, "repos", "repo-id"), RepoID: "repo-id", RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Update("fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["9"] = &state.Issue{Number: 9, Status: issuedomain.StatusBlocked, Branch: created.Branch, Worktree: created.Path, UpdatedAt: time.Now().UTC()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{Worktrees: worktrees, Remote: fakeRemote{open: map[int]string{}}}
	if _, err := manager.Purge(ctx, cfg, "repo-id", store, snapshot, 9, "wrong"); err == nil || !strings.Contains(err.Error(), ConfirmationToken("repo-id", 9)) {
		t.Fatalf("wrong confirmation accepted: %v", err)
	}
	if _, err := os.Stat(created.Path); err != nil {
		t.Fatalf("wrong confirmation mutated worktree: %v", err)
	}
	result, err := manager.Purge(ctx, cfg, "repo-id", store, snapshot, 9, ConfirmationToken("repo-id", 9))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Action != "purged" || !result.Entries[0].Safety.Dirty {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(created.Path); !os.IsNotExist(err) {
		t.Fatalf("dirty worktree remains: %v", err)
	}
	events, _ := os.ReadFile(store.EventsPath())
	if !strings.Contains(string(events), `"type":"worktree_purged"`) {
		t.Fatalf("purge audit event missing: %s", events)
	}
}

func TestPurgeCanRemoveExplicitlyConfirmedOrphanWorktree(t *testing.T) {
	ctx := context.Background()
	cfg, stateRoot := lifecycleRepository(t)
	worktrees := worktree.Manager{StateRoot: stateRoot, GitPath: "git"}
	created, err := worktrees.Ensure(ctx, cfg, "repo-id", 10, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(created.Path, "dirty.txt"), []byte("discard\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.Store{Dir: filepath.Join(stateRoot, "repos", "repo-id"), RepoID: "repo-id", RepoPath: cfg.RepoPath}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager := Manager{Worktrees: worktrees, Remote: fakeRemote{open: map[int]string{}}}
	result, err := manager.Purge(ctx, cfg, "repo-id", store, snapshot, 10, ConfirmationToken("repo-id", 10))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 1 || result.Entries[0].Action != "purged" || result.Entries[0].Status != "orphaned" || !result.Entries[0].Safety.Dirty {
		t.Fatalf("result=%+v", result)
	}
	if _, err := os.Stat(created.Path); !os.IsNotExist(err) {
		t.Fatalf("orphan worktree remains: %v", err)
	}
	events, _ := os.ReadFile(store.EventsPath())
	if !strings.Contains(string(events), `"type":"worktree_purged"`) || !strings.Contains(string(events), `"issue_number":10`) {
		t.Fatalf("orphan purge audit event missing: %s", events)
	}
}

func lifecycleRepository(t *testing.T) (config.Config, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitCommand(t, "", "init", "--bare", "-q", remote)
	repo := filepath.Join(root, "repo")
	gitCommand(t, "", "init", "-q", "-b", "main", repo)
	gitRun(t, repo, "config", "user.email", "loop@example.test")
	gitRun(t, repo, "config", "user.name", "Loop Test")
	gitRun(t, repo, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "README.md")
	gitRun(t, repo, "commit", "-m", "base")
	gitRun(t, repo, "remote", "add", "origin", remote)
	gitRun(t, repo, "push", "-u", "origin", "main")
	cfg := config.Defaults()
	cfg.RepoPath = repo
	cfg.GitHub.Repo = "owner/repo"
	cfg.Git.WorktreeRoot = filepath.Join(root, "worktrees")
	cfg.Worktrees.CompletedMaxAge = config.Duration{Duration: 24 * time.Hour}
	cfg.Worktrees.FailedMaxAge = config.Duration{Duration: 24 * time.Hour}
	return cfg, filepath.Join(root, "state")
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	if dir != "" {
		command.Dir = dir
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func gitRun(t *testing.T, repo string, args ...string) string {
	t.Helper()
	return gitCommand(t, "", append([]string{"-C", repo}, args...)...)
}

func assertPlan(t *testing.T, result Result, issueNumber int, eligible bool, reason string) {
	t.Helper()
	for _, entry := range result.Entries {
		if entry.IssueNumber == issueNumber {
			if entry.Eligible != eligible || !contains(entry.Reasons, reason) {
				t.Fatalf("Issue #%d entry=%+v", issueNumber, entry)
			}
			return
		}
	}
	t.Fatalf("Issue #%d missing from %+v", issueNumber, result)
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

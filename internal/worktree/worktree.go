package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type Manager struct {
	StateRoot string
	GitPath   string
}

type Result struct {
	Path   string
	Branch string
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func (m Manager) Ensure(ctx context.Context, cfg config.Config, repoID string, issueNumber int, title string) (Result, error) {
	root := cfg.Git.WorktreeRoot
	if root == "" {
		root = filepath.Join(m.StateRoot, "worktrees")
	}
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = "issue"
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	branch := fmt.Sprintf("%s%d-%s", cfg.Git.BranchPrefix, issueNumber, slug)
	path := filepath.Join(root, repoID, fmt.Sprintf("issue-%d", issueNumber))
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return Result{Path: path, Branch: branch}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Result{}, err
	}
	git := m.GitPath
	if git == "" {
		git = "git"
	}
	if out, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "fetch", "origin", cfg.Git.BaseBranch).CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("fetch base branch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	branchExists := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
	args := []string{"-C", cfg.RepoPath, "worktree", "add"}
	if branchExists {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, "origin/"+cfg.Git.BaseBranch)
	}
	if out, err := exec.CommandContext(ctx, git, args...).CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return Result{Path: path, Branch: branch}, nil
}

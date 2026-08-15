package worktree

import (
	"context"
	"errors"
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

type Inspection struct {
	Exists             bool
	Valid              bool
	Branch             string
	Head               string
	Dirty              bool
	LocalBranchExists  bool
	RemoteBranchExists bool
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
		inspection, inspectErr := m.Inspect(ctx, cfg, path, branch)
		if inspectErr != nil {
			return Result{}, fmt.Errorf("inspect existing worktree: %w", inspectErr)
		}
		if !inspection.Valid || inspection.Branch != branch || !inspection.LocalBranchExists {
			return Result{}, fmt.Errorf("existing worktree path is incomplete or belongs to another branch: %s", path)
		}
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

func (m Manager) Inspect(ctx context.Context, cfg config.Config, path, branch string) (Inspection, error) {
	inspection := Inspection{}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return inspection, nil
	}
	if err != nil {
		return inspection, err
	}
	inspection.Exists = info.IsDir()
	if !inspection.Exists {
		return inspection, nil
	}
	git := m.GitPath
	if git == "" {
		git = "git"
	}
	if out, err := exec.CommandContext(ctx, git, "-C", path, "rev-parse", "--is-inside-work-tree").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "true" {
		return inspection, nil
	}
	inspection.Valid = true
	if out, err := exec.CommandContext(ctx, git, "-C", path, "symbolic-ref", "--quiet", "--short", "HEAD").CombinedOutput(); err == nil {
		inspection.Branch = strings.TrimSpace(string(out))
	}
	if out, err := exec.CommandContext(ctx, git, "-C", path, "rev-parse", "HEAD").CombinedOutput(); err == nil {
		inspection.Head = strings.TrimSpace(string(out))
	} else {
		return inspection, fmt.Errorf("inspect worktree HEAD: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.CommandContext(ctx, git, "-C", path, "status", "--porcelain").CombinedOutput(); err == nil {
		inspection.Dirty = strings.TrimSpace(string(out)) != ""
	} else {
		return inspection, fmt.Errorf("inspect worktree status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if branch != "" {
		inspection.LocalBranchExists = exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
		out, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "ls-remote", "--heads", "origin", "refs/heads/"+branch).CombinedOutput()
		if err != nil {
			return inspection, fmt.Errorf("inspect remote branch: %w: %s", err, strings.TrimSpace(string(out)))
		}
		inspection.RemoteBranchExists = strings.TrimSpace(string(out)) != ""
	}
	return inspection, nil
}

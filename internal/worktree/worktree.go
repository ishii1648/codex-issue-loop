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
var validRepoID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,100}$`)

func (m Manager) Ensure(ctx context.Context, cfg config.Config, repoID string, issueNumber int, title string) (Result, error) {
	if !validRepoID.MatchString(repoID) {
		return Result{}, fmt.Errorf("invalid repository ID %q", repoID)
	}
	if issueNumber <= 0 {
		return Result{}, fmt.Errorf("issue number must be positive")
	}
	root := cfg.Git.WorktreeRoot
	if root == "" {
		root = filepath.Join(m.StateRoot, "worktrees")
	}
	secureRoot, err := canonicalPrivateRoot(root)
	if err != nil {
		return Result{}, err
	}
	repoRoot := filepath.Join(secureRoot, repoID)
	if !within(secureRoot, repoRoot) {
		return Result{}, fmt.Errorf("worktree repository path escapes configured root")
	}
	if err := os.MkdirAll(repoRoot, 0o700); err != nil {
		return Result{}, fmt.Errorf("create worktree repository directory: %w", err)
	}
	repoInfo, err := os.Lstat(repoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("inspect worktree repository directory: %w", err)
	}
	if repoInfo.Mode()&os.ModeSymlink != 0 || !repoInfo.IsDir() {
		return Result{}, fmt.Errorf("worktree repository path must be a real directory: %s", repoRoot)
	}
	if err := os.Chmod(repoRoot, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure worktree repository directory: %w", err)
	}
	canonicalRepoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return Result{}, fmt.Errorf("resolve worktree repository directory: %w", err)
	}
	if !within(secureRoot, canonicalRepoRoot) {
		return Result{}, fmt.Errorf("worktree repository directory escapes configured root")
	}
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if slug == "" {
		slug = "issue"
	}
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	branch := fmt.Sprintf("%s%d-%s", cfg.Git.BranchPrefix, issueNumber, slug)
	path := filepath.Join(canonicalRepoRoot, fmt.Sprintf("issue-%d", issueNumber))
	if !within(secureRoot, path) {
		return Result{}, fmt.Errorf("worktree path escapes configured root")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return Result{}, fmt.Errorf("worktree path must not be a symbolic link: %s", path)
	} else if err == nil && info.IsDir() {
		inspection, inspectErr := m.Inspect(ctx, cfg, path, branch)
		if inspectErr != nil {
			return Result{}, fmt.Errorf("inspect existing worktree: %w", inspectErr)
		}
		if !inspection.Valid || inspection.Branch != branch || !inspection.LocalBranchExists {
			return Result{}, fmt.Errorf("existing worktree path is incomplete or belongs to another branch: %s", path)
		}
		return Result{Path: path, Branch: branch}, nil
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
	root := cfg.Git.WorktreeRoot
	if root == "" {
		root = filepath.Join(m.StateRoot, "worktrees")
	}
	secureRoot, err := canonicalPrivateRoot(root)
	if err != nil {
		return inspection, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil || !within(secureRoot, absPath) {
		return inspection, fmt.Errorf("worktree path is outside configured root: %s", path)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return inspection, nil
	}
	if err != nil {
		return inspection, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return inspection, fmt.Errorf("worktree path must not be a symbolic link: %s", path)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return inspection, fmt.Errorf("resolve worktree path: %w", err)
	}
	if !within(secureRoot, resolvedPath) {
		return inspection, fmt.Errorf("worktree path resolves outside configured root: %s", path)
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

func canonicalPrivateRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", fmt.Errorf("create worktree root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root symlinks: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func within(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

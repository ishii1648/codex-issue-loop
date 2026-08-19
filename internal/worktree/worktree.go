package worktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/gitops"
)

type Manager struct {
	StateRoot string
	GitPath   string
	Gate      *gitops.Gate
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
	RemoteHead         string
	Dirty              bool
	UnpushedCommits    bool
	LocalBranchExists  bool
	RemoteBranchExists bool
	RemoteConsistent   bool
}

// LaunchValidation is the local, non-network identity of a linked worktree at
// a worker process boundary. CommonDir ties the checkout to the configured
// repository while TopLevel and Branch fence it to the saved Issue worktree.
type LaunchValidation struct {
	Valid        bool            `json:"valid"`
	ExpectedCWD  string          `json:"expected_cwd"`
	CanonicalCWD string          `json:"canonical_cwd,omitempty"`
	TopLevel     string          `json:"top_level,omitempty"`
	Branch       string          `json:"branch,omitempty"`
	CommonDir    string          `json:"git_common_dir,omitempty"`
	MainCheckout string          `json:"main_checkout,omitempty"`
	Checks       map[string]bool `json:"checks"`
}

// ValidateLaunch performs only local checks so it can be repeated immediately
// before every process spawn without depending on origin availability.
func (m Manager) ValidateLaunch(ctx context.Context, cfg config.Config, path, branch string) (LaunchValidation, error) {
	validation := LaunchValidation{ExpectedCWD: path, Checks: map[string]bool{}}
	root := cfg.Git.WorktreeRoot
	if root == "" {
		root = filepath.Join(m.StateRoot, "worktrees")
	}
	secureRoot, err := canonicalExistingRoot(root)
	if err != nil {
		return validation, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil || !within(secureRoot, absPath) {
		return validation, fmt.Errorf("worker worktree path is outside configured root: %s", path)
	}
	validation.Checks["managed_root"] = true
	if err := rejectSymlinkComponents(secureRoot, absPath); err != nil {
		return validation, err
	}
	validation.Checks["no_symlink_components"] = true
	canonicalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return validation, fmt.Errorf("resolve worker worktree path: %w", err)
	}
	validation.CanonicalCWD = canonicalPath
	if canonicalPath != absPath {
		return validation, fmt.Errorf("worker worktree path is not canonical: %s", path)
	}
	validation.Checks["canonical_path"] = true

	mainCheckout, err := config.CanonicalRepoPath(cfg.RepoPath)
	if err != nil {
		return validation, fmt.Errorf("resolve configured main checkout: %w", err)
	}
	validation.MainCheckout = mainCheckout
	if canonicalPath == mainCheckout {
		return validation, fmt.Errorf("refuse to launch worker in the main checkout: %s", canonicalPath)
	}
	validation.Checks["not_main_checkout"] = true

	git := m.GitPath
	if git == "" {
		git = "git"
	}
	readGitPath := func(repo string, args ...string) (string, error) {
		out, commandErr := exec.CommandContext(ctx, git, append([]string{"-C", repo}, args...)...).CombinedOutput()
		if commandErr != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), commandErr, strings.TrimSpace(string(out)))
		}
		value := strings.TrimSpace(string(out))
		if value == "" {
			return "", fmt.Errorf("git %s returned an empty path", strings.Join(args, " "))
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(repo, value)
		}
		return filepath.EvalSymlinks(filepath.Clean(value))
	}
	topLevel, err := readGitPath(canonicalPath, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return validation, fmt.Errorf("inspect worker worktree top-level: %w", err)
	}
	validation.TopLevel = topLevel
	if topLevel != canonicalPath {
		return validation, fmt.Errorf("worker cwd %s is not the Git worktree top-level %s", canonicalPath, topLevel)
	}
	validation.Checks["git_top_level"] = true
	commonDir, err := readGitPath(canonicalPath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return validation, fmt.Errorf("inspect worker repository identity: %w", err)
	}
	mainCommonDir, err := readGitPath(mainCheckout, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return validation, fmt.Errorf("inspect configured repository identity: %w", err)
	}
	validation.CommonDir = commonDir
	if commonDir != mainCommonDir {
		return validation, fmt.Errorf("worker worktree belongs to a different repository")
	}
	validation.Checks["repository_identity"] = true
	branchOut, err := exec.CommandContext(ctx, git, "-C", canonicalPath, "symbolic-ref", "--quiet", "--short", "HEAD").CombinedOutput()
	if err != nil {
		return validation, fmt.Errorf("inspect worker worktree branch: %w: %s", err, strings.TrimSpace(string(branchOut)))
	}
	validation.Branch = strings.TrimSpace(string(branchOut))
	if branch == "" || validation.Branch != branch {
		return validation, fmt.Errorf("worker worktree branch is %q, expected %q", validation.Branch, branch)
	}
	validation.Checks["saved_branch"] = true
	validation.Valid = true
	return validation, nil
}

func rejectSymlinkComponents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return fmt.Errorf("worker worktree path is outside configured root: %s", path)
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return fmt.Errorf("inspect worker worktree path component: %w", statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("worker worktree path must not contain a symbolic link: %s", current)
		}
	}
	return nil
}

// ContentDigest fingerprints tracked, staged, and untracked worktree content
// without mutating the index. It is used to fence operator validation from the
// later publication-only supervisor attempt.
func ContentDigest(ctx context.Context, git, path string) (string, error) {
	if git == "" {
		git = "git"
	}
	hash := sha256.New()
	run := func(args ...string) ([]byte, error) {
		out, err := exec.CommandContext(ctx, git, append([]string{"-C", path}, args...)...).Output()
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	for _, args := range [][]string{
		{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--"},
		{"diff", "--binary", "HEAD", "--"},
		{"diff", "--cached", "--binary", "HEAD", "--"},
	} {
		out, err := run(args...)
		if err != nil {
			return "", fmt.Errorf("fingerprint worktree with git %s: %w", strings.Join(args, " "), err)
		}
		hash.Write(out)
		hash.Write([]byte{0})
	}
	untracked, err := run("ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return "", fmt.Errorf("list untracked files for worktree fingerprint: %w", err)
	}
	paths := strings.Split(string(untracked), "\x00")
	sort.Strings(paths)
	for _, relative := range paths {
		if relative == "" {
			continue
		}
		blob, hashErr := run("hash-object", "--no-filters", "--", relative)
		if hashErr != nil {
			return "", fmt.Errorf("hash untracked worktree path %q: %w", relative, hashErr)
		}
		hash.Write([]byte(relative))
		hash.Write([]byte{0})
		hash.Write(blob)
		hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func (m Manager) ContentDigest(ctx context.Context, path string) (string, error) {
	return ContentDigest(ctx, m.GitPath, path)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)
var validRepoID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,100}$`)
var validObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ResolveBase fetches the configured base branch and resolves the fetched ref
// to an immutable commit. Callers persist this SHA before dispatching workers.
func (m Manager) ResolveBase(ctx context.Context, cfg config.Config) (string, error) {
	var baseSHA string
	err := m.Gate.Run(ctx, gitops.Base, func() error {
		git := m.gitPath()
		if out, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "fetch", "origin", cfg.Git.BaseBranch).CombinedOutput(); err != nil {
			return fmt.Errorf("fetch base branch: %w: %s", err, strings.TrimSpace(string(out)))
		}
		out, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "rev-parse", "--verify", "refs/remotes/origin/"+cfg.Git.BaseBranch+"^{commit}").CombinedOutput()
		if err != nil {
			return fmt.Errorf("resolve fetched base branch: %w: %s", err, strings.TrimSpace(string(out)))
		}
		baseSHA = strings.TrimSpace(string(out))
		if baseSHA == "" {
			return fmt.Errorf("resolve fetched base branch: empty commit SHA")
		}
		return nil
	})
	return baseSHA, err
}

func (m Manager) Ensure(ctx context.Context, cfg config.Config, repoID string, issueNumber int, title, baseSHA string) (Result, error) {
	var result Result
	err := m.Gate.Run(ctx, gitops.Worktree, func() error {
		var err error
		result, err = m.ensure(ctx, cfg, repoID, issueNumber, title, baseSHA)
		return err
	})
	return result, err
}

func (m Manager) ensure(ctx context.Context, cfg config.Config, repoID string, issueNumber int, title, baseSHA string) (Result, error) {
	if !validRepoID.MatchString(repoID) {
		return Result{}, fmt.Errorf("invalid repository ID %q", repoID)
	}
	if issueNumber <= 0 {
		return Result{}, fmt.Errorf("issue number must be positive")
	}
	baseSHA = strings.TrimSpace(baseSHA)
	if !validObjectID.MatchString(baseSHA) {
		return Result{}, fmt.Errorf("immutable base SHA is invalid: %q", baseSHA)
	}
	git := m.gitPath()
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
	}
	resolvedBase, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "rev-parse", "--verify", baseSHA+"^{commit}").CombinedOutput()
	if err != nil || strings.TrimSpace(string(resolvedBase)) != baseSHA {
		return Result{}, fmt.Errorf("immutable base SHA is unavailable: %s", baseSHA)
	}
	if info, err := os.Lstat(path); err == nil && info.IsDir() {
		inspection, inspectErr := m.inspect(ctx, cfg, path, branch)
		if inspectErr != nil {
			return Result{}, fmt.Errorf("inspect existing worktree: %w", inspectErr)
		}
		if !inspection.Valid || inspection.Branch != branch || !inspection.LocalBranchExists {
			return Result{}, fmt.Errorf("existing worktree path is incomplete or belongs to another branch: %s", path)
		}
		if err := exec.CommandContext(ctx, git, "-C", path, "merge-base", "--is-ancestor", baseSHA, "HEAD").Run(); err != nil {
			return Result{}, fmt.Errorf("existing worktree branch is not based on saved base SHA %s", baseSHA)
		}
		return Result{Path: path, Branch: branch}, nil
	}
	branchExists := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
	args := []string{"-C", cfg.RepoPath, "worktree", "add"}
	if branchExists {
		if err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "merge-base", "--is-ancestor", baseSHA, branch).Run(); err != nil {
			return Result{}, fmt.Errorf("existing branch %s is not based on saved base SHA %s", branch, baseSHA)
		}
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path, baseSHA)
	}
	if out, err := exec.CommandContext(ctx, git, args...).CombinedOutput(); err != nil {
		return Result{}, fmt.Errorf("create worktree: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return Result{Path: path, Branch: branch}, nil
}

func (m Manager) Inspect(ctx context.Context, cfg config.Config, path, branch string) (Inspection, error) {
	var inspection Inspection
	err := m.Gate.Run(ctx, gitops.Worktree, func() error {
		var err error
		inspection, err = m.inspect(ctx, cfg, path, branch)
		return err
	})
	return inspection, err
}

func (m Manager) inspect(ctx context.Context, cfg config.Config, path, branch string) (Inspection, error) {
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
	git := m.gitPath()
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
		remoteLine := strings.TrimSpace(string(out))
		inspection.RemoteBranchExists = remoteLine != ""
		if inspection.RemoteBranchExists {
			remoteFields := strings.Fields(remoteLine)
			if len(remoteFields) > 0 {
				inspection.RemoteHead = remoteFields[0]
				inspection.RemoteConsistent = exec.CommandContext(ctx, git, "-C", path, "merge-base", "--is-ancestor", inspection.RemoteHead, inspection.Head).Run() == nil
			}
			inspection.UnpushedCommits = len(remoteFields) == 0 || remoteFields[0] != inspection.Head
		} else {
			base := "origin/" + cfg.Git.BaseBranch
			count, countErr := exec.CommandContext(ctx, git, "-C", path, "rev-list", "--count", base+"..HEAD").CombinedOutput()
			if countErr != nil {
				return inspection, fmt.Errorf("inspect unpushed commits: %w: %s", countErr, strings.TrimSpace(string(count)))
			}
			inspection.UnpushedCommits = strings.TrimSpace(string(count)) != "0"
		}
	}
	return inspection, nil
}

// Remove removes only the linked worktree. It deliberately keeps the local
// branch so committed work remains recoverable. Force is reserved for the
// explicitly confirmed purge command.
func (m Manager) Remove(ctx context.Context, cfg config.Config, path string, force bool) error {
	return m.Gate.Run(ctx, gitops.Worktree, func() error {
		if err := m.remove(ctx, cfg, path, force); err != nil {
			return err
		}
		return m.prune(ctx, cfg)
	})
}

func (m Manager) remove(ctx context.Context, cfg config.Config, path string, force bool) error {
	root := cfg.Git.WorktreeRoot
	if root == "" {
		root = filepath.Join(m.StateRoot, "worktrees")
	}
	secureRoot, err := canonicalPrivateRoot(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil || !within(secureRoot, absPath) {
		return fmt.Errorf("worktree path is outside configured root: %s", path)
	}
	if info, statErr := os.Lstat(absPath); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("worktree path must not be a symbolic link: %s", path)
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	git := m.gitPath()
	if _, statErr := os.Stat(absPath); statErr == nil {
		args := []string{"-C", cfg.RepoPath, "worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		args = append(args, absPath)
		if out, removeErr := exec.CommandContext(ctx, git, args...).CombinedOutput(); removeErr != nil {
			return fmt.Errorf("remove worktree: %w: %s", removeErr, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func (m Manager) Prune(ctx context.Context, cfg config.Config) error {
	return m.Gate.Run(ctx, gitops.Worktree, func() error { return m.prune(ctx, cfg) })
}

func (m Manager) prune(ctx context.Context, cfg config.Config) error {
	git := m.gitPath()
	if out, err := exec.CommandContext(ctx, git, "-C", cfg.RepoPath, "worktree", "prune").CombinedOutput(); err != nil {
		return fmt.Errorf("prune worktree metadata: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m Manager) gitPath() string {
	if m.GitPath != "" {
		return m.GitPath
	}
	return "git"
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

func canonicalExistingRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve worktree root: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect worktree root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("worktree root must be a real directory: %s", abs)
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

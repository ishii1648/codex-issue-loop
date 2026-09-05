package conflict

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

const promptContextLimit = 64 * 1024

// NonRecoverableError identifies a publication-boundary violation or a broken
// worktree. Retrying the same worker cannot safely repair these conditions.
type NonRecoverableError struct{ Err error }

func (e NonRecoverableError) Error() string { return e.Err.Error() }
func (e NonRecoverableError) Unwrap() error { return e.Err }

type Preparation struct {
	PreviousBaseSHA string
	TargetBaseSHA   string
	OriginalHeadSHA string
	ConflictFiles   []string
	AllowedPaths    []string
	OriginalDiff    string
	BaseCommits     string
	ConflictContent string
	Resolved        bool
	Published       bool
	Commit          string
}

type Manager struct{ GitPath string }

func (m Manager) Prepare(ctx context.Context, cfg config.Config, worktreePath, branch string, previous *state.ConflictRecovery) (Preparation, error) {
	if worktreePath == "" || branch == "" {
		return Preparation{}, NonRecoverableError{fmt.Errorf("conflict recovery requires the saved worktree and branch")}
	}
	actualBranch, err := m.run(ctx, worktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(actualBranch) != branch {
		return Preparation{}, NonRecoverableError{fmt.Errorf("conflict recovery worktree branch mismatch: saved=%s actual=%s", branch, strings.TrimSpace(actualBranch))}
	}
	head, err := m.revParse(ctx, worktreePath, "HEAD")
	if err != nil {
		return Preparation{}, NonRecoverableError{fmt.Errorf("resolve conflict recovery HEAD: %w", err)}
	}
	mergeHead, _ := m.revParse(ctx, worktreePath, "MERGE_HEAD")
	if mergeHead != "" {
		if previous == nil || previous.TargetBaseSHA == "" || mergeHead != previous.TargetBaseSHA {
			return Preparation{}, NonRecoverableError{fmt.Errorf("worktree has an unrecorded merge target %s", mergeHead)}
		}
		return m.describe(ctx, worktreePath, head, previous.PreviousBaseSHA, mergeHead, previous.AllowedPaths)
	}

	// A crash may happen after commit or push but before the durable state
	// transition. Recognize only the exact recorded two-parent merge.
	if previous != nil && previous.TargetBaseSHA != "" {
		merged, parentErr := m.mergeCommitContains(ctx, worktreePath, head, previous.TargetBaseSHA)
		if parentErr != nil {
			return Preparation{}, parentErr
		}
		if merged {
			remoteHead, remoteErr := m.remoteHead(ctx, worktreePath, branch)
			if remoteErr != nil {
				return Preparation{}, remoteErr
			}
			return Preparation{
				PreviousBaseSHA: previous.PreviousBaseSHA, TargetBaseSHA: previous.TargetBaseSHA,
				OriginalHeadSHA: previous.OriginalHeadSHA, ConflictFiles: append([]string(nil), previous.ConflictFiles...),
				AllowedPaths: append([]string(nil), previous.AllowedPaths...), Resolved: true,
				Published: remoteHead == head, Commit: head,
			}, nil
		}
	}

	if _, err := m.run(ctx, worktreePath, "fetch", "origin", cfg.Git.BaseBranch); err != nil {
		return Preparation{}, fmt.Errorf("fetch latest base branch: %w", err)
	}
	target, err := m.revParse(ctx, worktreePath, "refs/remotes/origin/"+cfg.Git.BaseBranch)
	if err != nil {
		return Preparation{}, fmt.Errorf("resolve fetched base branch: %w", err)
	}
	base, err := m.run(ctx, worktreePath, "merge-base", head, target)
	if err != nil {
		return Preparation{}, NonRecoverableError{fmt.Errorf("resolve previous base SHA: %w", err)}
	}
	base = strings.TrimSpace(base)
	allowed, err := m.lines(ctx, worktreePath, "diff", "--name-only", target+"..."+head, "--")
	if err != nil {
		return Preparation{}, NonRecoverableError{fmt.Errorf("resolve Pull Request path scope: %w", err)}
	}
	if base == target {
		remoteHead, remoteErr := m.remoteHead(ctx, worktreePath, branch)
		if remoteErr != nil {
			return Preparation{}, remoteErr
		}
		if remoteHead != head {
			return Preparation{}, NonRecoverableError{fmt.Errorf("remote PR branch differs from the up-to-date worktree: local=%s remote=%s", head, remoteHead)}
		}
		return Preparation{
			PreviousBaseSHA: base, TargetBaseSHA: target, OriginalHeadSHA: head,
			AllowedPaths: allowed, Resolved: true, Published: true, Commit: head,
		}, nil
	}
	mergeOutput, mergeErr := m.run(ctx, worktreePath, "merge", "--no-ff", "--no-commit", target)
	conflicts, listErr := m.unmerged(ctx, worktreePath)
	if listErr != nil {
		return Preparation{}, NonRecoverableError{listErr}
	}
	if mergeErr != nil && len(conflicts) == 0 {
		return Preparation{}, fmt.Errorf("prepare base merge: %w: %s", mergeErr, strings.TrimSpace(mergeOutput))
	}
	actualMergeHead, _ := m.revParse(ctx, worktreePath, "MERGE_HEAD")
	if actualMergeHead != target {
		return Preparation{}, NonRecoverableError{fmt.Errorf("prepared merge target mismatch: expected=%s actual=%s", target, actualMergeHead)}
	}
	allowed = uniqueSorted(append(allowed, conflicts...))
	return m.describe(ctx, worktreePath, head, base, target, allowed)
}

func (m Manager) describe(ctx context.Context, worktreePath, head, previousBase, target string, allowed []string) (Preparation, error) {
	conflicts, err := m.unmerged(ctx, worktreePath)
	if err != nil {
		return Preparation{}, err
	}
	diff, err := m.run(ctx, worktreePath, "diff", "--no-ext-diff", "--unified=40", target+"..."+head, "--")
	if err != nil {
		return Preparation{}, fmt.Errorf("capture original Pull Request diff: %w", err)
	}
	commits := ""
	if previousBase != "" && previousBase != target {
		commits, err = m.run(ctx, worktreePath, "log", "--oneline", "--no-decorate", previousBase+".."+target, "--")
		if err != nil {
			return Preparation{}, fmt.Errorf("capture base commits: %w", err)
		}
	}
	var content strings.Builder
	for _, name := range conflicts {
		clean := filepath.Clean(name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return Preparation{}, NonRecoverableError{fmt.Errorf("conflict path escapes worktree: %s", name)}
		}
		data, readErr := os.ReadFile(filepath.Join(worktreePath, clean))
		if readErr != nil {
			return Preparation{}, fmt.Errorf("read conflict file %s: %w", name, readErr)
		}
		fmt.Fprintf(&content, "\n--- %s ---\n%s", name, data)
	}
	return Preparation{
		PreviousBaseSHA: previousBase, TargetBaseSHA: target, OriginalHeadSHA: head,
		ConflictFiles: conflicts, AllowedPaths: uniqueSorted(allowed), OriginalDiff: bounded(diff),
		BaseCommits: bounded(commits), ConflictContent: bounded(content.String()),
		Resolved: len(conflicts) == 0,
	}, nil
}

func (m Manager) Publish(ctx context.Context, cfg config.Config, issue gh.Issue, worktreePath, branch string, recovery state.ConflictRecovery, tests []worker.Test) (worker.GitResult, error) {
	if recovery.TargetBaseSHA == "" || recovery.OriginalHeadSHA == "" {
		return worker.GitResult{}, NonRecoverableError{fmt.Errorf("conflict recovery publication is missing immutable Git SHAs")}
	}
	if len(recovery.ConflictFiles) > 0 {
		if len(tests) == 0 {
			return worker.GitResult{}, fmt.Errorf("conflict worker completed without reporting required verification")
		}
		for _, test := range tests {
			if !test.Passed() {
				return worker.GitResult{}, fmt.Errorf("conflict worker verification is not explicitly green for %q: %s", test.Command, test.Result)
			}
		}
	}
	changed, err := m.lines(ctx, worktreePath, "diff", "--name-only", recovery.TargetBaseSHA, "--")
	if err != nil {
		return worker.GitResult{}, fmt.Errorf("inspect resolved path scope: %w", err)
	}
	untracked, err := m.lines(ctx, worktreePath, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return worker.GitResult{}, fmt.Errorf("inspect untracked recovery paths: %w", err)
	}
	changed = uniqueSorted(append(changed, untracked...))
	allowed := make(map[string]bool, len(recovery.AllowedPaths))
	for _, name := range recovery.AllowedPaths {
		allowed[name] = true
	}
	for _, name := range changed {
		if !allowed[name] {
			return worker.GitResult{}, NonRecoverableError{fmt.Errorf("resolved merge changes path outside the recorded scope: %s", name)}
		}
	}
	if output, checkErr := m.run(ctx, worktreePath, "diff", "--check", recovery.TargetBaseSHA, "--"); checkErr != nil {
		return worker.GitResult{}, fmt.Errorf("resolved merge contains whitespace errors or conflict markers: %w: %s", checkErr, strings.TrimSpace(output))
	}

	mergeHead, _ := m.revParse(ctx, worktreePath, "MERGE_HEAD")
	if mergeHead != "" {
		if mergeHead != recovery.TargetBaseSHA {
			return worker.GitResult{}, NonRecoverableError{fmt.Errorf("merge target changed before publication: expected=%s actual=%s", recovery.TargetBaseSHA, mergeHead)}
		}
		if _, err := m.run(ctx, worktreePath, "add", "--all"); err != nil {
			return worker.GitResult{}, fmt.Errorf("stage resolved merge: %w", err)
		}
		unmerged, err := m.unmerged(ctx, worktreePath)
		if err != nil {
			return worker.GitResult{}, err
		}
		if len(unmerged) != 0 {
			return worker.GitResult{}, fmt.Errorf("unmerged index entries remain after supervisor staging: %s", strings.Join(unmerged, ", "))
		}
		if output, err := m.run(ctx, worktreePath, "diff", "--cached", "--check"); err != nil {
			return worker.GitResult{}, fmt.Errorf("validate resolved merge: %w: %s", err, strings.TrimSpace(output))
		}
		git := m.gitPath()
		message := fmt.Sprintf("Resolve base merge conflicts for #%d", issue.Number)
		out, err := exec.CommandContext(ctx, git, "-c", "commit.gpgsign=false", "-C", worktreePath, "commit", "-m", message).CombinedOutput()
		if err != nil {
			return worker.GitResult{}, fmt.Errorf("commit resolved base merge: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	commit, err := m.revParse(ctx, worktreePath, "HEAD")
	if err != nil {
		return worker.GitResult{}, err
	}
	merged, err := m.mergeCommitContains(ctx, worktreePath, commit, recovery.TargetBaseSHA)
	if err != nil || !merged {
		return worker.GitResult{}, NonRecoverableError{fmt.Errorf("recovery commit does not retain target base SHA %s as a parent", recovery.TargetBaseSHA)}
	}
	remoteHead, err := m.remoteHead(ctx, worktreePath, branch)
	if err != nil {
		return worker.GitResult{}, err
	}
	if remoteHead != commit {
		if remoteHead != recovery.OriginalHeadSHA {
			return worker.GitResult{}, NonRecoverableError{fmt.Errorf("remote PR branch advanced during recovery: expected=%s actual=%s", recovery.OriginalHeadSHA, remoteHead)}
		}
		if _, err := m.run(ctx, worktreePath, "push", "origin", "HEAD:refs/heads/"+branch); err != nil {
			return worker.GitResult{}, fmt.Errorf("push resolved merge without force: %w", err)
		}
	}
	return worker.GitResult{Branch: branch, Commit: commit, PullRequestURL: recovery.PullRequestURL}, nil
}

func (m Manager) unmerged(ctx context.Context, path string) ([]string, error) {
	return m.lines(ctx, path, "diff", "--name-only", "--diff-filter=U", "--")
}

func (m Manager) mergeCommitContains(ctx context.Context, path, commit, target string) (bool, error) {
	parents, err := m.run(ctx, path, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(parents)
	for _, parent := range fields[1:] {
		if parent == target {
			return true, nil
		}
	}
	return false, nil
}

func (m Manager) remoteHead(ctx context.Context, path, branch string) (string, error) {
	out, err := m.run(ctx, path, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", NonRecoverableError{fmt.Errorf("remote PR branch is missing: %s", branch)}
	}
	return fields[0], nil
}

func (m Manager) revParse(ctx context.Context, path, ref string) (string, error) {
	out, err := m.run(ctx, path, "rev-parse", "--verify", ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (m Manager) lines(ctx context.Context, path string, args ...string) ([]string, error) {
	out, err := m.run(ctx, path, args...)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			result = append(result, line)
		}
	}
	return uniqueSorted(result), nil
}

func (m Manager) run(ctx context.Context, path string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", path}, args...)
	out, err := exec.CommandContext(ctx, m.gitPath(), cmdArgs...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}

func (m Manager) gitPath() string {
	if m.GitPath != "" {
		return m.GitPath
	}
	return "git"
}

func uniqueSorted(values []string) []string {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func bounded(value string) string {
	if len(value) <= promptContextLimit {
		return value
	}
	return strings.ToValidUTF8(value[:promptContextLimit], "") + "\n[TRUNCATED]"
}

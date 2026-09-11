package app

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

type worktreeAdoptionObservation struct {
	MergeHead     string
	ChangedPaths  []string
	UnmergedPaths []string
}

type pathListFlag []string

func (f *pathListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *pathListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func normalizeAdoptionAllowPaths(paths []string) ([]string, error) {
	seen := map[string]bool{}
	for _, path := range paths {
		if !validAdoptionPath(path) {
			return nil, fmt.Errorf("--allow-path must be a normalized relative path: %q", path)
		}
		seen[path] = true
	}
	result := make([]string, 0, len(seen))
	for path := range seen {
		result = append(result, path)
	}
	sort.Strings(result)
	return result, nil
}

func worktreeAdoptionReasons(cfg config.Config, item *state.Issue, launch worktree.LaunchValidation, launchErr error,
	inspection worktree.Inspection, inspectErr error, worktreeSHA256 string, worktreeDigestErr error,
	baseOK bool, baseErr error, remote gh.RemoteState, remoteErr error,
	adoption worktreeAdoptionObservation, adoptionErr error, adoptionAllowPaths []string,
) []string {
	reasons := conflictWorktreeReasons(cfg, item, launch, launchErr, inspection, inspectErr, worktreeSHA256, worktreeDigestErr, baseOK, baseErr, remote, remoteErr, adoption, adoptionErr, adoptionAllowPaths)
	if !state.CanAdoptWorktree(item) {
		reasons = append(reasons, "only a quarantined conflict continuation missing worktree_sha256 can be adopted")
	}
	currentUnapproved := adoptionUnapprovedPaths(item, adoption, nil)
	for _, path := range adoptionAllowPaths {
		if !containsPath(currentUnapproved, path) {
			reasons = append(reasons, fmt.Sprintf("explicitly allowed path is not a current unapproved change: %s", path))
		}
	}
	return reasons
}

func conflictWorktreeReasons(cfg config.Config, item *state.Issue, launch worktree.LaunchValidation, launchErr error,
	inspection worktree.Inspection, inspectErr error, worktreeSHA256 string, worktreeDigestErr error,
	baseOK bool, baseErr error, remote gh.RemoteState, remoteErr error,
	adoption worktreeAdoptionObservation, adoptionErr error, adoptionAllowPaths []string,
) []string {
	reasons := []string{}
	if item.WorkerPID != 0 || item.WorkerPGID != 0 {
		reasons = append(reasons, "worker identity is still recorded")
	}
	if launchErr != nil || !launch.Valid || inspectErr != nil || !inspection.Valid || inspection.Branch != item.Branch {
		reasons = append(reasons, "workspace or git worktree identity differs from canonical state")
	}
	if baseErr != nil || !baseOK {
		reasons = append(reasons, "checkpoint base is not an ancestor of the worktree head")
	}
	if worktreeDigestErr != nil || worktreeSHA256 == "" {
		reasons = append(reasons, "current worktree content cannot be fingerprinted")
	}
	checkpoint, recovery := item.Continuation, item.ConflictRecovery
	if checkpoint == nil || recovery == nil || checkpoint.HeadSHA == "" || item.HeadSHA == "" ||
		checkpoint.HeadSHA != item.HeadSHA || inspection.Head != checkpoint.HeadSHA || recovery.OriginalHeadSHA != checkpoint.HeadSHA ||
		recovery.TargetBaseSHA == "" || recovery.PullRequestURL == "" || recovery.PullRequestURL != item.PullRequestURL || len(recovery.AllowedPaths) == 0 {
		reasons = append(reasons, "saved conflict recovery identity is incomplete or inconsistent")
	}
	if !inspection.RemoteBranchExists || inspection.RemoteHead == "" || inspection.RemoteHead != inspection.Head {
		reasons = append(reasons, "remote branch head differs from the saved worktree head")
	}
	if remoteErr != nil {
		reasons = append(reasons, "GitHub state was not observed")
	} else if pullRequest, ok := matchingOpenPullRequest(item, remote.PullRequests); !ok ||
		!strings.EqualFold(pullRequest.HeadRepository, cfg.GitHub.Repo) || pullRequest.BaseRefName != cfg.Git.BaseBranch || pullRequest.HeadSHA != inspection.Head {
		reasons = append(reasons, "open Pull Request identity or head differs from the saved conflict recovery")
	}
	if adoptionErr != nil {
		reasons = append(reasons, "conflict worktree evidence could not be observed")
	} else if recovery != nil {
		if adoption.MergeHead == "" || adoption.MergeHead != recovery.TargetBaseSHA {
			reasons = append(reasons, "MERGE_HEAD differs from the recorded conflict target")
		}
		unapproved := adoptionUnapprovedPaths(item, adoption, adoptionAllowPaths)
		if len(unapproved) > 0 || !pathsWithinRecordedScope(adoption.UnmergedPaths, mergeAdoptionPaths(recovery.AllowedPaths, adoptionAllowPaths)) {
			reasons = append(reasons, "conflict worktree contains paths outside the recorded scope")
		}
	}
	return reasons
}

func adoptionUnapprovedPaths(item *state.Issue, adoption worktreeAdoptionObservation, adoptionAllowPaths []string) []string {
	if item == nil || item.ConflictRecovery == nil {
		return append([]string(nil), adoption.ChangedPaths...)
	}
	allowed := mergeAdoptionPaths(item.ConflictRecovery.AllowedPaths, adoptionAllowPaths)
	result := make([]string, 0)
	for _, path := range adoption.ChangedPaths {
		if !containsPath(allowed, path) {
			result = append(result, path)
		}
	}
	return result
}

func mergeAdoptionPaths(recorded, added []string) []string {
	result, _ := normalizeAdoptionAllowPaths(append(append([]string(nil), recorded...), added...))
	return result
}

func containsPath(paths []string, target string) bool {
	index := sort.SearchStrings(paths, target)
	return index < len(paths) && paths[index] == target
}

func inspectWorktreeAdoption(ctx context.Context, gitPath string, item *state.Issue) (worktreeAdoptionObservation, error) {
	if gitPath == "" {
		gitPath = "git"
	}
	run := func(args ...string) (string, error) {
		output, err := exec.CommandContext(ctx, gitPath, append([]string{"-C", item.Worktree}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
	mergeHead, err := run("rev-parse", "--verify", "MERGE_HEAD")
	if err != nil {
		return worktreeAdoptionObservation{}, err
	}
	changed, err := run("diff", "--no-renames", "--name-only", "-z", item.ConflictRecovery.TargetBaseSHA, "--")
	if err != nil {
		return worktreeAdoptionObservation{}, err
	}
	staged, err := run("diff", "--cached", "--no-renames", "--name-only", "-z", item.ConflictRecovery.TargetBaseSHA, "--")
	if err != nil {
		return worktreeAdoptionObservation{}, err
	}
	untracked, err := run("ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return worktreeAdoptionObservation{}, err
	}
	unmerged, err := run("diff", "--name-only", "-z", "--diff-filter=U", "--")
	if err != nil {
		return worktreeAdoptionObservation{}, err
	}
	return worktreeAdoptionObservation{
		MergeHead: strings.TrimSpace(mergeHead), ChangedPaths: uniqueSortedPaths(changed, staged, untracked), UnmergedPaths: uniqueSortedPaths(unmerged),
	}, nil
}

func uniqueSortedPaths(values ...string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		for _, line := range strings.Split(value, "\x00") {
			if line != "" {
				seen[line] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for line := range seen {
		result = append(result, line)
	}
	sort.Strings(result)
	return result
}

func pathsWithinRecordedScope(paths, allowed []string) bool {
	scope := make(map[string]bool, len(allowed))
	for _, path := range allowed {
		if !validAdoptionPath(path) {
			return false
		}
		scope[path] = true
	}
	for _, path := range paths {
		if !scope[path] {
			return false
		}
	}
	return true
}

func validAdoptionPath(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && !filepath.IsAbs(path) && clean == path && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

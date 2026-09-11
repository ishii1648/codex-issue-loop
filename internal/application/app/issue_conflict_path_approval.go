package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

var errConflictPathsAlreadyApproved = errors.New("conflict paths already approved")

func unrecordedApprovalPaths(item *state.Issue, paths []string) []string {
	var added []string
	for _, path := range paths {
		if !slices.Contains(item.ConflictRecovery.AllowedPaths, path) {
			added = append(added, path)
		}
	}
	return added
}

func conflictPathApprovalReasons(cfg config.Config, item *state.Issue, launch worktree.LaunchValidation, launchErr error,
	inspection worktree.Inspection, inspectErr error, digest string, digestErr error,
	baseOK bool, baseErr error, remote gh.RemoteState, remoteErr error,
	observation worktreeAdoptionObservation, observationErr error, paths []string,
) []string {
	reasons := conflictWorktreeReasons(cfg, item, launch, launchErr, inspection, inspectErr, digest, digestErr, baseOK, baseErr, remote, remoteErr, observation, observationErr, paths)
	if !state.CanApproveConflictPaths(item) {
		reasons = append(reasons, "conflict approval requires a complete active retryable checkpoint")
	}
	if digest == "" || digest != checkpointWorktreeSHA256(item) {
		reasons = append(reasons, "worktree content differs from the continuation checkpoint")
	}
	if item.Workspace == nil || !item.Workspace.Matches(item.Worktree, item.Branch, registry.RepoID(cfg.GitHub.Repo, cfg.RepoPath), cfg.GitHub.Repo, cfg.GitHub.RepositoryID, launch.CommonDir, launch.MainCheckout) {
		reasons = append(reasons, "saved workspace provenance differs from current worktree")
	}
	if remoteErr != nil || remote.Issue.Number != item.Number || !strings.EqualFold(remote.Issue.State, "open") || len(remote.PullRequests) != 1 ||
		!pullRequestsMatchCheckpoint(item, remote.PullRequests) || item.PullRequestMerged {
		reasons = append(reasons, "Issue or Pull Request identity is ambiguous or no longer open")
	}
	if !inspection.LocalBranchExists || !inspection.RemoteConsistent {
		reasons = append(reasons, "git branch identity is inconsistent")
	}
	if err := issuedomain.ValidateConflictApprovalPaths(paths); err != nil {
		reasons = append(reasons, err.Error())
	}
	for _, path := range paths {
		if !slices.Contains(observation.ChangedPaths, path) {
			reasons = append(reasons, fmt.Sprintf("explicitly allowed path is not an exact current change: %q", path))
		}
	}
	return reasons
}

func (a App) verifyConflictPathApproval(ctx context.Context, planned issuePlanningContext, root string) error {
	item := planned.issue
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	if controller.Alive(item.WorkerPID) || controller.GroupAlive(item.WorkerPGID) {
		return fmt.Errorf("worker is alive before conflict approval")
	}
	manager := worktree.Manager{StateRoot: root, GitPath: planned.gitPath}
	launch, err := manager.ValidateLaunch(ctx, planned.cfg, item.Worktree, item.Branch)
	if err != nil || !reflect.DeepEqual(launch, planned.launch) {
		return fmt.Errorf("workspace changed before conflict approval")
	}
	observation, err := inspectWorktreeAdoption(ctx, planned.gitPath, item)
	if err != nil || !reflect.DeepEqual(observation, planned.adoption) {
		return fmt.Errorf("merge evidence changed before conflict approval")
	}
	return verifyRecoveryWorkspace(ctx, planned, root)
}

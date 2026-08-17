package app

import (
	"context"
	"flag"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

func (a App) adoptMergedPullRequest(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("adopt-merged-pr", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "terminal Issue number")
	confirmed := fs.Bool("confirm-merged-pr-adoption", false, "confirm the saved branch was manually published and merged")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*confirmed {
		return exitError{2, fmt.Errorf("--confirm-merged-pr-adoption is required")}
	}
	entry, err := a.resolvePath(l, *repo)
	if err != nil {
		return err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	snapshot, err := store.Load()
	if err != nil {
		return err
	}
	current := snapshot.Issues[strconv.Itoa(*issueNumber)]
	if current == nil {
		return exitError{4, fmt.Errorf("Issue #%d is missing from durable state", *issueNumber)}
	}
	if current.MergedPullRequestAdoption != nil && current.Status == "completed" {
		if current.GitHubSync == "done" || current.MergedPullRequestAdoption.Status == "requested" {
			if err := syncAdoptedMergedPullRequest(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
				return err
			}
			current, err = issueFromStore(store, *issueNumber)
			if err != nil {
				return err
			}
		}
		return a.output(*jsonOut, mergedPullRequestAdoptionOutput(current, true))
	}
	if (current.Status != "blocked" && current.Status != "failed") || current.GitHubSync != "" {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and terminal before merged Pull Request adoption (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	if current.FailureKind != "issue" || current.LastError == "" || current.PullRequestURL != "" || current.PullRequestNumber != 0 || current.PullRequestMerged {
		return exitError{4, fmt.Errorf("Issue #%d does not retain an eligible unpublished terminal failure boundary", *issueNumber)}
	}
	if current.Status == "blocked" && (current.BlockedCause == nil || current.BlockedCause.Origin != "worker" || current.BlockedCause.Kind != "environment" || !current.BlockedCause.Resumable) {
		return exitError{4, fmt.Errorf("Issue #%d is not a supervisor-owned resumable worker block", *issueNumber)}
	}
	if current.ConflictRecovery != nil || current.EnvironmentResume != nil || current.PublicationRecovery != nil || current.PullRequestChecksRecovery != nil {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible in-progress recovery", *issueNumber)}
	}
	if !state.ValidID(current.RunID, "run_") || current.Worktree == "" || current.Branch == "" || current.Lease == nil {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, branch, and resource lease", *issueNumber)}
	}
	owner := current.Lease.Owner
	if owner.RunID != current.RunID || owner.Generation == 0 || owner.Generation != current.LeaseGeneration ||
		len(current.Lease.DeclaredResources) == 0 || len(current.Lease.ResolvedResources) == 0 || current.Lease.BaseSHA == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a complete fenced resource lease", *issueNumber)}
	}
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	pgid := current.WorkerPGID
	if pgid <= 1 {
		pgid = current.WorkerPID
	}
	if controller.Alive(current.WorkerPID) || controller.GroupAlive(pgid) {
		return exitError{4, fmt.Errorf("Issue #%d still has an active worker process", *issueNumber)}
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == *issueNumber && request.Status == "pending" {
			return exitError{4, fmt.Errorf("Issue #%d has a pending manual answer request", *issueNumber)}
		}
	}
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect merged Pull Request adoption worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists ||
		!inspection.RemoteBranchExists || inspection.Head == "" || inspection.RemoteHead == "" ||
		inspection.Head != inspection.RemoteHead || !inspection.RemoteConsistent || inspection.Dirty || inspection.UnpushedCommits {
		return exitError{4, fmt.Errorf("saved worktree/branch must be clean, fully pushed, and aligned before merged Pull Request adoption: %+v", inspection)}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect merged Pull Request adoption state: %w", err)
	}
	pr, err := gh.ValidateMergedPullRequestAdoption(cfg, remote, gh.MergedPullRequestAdoptionExpectation{
		IssueNumber: *issueNumber, PreviousStatus: current.Status, Branch: current.Branch,
		BaseBranch: cfg.Git.BaseBranch, HeadSHA: inspection.Head,
	})
	if err != nil {
		return exitError{4, err}
	}
	if err := verifyAdoptedGitHistory(ctx, entry.Commands["git"], current.Worktree, cfg.Git.BaseBranch, current.Lease.BaseSHA, inspection.Head, pr.MergeCommitSHA); err != nil {
		return exitError{4, err}
	}
	now := time.Now().UTC()
	generation := 1
	if current.MergedPullRequestAdoption != nil {
		generation = current.MergedPullRequestAdoption.Generation + 1
	}
	adoption := &state.MergedPullRequestAdoption{
		ID: state.NewID("merged_pr_adoption"), Status: "requested", Generation: generation,
		ConfirmedAt: now, AdoptedAt: now, PreviousStatus: current.Status, PreviousReason: current.LastError,
		PullRequestURL: pr.URL, PullRequestNumber: pr.Number, Branch: pr.HeadRefName,
		HeadSHA: pr.HeadSHA, MergeCommitSHA: pr.MergeCommitSHA,
	}
	_, err = store.Update("merged_pull_request_adopted", *issueNumber, current.RunID, map[string]any{
		"adoption_id": adoption.ID, "generation": generation, "operator_confirmed": true,
		"previous_status": current.Status, "pull_request_url": pr.URL, "pull_request_number": pr.Number,
		"branch": pr.HeadRefName, "head_sha": pr.HeadSHA, "merge_commit_sha": pr.MergeCommitSHA,
	}, func(s *state.Snapshot) error {
		if s.StateRevision != snapshot.StateRevision {
			return fmt.Errorf("Issue #%d durable state changed while merged Pull Request adoption was being prepared", *issueNumber)
		}
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while merged Pull Request adoption was being prepared", *issueNumber)
		}
		if err := state.ReleaseIssueLease(item, owner); err != nil {
			return err
		}
		item.Status = "completed"
		item.PullRequestURL = pr.URL
		item.PullRequestNumber = pr.Number
		item.HeadSHA = pr.HeadSHA
		item.PullRequestMerged = true
		item.GitHubSync = "done"
		item.FailureKind = ""
		item.LastError = ""
		item.RetryAfter = nil
		item.MergedPullRequestAdoption = adoption
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return err
	}
	current, err = issueFromStore(store, *issueNumber)
	if err != nil {
		return err
	}
	if err := syncAdoptedMergedPullRequest(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
		return err
	}
	current, err = issueFromStore(store, *issueNumber)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, mergedPullRequestAdoptionOutput(current, false))
}

func verifyAdoptedGitHistory(ctx context.Context, gitPath, worktreePath, baseBranch, leaseBaseSHA, headSHA, mergeCommitSHA string) error {
	if gitPath == "" {
		gitPath = "git"
	}
	if baseBranch == "" || leaseBaseSHA == "" || headSHA == "" || mergeCommitSHA == "" {
		return fmt.Errorf("merged Pull Request Git history boundary is incomplete")
	}
	run := func(args ...string) error {
		out, err := exec.CommandContext(ctx, gitPath, append([]string{"-C", worktreePath}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := run("cat-file", "-e", leaseBaseSHA+"^{commit}"); err != nil {
		return fmt.Errorf("saved lease base commit is not present in the saved repository: %w", err)
	}
	if err := run("merge-base", "--is-ancestor", leaseBaseSHA, headSHA); err != nil {
		return fmt.Errorf("saved lease base is not an ancestor of the saved branch head: %w", err)
	}
	if err := run("cat-file", "-e", mergeCommitSHA+"^{commit}"); err != nil {
		return fmt.Errorf("merged Pull Request commit is not present in the saved repository: %w", err)
	}
	if err := run("merge-base", "--is-ancestor", mergeCommitSHA, "origin/"+baseBranch); err != nil {
		return fmt.Errorf("merged Pull Request commit is not an ancestor of the configured remote-tracking base; fetch the base branch and retry: %w", err)
	}
	return nil
}

func syncAdoptedMergedPullRequest(ctx context.Context, store state.Store, cfg config.Config, ghPath string, current *state.Issue) error {
	if current == nil || current.MergedPullRequestAdoption == nil || current.Status != "completed" || !current.PullRequestMerged {
		return fmt.Errorf("merged Pull Request adoption metadata is missing or inconsistent")
	}
	adoption := current.MergedPullRequestAdoption
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, current.Number, adoption.Branch)
	if err != nil {
		return fmt.Errorf("reinspect adopted merged Pull Request before synchronization: %w", err)
	}
	if _, err := gh.ValidateMergedPullRequestAdoption(cfg, remote, gh.MergedPullRequestAdoptionExpectation{
		IssueNumber: current.Number, PreviousStatus: adoption.PreviousStatus, Branch: adoption.Branch,
		BaseBranch: cfg.Git.BaseBranch, HeadSHA: adoption.HeadSHA, PullRequestURL: adoption.PullRequestURL,
		PullRequestNumber: adoption.PullRequestNumber, MergeCommitSHA: adoption.MergeCommitSHA, AllowDone: true,
	}); err != nil {
		return fmt.Errorf("refuse adopted merged Pull Request synchronization: %w", err)
	}
	if current.GitHubSync == "done" {
		if err := client.MarkDone(ctx, cfg, current.Number, adoption.PullRequestURL); err != nil {
			return fmt.Errorf("sync adopted merged Pull Request completion to GitHub: %w", err)
		}
	}
	now := time.Now().UTC()
	_, err = store.Update("merged_pull_request_adoption_synced", current.Number, current.RunID, map[string]any{
		"adoption_id": adoption.ID, "generation": adoption.Generation, "pull_request_url": adoption.PullRequestURL,
		"merge_commit_sha": adoption.MergeCommitSHA,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.MergedPullRequestAdoption == nil || item.MergedPullRequestAdoption.ID != adoption.ID {
			return fmt.Errorf("Issue #%d merged Pull Request adoption changed during synchronization", current.Number)
		}
		if item.GitHubSync == "done" {
			item.GitHubSync = ""
		}
		item.MergedPullRequestAdoption.Status = "completed"
		item.UpdatedAt = now
		return nil
	})
	return err
}

func mergedPullRequestAdoptionOutput(current *state.Issue, idempotent bool) map[string]any {
	result := map[string]any{
		"issue": current.Number, "status": current.Status, "branch": current.Branch,
		"pull_request_url": current.PullRequestURL, "pull_request_number": current.PullRequestNumber,
		"lease_released": current.Lease == nil, "idempotent": idempotent,
		"worker_attempts": current.Attempts, "worker_continuations": current.Continuations,
	}
	if adoption := current.MergedPullRequestAdoption; adoption != nil {
		result["adoption_id"] = adoption.ID
		result["generation"] = adoption.Generation
		result["adoption_status"] = adoption.Status
		result["head_sha"] = adoption.HeadSHA
		result["merge_commit_sha"] = adoption.MergeCommitSHA
		result["confirmed_at"] = adoption.ConfirmedAt
		result["adopted_at"] = adoption.AdoptedAt
	}
	return result
}

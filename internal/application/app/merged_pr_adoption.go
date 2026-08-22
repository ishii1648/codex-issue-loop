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

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func (a App) adoptMergedPullRequest(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("adopt-merged-pr", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "terminal Issue number")
	confirmed := fs.Bool("confirm-merged-pr-adoption", false, "confirm adoption of the unique externally merged Pull Request")
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
	if current.MergedPullRequestAdoption == nil && current.Status == issuedomain.StatusCompleted {
		current, err = recoverMergedPullRequestAdoptionMetadata(ctx, store, cfg, l.Root, entry.Commands["git"], entry.Commands["gh"], current)
		if err != nil {
			return exitError{4, err}
		}
	}
	if current.MergedPullRequestAdoption != nil && current.Status == issuedomain.StatusCompleted {
		if current.GitHubSync == issuedomain.GitHubSyncDone {
			if err := syncMergedPullRequestAdoption(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
				return err
			}
			current, err = issueFromStore(store, *issueNumber)
			if err != nil {
				return err
			}
		}
		return a.output(*jsonOut, mergedPullRequestAdoptionOutput(current, true))
	}
	if (current.Status != issuedomain.StatusBlocked && current.Status != issuedomain.StatusFailed) || current.GitHubSync != issuedomain.GitHubSyncNone || current.PullRequestURL != "" || current.PullRequestNumber != 0 || current.PullRequestMerged {
		return exitError{4, fmt.Errorf("Issue #%d must be a fully synchronized terminal record without an adopted Pull Request (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	if !state.ValidID(current.RunID, "run_") || current.Worktree == "" || current.Branch == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, and branch", *issueNumber)}
	}
	if current.FailureKind != "issue" || current.LastError == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not have supervisor-owned Issue failure provenance", *issueNumber)}
	}
	if current.Lease == nil || current.Lease.Owner.RunID != current.RunID || current.Lease.Owner.Generation == 0 ||
		current.Lease.Owner.Generation != current.LeaseGeneration || current.Lease.BaseSHA == "" ||
		len(current.Lease.DeclaredResources) == 0 || len(current.Lease.ResolvedResources) == 0 {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent fenced resource lease", *issueNumber)}
	}
	if current.Status == issuedomain.StatusBlocked && (current.BlockedCause == nil || current.BlockedCause.Origin != "worker" || current.BlockedCause.Kind != "environment" || !current.BlockedCause.Resumable) {
		return exitError{4, fmt.Errorf("Issue #%d is not a resumable worker environment block", *issueNumber)}
	}
	if current.ConflictRecovery != nil || current.EnvironmentResume != nil || current.PublicationRecovery != nil || current.PullRequestChecksRecovery != nil {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible recovery in progress", *issueNumber)}
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
		if request != nil && request.IssueNumber == *issueNumber && request.Status == issuedomain.RequestStatusPending {
			return exitError{4, fmt.Errorf("Issue #%d has a pending manual answer request", *issueNumber)}
		}
	}

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect merged Pull Request adoption worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists ||
		!inspection.RemoteBranchExists || inspection.Head == "" || inspection.RemoteHead == "" || inspection.Head != inspection.RemoteHead || !inspection.RemoteConsistent {
		return exitError{4, fmt.Errorf("saved worktree/branch is not aligned with its pushed remote head: %+v", inspection)}
	}
	if inspection.Dirty || inspection.UnpushedCommits {
		return exitError{4, fmt.Errorf("saved worktree must be clean and fully pushed before merged Pull Request adoption")}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect merged Pull Request adoption state: %w", err)
	}
	pr, err := gh.ValidateMergedPullRequestAdoption(cfg, remote, gh.MergedPullRequestAdoptionExpectation{
		IssueNumber: current.Number, PreviousStatus: current.Status, Branch: current.Branch,
		BaseBranch: cfg.Git.BaseBranch, HeadSHA: inspection.Head,
	})
	if err != nil {
		return exitError{4, err}
	}
	if err := verifyAdoptedGitHistory(ctx, entry.Commands["git"], current.Worktree, cfg.Git.BaseBranch, current.Lease.BaseSHA, inspection.Head, pr.MergeSHA); err != nil {
		return exitError{4, err}
	}
	now := time.Now().UTC()
	adoption := &state.MergedPullRequestAdoption{
		ID: state.NewID("merged_pr_adoption"), Status: issuedomain.MergedPullRequestAdoptionStatusGitHubSyncPending, Generation: 1,
		ConfirmedAt: now, AdoptedAt: now, PullRequestURL: pr.URL, PullRequestNumber: pr.Number,
		PreviousStatus: current.Status, PreviousReason: current.LastError, Branch: current.Branch,
		HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, BaseBranch: pr.BaseRefName,
	}
	owner := current.Lease.Owner
	completion, err := issuedomain.Complete(current.Status, pr.URL)
	if err != nil {
		return exitError{4, err}
	}
	_, err = store.Update("merged_pull_request_adopted", *issueNumber, current.RunID, map[string]any{
		"adoption_id": adoption.ID, "generation": adoption.Generation, "operator_confirmed": true,
		"pull_request_url": pr.URL, "pull_request_number": pr.Number, "head_sha": pr.HeadSHA,
		"merge_sha": pr.MergeSHA, "base_branch": pr.BaseRefName, "previous_status": current.Status,
		"previous_reason": current.LastError, "branch": current.Branch, "confirmed_at": now, "adopted_at": now, "lease_owner": owner,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while merged Pull Request adoption was being prepared", *issueNumber)
		}
		if err := state.ReleaseIssueLease(item, owner); err != nil {
			return err
		}
		if err := state.ApplyIssueTransition(item, completion.Transition); err != nil {
			return err
		}
		item.PullRequestURL = completion.PullRequestURL
		item.PullRequestNumber = pr.Number
		item.HeadSHA = pr.HeadSHA
		item.PullRequestMerged = completion.PullRequestMerged
		item.GitHubSync = completion.GitHubSync
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
	if err := syncMergedPullRequestAdoption(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
		return err
	}
	current, err = issueFromStore(store, *issueNumber)
	if err != nil {
		return err
	}
	if current.MergedPullRequestAdoption == nil {
		record, recordErr := store.MergedPullRequestAdoptionRecord(current.Number, current.RunID)
		if recordErr != nil {
			return recordErr
		}
		current.MergedPullRequestAdoption = &record.Adoption
	}
	return a.output(*jsonOut, mergedPullRequestAdoptionOutput(current, false))
}

func verifyAdoptedGitHistory(ctx context.Context, gitPath, worktreePath, baseBranch, leaseBaseSHA, headSHA, mergeSHA string) error {
	if gitPath == "" {
		gitPath = "git"
	}
	if baseBranch == "" || leaseBaseSHA == "" || headSHA == "" || mergeSHA == "" {
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
	if err := run("cat-file", "-e", mergeSHA+"^{commit}"); err != nil {
		return fmt.Errorf("merged Pull Request commit is not present in the saved repository: %w", err)
	}
	if err := run("merge-base", "--is-ancestor", mergeSHA, "origin/"+baseBranch); err != nil {
		return fmt.Errorf("merged Pull Request commit is not an ancestor of the configured remote-tracking base; fetch the base branch and retry: %w", err)
	}
	return nil
}

func recoverMergedPullRequestAdoptionMetadata(ctx context.Context, store state.Store, cfg config.Config, stateRoot, gitPath, ghPath string, current *state.Issue) (*state.Issue, error) {
	record, err := store.MergedPullRequestAdoptionRecord(current.Number, current.RunID)
	if err != nil {
		return nil, err
	}
	adoption := record.Adoption
	if adoption.BaseBranch == "" {
		adoption.BaseBranch = cfg.Git.BaseBranch
	}
	if current.Status != issuedomain.StatusCompleted || current.Lease != nil || !current.PullRequestMerged || (current.GitHubSync != issuedomain.GitHubSyncNone && current.GitHubSync != issuedomain.GitHubSyncDone) ||
		current.PullRequestURL != adoption.PullRequestURL || current.PullRequestNumber != adoption.PullRequestNumber || current.HeadSHA != adoption.HeadSHA ||
		record.LeaseOwner.RunID != current.RunID || record.LeaseOwner.Generation != current.LeaseGeneration {
		return nil, fmt.Errorf("Issue #%d completed snapshot is inconsistent with its durable adoption event", current.Number)
	}
	manager := worktree.Manager{StateRoot: stateRoot, GitPath: gitPath}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return nil, fmt.Errorf("inspect adopted Pull Request worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || !inspection.RemoteBranchExists ||
		inspection.Dirty || inspection.UnpushedCommits || inspection.Head != adoption.HeadSHA || inspection.RemoteHead != adoption.HeadSHA {
		return nil, fmt.Errorf("Issue #%d adopted worktree and branch no longer match durable history", current.Number)
	}
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, current.Number, current.Branch)
	if err != nil {
		return nil, fmt.Errorf("reinspect adopted Pull Request metadata: %w", err)
	}
	if len(remote.PullRequests) != 1 {
		return nil, fmt.Errorf("Issue #%d adopted Pull Request count changed", current.Number)
	}
	pr := remote.PullRequests[0]
	labels := lowerLabelSet(remote.Issue.Labels)
	githubSynced := labels[strings.ToLower(cfg.GitHub.DoneLabel)] && hasAppComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->")
	terminalPending := current.GitHubSync == issuedomain.GitHubSyncDone && hasAppComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number))
	if (!githubSynced && !terminalPending) || pr.URL != adoption.PullRequestURL || pr.Number != adoption.PullRequestNumber || !strings.EqualFold(pr.State, "merged") || pr.MergedAt == nil ||
		pr.HeadRefName != current.Branch || pr.BaseRefName != adoption.BaseBranch || pr.HeadSHA != adoption.HeadSHA || pr.MergeSHA != adoption.MergeSHA ||
		!strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) {
		return nil, fmt.Errorf("Issue #%d authoritative GitHub completion no longer matches durable adoption history", current.Number)
	}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if labels[strings.ToLower(label)] && !(terminalPending && strings.EqualFold(label, "blocked")) {
			return nil, fmt.Errorf("Issue #%d has exclusion label %q after adoption", current.Number, label)
		}
	}
	now := time.Now().UTC()
	_, err = store.Update("merged_pull_request_adoption_recovered", current.Number, current.RunID, map[string]any{
		"adoption_id": adoption.ID, "generation": adoption.Generation, "pull_request_url": adoption.PullRequestURL,
		"pull_request_number": adoption.PullRequestNumber, "head_sha": adoption.HeadSHA, "merge_sha": adoption.MergeSHA,
		"adopted_at": adoption.AdoptedAt, "source": "durable_event_history",
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while adoption metadata was being recovered", current.Number)
		}
		item.MergedPullRequestAdoption = &adoption
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}
	updated, err := issueFromStore(store, current.Number)
	if err != nil {
		return nil, err
	}
	if updated.MergedPullRequestAdoption == nil {
		updated.MergedPullRequestAdoption = &adoption
	}
	return updated, nil
}

func syncMergedPullRequestAdoption(ctx context.Context, store state.Store, cfg config.Config, ghPath string, current *state.Issue) error {
	if current == nil || current.MergedPullRequestAdoption == nil || current.Status != issuedomain.StatusCompleted || current.GitHubSync != issuedomain.GitHubSyncDone {
		return fmt.Errorf("merged Pull Request adoption synchronization metadata is inconsistent")
	}
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, current.Number, current.Branch)
	if err != nil {
		return fmt.Errorf("reinspect merged Pull Request adoption before synchronization: %w", err)
	}
	adoption := current.MergedPullRequestAdoption
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("refuse merged Pull Request adoption synchronization: Pull Request count changed")
	}
	pr := remote.PullRequests[0]
	if adoption.PreviousStatus != issuedomain.StatusUnset {
		branch := adoption.Branch
		if branch == "" {
			branch = current.Branch
		}
		if _, err := gh.ValidateMergedPullRequestAdoption(cfg, remote, gh.MergedPullRequestAdoptionExpectation{
			IssueNumber: current.Number, PreviousStatus: adoption.PreviousStatus, Branch: branch,
			BaseBranch: adoption.BaseBranch, HeadSHA: adoption.HeadSHA, PullRequestURL: adoption.PullRequestURL,
			PullRequestNumber: adoption.PullRequestNumber, MergeCommitSHA: adoption.MergeSHA, AllowDone: true,
		}); err != nil {
			return fmt.Errorf("refuse merged Pull Request adoption synchronization: %w", err)
		}
	} else {
		if pr.URL != adoption.PullRequestURL || pr.Number != adoption.PullRequestNumber || !strings.EqualFold(pr.State, "merged") || pr.MergedAt == nil ||
			pr.HeadRefName != current.Branch || pr.BaseRefName != adoption.BaseBranch || pr.HeadSHA != adoption.HeadSHA || pr.MergeSHA != adoption.MergeSHA ||
			!strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) {
			return fmt.Errorf("refuse merged Pull Request adoption synchronization: authoritative Pull Request changed")
		}
		labels := lowerLabelSet(remote.Issue.Labels)
		for _, label := range cfg.GitHub.ExcludeLabels {
			if labels[strings.ToLower(label)] && !strings.EqualFold(label, "blocked") {
				return fmt.Errorf("refuse merged Pull Request adoption synchronization: label %q excludes adoption", label)
			}
		}
	}
	if err := client.MarkDone(ctx, cfg, current.Number, current.PullRequestURL); err != nil {
		return fmt.Errorf("sync merged Pull Request adoption to GitHub (durable completion remains pending): %w", err)
	}
	now := time.Now().UTC()
	_, err = store.Update("merged_pull_request_adoption_synced", current.Number, current.RunID, map[string]any{
		"adoption_id": adoption.ID, "generation": adoption.Generation, "pull_request_url": adoption.PullRequestURL,
		"pull_request_number": adoption.PullRequestNumber, "head_sha": adoption.HeadSHA, "merge_sha": adoption.MergeSHA,
		"adopted_at": adoption.AdoptedAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item != nil && item.GitHubSync == issuedomain.GitHubSyncDone && item.MergedPullRequestAdoption != nil && item.MergedPullRequestAdoption.ID == adoption.ID {
			item.GitHubSync = issuedomain.GitHubSyncNone
			item.MergedPullRequestAdoption.Status = issuedomain.MergedPullRequestAdoptionStatusSynced
			item.UpdatedAt = now
		}
		return nil
	})
	return err
}

func hasAppComment(comments []string, marker string) bool {
	for _, comment := range comments {
		if strings.Contains(comment, marker) {
			return true
		}
	}
	return false
}

func mergedPullRequestAdoptionOutput(current *state.Issue, idempotent bool) map[string]any {
	result := map[string]any{
		"issue": current.Number, "status": current.Status, "branch": current.Branch,
		"pull_request_url": current.PullRequestURL, "pull_request_number": current.PullRequestNumber,
		"head_sha": current.HeadSHA, "lease_released": current.Lease == nil, "idempotent": idempotent,
		"worker_attempts": current.Attempts, "worker_continuations": current.Continuations,
	}
	if adoption := current.MergedPullRequestAdoption; adoption != nil {
		result["adoption_id"] = adoption.ID
		result["generation"] = adoption.Generation
		result["adoption_status"] = adoption.Status
		result["merge_sha"] = adoption.MergeSHA
		result["adopted_at"] = adoption.AdoptedAt
	}
	return result
}

package app

import (
	"context"
	"flag"
	"fmt"
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

func (a App) recoverPullRequestChecks(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-checks", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "failed Issue number")
	confirmed := fs.Bool("confirm-external-fix", false, "confirm the saved Pull Request branch was externally fixed")
	dryRun := fs.Bool("dry-run", false, "evaluate all safe recovery predicates without mutation")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*confirmed && !*dryRun {
		return exitError{2, fmt.Errorf("--confirm-external-fix is required")}
	}
	if *dryRun {
		return a.explainRecoverChecks(ctx, l, *repo, *issueNumber, *jsonOut)
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
	progress := pullRequestChecksRecoveryProgress(current)
	if progress == recoveryProgressIdempotent {
		if current.GitHubSync == issuedomain.GitHubSyncPullRequestChecksRecovery {
			if err := syncPullRequestChecksRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
				return err
			}
			current, err = issueFromStore(store, *issueNumber)
			if err != nil {
				return err
			}
		}
		return a.output(*jsonOut, pullRequestChecksRecoveryOutput(current, true))
	}
	if progress != recoveryProgressFresh {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and failed before checks recovery (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	legacyCompatibility := false
	failureRecord := current.PullRequestChecksFailure
	if !state.RecoverablePullRequestChecksFailure(current) {
		failureRecord, err = store.LegacyPullRequestChecksFailure(*current, cfg.GitHub.Repo, cfg.Git.BaseBranch)
		if err != nil {
			return exitError{4, fmt.Errorf("Issue #%d does not have recoverable typed or exact legacy Pull Request checks retry exhaustion provenance: %w", *issueNumber, err)}
		}
		legacyCompatibility = true
	}
	if current.FailureKind != "issue" || current.PullRequestMerged || current.PullRequestURL != failureRecord.PullRequestURL ||
		current.PullRequestNumber != failureRecord.PullRequestNumber || current.Branch != failureRecord.Branch {
		return exitError{4, fmt.Errorf("Issue #%d Pull Request checks failure provenance is inconsistent with durable state", *issueNumber)}
	}
	if !state.ValidID(current.RunID, "run_") || current.Worktree == "" || current.Branch == "" || current.PullRequestURL == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, branch, and Pull Request", *issueNumber)}
	}
	if current.Lease == nil || current.LeaseGeneration == 0 || current.Lease.Owner.RunID != current.RunID ||
		current.Lease.Owner.Generation != current.LeaseGeneration ||
		(!legacyCompatibility && len(current.Lease.DeclaredResources) == 0) || len(current.Lease.ResolvedResources) == 0 {
		return exitError{4, fmt.Errorf("Issue #%d does not retain its fenced resource lease", *issueNumber)}
	}
	if (!legacyCompatibility && (current.ConflictRecovery != nil || current.BlockedCause != nil)) || current.PublicationRecovery != nil || current.PublicationFailure != nil {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible manual, worker, security, publication, or conflict recovery state", *issueNumber)}
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
		return fmt.Errorf("inspect checks recovery worktree: %w", err)
	}
	if !pullRequestChecksWorktreeMatches(current, inspection) {
		return exitError{4, state.RecoveryPredicateError{Code: state.RecoveryCodeWorktreeRemote, Err: fmt.Errorf("saved worktree/branch must be clean and aligned with its pushed remote head")}}
	}

	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect failed Issue checks recovery state: %w", err)
	}
	pr, err := validatePullRequestChecksRecovery(cfg, current, remote, inspection, true)
	if err != nil {
		return exitError{4, err}
	}
	if pr.HeadSHA == failureRecord.HeadSHA {
		return exitError{4, fmt.Errorf("Pull Request head has not changed since checks retry exhaustion")}
	}
	if pr.ChecksStatus != "pending" && pr.ChecksStatus != "success" && pr.ChecksStatus != "failure" {
		return exitError{4, fmt.Errorf("Pull Request returned unknown checks status %q", pr.ChecksStatus)}
	}
	if !pullRequestReplacementChecksAllowed(failureRecord, pr, legacyCompatibility) {
		return exitError{4, fmt.Errorf("legacy Pull Request checks recovery requires pending or successful replacement checks")}
	}
	now := time.Now().UTC()
	generation := 1
	if current.PullRequestChecksRecovery != nil {
		generation = current.PullRequestChecksRecovery.Generation + 1
		if current.PullRequestChecksRecovery.NewHeadSHA == pr.HeadSHA && current.PullRequestChecksRecovery.ChecksStatus == pr.ChecksStatus && current.PullRequestChecksRecovery.Status == issuedomain.PullRequestChecksRecoveryStatusChecksFailed {
			return a.output(*jsonOut, pullRequestChecksRecoveryOutput(current, true))
		}
	}
	recovery := &state.PullRequestChecksRecovery{
		ID: state.NewID("checks_recovery"), Generation: generation, ConfirmedAt: now,
		PreviousReason: current.LastError, OldHeadSHA: failureRecord.HeadSHA, NewHeadSHA: pr.HeadSHA, ChecksStatus: pr.ChecksStatus,
	}
	if pr.ChecksStatus == "failure" {
		recovery.Status = issuedomain.PullRequestChecksRecoveryStatusChecksFailed
		_, err = store.Update("pull_request_checks_recovery_observed", *issueNumber, current.RunID, map[string]any{
			"recovery_id": recovery.ID, "generation": generation, "old_head_sha": recovery.OldHeadSHA,
			"new_head_sha": recovery.NewHeadSHA, "checks_status": recovery.ChecksStatus, "resumed": false,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(*issueNumber)]
			if item == nil || !reflect.DeepEqual(item, current) {
				return fmt.Errorf("Issue #%d changed while checks recovery was being recorded", *issueNumber)
			}
			item.PullRequestChecksRecovery = recovery
			item.UpdatedAt = now
			return nil
		})
		if err != nil {
			return err
		}
		return a.output(*jsonOut, pullRequestChecksRecoveryOutputFrom(recovery, current, false))
	}

	recovery.Status = issuedomain.PullRequestChecksRecoveryStatusRequested
	recoveryTransition, err := issuedomain.RequestChecksRecovery(current.Status)
	if err != nil {
		return exitError{4, err}
	}
	_, err = store.Update("pull_request_checks_recovery_requested", *issueNumber, current.RunID, map[string]any{
		"recovery_id": recovery.ID, "generation": generation, "old_head_sha": recovery.OldHeadSHA,
		"new_head_sha": recovery.NewHeadSHA, "checks_status": recovery.ChecksStatus, "operator_confirmed": true,
		"legacy_compatibility": legacyCompatibility,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while checks recovery was being prepared", *issueNumber)
		}
		if err := state.ApplyIssueTransition(item, recoveryTransition); err != nil {
			return err
		}
		item.GitHubSync = issuedomain.GitHubSyncPullRequestChecksRecovery
		item.PullRequestChecksRecovery = recovery
		if legacyCompatibility {
			reconstructed := *failureRecord
			item.PullRequestChecksFailure = &reconstructed
		}
		item.HeadSHA = pr.HeadSHA
		item.RetryAfter = nil
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
	if err := syncPullRequestChecksRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
		return err
	}
	current, err = issueFromStore(store, *issueNumber)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, pullRequestChecksRecoveryOutput(current, false))
}

func validatePullRequestChecksRecovery(cfg config.Config, current *state.Issue, remote gh.RemoteState, inspection worktree.Inspection, requireFailedLabel bool) (gh.PullRequest, error) {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("Issue #%d is closed", current.Number)}
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	failed := labels[strings.ToLower(cfg.GitHub.FailedLabel)]
	running := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	if requireFailedLabel && (!failed || running) {
		return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("Issue #%d GitHub labels do not represent a synchronized failed state", current.Number)}
	}
	if !requireFailedLabel && failed == running {
		return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("Issue #%d GitHub labels are neither failed nor an idempotent running transition", current.Number)}
	}
	for _, label := range append(append([]string{cfg.GitHub.DoneLabel, cfg.GitHub.NeedsInputLabel}, cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if labels[strings.ToLower(label)] {
			return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("Issue #%d has manual or security exclusion label %q", current.Number, label)}
		}
	}
	if len(remote.PullRequests) != 1 {
		return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("Issue #%d must have exactly one saved Pull Request", current.Number)}
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL || pr.Number != current.PullRequestNumber || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil ||
		pr.HeadRefName != current.Branch || pr.BaseRefName != cfg.Git.BaseBranch || !strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) || pr.HeadSHA == "" ||
		pr.HeadSHA != inspection.Head || pr.HeadSHA != inspection.RemoteHead {
		return gh.PullRequest{}, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubIdentity, Err: fmt.Errorf("saved Pull Request, branch, worktree, and remote head do not match")}
	}
	return pr, nil
}

func pullRequestChecksWorktreeMatches(current *state.Issue, inspection worktree.Inspection) bool {
	return current != nil && inspection.Exists && inspection.Valid && inspection.Branch == current.Branch && inspection.LocalBranchExists &&
		inspection.RemoteBranchExists && inspection.Head != "" && inspection.RemoteHead != "" && inspection.Head == inspection.RemoteHead &&
		inspection.RemoteConsistent && !inspection.Dirty && !inspection.UnpushedCommits
}

func syncPullRequestChecksRecovery(ctx context.Context, store state.Store, cfg config.Config, ghPath string, current *state.Issue) error {
	if current == nil || current.PullRequestChecksRecovery == nil {
		return fmt.Errorf("Pull Request checks recovery metadata is missing")
	}
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, current.Number, current.Branch)
	if err != nil {
		return fmt.Errorf("reinspect Pull Request checks recovery before synchronization: %w", err)
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: saved Pull Request count changed")
	}
	pr := remote.PullRequests[0]
	labels := lowerLabelSet(remote.Issue.Labels)
	failed := labels[strings.ToLower(cfg.GitHub.FailedLabel)]
	running := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	if !strings.EqualFold(remote.Issue.State, "open") || failed == running ||
		pr.URL != current.PullRequestURL || pr.Number != current.PullRequestNumber || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil ||
		pr.HeadRefName != current.Branch || pr.BaseRefName != cfg.Git.BaseBranch || !strings.EqualFold(pr.HeadRepository, cfg.GitHub.Repo) || pr.HeadSHA != current.PullRequestChecksRecovery.NewHeadSHA ||
		(pr.ChecksStatus != "pending" && pr.ChecksStatus != "success") {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: authoritative Issue, Pull Request, head, or checks changed")
	}
	for _, label := range append(append([]string{cfg.GitHub.DoneLabel, cfg.GitHub.NeedsInputLabel}, cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("refuse Pull Request checks recovery synchronization: label %q excludes recovery", label)
		}
	}
	if err := client.MarkPullRequestChecksRecovery(ctx, cfg, current.Number, current.PullRequestChecksRecovery.ID); err != nil {
		return fmt.Errorf("sync Pull Request checks recovery to GitHub (durable recovery remains pending): %w", err)
	}
	checksDecision, err := issuedomain.AwaitChecks(current.Status)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = store.Update("pull_request_checks_recovery_resumed", current.Number, current.RunID, map[string]any{
		"recovery_id": current.PullRequestChecksRecovery.ID, "generation": current.PullRequestChecksRecovery.Generation,
		"old_head_sha": current.PullRequestChecksRecovery.OldHeadSHA, "new_head_sha": pr.HeadSHA, "checks_status": pr.ChecksStatus,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item != nil && item.GitHubSync == issuedomain.GitHubSyncPullRequestChecksRecovery && item.PullRequestChecksRecovery != nil && item.PullRequestChecksRecovery.ID == current.PullRequestChecksRecovery.ID {
			if err := state.ApplyIssueTransition(item, checksDecision.Transition); err != nil {
				return err
			}
			item.GitHubSync = issuedomain.GitHubSyncNone
			item.FailureKind = ""
			item.LastError = ""
			item.HeadSHA = pr.HeadSHA
			item.PullRequestChecksRecovery.Status = issuedomain.PullRequestChecksRecoveryStatusResumed
			item.PullRequestChecksRecovery.ChecksStatus = pr.ChecksStatus
			item.RetryAfter = &now
			item.UpdatedAt = now
		}
		return nil
	})
	return err
}

func pullRequestChecksRecoveryOutput(current *state.Issue, idempotent bool) map[string]any {
	return pullRequestChecksRecoveryOutputFrom(current.PullRequestChecksRecovery, current, idempotent)
}

func pullRequestChecksRecoveryOutputFrom(recovery *state.PullRequestChecksRecovery, current *state.Issue, idempotent bool) map[string]any {
	result := map[string]any{
		"issue": current.Number, "status": current.Status, "branch": current.Branch,
		"pull_request_url": current.PullRequestURL, "idempotent": idempotent,
		"worker_attempts": current.Attempts, "worker_continuations": current.Continuations,
	}
	if recovery != nil {
		result["recovery_id"] = recovery.ID
		result["generation"] = recovery.Generation
		result["recovery_status"] = recovery.Status
		result["old_head_sha"] = recovery.OldHeadSHA
		result["new_head_sha"] = recovery.NewHeadSHA
		result["checks_status"] = recovery.ChecksStatus
	}
	return result
}

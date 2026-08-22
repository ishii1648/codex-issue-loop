package app

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

func (a App) recoverPublication(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-publication", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "failed Issue number")
	confirmed := fs.Bool("confirm-prerequisite-resolved", false, "confirm the external publication prerequisite is resolved")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*confirmed {
		return exitError{2, fmt.Errorf("--confirm-prerequisite-resolved is required")}
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
	idempotentStatus := current.Status == "publication_recovery_pending" || current.Status == "awaiting_checks" || current.Status == "awaiting_merge" || current.Status == "completed"
	if current.PublicationRecovery != nil && current.PublicationRecovery.ID != "" && idempotentStatus {
		if current.GitHubSync == "publication_recovery" {
			if err := syncPublicationRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
				return err
			}
		}
		return a.output(*jsonOut, publicationRecoveryOutput(current, true))
	}
	if current.Status != "failed" || current.GitHubSync != "" {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and failed before publication recovery (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
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
	compatibleWorkerBlock := current.BlockedCause == nil || current.BlockedCause.Origin == "worker" && current.BlockedCause.Kind == "environment" && current.BlockedCause.Resumable
	if current.ConflictRecovery != nil || !compatibleWorkerBlock {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible worker, environment, or Pull Request recovery state", *issueNumber)}
	}
	if !state.ValidID(current.RunID, "run_") || current.Worktree == "" || current.Branch == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, and branch", *issueNumber)}
	}
	if len(current.DeclaredResources) == 0 {
		return exitError{4, fmt.Errorf("Issue #%d has no durable declared resource metadata", *issueNumber)}
	}
	legacy := eligibleLegacyBaseFailure(current, cfg.Queue.MaxAttempts)
	if !legacy && !eligibleTypedBaseFailure(current.PublicationFailure) {
		return exitError{4, fmt.Errorf("Issue #%d does not have recoverable typed pre-publication failure provenance", *issueNumber)}
	}
	if current.FailureKind != "issue" || current.PublicationAudit == nil || current.PublicationAudit.BaseSHA != "" {
		return exitError{4, fmt.Errorf("Issue #%d publication failure provenance and audit are inconsistent", *issueNumber)}
	}
	if current.Lease != nil && current.Lease.BaseSHA != "" {
		return exitError{4, fmt.Errorf("Issue #%d already has a base SHA inconsistent with the recorded missing-base failure", *issueNumber)}
	}
	if current.PublicationRecovery != nil && current.PublicationRecovery.Attempts >= current.PublicationRecovery.MaxAttempts {
		return exitError{4, fmt.Errorf("Issue #%d publication recovery budget is exhausted", *issueNumber)}
	}

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect publication recovery worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || inspection.Head == "" {
		return exitError{4, fmt.Errorf("saved worktree/branch is not consistent enough for publication recovery: %+v", inspection)}
	}
	if inspection.RemoteBranchExists {
		if inspection.RemoteHead == "" || !inspection.RemoteConsistent {
			return exitError{4, fmt.Errorf("saved local and remote branch histories are inconsistent for publication recovery")}
		}
	}
	if !inspection.Dirty && !inspection.UnpushedCommits {
		return exitError{4, fmt.Errorf("saved worktree has no dirty changes or unpushed commits to recover")}
	}
	baseSHA, err := verifiedPublicationBaseSHA(ctx, entry.Commands["git"], cfg, current, inspection, "publication recovery")
	if err != nil {
		return exitError{4, err}
	}
	result, resultBytes, err := worker.LoadLatestCompletedResult(filepath.Join(store.Dir, "runs", current.RunID))
	if err != nil {
		return exitError{4, fmt.Errorf("load saved completed worker result: %w", err)}
	}
	resultDigest := fmt.Sprintf("%x", sha256.Sum256(resultBytes))
	worktreeDigest, err := manager.ContentDigest(ctx, current.Worktree)
	if err != nil {
		return exitError{4, fmt.Errorf("fingerprint saved publication worktree: %w", err)}
	}

	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect failed Issue publication state: %w", err)
	}
	if !strings.EqualFold(remote.Issue.State, "open") {
		return exitError{4, fmt.Errorf("Issue #%d is closed without a completed publication", *issueNumber)}
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	if !labels[strings.ToLower(cfg.GitHub.FailedLabel)] || labels[strings.ToLower(cfg.GitHub.RunningLabel)] || labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return exitError{4, fmt.Errorf("Issue #%d GitHub labels do not represent a synchronized failed state", *issueNumber)}
	}
	for _, label := range cfg.GitHub.ReadyLabels {
		if labels[strings.ToLower(label)] {
			return exitError{4, fmt.Errorf("Issue #%d has conflicting ready label %q", *issueNumber, label)}
		}
	}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if labels[strings.ToLower(label)] {
			return exitError{4, fmt.Errorf("Issue #%d has manual exclusion label %q", *issueNumber, label)}
		}
	}
	if err := validateRecoveryPullRequests(current, remote, inspection, cfg.Git.BaseBranch); err != nil {
		return exitError{4, err}
	}

	now := time.Now().UTC()
	recoveryID := state.NewID("publication_recovery")
	maxAttempts := cfg.Queue.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	generation := 1
	priorAttempts := 0
	var priorHistory []state.PublicationRecoveryAttempt
	if current.PublicationRecovery != nil {
		generation = current.PublicationRecovery.Generation + 1
		priorAttempts = current.PublicationRecovery.Attempts
		priorHistory = append(priorHistory, current.PublicationRecovery.History...)
		if current.PublicationRecovery.MaxAttempts > 0 {
			maxAttempts = current.PublicationRecovery.MaxAttempts
		}
	}
	previousReason := current.LastError
	recoveryTransition, err := issuedomain.RequestPublicationRecovery(current.Status)
	if err != nil {
		return exitError{4, err}
	}
	recoveryDeclared := append([]string(nil), current.DeclaredResources...)
	var recoveryResolved, recoveryActual []string
	if current.Lease != nil {
		recoveryDeclared = append([]string(nil), current.Lease.DeclaredResources...)
		recoveryResolved = append([]string(nil), current.Lease.ResolvedResources...)
		recoveryActual = append([]string(nil), current.Lease.ActualResources...)
	} else if legacy {
		if len(recoveryDeclared) != 1 || recoveryDeclared[0] != state.RepositoryResource {
			return exitError{4, fmt.Errorf("legacy publication failure does not retain a conservative repository resource claim")}
		}
		recoveryResolved = []string{state.RepositoryResource}
	} else {
		failureResources := current.PublicationFailure
		if failureResources == nil || len(failureResources.ResolvedResources) == 0 || !reflect.DeepEqual(failureResources.DeclaredResources, recoveryDeclared) {
			return exitError{4, fmt.Errorf("typed publication failure does not retain consistent resource metadata")}
		}
		recoveryResolved = append([]string(nil), failureResources.ResolvedResources...)
		recoveryActual = append([]string(nil), failureResources.ActualResources...)
	}
	_, err = store.Update("publication_recovery_requested", *issueNumber, current.RunID, map[string]any{
		"recovery_id": recoveryID, "generation": generation, "base_sha": baseSHA,
		"result_sha256": resultDigest, "worktree_sha256": worktreeDigest, "previous_reason": previousReason, "legacy_failure": legacy,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while publication recovery was being prepared", *issueNumber)
		}
		if item.Lease == nil {
			item.LeaseGeneration++
			item.Lease = &state.ResourceLease{
				Owner: state.LeaseOwner{RunID: item.RunID, Generation: item.LeaseGeneration}, Slot: 0,
				DeclaredResources: append([]string(nil), recoveryDeclared...),
				ResolvedResources: append([]string(nil), recoveryResolved...),
				ActualResources:   append([]string(nil), recoveryActual...),
				BaseSHA:           baseSHA, ReservedAt: now,
			}
		} else if item.Lease.BaseSHA == "" {
			item.Lease.BaseSHA = baseSHA
		}
		if legacy {
			provenance := publication.ClassifyFailure(publication.DurableBaseMissingError{}, now)
			provenance.Reason = item.LastError
			provenance.DeclaredResources = append([]string(nil), recoveryDeclared...)
			provenance.ResolvedResources = append([]string(nil), recoveryResolved...)
			provenance.ActualResources = append([]string(nil), recoveryActual...)
			item.PublicationFailure = &provenance
		}
		if err := state.ApplyIssueTransition(item, recoveryTransition); err != nil {
			return err
		}
		item.GitHubSync = "publication_recovery"
		item.RetryAfter = nil
		item.PublicationRecovery = &state.PublicationRecovery{
			ID: recoveryID, Status: "requested", Generation: generation, Attempts: priorAttempts,
			MaxAttempts: maxAttempts, History: priorHistory, ConfirmedAt: now,
			PreviousReason: previousReason, ResultSHA256: resultDigest, Summary: result.Summary,
			ExpectedHeadSHA: inspection.Head, WorktreeSHA256: worktreeDigest,
			OriginalDirty: inspection.Dirty, OriginalUnpushed: inspection.UnpushedCommits,
		}
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
	if err := syncPublicationRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
		return err
	}
	current, err = issueFromStore(store, *issueNumber)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, publicationRecoveryOutput(current, false))
}

func eligibleTypedBaseFailure(value *publication.FailureProvenance) bool {
	return value != nil && value.Origin == publication.FailureOriginPublisher && value.Phase == publication.FailurePhasePrePublication &&
		value.Code == publication.FailureCodeDurableBaseMissing && value.Recoverable
}

func eligibleLegacyBaseFailure(issue *state.Issue, maxAttempts int) bool {
	return issue.PublicationFailure == nil && issue.PublicationAudit != nil && issue.PublicationAudit.BaseSHA == "" &&
		issue.FailureKind == "issue" && issue.Attempts >= maxAttempts &&
		strings.Contains(issue.LastError, "publish completed work: inspect publish changes: durable base SHA is missing")
}

func validateRecoveryPullRequests(current *state.Issue, remote gh.RemoteState, inspection worktree.Inspection, baseBranch string) error {
	if current.PullRequestMerged {
		return fmt.Errorf("Issue #%d records an already merged Pull Request", current.Number)
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) != 0 {
			return fmt.Errorf("Issue #%d has a Pull Request not recorded in durable state", current.Number)
		}
		return nil
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("Issue #%d saved Pull Request count is inconsistent with GitHub", current.Number)
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRefName != current.Branch || pr.BaseRefName != baseBranch || !inspection.RemoteBranchExists {
		return fmt.Errorf("Issue #%d saved Pull Request is not an open, matching publication target", current.Number)
	}
	return nil
}

func lowerLabelSet(labels []string) map[string]bool {
	result := make(map[string]bool, len(labels))
	for _, label := range labels {
		result[strings.ToLower(label)] = true
	}
	return result
}

func syncPublicationRecovery(ctx context.Context, store state.Store, cfg config.Config, ghPath string, current *state.Issue) error {
	if current == nil || current.PublicationRecovery == nil {
		return fmt.Errorf("publication recovery metadata is missing")
	}
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, current.Number, current.Branch)
	if err != nil {
		return fmt.Errorf("reinspect publication recovery GitHub state before synchronization: %w", err)
	}
	if err := validatePublicationRecoverySyncState(cfg, current, remote); err != nil {
		return fmt.Errorf("refuse publication recovery GitHub synchronization: %w", err)
	}
	if err := client.MarkPublicationRecovery(ctx, cfg, current.Number, current.PublicationRecovery.ID); err != nil {
		return fmt.Errorf("sync publication recovery to GitHub (durable recovery remains pending): %w", err)
	}
	_, err = store.Update("github_state_synced", current.Number, current.RunID, map[string]string{
		"state": "publication_recovery", "recovery_id": current.PublicationRecovery.ID,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item != nil && item.GitHubSync == "publication_recovery" && item.PublicationRecovery != nil && item.PublicationRecovery.ID == current.PublicationRecovery.ID {
			item.GitHubSync = ""
			item.PublicationRecovery.Status = "github_synced"
			item.UpdatedAt = time.Now().UTC()
		}
		return nil
	})
	return err
}

func validatePublicationRecoverySyncState(cfg config.Config, current *state.Issue, remote gh.RemoteState) error {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("GitHub Issue is no longer open")
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	failed := labels[strings.ToLower(cfg.GitHub.FailedLabel)]
	running := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	if failed == running || labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return fmt.Errorf("GitHub labels are neither synchronized failed nor an idempotent running transition")
	}
	for _, label := range append(append([]string(nil), cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("GitHub label %q excludes publication recovery", label)
		}
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) != 0 {
			return fmt.Errorf("a Pull Request appeared during publication recovery synchronization")
		}
		return nil
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("saved Pull Request count changed during publication recovery synchronization")
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRefName != current.Branch || pr.BaseRefName != cfg.Git.BaseBranch {
		return fmt.Errorf("saved Pull Request changed during publication recovery synchronization")
	}
	return nil
}

func issueFromStore(store state.Store, number int) (*state.Issue, error) {
	snapshot, err := store.Load()
	if err != nil {
		return nil, err
	}
	issue := snapshot.Issues[strconv.Itoa(number)]
	if issue == nil {
		return nil, fmt.Errorf("Issue #%d disappeared from durable state", number)
	}
	return issue, nil
}

func publicationRecoveryOutput(current *state.Issue, idempotent bool) map[string]any {
	baseSHA := ""
	if current.Lease != nil {
		baseSHA = current.Lease.BaseSHA
	}
	result := map[string]any{
		"issue": current.Number, "status": current.Status, "branch": current.Branch,
		"worktree": current.Worktree, "base_sha": baseSHA, "idempotent": idempotent,
	}
	if current.PublicationRecovery != nil {
		result["recovery_id"] = current.PublicationRecovery.ID
		result["generation"] = current.PublicationRecovery.Generation
		result["publication_attempts"] = current.PublicationRecovery.Attempts
	}
	return result
}

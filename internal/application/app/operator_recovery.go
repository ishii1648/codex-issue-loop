package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"github.com/ishii1648/codex-issue-loop/internal/domain/capability"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func (a App) retryConflict(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("retry", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "blocked Issue number")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
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
	if current.CapabilityRequirements != nil {
		capabilityEvaluation := capability.EvaluateRequirement(current.CapabilityRequirements, cfg.WorkerCapabilityProfiles())
		if !capabilityEvaluation.Compatible {
			data, _ := json.Marshal(capabilityEvaluation.Mismatches)
			return exitError{4, fmt.Errorf("Issue #%d capability mismatch: %s", *issueNumber, data)}
		}
	}
	if current.Status != issuedomain.StatusBlocked || current.GitHubSync != issuedomain.GitHubSyncNone {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and blocked before retry (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	reason := strings.ToLower(current.LastError)
	if current.ConflictRecovery == nil && !strings.Contains(reason, "merge conflict") && !strings.Contains(reason, "conflict recovery") {
		return exitError{4, fmt.Errorf("Issue #%d was not blocked by a Pull Request conflict", *issueNumber)}
	}
	if current.Worktree == "" || current.Branch == "" || current.PullRequestURL == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain the worktree, branch, and Pull Request required for retry", *issueNumber)}
	}
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect retry worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
		return exitError{4, fmt.Errorf("saved worktree/branch is not consistent enough to retry: %+v", inspection)}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect retry Pull Request: %w", err)
	}
	matched := false
	for _, pr := range remote.PullRequests {
		if pr.URL == current.PullRequestURL && strings.EqualFold(pr.State, "open") && pr.HeadRefName == current.Branch {
			matched = true
			break
		}
	}
	if !matched {
		return exitError{4, fmt.Errorf("saved Pull Request is not open on branch %s: %s", current.Branch, current.PullRequestURL)}
	}
	retryTransition, err := issuedomain.RetryConflict(current.Status)
	if err != nil {
		return exitError{4, err}
	}
	retryID := state.NewID("retry")
	_, err = store.Update("conflict_recovery_retry_requested", *issueNumber, current.RunID, map[string]any{
		"retry_id": retryID, "pull_request_url": current.PullRequestURL, "previous_reason": current.LastError,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item.Status != issuedomain.StatusBlocked || item.GitHubSync != issuedomain.GitHubSyncNone {
			return fmt.Errorf("Issue #%d changed while retry was being prepared", *issueNumber)
		}
		if item.ConflictRecovery == nil {
			item.ConflictRecovery = &state.ConflictRecovery{
				PullRequestURL: item.PullRequestURL, StartedAt: time.Now().UTC(),
			}
		}
		if err := state.ApplyIssueTransition(item, retryTransition); err != nil {
			return err
		}
		item.GitHubSync = issuedomain.GitHubSyncConflictRetry
		item.FailureKind = ""
		item.LastError = "explicit conflict recovery retry requested"
		item.RetryAfter = nil
		item.ConflictRecovery.Attempts = 0
		item.ConflictRecovery.BaseUpdates = 0
		item.ConflictRecovery.RetryID = retryID
		item.ConflictRecovery.LastReason = "explicit retry " + retryID
		item.ConflictRecovery.UpdatedAt = time.Now().UTC()
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	if err := client.MarkConflictRetry(ctx, cfg, *issueNumber, retryID); err != nil {
		return fmt.Errorf("sync explicit conflict retry to GitHub (durable retry remains pending): %w", err)
	}
	_, err = store.Update("github_state_synced", *issueNumber, current.RunID, map[string]string{"state": "conflict_retry", "retry_id": retryID}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item.GitHubSync == issuedomain.GitHubSyncConflictRetry {
			item.GitHubSync = issuedomain.GitHubSyncNone
		}
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	return a.output(*jsonOut, map[string]any{
		"issue": *issueNumber, "status": "resolving_conflict", "retry_id": retryID,
		"branch": current.Branch, "pull_request_url": current.PullRequestURL,
	})
}

func (a App) resumeBlocked(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("resume-blocked", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "environment-blocked Issue number")
	confirmed := fs.Bool("confirm-prerequisite-resolved", false, "confirm the external environment prerequisite is resolved")
	dryRun := fs.Bool("dry-run", false, "evaluate all safe recovery predicates without mutation")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*confirmed && !*dryRun {
		return exitError{2, fmt.Errorf("--confirm-prerequisite-resolved is required")}
	}
	if *dryRun {
		return a.explainResumeBlocked(ctx, l, *repo, *issueNumber, *jsonOut)
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
	if current.CapabilityRequirements != nil {
		capabilityEvaluation := capability.EvaluateRequirement(current.CapabilityRequirements, cfg.WorkerCapabilityProfiles())
		if !capabilityEvaluation.Compatible {
			data, _ := json.Marshal(capabilityEvaluation.Mismatches)
			return exitError{4, fmt.Errorf("Issue #%d capability mismatch: %s", *issueNumber, data)}
		}
	}
	progress := environmentResumeProgress(current)
	pendingResume := progress == recoveryProgressPendingSync
	idempotentResume := progress == recoveryProgressIdempotent
	if current.EnvironmentResume != nil && current.EnvironmentResume.ID != "" && current.Status != issuedomain.StatusBlocked {
		if current.Status != issuedomain.StatusEnvironmentResumePending {
			return exitError{4, fmt.Errorf("Issue #%d is not waiting for an environment resume (status=%s)", *issueNumber, current.Status)}
		}
		if current.Lease == nil || current.RunID == "" || current.Worktree == "" || current.Branch == "" {
			return exitError{4, fmt.Errorf("Issue #%d pending environment resume does not retain a consistent run, worktree, branch, and resource lease", *issueNumber)}
		}
		if progress != recoveryProgressPendingSync && progress != recoveryProgressIdempotent {
			return exitError{4, fmt.Errorf("Issue #%d pending environment resume has inconsistent durable synchronization state", *issueNumber)}
		}
	}
	if progress == recoveryProgressInvalid {
		return exitError{4, fmt.Errorf("Issue #%d must be fully synchronized and blocked before environment resume (status=%s github_sync=%s)", *issueNumber, current.Status, current.GitHubSync)}
	}
	interruptedResume := progress == recoveryProgressInterrupted
	interruptedWorkspaceRecovery := false
	var interruptedWorkspaceEvidence *state.InterruptedWorkspaceResumeEvidence
	if state.MayHaveInterruptedWorkspaceResumeEvidence(current) {
		interruptedWorkspaceEvidence, err = store.InterruptedWorkspaceResumeEvidence(*current)
		if err != nil {
			return exitError{4, fmt.Errorf("verify interrupted missing-workspace environment resume: %w; state was not changed", err)}
		}
		interruptedWorkspaceRecovery = true
		// A running EnvironmentResume is only an interrupted intent after the
		// exact v0.6.14 durable chain above has established that authority.
		interruptedResume = true
	}
	resumeIntent := interruptedResume || pendingResume || idempotentResume
	var legacyCause *state.BlockedCause
	var legacyRecovery *state.LegacyWorkerBlockRecovery
	if state.MayHaveLegacyWorkerBlockRecoveryProvenance(current) {
		legacyCause, _ = store.LegacyWorkerBlockProvenance(*current)
		if current.Lease == nil {
			legacyRecovery, _ = store.LegacyWorkerBlockRecoveryEvidence(*current)
		}
	}
	legacyWorkerBlock := legacyCause != nil
	if !legacyWorkerBlock && !interruptedWorkspaceRecovery && (current.BlockedCause == nil || current.BlockedCause.Origin != "worker" || current.BlockedCause.Kind != "environment" || !current.BlockedCause.Resumable) {
		return exitError{4, fmt.Errorf("Issue #%d is not a resumable worker environment block", *issueNumber)}
	}
	if current.ConflictRecovery != nil {
		return exitError{4, fmt.Errorf("Issue #%d is a Pull Request conflict block; use agent-loop retry", *issueNumber)}
	}
	parkedClaim := current.ResourcePark != nil && current.ResourcePark.Status == issuedomain.ResourceParkStatusParked && current.ResourcePark.ID != ""
	if current.RunID == "" || current.Worktree == "" || current.Branch == "" || (current.Lease == nil && !parkedClaim && legacyRecovery == nil && !interruptedResume) {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a consistent run, worktree, branch, and resource lease", *issueNumber)}
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
			return exitError{4, fmt.Errorf("Issue #%d has a pending manual answer request and is not an environment-blocked resume candidate", *issueNumber)}
		}
	}
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect environment resume worktree: %w", err)
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists {
		return exitError{4, fmt.Errorf("saved worktree/branch is not consistent enough to resume: %+v", inspection)}
	}
	if interruptedWorkspaceRecovery && interruptedWorkspaceEvidence.WorktreeHead != "" && inspection.Head != interruptedWorkspaceEvidence.WorktreeHead {
		return exitError{4, state.RecoveryPredicateError{Code: state.RecoveryCodeWorktreeHeadRemote, Err: fmt.Errorf("Issue #%d interrupted environment resume worktree HEAD changed; state was not changed", *issueNumber)}}
	}
	if interruptedWorkspaceRecovery && interruptedWorkspaceEvidence.WorktreeHead != "" &&
		!interruptedWorkspaceInspectionMatches(interruptedWorkspaceEvidence.WorktreeHead, inspection) {
		return exitError{4, state.RecoveryPredicateError{Code: state.RecoveryCodeWorktreeHeadRemote, Err: fmt.Errorf("Issue #%d interrupted environment resume worktree is not the exact dirty local-only branch; state was not changed", *issueNumber)}}
	}
	launchValidator := a.validateResumeWorkspace
	if launchValidator == nil {
		launchValidator = func(ctx context.Context, manager worktree.Manager, cfg config.Config, path, branch string) (worktree.LaunchValidation, error) {
			return manager.ValidateLaunch(ctx, cfg, path, branch)
		}
	}
	launch, err := launchValidator(ctx, manager, cfg, current.Worktree, current.Branch)
	if err != nil || !launch.Valid {
		if err == nil {
			err = fmt.Errorf("worktree validator did not establish a valid launch boundary")
		}
		return exitError{4, fmt.Errorf("saved workspace provenance recovery validation failed: %w", err)}
	}
	workspaceBackfill := current.Workspace == nil
	if workspaceBackfill && (pendingResume || idempotentResume) {
		return exitError{4, fmt.Errorf("Issue #%d has an ambiguous in-progress environment resume with missing workspace provenance", *issueNumber)}
	}
	if current.Workspace != nil && !current.Workspace.Matches(launch.CanonicalCWD, launch.Branch, entry.RepoID, cfg.GitHub.Repo,
		cfg.GitHub.RepositoryID, launch.CommonDir, launch.MainCheckout) {
		return exitError{4, fmt.Errorf("saved workspace provenance does not match the validated launch target")}
	}
	if idempotentResume {
		baseSHA := current.Lease.BaseSHA
		output := map[string]any{
			"issue": *issueNumber, "status": current.Status, "resume_id": current.EnvironmentResume.ID,
			"branch": current.Branch, "worktree": current.Worktree, "base_sha": baseSHA,
			"current_base_sha": current.EnvironmentResume.CurrentBaseSHA, "idempotent": true,
			"workspace_provenance": current.Workspace,
		}
		if current.ResourcePark != nil {
			output["resource_park_id"] = current.ResourcePark.ID
			output["lease_owner"] = current.Lease.Owner
		}
		return a.output(*jsonOut, output)
	}
	if interruptedResume && current.Lease == nil && current.EnvironmentResume.BaseSHA == "" {
		baseSHA, recoverErr := store.EnvironmentResumeBaseSHA(*issueNumber, current.RunID, current.EnvironmentResume.ID)
		if recoverErr != nil {
			return exitError{4, fmt.Errorf("recover publication base SHA for interrupted environment resume: %w; state was not changed", recoverErr)}
		}
		current.EnvironmentResume.BaseSHA = baseSHA
	}
	publicationState := current
	if current.Lease == nil && parkedClaim {
		copy := *current
		lease := current.ResourcePark.OriginalLease
		copy.Lease = &lease
		publicationState = &copy
	} else if current.Lease == nil && legacyRecovery != nil {
		copy := *current
		copy.Lease = &state.ResourceLease{BaseSHA: legacyRecovery.BaseSHA}
		publicationState = &copy
	}
	baseSHA, err := environmentResumeBaseSHA(ctx, entry.Commands["git"], cfg, publicationState, inspection)
	if err != nil {
		return exitError{4, err}
	}
	currentBaseSHA, err := currentRemoteBaseSHA(ctx, entry.Commands["git"], current.Worktree, cfg.Git.BaseBranch)
	if err != nil {
		return exitError{4, err}
	}
	if interruptedWorkspaceRecovery && (baseSHA != interruptedWorkspaceEvidence.BaseSHA || currentBaseSHA != interruptedWorkspaceEvidence.CurrentBaseSHA) {
		return exitError{4, state.RecoveryPredicateError{Code: state.RecoveryCodeBaseSHAIdentity, Err: fmt.Errorf("Issue #%d interrupted environment resume base SHA provenance changed; state was not changed", *issueNumber)}}
	}
	client := gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect environment-blocked Issue: %w", err)
	}
	if !strings.EqualFold(remote.Issue.State, "open") {
		return exitError{4, fmt.Errorf("Issue #%d is not open", *issueNumber)}
	}
	labels := map[string]bool{}
	for _, label := range remote.Issue.Labels {
		labels[strings.ToLower(label)] = true
	}
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = strings.ToLower(label)
			continue
		}
		if labels[strings.ToLower(label)] {
			return exitError{4, fmt.Errorf("Issue #%d has manual exclusion label %q", *issueNumber, label)}
		}
	}
	if blockedLabel == "" {
		return exitError{4, fmt.Errorf("GitHub exclude labels do not define the supervisor-owned blocked label")}
	}
	runningLabel := labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	blocked := labels[blockedLabel]
	if interruptedWorkspaceRecovery {
		if !interruptedWorkspaceRemoteMarkersMatch(cfg, current, remote) {
			return exitError{4, state.RecoveryPredicateError{Code: state.RecoveryCodeGitHubCommentMarkers, Err: fmt.Errorf("Issue #%d GitHub state does not prove the interrupted missing-workspace resume", *issueNumber)}}
		}
	} else if resumeIntent {
		switch current.EnvironmentResume.Status {
		case issuedomain.EnvironmentResumeStatusRequested:
			if blocked == runningLabel {
				return exitError{4, fmt.Errorf("Issue #%d interrupted environment resume has ambiguous GitHub blocked/running labels", *issueNumber)}
			}
		case issuedomain.EnvironmentResumeStatusGitHubSynced:
			marker := "<!-- codex-issue-loop:environment-resume:" + current.EnvironmentResume.ID + " -->"
			commented := false
			for _, comment := range remote.Issue.Comments {
				commented = commented || strings.Contains(comment, marker)
			}
			if blocked || !runningLabel || !commented {
				return exitError{4, fmt.Errorf("Issue #%d GitHub state no longer matches the synchronized environment resume", *issueNumber)}
			}
		}
	} else if !blocked || runningLabel {
		return exitError{4, fmt.Errorf("Issue #%d does not have only the supervisor-owned blocked label", *issueNumber)}
	}
	if labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.FailedLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return exitError{4, fmt.Errorf("Issue #%d has an incompatible terminal or needs-input label", *issueNumber)}
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) != 0 {
			return exitError{4, fmt.Errorf("Issue #%d has a Pull Request not recorded in durable state", *issueNumber)}
		}
	} else {
		matched := len(remote.PullRequests) == 1 && remote.PullRequests[0].URL == current.PullRequestURL &&
			strings.EqualFold(remote.PullRequests[0].State, "open") && remote.PullRequests[0].HeadRefName == current.Branch
		if !matched {
			return exitError{4, fmt.Errorf("Issue #%d saved Pull Request is inconsistent with GitHub", *issueNumber)}
		}
	}

	resumeID := state.NewID("resume")
	if resumeIntent {
		resumeID = current.EnvironmentResume.ID
	}
	now := time.Now().UTC()
	validatedWorkspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch,
		RepoID: entry.RepoID, Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout, CapturedAt: now,
	}
	previousReason := current.LastError
	if interruptedWorkspaceRecovery {
		previousReason = interruptedWorkspaceEvidence.PreviousReason
	} else if current.BlockedCause != nil && current.BlockedCause.Reason != "" {
		previousReason = current.BlockedCause.Reason
	} else if legacyCause != nil {
		previousReason = legacyCause.Reason
	}
	if !pendingResume {
		resumeTransition, transitionErr := issuedomain.RequestEnvironmentResume(current.Status)
		if transitionErr != nil {
			return exitError{4, transitionErr}
		}
		eventType := "environment_resume_requested"
		if interruptedResume {
			eventType = "environment_resume_recovered"
		}
		resumePayload := map[string]any{
			"resume_id": resumeID, "previous_reason": previousReason, "resource_park_id": resourceParkID(current), "parked_lease_reacquired": parkedClaim,
			"legacy_worker_block":    legacyWorkerBlock || (interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered),
			"legacy_lease_recovered": legacyRecovery != nil || (interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered),
			"interrupted_resume":     interruptedResume,
			"base_sha":               baseSHA, "current_base_sha": currentBaseSHA,
			"interrupted_workspace_recovery": interruptedWorkspaceRecovery,
			"interrupted_blocked_cause":      current.BlockedCause,
			"workspace_recovery": map[string]any{
				"old_provenance_missing": workspaceBackfill,
				"operator_confirmation":  map[string]any{"confirm_prerequisite_resolved": true},
				"expected": map[string]any{
					"path": current.Worktree, "branch": current.Branch, "repo_id": entry.RepoID,
					"repository": cfg.GitHub.Repo, "repository_id": cfg.GitHub.RepositoryID,
					"git_common_dir": launch.CommonDir, "main_checkout": launch.MainCheckout,
				},
				"actual": map[string]any{
					"path": launch.CanonicalCWD, "branch": launch.Branch, "repo_id": entry.RepoID,
					"repository": cfg.GitHub.Repo, "repository_id": cfg.GitHub.RepositoryID,
					"git_common_dir": launch.CommonDir, "main_checkout": launch.MainCheckout,
					"validation": launch,
				},
			},
		}
		_, err = store.Update(eventType, *issueNumber, current.RunID, resumePayload, func(s *state.Snapshot) error {
			if s.StateRevision != snapshot.StateRevision {
				return fmt.Errorf("Issue #%d durable state changed while environment resume was being prepared", *issueNumber)
			}
			item := s.Issues[strconv.Itoa(*issueNumber)]
			if item == nil || item.RunID != current.RunID || item.Status != issuedomain.StatusBlocked || item.GitHubSync != issuedomain.GitHubSyncNone ||
				(interruptedResume && (item.EnvironmentResume == nil || item.EnvironmentResume.ID != resumeID || item.EnvironmentResume.Status != current.EnvironmentResume.Status)) {
				return fmt.Errorf("Issue #%d changed while environment resume was being prepared", *issueNumber)
			}
			if item.Worktree != current.Worktree || item.Branch != current.Branch || item.PullRequestURL != current.PullRequestURL ||
				!reflect.DeepEqual(item.Lease, current.Lease) || !reflect.DeepEqual(item.Workspace, current.Workspace) {
				return fmt.Errorf("Issue #%d worktree, branch, Pull Request, resource lease, or workspace provenance changed while environment resume was being prepared", *issueNumber)
			}
			transactionPGID := item.WorkerPGID
			if transactionPGID <= 1 {
				transactionPGID = item.WorkerPID
			}
			if !reflect.DeepEqual(item.BlockedCause, current.BlockedCause) || item.WorkerPID != current.WorkerPID || item.WorkerPGID != current.WorkerPGID ||
				controller.Alive(item.WorkerPID) || controller.GroupAlive(transactionPGID) {
				return fmt.Errorf("Issue #%d block provenance or worker process changed while environment resume was being prepared", *issueNumber)
			}
			for _, request := range s.PendingRequests {
				if request != nil && request.IssueNumber == *issueNumber && request.Status == issuedomain.RequestStatusPending {
					return fmt.Errorf("Issue #%d gained a pending manual answer request while environment resume was being prepared", *issueNumber)
				}
			}
			if item.Lease == nil && legacyRecovery != nil {
				if legacyRecovery.BaseSHA != baseSHA || !reflect.DeepEqual(&legacyRecovery.Cause, legacyCause) ||
					(item.BlockedCause != nil && !reflect.DeepEqual(item.BlockedCause, &legacyRecovery.Cause)) {
					return fmt.Errorf("Issue #%d legacy recovery evidence changed while environment resume was being prepared", *issueNumber)
				}
			}
			if item.Lease == nil && (legacyRecovery != nil || interruptedResume) {
				for _, other := range s.Issues {
					if other != nil && other.Number != item.Number && other.Lease != nil {
						return fmt.Errorf("Issue #%d cannot recover repo:* while Issue #%d retains a resource lease", *issueNumber, other.Number)
					}
				}
			}
			if interruptedWorkspaceEvidence != nil && interruptedWorkspaceEvidence.LegacyLeaseRecovered {
				if item.Lease == nil || item.Lease.Owner != interruptedWorkspaceEvidence.LeaseOwner ||
					item.Lease.Slot != interruptedWorkspaceEvidence.LeaseSlot || item.LeaseGeneration != interruptedWorkspaceEvidence.LeaseOwner.Generation {
					return fmt.Errorf("Issue #%d v0.6.14 recovered lease changed while workspace recovery was being prepared", *issueNumber)
				}
				for _, other := range s.Issues {
					if other == nil || other.Number == item.Number || other.Lease == nil {
						continue
					}
					if other.Status.OccupiesWorkerSlot() && other.Lease.Slot == item.Lease.Slot {
						return fmt.Errorf("Issue #%d cannot fence recovered slot %d while Issue #%d occupies it", *issueNumber, item.Lease.Slot, other.Number)
					}
					if state.ResourcesConflict(item.Lease.ResolvedResources, other.Lease.ResolvedResources) {
						return fmt.Errorf("Issue #%d cannot fence recovered resources while Issue #%d retains a conflicting lease", *issueNumber, other.Number)
					}
				}
				owner, transferErr := state.TransferIssueLease(item, interruptedWorkspaceEvidence.LeaseOwner, item.RunID)
				if transferErr != nil {
					return fmt.Errorf("Issue #%d cannot fence v0.6.14 recovered lease: %w", *issueNumber, transferErr)
				}
				resumePayload["lease_owner"] = owner
				resumePayload["lease_slot"] = item.Lease.Slot
				resumePayload["previous_lease_owner"] = interruptedWorkspaceEvidence.LeaseOwner
			}
			if parkedClaim {
				if item.ResourcePark == nil || current.ResourcePark == nil || !reflect.DeepEqual(item.ResourcePark, current.ResourcePark) {
					return fmt.Errorf("Issue #%d parked resource claim changed while environment resume was being prepared", *issueNumber)
				}
				slot, ok := availableLeaseSlot(s, cfg.Queue.Concurrency, item.ResourcePark.OriginalLease.Slot, item.Number)
				if !ok {
					return fmt.Errorf("Issue #%d parked resource claim is waiting for an available worker slot", *issueNumber)
				}
				owner, reserveErr := state.ResumeParkedLease(s, item.Number, item.ResourcePark.ID, slot, now)
				if reserveErr != nil {
					return fmt.Errorf("Issue #%d cannot reacquire parked resource claim: %w", *issueNumber, reserveErr)
				}
				if item.Lease.BaseSHA == "" {
					item.Lease.BaseSHA = baseSHA
				}
				if item.ResourcePark.ResumeOwner == nil || *item.ResourcePark.ResumeOwner != owner {
					return fmt.Errorf("Issue #%d parked resource owner was not fenced", *issueNumber)
				}
				resumePayload["lease_owner"] = owner
				resumePayload["lease_slot"] = item.Lease.Slot
			}
			if workspaceBackfill {
				if item.Workspace != nil {
					return fmt.Errorf("Issue #%d workspace provenance changed while environment resume was being prepared", *issueNumber)
				}
				workspace := validatedWorkspace
				item.Workspace = &workspace
			}
			if err := state.ApplyIssueTransition(item, resumeTransition); err != nil {
				return err
			}
			item.GitHubSync = issuedomain.GitHubSyncEnvironmentResume
			if interruptedWorkspaceRecovery {
				blockedAt := item.EnvironmentResume.ConfirmedAt
				if item.ResourcePark != nil && !item.ResourcePark.ParkedAt.IsZero() {
					blockedAt = item.ResourcePark.ParkedAt
				}
				item.BlockedCause = &state.BlockedCause{
					Origin: "worker", Kind: "environment", Resumable: true,
					Reason: interruptedWorkspaceEvidence.PreviousReason, BlockedAt: blockedAt,
				}
				item.LastError = interruptedWorkspaceEvidence.PreviousReason
			}
			if item.BlockedCause == nil && legacyWorkerBlock {
				cause := *legacyCause
				item.BlockedCause = &cause
			}
			if item.Lease == nil && (legacyRecovery != nil || interruptedResume) {
				item.LeaseGeneration++
				owner := state.LeaseOwner{RunID: item.RunID, Generation: item.LeaseGeneration}
				item.Lease = &state.ResourceLease{
					Owner: owner, Slot: 0, DeclaredResources: []string{},
					ResolvedResources: []string{state.RepositoryResource}, BaseSHA: baseSHA, ReservedAt: now,
				}
			} else if item.Lease != nil && item.Lease.BaseSHA == "" {
				item.Lease.BaseSHA = baseSHA
			}
			if interruptedResume {
				item.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusRequested
				item.EnvironmentResume.BaseSHA = baseSHA
				item.EnvironmentResume.CurrentBaseSHA = currentBaseSHA
			} else {
				item.EnvironmentResume = &state.EnvironmentResume{ID: resumeID, Status: issuedomain.EnvironmentResumeStatusRequested, ConfirmedAt: now, PreviousReason: previousReason, BaseSHA: baseSHA, CurrentBaseSHA: currentBaseSHA}
			}
			item.RetryAfter = nil
			item.UpdatedAt = now
			return nil
		})
		if err != nil {
			return err
		}
	}
	if err := client.MarkEnvironmentResume(ctx, cfg, *issueNumber, resumeID); err != nil {
		return fmt.Errorf("sync environment resume to GitHub (durable resume remains pending): %w", err)
	}
	updated, err := store.Update("github_state_synced", *issueNumber, current.RunID, map[string]string{"state": "environment_resume", "resume_id": resumeID}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(*issueNumber)]
		if item != nil && item.GitHubSync == issuedomain.GitHubSyncEnvironmentResume && item.EnvironmentResume != nil && item.EnvironmentResume.ID == resumeID {
			item.GitHubSync = issuedomain.GitHubSyncNone
			item.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusGitHubSynced
			item.UpdatedAt = time.Now().UTC()
		}
		return nil
	})
	if err != nil {
		return err
	}
	status := issuedomain.StatusEnvironmentResumePending
	var leaseOwner *state.LeaseOwner
	parkID := resourceParkID(current)
	if item := updated.Issues[strconv.Itoa(*issueNumber)]; item != nil {
		status = item.Status
		if item.Lease != nil {
			owner := item.Lease.Owner
			leaseOwner = &owner
		}
		if item.ResourcePark != nil {
			parkID = item.ResourcePark.ID
		}
	}
	output := map[string]any{
		"issue": *issueNumber, "status": status, "resume_id": resumeID,
		"branch": current.Branch, "worktree": current.Worktree, "session_id": current.SessionID,
		"base_sha": baseSHA, "current_base_sha": currentBaseSHA,
		"dirty": inspection.Dirty, "unpushed_commits": inspection.UnpushedCommits,
		"workspace_provenance_backfilled": workspaceBackfill,
	}
	if leaseOwner != nil {
		output["lease_owner"] = leaseOwner
	}
	if parkID != "" {
		output["resource_park_id"] = parkID
	}
	return a.output(*jsonOut, output)
}

func exactFailureComment(comment string, issueNumber int, reason string) bool {
	return comment == failureComment(issueNumber, reason)
}

func failureComment(issueNumber int, reason string) string {
	return fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->\n%s\nAutomation stopped: %s", issueNumber, failureIDMarker(reason), reason)
}

func failureIDMarker(reason string) string {
	digest := sha256.Sum256([]byte(reason))
	return fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", digest[:8])
}

func resourceParkID(issue *state.Issue) string {
	if issue == nil || issue.ResourcePark == nil {
		return ""
	}
	return issue.ResourcePark.ID
}

func availableLeaseSlot(snapshot *state.Snapshot, limit, preferred, issueNumber int) (int, bool) {
	if limit < 1 {
		limit = 1
	}
	used := map[int]bool{}
	for _, other := range snapshot.Issues {
		if other == nil || other.Number == issueNumber || other.Lease == nil || !other.Status.OccupiesWorkerSlot() {
			continue
		}
		used[other.Lease.Slot] = true
	}
	if preferred >= 0 && preferred < limit && !used[preferred] {
		return preferred, true
	}
	for slot := 0; slot < limit; slot++ {
		if !used[slot] {
			return slot, true
		}
	}
	return -1, false
}

func environmentResumeBaseSHA(ctx context.Context, git string, cfg config.Config, current *state.Issue, inspection worktree.Inspection) (string, error) {
	return verifiedPublicationBaseSHA(ctx, git, cfg, current, inspection, "environment resume")
}

func currentRemoteBaseSHA(ctx context.Context, git, worktreePath, baseBranch string) (string, error) {
	if git == "" {
		git = "git"
	}
	ref := "refs/heads/" + baseBranch
	out, err := exec.CommandContext(ctx, git, "-C", worktreePath, "ls-remote", "--exit-code", "origin", ref).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("inspect current configured base branch %q for environment resume: %w: %s; state was not changed", baseBranch, err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 || fields[1] != ref || !canonicalGitObjectID(fields[0]) {
		return "", fmt.Errorf("inspect current configured base branch %q for environment resume: git returned an invalid ref; state was not changed", baseBranch)
	}
	return fields[0], nil
}

func canonicalGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func verifiedPublicationBaseSHA(ctx context.Context, git string, cfg config.Config, current *state.Issue, inspection worktree.Inspection, operation string) (string, error) {
	if git == "" {
		git = "git"
	}
	baseSHA := ""
	if current.Lease != nil {
		baseSHA = current.Lease.BaseSHA
		if baseSHA != strings.TrimSpace(baseSHA) {
			return "", fmt.Errorf("saved publication base SHA is not canonical; state was not changed")
		}
	}
	if baseSHA == "" && current.EnvironmentResume != nil {
		baseSHA = current.EnvironmentResume.BaseSHA
		if baseSHA != strings.TrimSpace(baseSHA) {
			return "", fmt.Errorf("saved environment resume base SHA is not canonical; state was not changed")
		}
	}
	if baseSHA == "" {
		ref := "refs/remotes/origin/" + cfg.Git.BaseBranch
		out, err := exec.CommandContext(ctx, git, "-C", current.Worktree, "rev-parse", "--verify", ref+"^{commit}").Output()
		if err != nil {
			return "", fmt.Errorf("resolve configured base branch %q for %s: %w; fetch origin/%s and retry without editing durable state", cfg.Git.BaseBranch, operation, err, cfg.Git.BaseBranch)
		}
		baseSHA = strings.TrimSpace(string(out))
		if baseSHA == "" {
			return "", fmt.Errorf("resolve configured base branch %q for %s: git returned an empty commit SHA; state was not changed", cfg.Git.BaseBranch, operation)
		}
	}
	verified, err := exec.CommandContext(ctx, git, "-C", current.Worktree, "rev-parse", "--verify", baseSHA+"^{commit}").Output()
	if err != nil {
		return "", fmt.Errorf("verify publication base SHA %q for %s: %w; state was not changed", baseSHA, operation, err)
	}
	if strings.TrimSpace(string(verified)) != baseSHA {
		return "", fmt.Errorf("verify publication base SHA %q for %s: value is not a full canonical commit SHA; state was not changed", baseSHA, operation)
	}
	if inspection.Head == "" {
		return "", fmt.Errorf("verify publication base SHA for %s: saved worktree HEAD is empty; state was not changed", operation)
	}
	if err := exec.CommandContext(ctx, git, "-C", current.Worktree, "merge-base", "--is-ancestor", baseSHA, inspection.Head).Run(); err != nil {
		return "", fmt.Errorf("verify publication base SHA %s for %s: it is not an ancestor of worktree HEAD %s; state was not changed", baseSHA, operation, inspection.Head)
	}
	return baseSHA, nil
}

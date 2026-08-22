package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/supervisor"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type answeredWorkspacePlan struct {
	Issue                int                       `json:"issue"`
	Eligible             bool                      `json:"eligible"`
	ConfirmationRequired bool                      `json:"confirmation_required"`
	RunID                string                    `json:"run_id"`
	RequestID            string                    `json:"request_id"`
	ResourceParkID       string                    `json:"resource_park_id"`
	Branch               string                    `json:"branch"`
	Worktree             string                    `json:"worktree"`
	BaseSHA              string                    `json:"base_sha"`
	HeadSHA              string                    `json:"head_sha"`
	WorktreeSHA256       string                    `json:"worktree_sha256"`
	OldOwner             state.LeaseOwner          `json:"old_owner"`
	NewOwner             state.LeaseOwner          `json:"new_owner"`
	ExpectedWorkspace    state.WorkerWorkspace     `json:"expected_workspace"`
	ActualWorkspace      state.WorkerWorkspace     `json:"actual_workspace"`
	Validation           worktree.LaunchValidation `json:"validation"`
	VerifiedProvenance   bool                      `json:"verified_provenance_recovery"`
}

func (a App) recoverAnsweredWorkspace(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-answered-workspace", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "blocked Issue number")
	confirmed := fs.Bool("confirm-exact-chain", false, "confirm recovery of the fully validated exact legacy chain")
	dryRun := fs.Bool("dry-run", false, "preview all recovery predicates without mutation")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	if fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))}
	}
	if *issueNumber <= 0 {
		return exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	if !*dryRun && !*confirmed {
		return exitError{2, fmt.Errorf("--confirm-exact-chain is required")}
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
	if current.AnsweredWorkspaceRecovery != nil {
		if err := validateExistingAnsweredWorkspaceRecovery(snapshot, current); err != nil {
			return exitError{4, err}
		}
		if *dryRun {
			return a.output(*jsonOut, answeredWorkspaceRecoveryOutput(current, true))
		}
		if current.GitHubSync == "answered_workspace_recovery" {
			if err := syncAnsweredWorkspaceRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
				return err
			}
			current, err = issueFromStore(store, *issueNumber)
			if err != nil {
				return err
			}
		}
		return a.output(*jsonOut, answeredWorkspaceRecoveryOutput(current, true))
	}

	request, err := exactAnsweredRequest(snapshot, current)
	if err != nil {
		return exitError{4, err}
	}
	evidence, err := store.AnsweredWorkspaceRecoveryEvidence(*current, *request)
	if err != nil {
		return exitError{4, fmt.Errorf("verify exact answered missing-workspace chain: %w; state was not changed", err)}
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

	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
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
		return exitError{4, fmt.Errorf("answered workspace recovery launch validation failed: %w; state was not changed", err)}
	}
	if launch.CanonicalCWD != evidence.RejectedLaunch.CanonicalCWD || launch.TopLevel != evidence.RejectedLaunch.TopLevel ||
		launch.Branch != evidence.RejectedLaunch.Branch || launch.CommonDir != evidence.RejectedLaunch.CommonDir ||
		launch.MainCheckout != evidence.RejectedLaunch.MainCheckout {
		return exitError{4, fmt.Errorf("validated workspace identity changed since the missing-Workspace rejection; state was not changed")}
	}
	if evidence.VerifiedLaunch != nil && !reflect.DeepEqual(launch, *evidence.VerifiedLaunch) {
		return exitError{4, fmt.Errorf("validated workspace identity or checks changed since verified provenance recovery; state was not changed")}
	}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil || !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || inspection.Head == "" {
		return exitError{4, fmt.Errorf("saved worktree/branch/HEAD is not consistent enough for answered workspace recovery: %+v", inspection)}
	}
	if !validCommitID(current.Lease.BaseSHA) {
		return exitError{4, fmt.Errorf("saved base SHA is malformed; state was not changed")}
	}
	gitPath := entry.Commands["git"]
	if gitPath == "" {
		gitPath = "git"
	}
	if out, verifyErr := exec.CommandContext(ctx, gitPath, "-C", current.Worktree, "cat-file", "-e", current.Lease.BaseSHA+"^{commit}").CombinedOutput(); verifyErr != nil {
		return exitError{4, fmt.Errorf("saved base SHA is not a commit in the retained repository: %w: %s", verifyErr, strings.TrimSpace(string(out)))}
	}
	digest, err := manager.ContentDigest(ctx, current.Worktree)
	if err != nil {
		return fmt.Errorf("fingerprint answered workspace recovery worktree: %w", err)
	}
	remote, err := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, *issueNumber, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect blocked answered workspace Issue: %w", err)
	}
	if err := validateAnsweredWorkspaceRemote(cfg, current, request, remote, ""); err != nil {
		return exitError{4, err}
	}

	validatedWorkspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch, RepoID: entry.RepoID,
		Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout,
	}
	workspace := validatedWorkspace
	if current.WorkspaceRecovery != nil {
		if !validExistingWorkspaceRecovery(current, digest, inspection.Head) || current.Workspace == nil ||
			!current.Workspace.Matches(validatedWorkspace.Path, validatedWorkspace.Branch, validatedWorkspace.RepoID,
				validatedWorkspace.Repository, validatedWorkspace.RepositoryID, validatedWorkspace.GitCommonDir, validatedWorkspace.MainCheckout) {
			return exitError{4, fmt.Errorf("verified workspace provenance no longer matches HEAD, content, repository, or validator evidence; state was not changed")}
		}
		workspace = *current.Workspace
	}
	plan := answeredWorkspacePlan{
		Issue: current.Number, Eligible: true, ConfirmationRequired: true, RunID: current.RunID,
		RequestID: request.ID, ResourceParkID: current.ResourcePark.ID, Branch: current.Branch, Worktree: current.Worktree,
		BaseSHA: current.Lease.BaseSHA, HeadSHA: inspection.Head, WorktreeSHA256: digest,
		OldOwner: current.Lease.Owner, NewOwner: state.LeaseOwner{RunID: current.RunID, Generation: current.LeaseGeneration + 1},
		ExpectedWorkspace: workspace, ActualWorkspace: workspace, Validation: launch,
		VerifiedProvenance: current.WorkspaceRecovery != nil,
	}
	if *dryRun {
		return a.output(*jsonOut, plan)
	}

	now := time.Now().UTC()
	recoveryID := state.NewID("answered_workspace_recovery")
	answerDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(request.Answer)))
	if current.WorkspaceRecovery == nil {
		workspace.CapturedAt = now
		plan.ExpectedWorkspace.CapturedAt = now
		plan.ActualWorkspace.CapturedAt = now
	}
	resumeTransition, err := issuedomain.RecoverAnsweredWorkspace(current.Status)
	if err != nil {
		return exitError{4, err}
	}
	payload := map[string]any{
		"recovery_id": recoveryID, "operator_confirmation": map[string]bool{"confirm_exact_chain": true},
		"old_provenance_missing": true, "request_id": request.ID, "resource_park_id": current.ResourcePark.ID,
		"answer_sha256": answerDigest, "base_sha": current.Lease.BaseSHA, "head_sha": inspection.Head,
		"worktree_sha256": digest, "expected_workspace": plan.ExpectedWorkspace, "actual_workspace": plan.ActualWorkspace,
		"validator": launch, "old_owner": plan.OldOwner, "new_owner": plan.NewOwner,
	}
	_, err = store.Update("answered_workspace_recovery_requested", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
		if s.StateRevision != snapshot.StateRevision {
			return fmt.Errorf("Issue #%d durable state changed while answered workspace recovery was being prepared", current.Number)
		}
		item := s.Issues[strconv.Itoa(current.Number)]
		candidate := s.PendingRequests[request.ID]
		if item == nil || candidate == nil || !reflect.DeepEqual(item, current) || !reflect.DeepEqual(candidate, request) {
			return fmt.Errorf("Issue #%d or its answered request changed while recovery was being prepared", current.Number)
		}
		transactionPGID := item.WorkerPGID
		if transactionPGID <= 1 {
			transactionPGID = item.WorkerPID
		}
		if controller.Alive(item.WorkerPID) || controller.GroupAlive(transactionPGID) {
			return fmt.Errorf("Issue #%d gained an active worker process while recovery was being prepared", current.Number)
		}
		for _, pending := range s.PendingRequests {
			if pending != nil && pending.IssueNumber == current.Number && pending.Status == "pending" {
				return fmt.Errorf("Issue #%d gained a pending manual request while recovery was being prepared", current.Number)
			}
		}
		newOwner, transferErr := state.TransferIssueLease(item, plan.OldOwner, item.RunID)
		if transferErr != nil || newOwner != plan.NewOwner {
			return fmt.Errorf("fence answered workspace recovery lease: owner=%+v: %w", newOwner, transferErr)
		}
		item.Workspace = &workspace
		item.AnsweredWorkspaceRecovery = &state.AnsweredWorkspaceRecovery{
			ID: recoveryID, Status: "requested", ConfirmedAt: now, OperatorConfirmed: true, OldProvenanceMissing: true,
			RequestID: request.ID, ResourceParkID: current.ResourcePark.ID, AnswerSHA256: answerDigest,
			HeadSHA: inspection.Head, WorktreeSHA256: digest, ExpectedWorkspace: plan.ExpectedWorkspace,
			ActualWorkspace: plan.ActualWorkspace, ValidatorChecks: cloneBoolMap(launch.Checks),
			OldOwner: plan.OldOwner, NewOwner: newOwner,
		}
		if err := state.ApplyIssueTransition(item, resumeTransition); err != nil {
			return err
		}
		item.GitHubSync = "answered_workspace_recovery"
		item.RetryAfter = nil
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		latestSnapshot, loadErr := store.Load()
		latest := latestSnapshot.Issues[strconv.Itoa(current.Number)]
		if loadErr == nil && latest != nil && latest.AnsweredWorkspaceRecovery != nil &&
			latest.AnsweredWorkspaceRecovery.RequestID == request.ID && latest.AnsweredWorkspaceRecovery.ResourceParkID == current.ResourcePark.ID &&
			latest.AnsweredWorkspaceRecovery.OldOwner == plan.OldOwner && latest.AnsweredWorkspaceRecovery.NewOwner == plan.NewOwner &&
			validateExistingAnsweredWorkspaceRecovery(latestSnapshot, latest) == nil {
			current = latest
		} else {
			return err
		}
	} else {
		current, err = issueFromStore(store, current.Number)
		if err != nil {
			return err
		}
	}
	if current.GitHubSync == "answered_workspace_recovery" {
		if err := syncAnsweredWorkspaceRecovery(ctx, store, cfg, entry.Commands["gh"], current); err != nil {
			return err
		}
		current, err = issueFromStore(store, current.Number)
		if err != nil {
			return err
		}
	}
	return a.output(*jsonOut, answeredWorkspaceRecoveryOutput(current, false))
}

func validateExistingAnsweredWorkspaceRecovery(snapshot state.Snapshot, issue *state.Issue) error {
	recovery := issue.AnsweredWorkspaceRecovery
	if recovery == nil || !state.ValidID(recovery.ID, "answered_workspace_recovery_") ||
		(recovery.Status != "requested" && recovery.Status != "github_synced") || !recovery.OperatorConfirmed || !recovery.OldProvenanceMissing ||
		recovery.RequestID == "" || recovery.ResourceParkID == "" || recovery.AnswerSHA256 == "" || recovery.HeadSHA == "" || recovery.WorktreeSHA256 == "" ||
		recovery.OldOwner.RunID != issue.RunID || recovery.OldOwner.Generation != 2 || recovery.NewOwner.RunID != issue.RunID || recovery.NewOwner.Generation != 3 ||
		issue.Workspace == nil || issue.Workspace.CapturedAt.IsZero() || recovery.ExpectedWorkspace.CapturedAt.IsZero() || recovery.ActualWorkspace.CapturedAt.IsZero() ||
		*issue.Workspace != recovery.ActualWorkspace || !recovery.ExpectedWorkspace.Matches(recovery.ActualWorkspace.Path, recovery.ActualWorkspace.Branch,
		recovery.ActualWorkspace.RepoID, recovery.ActualWorkspace.Repository, recovery.ActualWorkspace.RepositoryID,
		recovery.ActualWorkspace.GitCommonDir, recovery.ActualWorkspace.MainCheckout) {
		return fmt.Errorf("Issue #%d existing answered workspace recovery audit is inconsistent", issue.Number)
	}
	request := snapshot.PendingRequests[recovery.RequestID]
	if request == nil || request.Status != "answered" || request.ResourceParkID != recovery.ResourceParkID || request.RunID != issue.RunID ||
		fmt.Sprintf("%x", sha256.Sum256([]byte(request.Answer))) != recovery.AnswerSHA256 {
		return fmt.Errorf("Issue #%d existing answered workspace recovery request is inconsistent", issue.Number)
	}
	park := issue.ResourcePark
	if park == nil || park.ID != recovery.ResourceParkID || park.RequestID != recovery.RequestID || park.Status != "resumed" ||
		park.OriginalLease.Owner.Generation != 1 || park.ResumeOwner == nil || *park.ResumeOwner != recovery.OldOwner {
		return fmt.Errorf("Issue #%d existing answered workspace recovery park is inconsistent", issue.Number)
	}
	if issue.Lease != nil && issue.Lease.Owner.Generation == recovery.NewOwner.Generation && issue.Lease.Owner != recovery.NewOwner {
		return fmt.Errorf("Issue #%d existing answered workspace recovery lease fence is inconsistent", issue.Number)
	}
	if issue.GitHubSync == "answered_workspace_recovery" &&
		(issue.Status != issuedomain.StatusResumePending || recovery.Status != "requested" || issue.Lease == nil || issue.Lease.Owner != recovery.NewOwner ||
			issue.LeaseGeneration != recovery.NewOwner.Generation || issue.WorkerPID != 0 || issue.WorkerPGID != 0) {
		return fmt.Errorf("Issue #%d pending answered workspace recovery synchronization is inconsistent", issue.Number)
	}
	return nil
}

func exactAnsweredRequest(snapshot state.Snapshot, issue *state.Issue) (*state.Request, error) {
	if issue == nil || issue.ResourcePark == nil || issue.ResourcePark.RequestID == "" {
		return nil, fmt.Errorf("Issue does not retain a needs-input request/park")
	}
	request := snapshot.PendingRequests[issue.ResourcePark.RequestID]
	if request == nil {
		return nil, fmt.Errorf("Issue #%d answered request %s is missing", issue.Number, issue.ResourcePark.RequestID)
	}
	for _, candidate := range snapshot.PendingRequests {
		if candidate != nil && candidate.IssueNumber == issue.Number && candidate.Status == "pending" {
			return nil, fmt.Errorf("Issue #%d has a pending manual request", issue.Number)
		}
	}
	return request, nil
}

func validCommitID(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func cloneBoolMap(input map[string]bool) map[string]bool {
	output := make(map[string]bool, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func answeredWorkspaceRecoveryOutput(issue *state.Issue, idempotent bool) map[string]any {
	result := map[string]any{"issue": issue.Number, "status": issue.Status, "idempotent": idempotent}
	if recovery := issue.AnsweredWorkspaceRecovery; recovery != nil {
		result["recovery_id"] = recovery.ID
		result["recovery_status"] = recovery.Status
		result["request_id"] = recovery.RequestID
		result["resource_park_id"] = recovery.ResourceParkID
		result["old_owner"] = recovery.OldOwner
		result["new_owner"] = recovery.NewOwner
		result["workspace_provenance"] = issue.Workspace
	}
	return result
}

func syncAnsweredWorkspaceRecovery(ctx context.Context, store state.Store, cfg config.Config, ghPath string, issue *state.Issue) error {
	if issue == nil || issue.AnsweredWorkspaceRecovery == nil || issue.GitHubSync != "answered_workspace_recovery" {
		return fmt.Errorf("answered workspace recovery synchronization metadata is inconsistent")
	}
	client := gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}
	remote, err := client.Inspect(ctx, cfg, issue.Number, issue.Branch)
	if err != nil {
		return fmt.Errorf("inspect answered workspace recovery synchronization: %w", err)
	}
	request := &state.Request{ID: issue.AnsweredWorkspaceRecovery.RequestID}
	mode, err := answeredWorkspaceRemoteMode(cfg, issue, request, remote, issue.AnsweredWorkspaceRecovery.ID)
	if err != nil {
		return err
	}
	if mode == "blocked" {
		if err := client.MarkAnsweredWorkspaceRecovery(ctx, cfg, issue.Number, issue.AnsweredWorkspaceRecovery.ID); err != nil {
			return err
		}
	}
	_, err = store.Update("github_state_synced", issue.Number, issue.RunID, map[string]string{
		"state": "answered_workspace_recovery", "recovery_id": issue.AnsweredWorkspaceRecovery.ID,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil || item.GitHubSync != "answered_workspace_recovery" || item.AnsweredWorkspaceRecovery == nil ||
			item.AnsweredWorkspaceRecovery.ID != issue.AnsweredWorkspaceRecovery.ID {
			return fmt.Errorf("Issue #%d answered workspace recovery changed during GitHub synchronization", issue.Number)
		}
		item.GitHubSync = ""
		item.AnsweredWorkspaceRecovery.Status = "github_synced"
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		latest, loadErr := issueFromStore(store, issue.Number)
		if loadErr == nil && latest.GitHubSync == "" && latest.AnsweredWorkspaceRecovery != nil &&
			latest.AnsweredWorkspaceRecovery.ID == issue.AnsweredWorkspaceRecovery.ID && latest.AnsweredWorkspaceRecovery.Status == "github_synced" {
			return nil
		}
	}
	return err
}

func validateAnsweredWorkspaceRemote(cfg config.Config, issue *state.Issue, request *state.Request, remote gh.RemoteState, recoveryID string) error {
	_, err := answeredWorkspaceRemoteMode(cfg, issue, request, remote, recoveryID)
	return err
}

func answeredWorkspaceRemoteMode(cfg config.Config, issue *state.Issue, request *state.Request, remote gh.RemoteState, recoveryID string) (string, error) {
	if !strings.EqualFold(remote.Issue.State, "open") || len(remote.PullRequests) != 0 {
		return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_IDENTITY", Err: fmt.Errorf("Issue #%d is not the open no-Pull-Request boundary", issue.Number)}
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = strings.ToLower(label)
		} else if labels[strings.ToLower(label)] {
			return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_LABELS", Err: fmt.Errorf("Issue #%d has manual exclusion label %q", issue.Number, label)}
		}
	}
	if blockedLabel == "" {
		return "", fmt.Errorf("GitHub exclude labels do not define the supervisor-owned blocked label")
	}
	blocked, running := labels[blockedLabel], labels[strings.ToLower(cfg.GitHub.RunningLabel)]
	mode := "blocked"
	if recoveryID != "" && !blocked && running {
		mode = "running"
	} else if !blocked || running {
		return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_LABELS", Err: fmt.Errorf("Issue #%d blocked/running labels changed", issue.Number)}
	}
	for _, label := range append(append([]string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}, cfg.GitHub.ReadyLabels...), "") {
		if label != "" && labels[strings.ToLower(label)] {
			return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_LABELS", Err: fmt.Errorf("Issue #%d has incompatible label %q", issue.Number, label)}
		}
	}
	requestMarker := "<!-- codex-issue-loop:request:" + request.ID + " -->"
	failedMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", issue.Number)
	failureDigest := sha256.Sum256([]byte(issue.LastError))
	failureMarker := fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", failureDigest[:8])
	if countCommentMarker(remote.Issue.Comments, requestMarker) != 1 || countCommentMarker(remote.Issue.Comments, failedMarker) != 1 ||
		countCommentMarker(remote.Issue.Comments, failureMarker) != 1 {
		return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_COMMENT_MARKERS", Err: fmt.Errorf("Issue #%d request/blocked comment markers do not match the durable chain", issue.Number)}
	}
	if recoveryID != "" {
		recoveryMarker := "<!-- codex-issue-loop:answered-workspace-recovery:" + recoveryID + " -->"
		count := countCommentMarker(remote.Issue.Comments, recoveryMarker)
		if (mode == "running" && count != 1) || (mode == "blocked" && count != 0) {
			return "", state.RecoveryPredicateError{Code: "RECOVERY_GITHUB_COMMENT_MARKERS", Err: fmt.Errorf("Issue #%d recovery marker does not match its synchronization boundary", issue.Number)}
		}
	}
	return mode, nil
}

func countCommentMarker(comments []string, marker string) int {
	count := 0
	for _, comment := range comments {
		if strings.Contains(comment, marker) {
			count++
		}
	}
	return count
}

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

type workspaceRecoveryPlan struct {
	Issue                int                       `json:"issue"`
	Eligible             bool                      `json:"eligible"`
	ConfirmationRequired bool                      `json:"confirmation_required"`
	MutationScope        []string                  `json:"mutation_scope"`
	RunID                string                    `json:"run_id"`
	Status               string                    `json:"status"`
	Branch               string                    `json:"branch"`
	Worktree             string                    `json:"worktree"`
	HeadSHA              string                    `json:"head_sha"`
	WorktreeSHA256       string                    `json:"worktree_sha256"`
	PullRequestURL       string                    `json:"pull_request_url,omitempty"`
	ExpectedWorkspace    state.WorkerWorkspace     `json:"expected_workspace"`
	ActualWorkspace      state.WorkerWorkspace     `json:"actual_workspace"`
	Validation           worktree.LaunchValidation `json:"validation"`
	Idempotent           bool                      `json:"idempotent"`
}

// recoverWorkspace validates and backfills only immutable Workspace
// provenance. It is intentionally lifecycle-neutral: status, lease, resource
// park, session, retry accounting, and GitHub state are preserved byte-for-
// byte by the durable transaction.
func (a App) recoverWorkspace(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("recover-workspace", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	issueNumber := fs.Int("issue", 0, "blocked or failed legacy Issue number")
	confirmed := fs.Bool("confirm-verified-workspace", false, "confirm validation-only Workspace provenance recovery")
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
		return exitError{2, fmt.Errorf("--confirm-verified-workspace is required")}
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
	if current.Status != issuedomain.StatusBlocked && current.Status != issuedomain.StatusFailed {
		return exitError{4, fmt.Errorf("Issue #%d must be blocked or failed for validation-only workspace recovery (status=%s)", *issueNumber, current.Status)}
	}
	if current.Workspace == nil && current.WorkspaceRecovery != nil {
		return exitError{4, fmt.Errorf("Issue #%d has a workspace recovery audit without workspace provenance", *issueNumber)}
	}
	if current.GitHubSync != issuedomain.GitHubSyncNone || current.RunID == "" || current.Worktree == "" || current.Branch == "" {
		return exitError{4, fmt.Errorf("Issue #%d does not retain a fully synchronized run, worktree, and branch", *issueNumber)}
	}
	if request, requestErr := exactAnsweredRequest(snapshot, current); requestErr == nil {
		if _, evidenceErr := store.AnsweredWorkspaceRecoveryEvidence(*current, *request); evidenceErr == nil {
			command := fmt.Sprintf("agent-loop recover-answered-workspace --repo %q --issue %d --dry-run --json", entry.RepoPath, current.Number)
			if *dryRun {
				return a.output(*jsonOut, map[string]any{
					"issue": current.Number, "status": current.Status, "eligible": false,
					"lifecycle_candidate": "answered_missing_workspace", "recommended_command": command,
					"remediation": "use recover-answered-workspace before validation-only recover-workspace",
				})
			}
			return exitError{4, fmt.Errorf("Issue #%d is an answered missing-workspace lifecycle candidate; use %s before recover-workspace; state was not changed", current.Number, command)}
		}
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
		if request != nil && request.IssueNumber == current.Number && request.Status == issuedomain.RequestStatusPending {
			return exitError{4, fmt.Errorf("Issue #%d has a pending manual request", *issueNumber)}
		}
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
		return exitError{4, fmt.Errorf("workspace provenance launch validation failed: %w; state was not changed", err)}
	}
	inspection, err := manager.Inspect(ctx, cfg, current.Worktree, current.Branch)
	if err != nil || !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists ||
		inspection.Branch != current.Branch || inspection.Head == "" {
		return exitError{4, fmt.Errorf("saved worktree/branch/HEAD is not consistent enough for workspace recovery: %+v", inspection)}
	}
	digest, err := manager.ContentDigest(ctx, current.Worktree)
	if err != nil {
		return fmt.Errorf("fingerprint workspace recovery worktree: %w", err)
	}
	remote, err := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, current.Number, current.Branch)
	if err != nil {
		return fmt.Errorf("inspect workspace recovery Issue: %w", err)
	}
	if err := validateWorkspaceRecoveryRemote(cfg, current, inspection, remote); err != nil {
		return exitError{4, err}
	}

	expectedWorkspace := state.WorkerWorkspace{
		Path: current.Worktree, Branch: current.Branch, RepoID: entry.RepoID,
		Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout,
	}
	workspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch, RepoID: entry.RepoID,
		Repository: cfg.GitHub.Repo, RepositoryID: cfg.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout,
	}
	plan := workspaceRecoveryPlan{
		Issue: current.Number, Eligible: true, ConfirmationRequired: true,
		MutationScope: []string{"issues[].workspace", "issues[].workspace_provenance_recovery", "events.jsonl"},
		RunID:         current.RunID, Status: current.Status.String(), Branch: current.Branch, Worktree: current.Worktree,
		HeadSHA: inspection.Head, WorktreeSHA256: digest, PullRequestURL: current.PullRequestURL,
		ExpectedWorkspace: expectedWorkspace, ActualWorkspace: workspace, Validation: launch,
	}
	if current.Workspace != nil {
		if !current.Workspace.Matches(workspace.Path, workspace.Branch, workspace.RepoID, workspace.Repository,
			workspace.RepositoryID, workspace.GitCommonDir, workspace.MainCheckout) || current.Workspace.CapturedAt.IsZero() {
			return exitError{4, fmt.Errorf("Issue #%d already has inconsistent workspace provenance", current.Number)}
		}
		if current.WorkspaceRecovery != nil && !validExistingWorkspaceRecovery(current, digest, inspection.Head) {
			return exitError{4, fmt.Errorf("Issue #%d existing workspace recovery audit is inconsistent", current.Number)}
		}
		plan.ExpectedWorkspace = *current.Workspace
		plan.ActualWorkspace = *current.Workspace
		plan.Idempotent = true
		return a.output(*jsonOut, plan)
	}
	if *dryRun {
		return a.output(*jsonOut, plan)
	}

	now := time.Now().UTC()
	workspace.CapturedAt = now
	plan.ExpectedWorkspace.CapturedAt = now
	plan.ActualWorkspace.CapturedAt = now
	recoveryID := state.NewID("workspace_recovery")
	payload := map[string]any{
		"recovery_id":           recoveryID,
		"operator_confirmation": map[string]bool{"confirm_verified_workspace": true},
		"mutation_scope":        plan.MutationScope, "previous_status": current.Status,
		"old_provenance_missing": true, "head_sha": inspection.Head, "worktree_sha256": digest,
		"pull_request_url": current.PullRequestURL, "expected_workspace": plan.ExpectedWorkspace,
		"actual_workspace": plan.ActualWorkspace, "validator": launch,
	}
	_, err = store.Update("workspace_provenance_recovered", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
		if s.StateRevision != snapshot.StateRevision {
			return fmt.Errorf("Issue #%d durable state changed while workspace recovery was being prepared", current.Number)
		}
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || !reflect.DeepEqual(item, current) {
			return fmt.Errorf("Issue #%d changed while workspace recovery was being prepared", current.Number)
		}
		transactionPGID := item.WorkerPGID
		if transactionPGID <= 1 {
			transactionPGID = item.WorkerPID
		}
		if controller.Alive(item.WorkerPID) || controller.GroupAlive(transactionPGID) {
			return fmt.Errorf("Issue #%d gained an active worker process while workspace recovery was being prepared", current.Number)
		}
		for _, request := range s.PendingRequests {
			if request != nil && request.IssueNumber == current.Number && request.Status == issuedomain.RequestStatusPending {
				return fmt.Errorf("Issue #%d gained a pending manual request while workspace recovery was being prepared", current.Number)
			}
		}
		item.Workspace = &workspace
		item.WorkspaceRecovery = &state.WorkspaceProvenanceRecovery{
			ID: recoveryID, Status: issuedomain.WorkspaceProvenanceRecoveryStatusVerified, ConfirmedAt: now, OperatorConfirmed: true,
			OldProvenanceMissing: true, PreviousStatus: current.Status, RunID: current.RunID,
			HeadSHA: inspection.Head, WorktreeSHA256: digest,
			ExpectedWorkspace: plan.ExpectedWorkspace, ActualWorkspace: plan.ActualWorkspace,
			ValidatorChecks: cloneBoolMap(launch.Checks),
		}
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		latest, loadErr := store.Load()
		item := latest.Issues[strconv.Itoa(current.Number)]
		if loadErr == nil && item != nil && validExistingWorkspaceRecovery(item, digest, inspection.Head) {
			return a.output(*jsonOut, workspaceRecoveryOutput(item, true))
		}
		return err
	}
	recovered, err := issueFromStore(store, current.Number)
	if err != nil {
		return err
	}
	return a.output(*jsonOut, workspaceRecoveryOutput(recovered, false))
}

func validateWorkspaceRecoveryRemote(cfg config.Config, issue *state.Issue, inspection worktree.Inspection, remote gh.RemoteState) error {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("Issue #%d is not open", issue.Number)
	}
	labels := lowerLabelSet(remote.Issue.Labels)
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = strings.ToLower(label)
			continue
		}
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("Issue #%d has an unsupported exclusion label %q", issue.Number, label)
		}
	}
	if blockedLabel == "" || labels[strings.ToLower(cfg.GitHub.RunningLabel)] {
		return fmt.Errorf("Issue #%d does not have an unambiguous terminal lifecycle label", issue.Number)
	}
	blocked := labels[blockedLabel]
	failed := labels[strings.ToLower(cfg.GitHub.FailedLabel)]
	if (issue.Status == issuedomain.StatusBlocked && (!blocked || failed)) || (issue.Status == issuedomain.StatusFailed && (!failed || blocked)) {
		return fmt.Errorf("Issue #%d durable status and GitHub terminal label do not match", issue.Number)
	}
	for _, label := range append(append([]string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel}, cfg.GitHub.ReadyLabels...), "") {
		if label != "" && labels[strings.ToLower(label)] {
			return fmt.Errorf("Issue #%d has incompatible lifecycle label %q", issue.Number, label)
		}
	}
	if issue.PullRequestURL == "" {
		if len(remote.PullRequests) != 0 {
			return fmt.Errorf("Issue #%d has a Pull Request not recorded in durable state", issue.Number)
		}
		return nil
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("Issue #%d saved Pull Request count changed", issue.Number)
	}
	pr := remote.PullRequests[0]
	if pr.URL != issue.PullRequestURL || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRepository != cfg.GitHub.Repo ||
		pr.HeadRefName != issue.Branch || pr.BaseRefName != cfg.Git.BaseBranch || pr.HeadSHA == "" ||
		pr.HeadSHA != inspection.Head || !inspection.RemoteBranchExists || inspection.RemoteHead != pr.HeadSHA || !inspection.RemoteConsistent {
		return fmt.Errorf("Issue #%d saved Pull Request, branch, or worktree HEAD is inconsistent", issue.Number)
	}
	return nil
}

func validExistingWorkspaceRecovery(issue *state.Issue, digest, head string) bool {
	recovery := issue.WorkspaceRecovery
	return recovery != nil && state.ValidID(recovery.ID, "workspace_recovery_") && recovery.Status == issuedomain.WorkspaceProvenanceRecoveryStatusVerified &&
		recovery.OperatorConfirmed && recovery.OldProvenanceMissing && recovery.PreviousStatus == issue.Status &&
		recovery.RunID == issue.RunID && recovery.HeadSHA == head && recovery.WorktreeSHA256 == digest &&
		issue.Workspace != nil && !issue.Workspace.CapturedAt.IsZero() && *issue.Workspace == recovery.ActualWorkspace &&
		recovery.ExpectedWorkspace.Matches(recovery.ActualWorkspace.Path, recovery.ActualWorkspace.Branch,
			recovery.ActualWorkspace.RepoID, recovery.ActualWorkspace.Repository, recovery.ActualWorkspace.RepositoryID,
			recovery.ActualWorkspace.GitCommonDir, recovery.ActualWorkspace.MainCheckout)
}

func workspaceRecoveryOutput(issue *state.Issue, idempotent bool) map[string]any {
	result := map[string]any{"issue": issue.Number, "status": issue.Status, "idempotent": idempotent, "workspace_provenance": issue.Workspace}
	if recovery := issue.WorkspaceRecovery; recovery != nil {
		result["recovery_id"] = recovery.ID
		result["recovery_status"] = recovery.Status
		result["head_sha"] = recovery.HeadSHA
		result["worktree_sha256"] = recovery.WorktreeSHA256
		result["mutation_scope"] = []string{"issues[].workspace", "issues[].workspace_provenance_recovery", "events.jsonl"}
	}
	return result
}

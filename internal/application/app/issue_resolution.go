package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

type issueActionPlan struct {
	Action   issuedomain.ResolutionAction `json:"action"`
	Eligible bool                         `json:"eligible"`
	Reasons  []string                     `json:"reasons"`
}

type issuePlanReport struct {
	SchemaVersion int                           `json:"schema_version"`
	IssueNumber   int                           `json:"issue_number"`
	StateRevision uint64                        `json:"state_revision"`
	Status        issuedomain.Status            `json:"status"`
	Quarantine    *state.QuarantineRecord       `json:"quarantine,omitempty"`
	Suspension    *state.Suspension             `json:"suspension,omitempty"`
	Checkpoint    *state.ContinuationCheckpoint `json:"continuation_checkpoint,omitempty"`
	Observations  map[string]any                `json:"observations"`
	Actions       []issueActionPlan             `json:"actions"`
	ReadOnly      bool                          `json:"read_only"`
}

type issuePlanningContext struct {
	gitPath            string
	ghPath             string
	cfg                config.Config
	store              state.Store
	snapshot           state.Snapshot
	issue              *state.Issue
	quarantine         *state.QuarantineRecord
	remote             gh.RemoteState
	remoteErr          error
	launch             worktree.LaunchValidation
	launchErr          error
	inspection         worktree.Inspection
	inspectErr         error
	worktreeSHA256     string
	worktreeDigestErr  error
	baseOK             bool
	baseErr            error
	workerLive         bool
	pending            []string
	resultSummary      string
	resultSHA256       string
	resultErr          error
	adoption           worktreeAdoptionObservation
	adoptionErr        error
	adoptionAllowPaths []string
	report             issuePlanReport
}

func (a App) issuePlan(ctx context.Context, l layout.Layout, args []string) error {
	repo, number, allowPaths, jsonOut, err := a.parseIssuePlanArgs(args)
	if err != nil {
		return err
	}
	planned, err := a.buildIssuePlan(ctx, l, repo, number, allowPaths)
	if err != nil {
		return err
	}
	return a.output(jsonOut, planned.report)
}

func (a App) parseIssuePlanArgs(args []string) (string, int, []string, bool, error) {
	fs := flag.NewFlagSet("issue plan", flag.ContinueOnError)
	fs.SetOutput(a.Err)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "Issue number")
	var allowPaths pathListFlag
	fs.Var(&allowPaths, "allow-path", "explicit changed path to add to conflict recovery scope; repeatable")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return "", 0, nil, false, exitError{2, err}
	}
	if *number <= 0 || fs.NArg() != 0 {
		return "", 0, nil, false, exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	normalized, err := normalizeAdoptionAllowPaths(allowPaths)
	if err != nil {
		return "", 0, nil, false, exitError{2, err}
	}
	return *repo, *number, normalized, *jsonOut, nil
}

func (a App) buildIssuePlan(ctx context.Context, l layout.Layout, repo string, number int, adoptionAllowPaths []string) (issuePlanningContext, error) {
	entry, err := a.resolvePath(l, repo)
	if err != nil {
		return issuePlanningContext{}, err
	}
	cfg, err := config.Load(entry.RepoPath)
	if err != nil {
		return issuePlanningContext{}, err
	}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: entry.RepoPath, Secrets: cfg.RedactionValues()}
	before, err := os.ReadFile(store.StatePath())
	if err != nil {
		return issuePlanningContext{}, err
	}
	snapshot, err := store.ReadCanonicalSnapshot()
	if err != nil {
		return issuePlanningContext{}, err
	}
	item := snapshot.Issues[strconv.Itoa(number)]
	if item == nil {
		quarantine := snapshot.QuarantinedIssues[strconv.Itoa(number)]
		if quarantine != nil {
			after, readErr := os.ReadFile(store.StatePath())
			report := issuePlanReport{
				SchemaVersion: 1, IssueNumber: number, StateRevision: snapshot.StateRevision,
				Quarantine: quarantine,
				Observations: map[string]any{
					"quarantined": true, "reason_code": quarantine.ReasonCode,
					"rejected_status": quarantine.RejectedStatus,
				},
				Actions: []issueActionPlan{
					{Action: issuedomain.ResolutionResume, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionRetryStage, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionAdoptHead, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionAdoptWorktree, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionApproveConflictPaths, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionAdoptPR, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionCancel, Eligible: true},
				},
				ReadOnly: readErr == nil && bytes.Equal(before, after),
			}
			p := a.inspectInputAdoption(ctx, l, issuePlanningContext{gitPath: entry.Commands["git"], ghPath: entry.Commands["gh"], cfg: cfg, store: store, snapshot: snapshot, quarantine: quarantine, report: report})
			return a.inspectChecksRecovery(ctx, l, p), nil
		}
		return issuePlanningContext{}, exitError{4, fmt.Errorf("Issue #%d is missing from canonical state", number)}
	}
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	pgid := item.WorkerPGID
	if pgid <= 1 {
		pgid = item.WorkerPID
	}
	workerLive := controller.Alive(item.WorkerPID) || controller.GroupAlive(pgid)
	manager := worktree.Manager{StateRoot: l.Root, GitPath: entry.Commands["git"]}
	launch, launchErr := manager.ValidateLaunch(ctx, cfg, item.Worktree, item.Branch)
	inspection := worktree.Inspection{}
	inspectErr := launchErr
	if launchErr == nil && launch.Valid {
		inspection, inspectErr = manager.Inspect(ctx, cfg, item.Worktree, item.Branch)
	}
	worktreeSHA256, worktreeDigestErr := "", error(nil)
	if inspectErr == nil && inspection.Valid {
		worktreeSHA256, worktreeDigestErr = manager.ContentDigest(ctx, item.Worktree)
	}
	baseOK, baseErr := checkpointBaseAncestor(ctx, entry.Commands["git"], item, inspection)
	remote, remoteErr := (gh.CLI{Path: entry.Commands["gh"], Secrets: cfg.RedactionValues()}).Inspect(ctx, cfg, number, item.Branch)
	pending := pendingRequestIDs(snapshot, number)
	resultSummary, resultSHA256, resultErr := "", "", error(nil)
	if item.Continuation != nil && item.Continuation.Stage == issuedomain.ContinuationStagePublish {
		result, encoded, loadErr := worker.LoadLatestCompletedResult(filepath.Join(store.Dir, "runs", item.RunID))
		if loadErr != nil {
			resultErr = loadErr
		} else {
			resultSummary = result.Summary
			resultSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
		}
	}
	adoption, adoptionErr := worktreeAdoptionObservation{}, error(nil)
	if (state.CanAdoptWorktree(item) || state.CanApproveConflictPaths(item)) && launchErr == nil && launch.Valid && inspectErr == nil && inspection.Valid {
		adoption, adoptionErr = inspectWorktreeAdoption(ctx, entry.Commands["git"], item)
	}
	publicationHeadRepair := remoteErr == nil && resultErr == nil && item.Continuation != nil &&
		resultSummary == item.Continuation.Summary && resultSHA256 == item.Continuation.ResultSHA256 &&
		publicationCheckpointHeadRepair(ctx, cfg, entry.Commands["gh"], item, inspection, remote)
	actions := plannedIssueActions(cfg, item, snapshot.ActiveExecution, workerLive, launch, launchErr, inspection, inspectErr, worktreeSHA256, worktreeDigestErr, baseOK, baseErr, pending, remote, remoteErr, resultErr, adoption, adoptionErr, adoptionAllowPaths, publicationHeadRepair)
	after, readErr := os.ReadFile(store.StatePath())
	readOnly := readErr == nil && bytes.Equal(before, after)
	report := issuePlanReport{
		SchemaVersion: 1, IssueNumber: number, StateRevision: snapshot.StateRevision, Status: item.Status,
		Suspension: item.Suspension, Checkpoint: item.Continuation,
		Observations: map[string]any{
			"publication_head_repair": publicationHeadRepair,
			"worker_live":             workerLive, "workspace_valid": launchErr == nil && launch.Valid,
			"workspace_error": errorText(launchErr), "github_observed": remoteErr == nil,
			"github_state_reason": remote.Issue.StateReason,
			"git_valid":           inspectErr == nil && inspection.Valid,
			"git_error":           errorText(inspectErr), "git_head": inspection.Head, "git_remote_head": inspection.RemoteHead,
			"git_dirty": inspection.Dirty, "git_unpushed": inspection.UnpushedCommits,
			"worktree_sha256": worktreeSHA256, "worktree_digest_error": errorText(worktreeDigestErr),
			"checkpoint_worktree_sha256": checkpointWorktreeSHA256(item),
			"checkpoint_base_ancestor":   baseOK, "checkpoint_base_error": errorText(baseErr),
			"github_error": errorText(remoteErr), "open_pull_requests": countOpenPullRequests(remote.PullRequests),
			"pending_request_ids":       pending,
			"publication_result_sha256": resultSHA256,
			"publication_result_error":  errorText(resultErr),
			"adoption_merge_head":       adoption.MergeHead, "adoption_changed_paths": adoption.ChangedPaths,
			"adoption_unmerged_paths": adoption.UnmergedPaths, "adoption_error": errorText(adoptionErr),
			"adoption_allow_paths":              append([]string(nil), adoptionAllowPaths...),
			"adoption_unapproved_changed_paths": adoptionUnapprovedPaths(item, adoption, adoptionAllowPaths),
		},
		Actions: actions, ReadOnly: readOnly,
	}
	return issuePlanningContext{gitPath: entry.Commands["git"], ghPath: entry.Commands["gh"], cfg: cfg, store: store, snapshot: snapshot, issue: item,
		remote: remote, remoteErr: remoteErr, launch: launch, launchErr: launchErr,
		inspection: inspection, inspectErr: inspectErr, worktreeSHA256: worktreeSHA256, worktreeDigestErr: worktreeDigestErr,
		baseOK: baseOK, baseErr: baseErr,
		workerLive: workerLive, pending: pending, resultSummary: resultSummary, resultSHA256: resultSHA256, resultErr: resultErr,
		adoption: adoption, adoptionErr: adoptionErr, adoptionAllowPaths: append([]string(nil), adoptionAllowPaths...), report: report}, nil
}

func plannedIssueActions(cfg config.Config, item *state.Issue, activeExecution *state.ActiveExecution, workerLive bool, launch worktree.LaunchValidation, launchErr error,
	inspection worktree.Inspection, inspectErr error, worktreeSHA256 string, worktreeDigestErr error,
	baseOK bool, baseErr error, pending []string, remote gh.RemoteState, remoteErr error,
	resultErr error, adoption worktreeAdoptionObservation, adoptionErr error, adoptionAllowPaths []string, publicationHeadRepair bool,
) []issueActionPlan {
	actions := []issuedomain.ResolutionAction{issuedomain.ResolutionResume, issuedomain.ResolutionRetryStage, issuedomain.ResolutionAdoptInput, issuedomain.ResolutionAdoptHead, issuedomain.ResolutionAdoptWorktree, issuedomain.ResolutionApproveConflictPaths, issuedomain.ResolutionAdoptPR, issuedomain.ResolutionCancel}
	result := make([]issueActionPlan, 0, len(actions))
	for _, action := range actions {
		reasons := []string{}
		suspensionEligible := item.Suspension != nil && containsAction(item.Suspension.AllowedActions, action) &&
			(item.Suspension.Status == issuedomain.SuspensionActive ||
				(item.Suspension.Status == issuedomain.SuspensionQuarantined && action == issuedomain.ResolutionCancel))
		if action == issuedomain.ResolutionAdoptWorktree {
			suspensionEligible = state.CanAdoptWorktree(item)
		}
		if action == issuedomain.ResolutionApproveConflictPaths {
			suspensionEligible = state.CanApproveConflictPaths(item)
		}
		if action == issuedomain.ResolutionAdoptHead {
			suspensionEligible = state.CanAdoptHead(item)
		}
		if !suspensionEligible {
			reasons = append(reasons, "action is not allowed by the active suspension")
		}
		if workerLive {
			reasons = append(reasons, "worker process is alive")
		}
		if resolutionRequiresExecutionSlot(action) && activeExecution != nil {
			reasons = append(reasons, fmt.Sprintf("repository active execution is occupied by Issue #%d", activeExecution.IssueNumber))
		}
		if (action == issuedomain.ResolutionAdoptWorktree || action == issuedomain.ResolutionAdoptHead || action == issuedomain.ResolutionApproveConflictPaths) && activeExecution != nil {
			reasons = append(reasons, fmt.Sprintf("repository active execution is occupied by Issue #%d", activeExecution.IssueNumber))
		}
		if len(pending) > 0 && action != issuedomain.ResolutionCancel {
			reasons = append(reasons, "Issue has a pending operator request")
		}
		switch action {
		case issuedomain.ResolutionResume, issuedomain.ResolutionRetryStage:
			if item.Continuation == nil {
				reasons = append(reasons, "continuation checkpoint is missing")
			}
			if launchErr != nil || !launch.Valid {
				reasons = append(reasons, "workspace validation failed")
			}
			if inspectErr != nil || !inspection.Valid || inspection.Branch != item.Branch {
				reasons = append(reasons, "git worktree observation failed")
			}
			if baseErr != nil || !baseOK {
				reasons = append(reasons, "checkpoint base is not an ancestor of the worktree head")
			}
			if item.Continuation != nil && (item.Continuation.Stage == issuedomain.ContinuationStageResume || item.Continuation.Stage == issuedomain.ContinuationStagePublish) {
				if (item.Continuation.HeadSHA == "" || inspection.Head != item.Continuation.HeadSHA) && !(action == issuedomain.ResolutionRetryStage && publicationHeadRepair) {
					reasons = append(reasons, "worktree head differs from the continuation checkpoint")
				}
				if item.Continuation.WorktreeSHA256 == "" || worktreeDigestErr != nil || worktreeSHA256 != item.Continuation.WorktreeSHA256 {
					reasons = append(reasons, "worktree content differs from the continuation checkpoint")
				}
			}
			if remoteErr != nil {
				reasons = append(reasons, "GitHub Issue could not be inspected")
			}
			if !pullRequestsMatchCheckpoint(item, remote.PullRequests) {
				reasons = append(reasons, "Pull Request observation differs from checkpoint")
			}
			if action == issuedomain.ResolutionRetryStage && item.Continuation != nil && item.Continuation.Stage == issuedomain.ContinuationStageChecks {
				pullRequest, ok := matchingOpenPullRequest(item, remote.PullRequests)
				if !ok || inspection.Dirty || inspection.UnpushedCommits || !inspection.LocalBranchExists || !inspection.RemoteBranchExists ||
					inspection.Head == "" || inspection.Head != inspection.RemoteHead || inspection.Head != pullRequest.HeadSHA ||
					(pullRequest.ChecksStatus != "pending" && pullRequest.ChecksStatus != "success") {
					reasons = append(reasons, "repaired Pull Request head is not cleanly reproducible or checks are not runnable")
				}
			}
			if action == issuedomain.ResolutionRetryStage && item.Continuation != nil && item.Continuation.Stage == issuedomain.ContinuationStagePublish && resultErr != nil {
				reasons = append(reasons, "saved completed worker result is unavailable")
			}
		case issuedomain.ResolutionAdoptHead:
			if !state.CanAdoptHead(item) {
				reasons = append(reasons, "only an active worker resume checkpoint missing head_sha can be adopted")
			}
			if launchErr != nil || !launch.Valid || inspectErr != nil || !inspection.Valid || inspection.Branch != item.Branch || inspection.Head == "" {
				reasons = append(reasons, "workspace or git worktree identity differs from canonical state")
			}
			if baseErr != nil || !baseOK {
				reasons = append(reasons, "checkpoint base is not an ancestor of the worktree head")
			}
			if worktreeDigestErr != nil || worktreeSHA256 == "" || worktreeSHA256 != checkpointWorktreeSHA256(item) {
				reasons = append(reasons, "worktree content differs from the continuation checkpoint")
			}
			if remoteErr != nil || !strings.EqualFold(remote.Issue.State, "open") || len(remote.PullRequests) != 0 || inspection.RemoteHead != "" {
				reasons = append(reasons, "head adoption requires an open Issue with no published branch or Pull Request")
			}
		case issuedomain.ResolutionAdoptWorktree:
			reasons = append(reasons, worktreeAdoptionReasons(cfg, item, launch, launchErr, inspection, inspectErr, worktreeSHA256, worktreeDigestErr, baseOK, baseErr, remote, remoteErr, adoption, adoptionErr, adoptionAllowPaths)...)
		case issuedomain.ResolutionApproveConflictPaths:
			reasons = append(reasons, conflictPathApprovalReasons(cfg, item, launch, launchErr, inspection, inspectErr, worktreeSHA256, worktreeDigestErr, baseOK, baseErr, remote, remoteErr, adoption, adoptionErr, adoptionAllowPaths)...)
		case issuedomain.ResolutionAdoptPR:
			if remoteErr != nil {
				reasons = append(reasons, "GitHub state was not observed")
			} else if pr, ok := mergedPullRequestForIssue(item, remote.PullRequests); !ok {
				reasons = append(reasons, "exactly one matching merged Pull Request was not observed")
			} else {
				if pr.HeadRepository != cfg.GitHub.Repo || pr.BaseRefName != cfg.Git.BaseBranch {
					reasons = append(reasons, "merged Pull Request repository or base differs")
				}
				if inspectErr != nil || !inspection.Valid || inspection.Dirty || inspection.UnpushedCommits ||
					!inspection.LocalBranchExists || !inspection.RemoteBranchExists || inspection.Head == "" ||
					inspection.Head != inspection.RemoteHead || inspection.Head != pr.HeadSHA {
					reasons = append(reasons, "worktree is not clean and byte-identical to the merged Pull Request head")
				}
				if baseErr != nil || !baseOK {
					reasons = append(reasons, "checkpoint base is not an ancestor of the merged head")
				}
			}
		}
		result = append(result, issueActionPlan{Action: action, Eligible: len(reasons) == 0, Reasons: reasons})
	}
	return result
}

func resolutionRequiresExecutionSlot(action issuedomain.ResolutionAction) bool {
	return action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage
}

func checkpointWorktreeSHA256(item *state.Issue) string {
	if item == nil || item.Continuation == nil {
		return ""
	}
	return item.Continuation.WorktreeSHA256
}

func issueResolutionAudit(planned issuePlanningContext, action issuedomain.ResolutionAction, issueNumber int, now time.Time,
	mergedPR gh.PullRequest, hasMergedPR bool,
) (string, map[string]any) {
	payload := map[string]any{
		"action": action, "checkpoint_id": planned.issue.Suspension.CheckpointID,
		"planned_state_revision": planned.snapshot.StateRevision,
		"worker_live":            planned.workerLive,
		"workspace_valid":        planned.launchErr == nil && planned.launch.Valid,
		"git_valid":              planned.inspectErr == nil && planned.inspection.Valid,
		"git_head":               planned.inspection.Head, "git_remote_head": planned.inspection.RemoteHead,
		"git_dirty": planned.inspection.Dirty, "git_unpushed": planned.inspection.UnpushedCommits,
		"worktree_sha256":          planned.worktreeSHA256,
		"checkpoint_base_ancestor": planned.baseOK,
		"github_observed":          planned.remoteErr == nil, "github_issue_state": planned.remote.Issue.State,
		"github_state_reason": planned.remote.Issue.StateReason,
		"open_pull_requests":  countOpenPullRequests(planned.remote.PullRequests),
		"pending_request_ids": append([]string(nil), planned.pending...),
	}
	if action == issuedomain.ResolutionAdoptHead {
		payload["previous_head_sha"] = ""
		payload["adopted_head_sha"] = planned.inspection.Head
		return "issue_checkpoint_head_adopted", payload
	}
	if action == issuedomain.ResolutionAdoptWorktree || action == issuedomain.ResolutionApproveConflictPaths {
		payload["target_base_sha"] = planned.issue.ConflictRecovery.TargetBaseSHA
		payload["changed_paths"] = append([]string(nil), planned.adoption.ChangedPaths...)
		payload["unmerged_paths"] = append([]string(nil), planned.adoption.UnmergedPaths...)
		payload["allowed_paths_added"] = append([]string(nil), planned.adoptionAllowPaths...)
		if action == issuedomain.ResolutionApproveConflictPaths {
			payload["generation"] = planned.issue.Generation
			payload["suspension_id"] = planned.issue.Suspension.ID
			payload["pull_request_url"] = planned.issue.PullRequestURL
			payload["allowed_paths_added"] = unrecordedApprovalPaths(planned.issue, planned.adoptionAllowPaths)
			return "issue_conflict_paths_approved", payload
		}
		return "issue_worktree_adopted", payload
	}
	if action == issuedomain.ResolutionRetryStage && planned.issue.Continuation != nil && planned.issue.Continuation.Stage == issuedomain.ContinuationStagePublish {
		payload["publication_result_sha256"] = planned.resultSHA256
		if planned.report.Observations["publication_head_repair"] == true {
			payload["previous_head_sha"] = planned.issue.Continuation.HeadSHA
			payload["adopted_head_sha"] = planned.inspection.Head
		}
	}
	if action == issuedomain.ResolutionRetryStage && planned.issue.ConflictRecovery != nil {
		payload["conflict_attempts_reset"] = planned.issue.ConflictRecovery.Attempts
	}
	if action == issuedomain.ResolutionAdoptPR && hasMergedPR {
		payload["adopted_pull_request"] = map[string]any{
			"number": mergedPR.Number, "url": mergedPR.URL, "head_sha": mergedPR.HeadSHA,
			"head_ref": mergedPR.HeadRefName, "base_ref": mergedPR.BaseRefName,
			"head_repository": mergedPR.HeadRepository, "merged_at": mergedPR.MergedAt,
		}
	}
	if action != issuedomain.ResolutionCancel {
		return "issue_suspension_resolved", payload
	}
	payload["issue_number"] = issueNumber
	payload["run_id"] = planned.issue.RunID
	payload["previous_status"] = planned.issue.Status
	payload["execution_release_result"] = "not_present"
	payload["canceled_at"] = now
	payload["source"] = "operator_resolution"
	return "issue_canceled", payload
}

func (a App) issueResolve(ctx context.Context, l layout.Layout, args []string) error {
	opts, err := a.parseIssueResolveArgs(args)
	if err != nil {
		return err
	}
	repo, number, expectedHead, jsonOut := &opts.repo, &opts.number, &opts.expectedHead, &opts.jsonOut
	action, normalizedAllowPaths := opts.action, opts.allowPaths
	planned, err := a.buildIssuePlan(ctx, l, *repo, *number, normalizedAllowPaths)
	if err != nil {
		return err
	}
	if planned.quarantine != nil {
		return a.resolveIssueQuarantine(ctx, l, opts, planned)
	}
	if action == issuedomain.ResolutionCancel && planned.issue.Status == issuedomain.StatusCanceled {
		if err := (gh.CLI{Path: planned.ghPath, Secrets: planned.cfg.RedactionValues()}).ReconcileIssue(ctx, planned.cfg, *number, planned.issue.Status, planned.snapshot.NeedsHuman(*number, planned.cfg.Completion.AutoMerge)); err != nil {
			return err
		}
		return a.output(*jsonOut, map[string]any{"schema_version": 1, "issue_number": *number, "action": action, "idempotent": true, "status": planned.issue.Status})
	}
	if action == issuedomain.ResolutionAdoptPR && planned.issue.Status == issuedomain.StatusCompleted && planned.issue.PullRequestMerged {
		if effect := state.PendingEffect(&planned.snapshot, planned.issue.Number); effect != nil && effect.Kind == issuedomain.EffectMarkDone {
			if err := a.synchronizeIssueResolution(ctx, planned, action, *number); err != nil {
				return err
			}
		}
		return a.output(*jsonOut, map[string]any{"schema_version": 1, "issue_number": *number, "action": action, "idempotent": true, "status": issuedomain.StatusCompleted})
	}
	if planned.issue.Suspension != nil && planned.issue.Suspension.Status == issuedomain.SuspensionResolved && planned.issue.Suspension.Resolution == action {
		if effect := state.PendingEffect(&planned.snapshot, planned.issue.Number); effect != nil && effect.Kind == issuedomain.EffectApplyResolution {
			if err := a.synchronizeIssueResolution(ctx, planned, action, *number); err != nil {
				return err
			}
		}
		return a.output(*jsonOut, map[string]any{"schema_version": 1, "issue_number": *number, "action": action, "idempotent": true, "status": planned.issue.Status})
	}
	eligible := false
	var reasons []string
	for _, candidate := range planned.report.Actions {
		if candidate.Action == action {
			eligible, reasons = candidate.Eligible, candidate.Reasons
		}
	}
	if !eligible {
		return exitError{4, fmt.Errorf("Issue #%d action %s is not eligible: %s", *number, action, strings.Join(reasons, "; "))}
	}
	revalidated, err := a.buildIssuePlan(ctx, l, *repo, *number, normalizedAllowPaths)
	if err != nil {
		return err
	}
	revalidatedEligible := false
	for _, candidate := range revalidated.report.Actions {
		if candidate.Action == action {
			revalidatedEligible = candidate.Eligible
			break
		}
	}
	if revalidated.snapshot.StateRevision != planned.snapshot.StateRevision || !revalidatedEligible {
		return exitError{4, fmt.Errorf("Issue #%d observations changed after planning", *number)}
	}
	if action == issuedomain.ResolutionAdoptHead && (*expectedHead != planned.inspection.Head || *expectedHead != revalidated.inspection.Head || !reflect.DeepEqual(planned.report.Observations, revalidated.report.Observations)) {
		return exitError{4, fmt.Errorf("Issue #%d adopted HEAD or evidence changed after planning", *number)}
	}
	if action == issuedomain.ResolutionApproveConflictPaths && (!reflect.DeepEqual(planned.cfg, revalidated.cfg) || !reflect.DeepEqual(planned.issue, revalidated.issue) || !reflect.DeepEqual(planned.report.Observations, revalidated.report.Observations) || !reflect.DeepEqual(planned.remote, revalidated.remote)) {
		return exitError{4, fmt.Errorf("Issue #%d conflict approval evidence changed after planning", *number)}
	}
	planned = revalidated
	if action == issuedomain.ResolutionRetryStage && planned.issue.Continuation != nil && planned.issue.Continuation.Stage == issuedomain.ContinuationStagePublish {
		result, encoded, loadErr := worker.LoadLatestCompletedResult(filepath.Join(planned.store.Dir, "runs", planned.issue.RunID))
		if loadErr != nil || result.Summary != planned.resultSummary || fmt.Sprintf("%x", sha256.Sum256(encoded)) != planned.resultSHA256 {
			return exitError{4, fmt.Errorf("Issue #%d saved completed worker result changed after planning", *number)}
		}
	}
	now := time.Now().UTC()
	mergedPR, hasMergedPR := mergedPullRequestForIssue(planned.issue, planned.remote.PullRequests)
	eventType, payload := issueResolutionAudit(planned, action, *number, now, mergedPR, hasMergedPR)
	result, err := planned.store.Update(eventType, *number, planned.issue.RunID, payload, func(snapshot *state.Snapshot) error {
		if snapshot.StateRevision != planned.snapshot.StateRevision {
			return fmt.Errorf("Issue #%d canonical state changed after planning", *number)
		}
		item := snapshot.Issues[strconv.Itoa(*number)]
		if item == nil || !reflect.DeepEqual(item.Suspension, planned.issue.Suspension) {
			return fmt.Errorf("Issue #%d suspension changed after planning", *number)
		}
		if resolutionRequiresExecutionSlot(action) && snapshot.ActiveExecution != nil {
			return fmt.Errorf("Issue #%d execution slot changed after planning", *number)
		}

		observed := state.OperatorResolutionObservation{
			HeadSHA: planned.inspection.Head, WorktreeSHA256: planned.worktreeSHA256,
			ResultSummary: planned.resultSummary, ResultSHA256: planned.resultSHA256,
			RepairPublicationHead: planned.report.Observations["publication_head_repair"] == true,
			GitHubStateReason:     planned.remote.Issue.StateReason,
		}
		if action == issuedomain.ResolutionAdoptHead {
			if snapshot.ActiveExecution != nil || !reflect.DeepEqual(item, planned.issue) {
				return fmt.Errorf("Issue #%d head adoption evidence changed", *number)
			}
			if err := verifyAdoptedHead(ctx, planned, *expectedHead, l.Root); err != nil {
				return err
			}
			observed.HeadSHA = *expectedHead
		}
		if action == issuedomain.ResolutionApproveConflictPaths {
			if snapshot.ActiveExecution != nil || !reflect.DeepEqual(item, planned.issue) {
				return fmt.Errorf("conflict approval identity changed")
			}
			if err := a.verifyConflictPathApproval(ctx, planned, l.Root); err != nil {
				return err
			}
			observed.AllowedPaths = unrecordedApprovalPaths(item, planned.adoptionAllowPaths)
			if len(observed.AllowedPaths) == 0 {
				return errConflictPathsAlreadyApproved
			}
		}
		if action == issuedomain.ResolutionAdoptWorktree {
			observed.AllowedPaths = planned.adoptionAllowPaths
		}
		if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStagePublish {
			if planned.resultErr != nil || planned.resultSummary == "" || planned.resultSHA256 == "" {
				return fmt.Errorf("Issue #%d saved completed worker result changed after planning", *number)
			}
			if observed.RepairPublicationHead {
				if err := verifyRecoveryWorkspace(ctx, planned, l.Root); err != nil {
					return err
				}
			}
		}
		if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStageChecks {
			pullRequest, ok := matchingOpenPullRequest(item, planned.remote.PullRequests)
			if !ok || pullRequest.HeadSHA != planned.inspection.Head {
				return fmt.Errorf("Issue #%d repaired Pull Request changed after planning", *number)
			}
			observed.PullRequestNumber = pullRequest.Number
		}
		if action == issuedomain.ResolutionAdoptPR {
			if !hasMergedPR {
				return fmt.Errorf("Issue #%d matching merged Pull Request changed after planning", *number)
			}
			observed.PullRequestURL, observed.PullRequestNumber, observed.HeadSHA = mergedPR.URL, mergedPR.Number, mergedPR.HeadSHA
		}
		return state.ResolveOperatorSuspension(snapshot, *number, action, observed, now)
	})
	if errors.Is(err, errConflictPathsAlreadyApproved) {
		return a.output(*jsonOut, map[string]any{"schema_version": 1, "issue_number": *number, "action": action, "status": planned.issue.Status, "state_revision": planned.snapshot.StateRevision, "idempotent": true})
	}
	if err != nil {
		return exitError{4, err}
	}
	if action != issuedomain.ResolutionAdoptWorktree && action != issuedomain.ResolutionAdoptHead && action != issuedomain.ResolutionApproveConflictPaths {
		planned.issue = result.Issues[strconv.Itoa(*number)]
		if err := a.synchronizeIssueResolution(ctx, planned, action, *number); err != nil {
			return err
		}
	}
	return a.output(*jsonOut, map[string]any{"schema_version": 1, "issue_number": *number, "action": action,
		"status": result.Issues[strconv.Itoa(*number)].Status, "state_revision": result.StateRevision, "idempotent": false})
}

func (a App) resolveQuarantinedIssue(ctx context.Context, l layout.Layout, repo string, number int,
	action issuedomain.ResolutionAction, jsonOut bool, planned issuePlanningContext,
) error {
	revalidated, err := a.buildIssuePlan(ctx, l, repo, number, nil)
	if err != nil {
		return err
	}
	if revalidated.snapshot.StateRevision != planned.snapshot.StateRevision ||
		!reflect.DeepEqual(revalidated.quarantine, planned.quarantine) {
		return exitError{4, fmt.Errorf("Issue #%d quarantine changed after planning", number)}
	}
	result, err := planned.store.Update("issue_quarantine_resolved", number, planned.quarantine.RunID,
		map[string]any{"action": action, "planned_state_revision": planned.snapshot.StateRevision, "quarantine": planned.quarantine}, func(snapshot *state.Snapshot) error {
			if snapshot.StateRevision != planned.snapshot.StateRevision ||
				!reflect.DeepEqual(snapshot.QuarantinedIssues[strconv.Itoa(number)], planned.quarantine) {
				return fmt.Errorf("Issue #%d quarantine changed after planning", number)
			}
			return state.CancelQuarantinedIssue(snapshot, number, time.Now().UTC())
		})
	if err != nil {
		return exitError{4, err}
	}
	planned.issue = result.Issues[strconv.Itoa(number)]
	if err := a.synchronizeIssueResolution(ctx, planned, action, number); err != nil {
		return err
	}
	return a.output(jsonOut, map[string]any{"schema_version": 1, "issue_number": number, "action": action,
		"status": planned.issue.Status, "state_revision": result.StateRevision, "idempotent": false})
}

func pendingRequestIDs(snapshot state.Snapshot, issueNumber int) []string {
	ids := make([]string, 0)
	for id, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func checkpointBaseAncestor(ctx context.Context, gitPath string, item *state.Issue, inspection worktree.Inspection) (bool, error) {
	if item == nil || item.Continuation == nil || item.Continuation.BaseSHA == "" || inspection.Head == "" || item.Worktree == "" {
		return false, fmt.Errorf("checkpoint base or worktree head is missing")
	}
	if gitPath == "" {
		gitPath = "git"
	}
	command := exec.CommandContext(ctx, gitPath, "-C", item.Worktree, "merge-base", "--is-ancestor", item.Continuation.BaseSHA, inspection.Head)
	if output, err := command.CombinedOutput(); err != nil {
		return false, fmt.Errorf("verify checkpoint base ancestry: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return true, nil
}

func containsAction(actions []issuedomain.ResolutionAction, target issuedomain.ResolutionAction) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
}

func pullRequestsMatchCheckpoint(item *state.Issue, pullRequests []gh.PullRequest) bool {
	if item.PullRequestURL == "" {
		return countOpenPullRequests(pullRequests) == 0
	}
	for _, pr := range pullRequests {
		if strings.EqualFold(pr.State, "open") && pr.URL == item.PullRequestURL && pr.HeadRefName == item.Branch {
			return true
		}
	}
	return false
}

func matchingOpenPullRequest(item *state.Issue, pullRequests []gh.PullRequest) (gh.PullRequest, bool) {
	matches := make([]gh.PullRequest, 0, 1)
	for _, pullRequest := range pullRequests {
		if strings.EqualFold(pullRequest.State, "open") && pullRequest.MergedAt == nil &&
			pullRequest.URL == item.PullRequestURL && (item.PullRequestNumber == 0 || pullRequest.Number == item.PullRequestNumber) && pullRequest.HeadRefName == item.Branch {
			matches = append(matches, pullRequest)
		}
	}
	if len(matches) != 1 {
		return gh.PullRequest{}, false
	}
	return matches[0], true
}

func countOpenPullRequests(values []gh.PullRequest) int {
	count := 0
	for _, value := range values {
		if strings.EqualFold(value.State, "open") {
			count++
		}
	}
	return count
}

func countMergedPullRequests(values []gh.PullRequest) int {
	count := 0
	for _, value := range values {
		if strings.EqualFold(value.State, "merged") && value.MergedAt != nil {
			count++
		}
	}
	return count
}

func mergedPullRequestForIssue(item *state.Issue, values []gh.PullRequest) (gh.PullRequest, bool) {
	matches := make([]gh.PullRequest, 0, 1)
	for _, value := range values {
		if !strings.EqualFold(value.State, "merged") || value.MergedAt == nil || value.HeadRefName != item.Branch {
			continue
		}
		if item.PullRequestURL != "" && value.URL != item.PullRequestURL {
			continue
		}
		matches = append(matches, value)
	}
	if len(matches) != 1 {
		return gh.PullRequest{}, false
	}
	return matches[0], true
}

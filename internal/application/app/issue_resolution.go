package app

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	ghPath            string
	cfg               config.Config
	store             state.Store
	snapshot          state.Snapshot
	issue             *state.Issue
	quarantine        *state.QuarantineRecord
	remote            gh.RemoteState
	remoteErr         error
	launch            worktree.LaunchValidation
	launchErr         error
	inspection        worktree.Inspection
	inspectErr        error
	worktreeSHA256    string
	worktreeDigestErr error
	baseOK            bool
	baseErr           error
	workerLive        bool
	pending           []string
	resultSummary     string
	resultSHA256      string
	resultErr         error
	report            issuePlanReport
}

func (a App) issueCommand(ctx context.Context, l layout.Layout, args []string) error {
	if len(args) == 0 {
		return exitError{2, fmt.Errorf("issue requires plan or resolve")}
	}
	switch args[0] {
	case "plan":
		return a.issuePlan(ctx, l, args[1:])
	case "resolve":
		return a.issueResolve(ctx, l, args[1:])
	case "help", "--help", "-h":
		fmt.Fprintln(a.Out, "Usage: agent-loop issue plan --repo PATH --issue N --json\n       agent-loop issue resolve --repo PATH --issue N --action resume|retry-stage|adopt-pr|cancel --json")
		return nil
	default:
		return exitError{2, fmt.Errorf("unknown issue command %q", args[0])}
	}
}

func (a App) issuePlan(ctx context.Context, l layout.Layout, args []string) error {
	repo, number, jsonOut, err := parseIssuePlanArgs(args)
	if err != nil {
		return err
	}
	planned, err := a.buildIssuePlan(ctx, l, repo, number)
	if err != nil {
		return err
	}
	return a.output(jsonOut, planned.report)
}

func parseIssuePlanArgs(args []string) (string, int, bool, error) {
	fs := flag.NewFlagSet("issue plan", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "Issue number")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return "", 0, false, exitError{2, err}
	}
	if *number <= 0 || fs.NArg() != 0 {
		return "", 0, false, exitError{2, fmt.Errorf("--issue must be a positive Issue number")}
	}
	return *repo, *number, *jsonOut, nil
}

func (a App) buildIssuePlan(ctx context.Context, l layout.Layout, repo string, number int) (issuePlanningContext, error) {
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
					{Action: issuedomain.ResolutionAdoptPR, Eligible: false, Reasons: []string{"Issue is quarantined"}},
					{Action: issuedomain.ResolutionCancel, Eligible: true},
				},
				ReadOnly: readErr == nil && bytes.Equal(before, after),
			}
			return issuePlanningContext{cfg: cfg, store: store, snapshot: snapshot, quarantine: quarantine, report: report}, nil
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
	actions := plannedIssueActions(cfg, item, workerLive, launch, launchErr, inspection, inspectErr, worktreeSHA256, worktreeDigestErr, baseOK, baseErr, pending, remote, remoteErr, resultErr)
	after, readErr := os.ReadFile(store.StatePath())
	readOnly := readErr == nil && bytes.Equal(before, after)
	report := issuePlanReport{
		SchemaVersion: 1, IssueNumber: number, StateRevision: snapshot.StateRevision, Status: item.Status,
		Suspension: item.Suspension, Checkpoint: item.Continuation,
		Observations: map[string]any{
			"worker_live": workerLive, "workspace_valid": launchErr == nil && launch.Valid,
			"workspace_error": errorText(launchErr), "github_observed": remoteErr == nil,
			"git_valid": inspectErr == nil && inspection.Valid,
			"git_error": errorText(inspectErr), "git_head": inspection.Head, "git_remote_head": inspection.RemoteHead,
			"git_dirty": inspection.Dirty, "git_unpushed": inspection.UnpushedCommits,
			"worktree_sha256": worktreeSHA256, "worktree_digest_error": errorText(worktreeDigestErr),
			"checkpoint_worktree_sha256": checkpointWorktreeSHA256(item),
			"checkpoint_base_ancestor":   baseOK, "checkpoint_base_error": errorText(baseErr),
			"github_error": errorText(remoteErr), "open_pull_requests": countOpenPullRequests(remote.PullRequests),
			"pending_request_ids":       pending,
			"publication_result_sha256": resultSHA256,
			"publication_result_error":  errorText(resultErr),
		},
		Actions: actions, ReadOnly: readOnly,
	}
	return issuePlanningContext{ghPath: entry.Commands["gh"], cfg: cfg, store: store, snapshot: snapshot, issue: item,
		remote: remote, remoteErr: remoteErr, launch: launch, launchErr: launchErr,
		inspection: inspection, inspectErr: inspectErr, worktreeSHA256: worktreeSHA256, worktreeDigestErr: worktreeDigestErr,
		baseOK: baseOK, baseErr: baseErr,
		workerLive: workerLive, pending: pending, resultSummary: resultSummary, resultSHA256: resultSHA256, resultErr: resultErr, report: report}, nil
}

func plannedIssueActions(cfg config.Config, item *state.Issue, workerLive bool, launch worktree.LaunchValidation, launchErr error,
	inspection worktree.Inspection, inspectErr error, worktreeSHA256 string, worktreeDigestErr error,
	baseOK bool, baseErr error, pending []string, remote gh.RemoteState, remoteErr error,
	resultErr error,
) []issueActionPlan {
	actions := []issuedomain.ResolutionAction{issuedomain.ResolutionResume, issuedomain.ResolutionRetryStage, issuedomain.ResolutionAdoptPR, issuedomain.ResolutionCancel}
	result := make([]issueActionPlan, 0, len(actions))
	for _, action := range actions {
		reasons := []string{}
		suspensionEligible := item.Suspension != nil && containsAction(item.Suspension.AllowedActions, action) &&
			(item.Suspension.Status == issuedomain.SuspensionActive ||
				(item.Suspension.Status == issuedomain.SuspensionQuarantined && action == issuedomain.ResolutionCancel))
		if !suspensionEligible {
			reasons = append(reasons, "action is not allowed by the active suspension")
		}
		if workerLive {
			reasons = append(reasons, "worker process is alive")
		}
		if len(pending) > 0 && action != issuedomain.ResolutionCancel {
			reasons = append(reasons, "Issue has a pending operator request")
		}
		if action != issuedomain.ResolutionCancel && remoteErr == nil {
			if err := terminalGitHubStateAligned(cfg, item, remote.Issue); err != nil {
				reasons = append(reasons, err.Error())
			}
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
				if item.Continuation.HeadSHA == "" || inspection.Head != item.Continuation.HeadSHA {
					reasons = append(reasons, "worktree head differs from the continuation checkpoint")
				}
				if item.Continuation.WorktreeSHA256 == "" || worktreeDigestErr != nil || worktreeSHA256 != item.Continuation.WorktreeSHA256 {
					reasons = append(reasons, "worktree content differs from the continuation checkpoint")
				}
			}
			if remoteErr != nil || !strings.EqualFold(remote.Issue.State, "open") {
				reasons = append(reasons, "GitHub Issue is not observably open")
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

func checkpointWorktreeSHA256(item *state.Issue) string {
	if item == nil || item.Continuation == nil {
		return ""
	}
	return item.Continuation.WorktreeSHA256
}

func (a App) issueResolve(ctx context.Context, l layout.Layout, args []string) error {
	fs := flag.NewFlagSet("issue resolve", flag.ContinueOnError)
	repo := fs.String("repo", "", "repository path")
	number := fs.Int("issue", 0, "Issue number")
	actionText := fs.String("action", "", "resume, retry-stage, adopt-pr, or cancel")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return exitError{2, err}
	}
	action := issuedomain.ResolutionAction(*actionText)
	if *number <= 0 || action.Validate() != nil || fs.NArg() != 0 {
		return exitError{2, fmt.Errorf("--issue and a valid --action are required")}
	}
	planned, err := a.buildIssuePlan(ctx, l, *repo, *number)
	if err != nil {
		return err
	}
	quarantineOnly := planned.issue == nil && planned.quarantine != nil
	if quarantineOnly && action != issuedomain.ResolutionCancel {
		return exitError{4, fmt.Errorf("Issue #%d is quarantined; only cancel is eligible", *number)}
	}
	if quarantineOnly {
		return a.resolveQuarantinedIssue(ctx, l, *repo, *number, action, *jsonOut, planned)
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
	revalidated, err := a.buildIssuePlan(ctx, l, *repo, *number)
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
	planned = revalidated
	if action == issuedomain.ResolutionRetryStage && planned.issue.Continuation != nil && planned.issue.Continuation.Stage == issuedomain.ContinuationStagePublish {
		result, encoded, loadErr := worker.LoadLatestCompletedResult(filepath.Join(planned.store.Dir, "runs", planned.issue.RunID))
		if loadErr != nil || result.Summary != planned.resultSummary || fmt.Sprintf("%x", sha256.Sum256(encoded)) != planned.resultSHA256 {
			return exitError{4, fmt.Errorf("Issue #%d saved completed worker result changed after planning", *number)}
		}
	}
	now := time.Now().UTC()
	mergedPR, hasMergedPR := mergedPullRequestForIssue(planned.issue, planned.remote.PullRequests)
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
		"open_pull_requests":  countOpenPullRequests(planned.remote.PullRequests),
		"pending_request_ids": append([]string(nil), planned.pending...),
	}
	if action == issuedomain.ResolutionRetryStage && planned.issue.Continuation != nil && planned.issue.Continuation.Stage == issuedomain.ContinuationStagePublish {
		payload["publication_result_sha256"] = planned.resultSHA256
	}
	if action == issuedomain.ResolutionAdoptPR && hasMergedPR {
		payload["adopted_pull_request"] = map[string]any{
			"number": mergedPR.Number, "url": mergedPR.URL, "head_sha": mergedPR.HeadSHA,
			"head_ref": mergedPR.HeadRefName, "base_ref": mergedPR.BaseRefName,
			"head_repository": mergedPR.HeadRepository, "merged_at": mergedPR.MergedAt,
		}
	}
	result, err := planned.store.Update("issue_suspension_resolved", *number, planned.issue.RunID,
		payload, func(snapshot *state.Snapshot) error {
			if snapshot.StateRevision != planned.snapshot.StateRevision {
				return fmt.Errorf("Issue #%d canonical state changed after planning", *number)
			}
			item := snapshot.Issues[strconv.Itoa(*number)]
			if item == nil || !reflect.DeepEqual(item.Suspension, planned.issue.Suspension) || snapshot.ActiveExecution != nil {
				return fmt.Errorf("Issue #%d suspension changed after planning", *number)
			}
			if action == issuedomain.ResolutionResume || action == issuedomain.ResolutionRetryStage {
				if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStagePublish {
					if planned.resultErr != nil || planned.resultSummary == "" || planned.resultSHA256 == "" {
						return fmt.Errorf("Issue #%d saved completed worker result changed after planning", *number)
					}
					item.Continuation.Summary = planned.resultSummary
					item.Continuation.ResultSHA256 = planned.resultSHA256
				}
				if _, err := state.ResumeContinuation(snapshot, item.Number, item.Continuation.ID, now); err != nil {
					return err
				}
				transition, err := issuedomain.ResolveSuspension(item.Status, action, item.Continuation.Stage)
				if err != nil {
					return err
				}
				if err := state.ApplyIssueTransition(item, transition); err != nil {
					return err
				}
				if action == issuedomain.ResolutionRetryStage && item.Continuation.Stage == issuedomain.ContinuationStageChecks {
					pullRequest, ok := matchingOpenPullRequest(item, planned.remote.PullRequests)
					if !ok || pullRequest.HeadSHA != planned.inspection.Head {
						return fmt.Errorf("Issue #%d repaired Pull Request changed after planning", *number)
					}
					item.HeadSHA = pullRequest.HeadSHA
					item.PullRequestNumber = pullRequest.Number
				}
				if err := state.SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectApplyResolution, now); err != nil {
					return err
				}
			} else if action == issuedomain.ResolutionAdoptPR {
				if !hasMergedPR {
					return fmt.Errorf("Issue #%d matching merged Pull Request changed after planning", *number)
				}
				transition, transitionErr := issuedomain.ResolveSuspension(item.Status, action, issuedomain.ContinuationStageNone)
				if transitionErr != nil {
					return transitionErr
				}
				item.PullRequestURL = mergedPR.URL
				item.PullRequestNumber = mergedPR.Number
				item.HeadSHA = mergedPR.HeadSHA
				item.PullRequestMerged = true
				item.Suspension.Status = issuedomain.SuspensionResolved
				item.Suspension.Resolution = action
				item.Suspension.ResolvedAt = now
				if err := state.SetEffect(snapshot, item.Number, item.RunID, issuedomain.EffectMarkDone, now); err != nil {
					return err
				}
				if err := state.ApplyIssueTransition(item, transition); err != nil {
					return err
				}
			} else {
				transition, transitionErr := issuedomain.ResolveSuspension(item.Status, action, issuedomain.ContinuationStageNone)
				if transitionErr != nil {
					return transitionErr
				}
				if err := state.ApplyIssueTransition(item, transition); err != nil {
					return err
				}
				state.CancelPendingRequests(snapshot, item.Number)
			}
			if item.Suspension != nil {
				item.Suspension.Status = issuedomain.SuspensionResolved
				item.Suspension.Resolution = action
				item.Suspension.ResolvedAt = now
			}
			item.UpdatedAt = now
			return nil
		})
	if err != nil {
		return exitError{4, err}
	}
	if action != issuedomain.ResolutionCancel {
		planned.snapshot = result
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
	revalidated, err := a.buildIssuePlan(ctx, l, repo, number)
	if err != nil {
		return err
	}
	if revalidated.snapshot.StateRevision != planned.snapshot.StateRevision ||
		!reflect.DeepEqual(revalidated.quarantine, planned.quarantine) {
		return exitError{4, fmt.Errorf("Issue #%d quarantine changed after planning", number)}
	}
	result, err := planned.store.Update("issue_quarantine_resolved", number, planned.quarantine.RunID,
		map[string]any{"action": action, "planned_state_revision": planned.snapshot.StateRevision}, func(snapshot *state.Snapshot) error {
			if snapshot.StateRevision != planned.snapshot.StateRevision ||
				!reflect.DeepEqual(snapshot.QuarantinedIssues[strconv.Itoa(number)], planned.quarantine) {
				return fmt.Errorf("Issue #%d quarantine changed after planning", number)
			}
			delete(snapshot.QuarantinedIssues, strconv.Itoa(number))
			return nil
		})
	if err != nil {
		return exitError{4, err}
	}
	return a.output(jsonOut, map[string]any{"schema_version": 1, "issue_number": number, "action": action,
		"status": "quarantine_cleared", "state_revision": result.StateRevision, "idempotent": false})
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

func terminalGitHubStateAligned(cfg config.Config, item *state.Issue, remote gh.Issue) error {
	labels := make(map[string]bool, len(remote.Labels))
	for _, label := range remote.Labels {
		labels[strings.ToLower(label)] = true
	}
	blockedLabel := ""
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			blockedLabel = label
			continue
		}
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("GitHub Issue has a manual exclusion label")
		}
	}
	want := cfg.GitHub.FailedLabel
	if item.Status == issuedomain.StatusBlocked {
		want = blockedLabel
	}
	if want == "" || !labels[strings.ToLower(want)] {
		return fmt.Errorf("GitHub terminal label differs from canonical state")
	}
	for _, incompatible := range append(append([]string{}, cfg.GitHub.ReadyLabels...), cfg.GitHub.RunningLabel, cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel) {
		if incompatible != "" && labels[strings.ToLower(incompatible)] {
			return fmt.Errorf("GitHub lifecycle labels are ambiguous")
		}
	}
	if item.Status == issuedomain.StatusBlocked && cfg.GitHub.FailedLabel != "" && labels[strings.ToLower(cfg.GitHub.FailedLabel)] {
		return fmt.Errorf("GitHub lifecycle labels are ambiguous")
	}
	if item.Status == issuedomain.StatusFailed && blockedLabel != "" && labels[strings.ToLower(blockedLabel)] {
		return fmt.Errorf("GitHub lifecycle labels are ambiguous")
	}
	return nil
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

func (a App) synchronizeIssueResolution(ctx context.Context, planned issuePlanningContext, action issuedomain.ResolutionAction, number int) error {
	client := gh.CLI{Path: planned.ghPath, Secrets: planned.cfg.RedactionValues()}
	var err error
	if action == issuedomain.ResolutionAdoptPR {
		err = client.MarkDone(ctx, planned.cfg, number, planned.issue.PullRequestURL)
	} else {
		err = client.MarkRunning(ctx, planned.cfg, number)
	}
	if err != nil {
		return fmt.Errorf("synchronize Issue #%d resolution %s: %w", number, action, err)
	}
	_, err = planned.store.Update("issue_resolution_github_synced", number, planned.issue.RunID, map[string]any{"action": action}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub synchronization", number)
		}
		expected := issuedomain.EffectApplyResolution
		if action == issuedomain.ResolutionAdoptPR {
			expected = issuedomain.EffectMarkDone
		}
		effect := state.PendingEffect(snapshot, number)
		if effect == nil {
			return nil
		}
		if effect.Kind != expected {
			return fmt.Errorf("Issue #%d resolution synchronization changed", number)
		}
		if err := state.ClearEffect(snapshot, number, effect.ID); err != nil {
			return err
		}
		if action == issuedomain.ResolutionAdoptPR {
			item.Continuation = nil
			item.Suspension = nil
		}
		return nil
	})
	return err
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

package app

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/supervisor"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
)

func (a App) inspectChecksRecovery(ctx context.Context, l layout.Layout, p issuePlanningContext) issuePlanningContext {
	item, err := state.AnsweredChecksRecoveryCandidate(p.quarantine)
	if err != nil {
		return p
	}
	p.issue = item
	observations := make(map[string]any, len(p.report.Observations))
	for key, value := range p.report.Observations {
		observations[key] = value
	}
	p.report.Observations = observations
	p.report.Actions = append([]issueActionPlan(nil), p.report.Actions...)
	action := issueActionPlan{Action: issuedomain.ResolutionRetryStage}
	controller := a.ProcessController
	if controller == nil {
		controller = supervisor.OSProcessGroupController{}
	}
	p.workerLive = controller.Alive(item.WorkerPID) || controller.GroupAlive(item.WorkerPGID)
	if p.workerLive || p.snapshot.ActiveExecution != nil {
		action.Reasons = append(action.Reasons, "repository execution is occupied")
	}
	manager := worktree.Manager{StateRoot: l.Root}
	p.launch, p.launchErr = manager.ValidateLaunch(ctx, p.cfg, item.Worktree, item.Branch)
	p.inspection, p.inspectErr = manager.Inspect(ctx, p.cfg, item.Worktree, item.Branch)
	p.worktreeSHA256, p.worktreeDigestErr = manager.ContentDigest(ctx, item.Worktree)
	p.baseOK, p.baseErr = checkpointBaseAncestor(ctx, "git", item, p.inspection)
	client := gh.CLI{Path: p.ghPath, Secrets: p.cfg.RedactionValues()}
	p.remote, p.remoteErr = client.Inspect(ctx, p.cfg, item.Number, item.Branch)
	headContained, ancestryErr := client.IsCommitAncestor(ctx, p.cfg, p.inspection.Head, item.HeadSHA)
	if p.launchErr != nil || !p.launch.Valid || p.inspectErr != nil || !p.inspection.Valid || !headContained || ancestryErr != nil || p.inspection.RemoteHead != item.HeadSHA || p.inspection.Dirty || p.worktreeDigestErr != nil || p.worktreeSHA256 == "" || !p.baseOK || p.baseErr != nil {
		action.Reasons = append(action.Reasons, "saved published workspace cannot be verified")
	}
	if p.remoteErr != nil || p.remote.Issue.Number != item.Number || p.remote.Issue.State != "OPEN" || len(p.remote.PullRequests) != 1 {
		action.Reasons = append(action.Reasons, "single open published Pull Request cannot be verified")
	} else {
		pr := p.remote.PullRequests[0]
		if pr.Number != item.PullRequestNumber || pr.URL != item.PullRequestURL || pr.State != "OPEN" || pr.MergedAt != nil || pr.HeadSHA != item.HeadSHA || pr.HeadRefName != item.Branch || pr.BaseRefName != p.cfg.Git.BaseBranch || pr.HeadRepository != p.cfg.GitHub.Repo {
			action.Reasons = append(action.Reasons, "published Pull Request identity differs from saved evidence")
		}
	}
	p.report.Observations["published_head_contains_worktree"] = headContained
	p.report.Observations["git_head"] = p.inspection.Head
	p.report.Observations["remote_head"] = p.inspection.RemoteHead
	p.report.Observations["worktree_sha256"] = p.worktreeSHA256
	p.report.Observations["worker_live"] = p.workerLive
	p.report.Observations["checkpoint_base_ancestor"] = p.baseOK
	p.report.Observations["pull_requests"] = p.remote.PullRequests
	action.Eligible = len(action.Reasons) == 0
	for i := range p.report.Actions {
		if p.report.Actions[i].Action == action.Action {
			p.report.Actions[i] = action
		}
	}
	return p
}

func (a App) resolveChecksRecovery(ctx context.Context, l layout.Layout, opts *issueResolveOptions, p issuePlanningContext) error {
	eligible := false
	for _, action := range p.report.Actions {
		if action.Action == issuedomain.ResolutionRetryStage {
			eligible = action.Eligible
		}
	}
	if !eligible {
		return exitError{4, fmt.Errorf("answered checks recovery is not eligible")}
	}
	latest, err := a.buildIssuePlan(ctx, l, opts.repo, opts.number, nil)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p.quarantine, latest.quarantine) || !reflect.DeepEqual(p.report.Observations, latest.report.Observations) || !reflect.DeepEqual(p.report.Actions, latest.report.Actions) {
		return fmt.Errorf("checks recovery evidence changed")
	}
	updated, err := p.store.Update("issue_answered_checks_restored", opts.number, p.quarantine.RunID, map[string]any{"quarantine": p.quarantine, "observations": p.report.Observations}, func(s *state.Snapshot) error {
		if s.StateRevision != latest.snapshot.StateRevision || !reflect.DeepEqual(s.QuarantinedIssues[strconv.Itoa(opts.number)], p.quarantine) {
			return fmt.Errorf("checks recovery state changed")
		}
		if err := verifyRecoveryWorkspace(ctx, latest, l.Root); err != nil {
			return err
		}
		return state.RestoreAnsweredChecks(s, opts.number, p.inspection.Head, p.worktreeSHA256, time.Now().UTC())
	})
	if err != nil {
		return err
	}
	return a.output(opts.jsonOut, map[string]any{"schema_version": 1, "issue_number": opts.number, "action": opts.action, "status": updated.Issues[strconv.Itoa(opts.number)].Status, "state_revision": updated.StateRevision})
}

func (a App) resolveIssueQuarantine(ctx context.Context, l layout.Layout, opts *issueResolveOptions, planned issuePlanningContext) error {
	switch opts.action {
	case issuedomain.ResolutionRetryStage:
		return a.resolveChecksRecovery(ctx, l, opts, planned)
	case issuedomain.ResolutionAdoptInput:
		return a.resolveInputAdoption(ctx, l, opts, planned)
	case issuedomain.ResolutionCancel:
		return a.resolveQuarantinedIssue(ctx, l, opts.repo, opts.number, opts.action, opts.jsonOut, planned)
	default:
		return exitError{4, fmt.Errorf("Issue #%d action is not eligible for quarantine", opts.number)}
	}
}

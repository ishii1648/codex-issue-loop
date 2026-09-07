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

func (a App) inspectInputAdoption(ctx context.Context, l layout.Layout, p issuePlanningContext) issuePlanningContext {
	action := issueActionPlan{Action: issuedomain.ResolutionAdoptInput}
	item, err := state.InputRecoveryCandidate(p.quarantine)
	if err != nil {
		action.Reasons = append(action.Reasons, err.Error())
	} else {
		p.issue = item
		controller := a.ProcessController
		if controller == nil {
			controller = supervisor.OSProcessGroupController{}
		}
		pgid := item.WorkerPGID
		if pgid <= 1 {
			pgid = item.WorkerPID
		}
		p.workerLive = controller.Alive(item.WorkerPID) || controller.GroupAlive(pgid)
		if p.workerLive {
			action.Reasons = append(action.Reasons, "saved worker is still alive")
		}
		if p.snapshot.ActiveExecution != nil {
			action.Reasons = append(action.Reasons, "repository execution is occupied")
		}
		manager := worktree.Manager{StateRoot: l.Root, GitPath: p.gitPath}
		p.launch, p.launchErr = manager.ValidateLaunch(ctx, p.cfg, item.Worktree, item.Branch)
		p.inspection, p.inspectErr = manager.Inspect(ctx, p.cfg, item.Worktree, item.Branch)
		p.worktreeSHA256, p.worktreeDigestErr = manager.ContentDigest(ctx, item.Worktree)
		p.baseOK, p.baseErr = checkpointBaseAncestor(ctx, p.gitPath, item, p.inspection)
		p.remote, p.remoteErr = (gh.CLI{Path: p.ghPath, Secrets: p.cfg.RedactionValues()}).Inspect(ctx, p.cfg, item.Number, item.Branch)
		if p.launchErr != nil || !p.launch.Valid || p.inspectErr != nil || !p.inspection.Valid || p.inspection.Head == "" || p.inspection.RemoteHead != "" || p.worktreeDigestErr != nil || p.worktreeSHA256 == "" || !p.baseOK || p.baseErr != nil {
			action.Reasons = append(action.Reasons, "saved workspace or unpublished branch cannot be verified")
		}
		if p.remoteErr != nil || len(p.remote.PullRequests) != 0 {
			action.Reasons = append(action.Reasons, "absence of publication cannot be verified")
		}
		p.report.Observations["git_head"] = p.inspection.Head
		p.report.Observations["worktree_sha256"] = p.worktreeSHA256
		p.report.Observations["worker_live"] = p.workerLive
		p.report.Observations["checkpoint_base_ancestor"] = p.baseOK
	}
	action.Eligible = len(action.Reasons) == 0
	p.report.Actions = append(p.report.Actions, action)
	return p
}

func (a App) resolveInputAdoption(ctx context.Context, l layout.Layout, opts *issueResolveOptions, p issuePlanningContext) error {
	eligible := false
	for _, action := range p.report.Actions {
		if action.Action == issuedomain.ResolutionAdoptInput {
			eligible = action.Eligible
		}
	}
	if !eligible || opts.expectedHead != p.inspection.Head {
		return exitError{4, fmt.Errorf("input recovery is not eligible or expected HEAD differs")}
	}
	latest, err := a.buildIssuePlan(ctx, l, opts.repo, opts.number, nil)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p.quarantine, latest.quarantine) || !reflect.DeepEqual(p.report.Observations, latest.report.Observations) || !reflect.DeepEqual(p.report.Actions, latest.report.Actions) {
		return fmt.Errorf("input recovery evidence changed")
	}
	if err := verifyAdoptedHead(ctx, latest, opts.expectedHead, l.Root); err != nil {
		return err
	}
	updated, err := p.store.Update("issue_input_checkpoint_adopted", opts.number, p.quarantine.RunID, map[string]any{"quarantine": p.quarantine, "head_sha": opts.expectedHead, "worktree_sha256": p.worktreeSHA256}, func(s *state.Snapshot) error {
		if s.StateRevision != latest.snapshot.StateRevision || !reflect.DeepEqual(s.QuarantinedIssues[strconv.Itoa(opts.number)], p.quarantine) {
			return fmt.Errorf("input recovery state changed")
		}
		if err := verifyAdoptedHead(ctx, latest, opts.expectedHead, l.Root); err != nil {
			return err
		}
		return state.RestoreInput(s, opts.number, opts.expectedHead, p.worktreeSHA256, time.Now().UTC())
	})
	if err != nil {
		return err
	}
	return a.output(opts.jsonOut, map[string]any{"schema_version": 1, "issue_number": opts.number, "action": opts.action, "status": updated.Issues[strconv.Itoa(opts.number)].Status, "state_revision": updated.StateRevision})
}

package app

import (
	"context"
	"fmt"
	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"reflect"
)

func missingResumeHead(item *state.Issue) bool {
	if item == nil || item.Continuation == nil || item.Suspension == nil {
		return false
	}
	c, s := item.Continuation, item.Suspension
	return (item.Status == issuedomain.StatusBlocked || item.Status == issuedomain.StatusFailed) &&
		s.Status == issuedomain.SuspensionActive && s.Origin == "worker" && containsAction(s.AllowedActions, issuedomain.ResolutionResume) &&
		s.CheckpointID == c.ID && c.RunID == item.RunID && c.Generation == item.Generation &&
		c.Stage == issuedomain.ContinuationStageResume && c.HeadSHA == "" && c.WorktreeSHA256 != "" &&
		c.Session != nil && c.Session.ID != "" && c.Workspace != nil && reflect.DeepEqual(c.Workspace, item.Workspace) &&
		item.WorkerPID == 0 && item.WorkerPGID == 0 && item.ConflictRecovery == nil &&
		item.PullRequestURL == "" && item.PullRequestNumber == 0 && c.PullRequestURL == "" && c.PullRequestNumber == 0
}

func verifyAdoptedHead(ctx context.Context, planned issuePlanningContext, head, root string) error {
	manager := worktree.Manager{StateRoot: root}
	inspection, err := manager.Inspect(ctx, planned.cfg, planned.issue.Worktree, planned.issue.Branch)
	if err != nil || !inspection.Valid || inspection.Head != head || inspection.RemoteHead != "" || inspection.Branch != planned.issue.Branch {
		return fmt.Errorf("worktree HEAD changed before adoption")
	}
	digest, err := manager.ContentDigest(ctx, planned.issue.Worktree)
	if err != nil || digest != planned.worktreeSHA256 {
		return fmt.Errorf("worktree content changed before adoption")
	}
	return nil
}

func publicationCheckpointHeadRepair(ctx context.Context, cfg config.Config, ghPath string, item *state.Issue, inspection worktree.Inspection, remote gh.RemoteState) bool {
	if item == nil || item.Continuation == nil || item.Suspension == nil || item.PublicationAudit == nil {
		return false
	}
	c, s := item.Continuation, item.Suspension
	if item.Status != issuedomain.StatusFailed || s.Status != issuedomain.SuspensionActive || s.Origin != "runtime" ||
		!containsAction(s.AllowedActions, issuedomain.ResolutionRetryStage) || s.CheckpointID != c.ID ||
		c.Stage != issuedomain.ContinuationStagePublish || c.RunID != item.RunID || c.Generation != item.Generation ||
		c.HeadSHA == "" || c.HeadSHA != item.HeadSHA || c.HeadSHA == inspection.Head || c.WorktreeSHA256 == "" ||
		c.ResultSHA256 == "" || c.Summary == "" || c.Workspace == nil || !reflect.DeepEqual(c.Workspace, item.Workspace) ||
		item.PublicationAudit.Reason != publication.ReasonPullRequestMismatch || item.WorkerPID != 0 || item.WorkerPGID != 0 || item.ConflictRecovery != nil ||
		!inspection.Valid || !inspection.LocalBranchExists || !inspection.RemoteBranchExists || inspection.Head == "" || inspection.RemoteHead != c.HeadSHA ||
		remote.Issue.State != "OPEN" || remote.Issue.Number != item.Number || len(remote.PullRequests) != 1 {
		return false
	}
	pr := remote.PullRequests[0]
	if pr.State != "OPEN" || pr.MergedAt != nil || pr.Number != item.PullRequestNumber || pr.URL != item.PullRequestURL ||
		pr.Number != c.PullRequestNumber || pr.URL != c.PullRequestURL || pr.HeadSHA != c.HeadSHA ||
		pr.HeadRefName != item.Branch || pr.BaseRefName != cfg.Git.BaseBranch || pr.HeadRepository != cfg.GitHub.Repo {
		return false
	}
	contained, err := (gh.CLI{Path: ghPath, Secrets: cfg.RedactionValues()}).IsCommitAncestor(ctx, cfg, inspection.Head, pr.HeadSHA)
	return err == nil && contained
}

func verifyPublicationCheckpointHeadRepair(ctx context.Context, planned issuePlanningContext, root string) error {
	item := planned.issue
	manager := worktree.Manager{StateRoot: root}
	inspection, inspectErr := manager.Inspect(ctx, planned.cfg, item.Worktree, item.Branch)
	digest, digestErr := manager.ContentDigest(ctx, item.Worktree)
	remote, remoteErr := (gh.CLI{Path: planned.ghPath, Secrets: planned.cfg.RedactionValues()}).Inspect(ctx, planned.cfg, item.Number, item.Branch)
	if inspectErr != nil || digestErr != nil || remoteErr != nil || !reflect.DeepEqual(inspection, planned.inspection) || digest != planned.worktreeSHA256 ||
		!publicationCheckpointHeadRepair(ctx, planned.cfg, planned.ghPath, item, inspection, remote) {
		return fmt.Errorf("Issue #%d publication HEAD evidence changed before recovery", item.Number)
	}
	return nil
}

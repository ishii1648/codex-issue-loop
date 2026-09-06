package app

import (
	"context"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
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

package state

import (
	"fmt"
	"reflect"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// InputRecoveryCandidate accepts only the rejected second-question boundary left by a resolved suspension.
func InputRecoveryCandidate(q *QuarantineRecord) (*Issue, error) {
	if q == nil || q.ReasonCode != "issue_invariant_violation" || q.RejectedStatus != issuedomain.StatusNeedsInput || q.LastValid == nil || len(q.Requests) != 1 {
		return nil, fmt.Errorf("quarantine is not a single rejected input boundary")
	}
	item := q.LastValid
	r := q.Requests[0]
	c, suspension := item.Continuation, item.Suspension
	if r == nil || c == nil || suspension == nil || item.Status != issuedomain.StatusRunning ||
		item.Number != q.IssueNumber || item.RunID != q.RunID || item.Generation != q.Generation ||
		suspension.Status != issuedomain.SuspensionResolved || suspension.CheckpointID != c.ID ||
		r.IssueNumber != item.Number || r.RunID != item.RunID || r.ReleasedExecution == nil ||
		r.ReleasedExecution.RunID != item.RunID || r.ReleasedExecution.Generation != item.Generation ||
		!ValidID(r.ID, "req_") || !ValidID(r.CheckpointID, "checkpoint_") || r.CheckpointID == c.ID || r.CreatedAt.IsZero() ||
		(r.Status != issuedomain.RequestStatusPending && r.Status != issuedomain.RequestStatusAnswered) ||
		item.Session == nil || item.Session.ID == "" || !reflect.DeepEqual(item.Session, c.Session) || item.Workspace == nil || !reflect.DeepEqual(item.Workspace, c.Workspace) ||
		item.ConflictRecovery != nil || item.PullRequestURL != "" || item.PullRequestNumber != 0 || c.BaseSHA == "" {
		return nil, fmt.Errorf("rejected input provenance is incomplete or inconsistent")
	}
	return cloneIssue(item), nil
}

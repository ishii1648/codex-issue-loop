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

func AnsweredChecksRecoveryCandidate(q *QuarantineRecord) (*Issue, error) {
	if q == nil || q.ReasonCode != "issue_invariant_violation" || q.RejectedStatus != issuedomain.StatusLaunching || q.LastValid == nil {
		return nil, fmt.Errorf("quarantine is not a rejected checks retry")
	}
	item := q.LastValid
	c := item.Continuation
	if item.Status != issuedomain.StatusRetryWait || item.Number != q.IssueNumber || item.RunID != q.RunID || item.Generation == 0 || item.Generation+1 != q.Generation ||
		item.WorkerPID != 0 || item.WorkerPGID != 0 || item.Suspension != nil || item.ConflictRecovery != nil ||
		item.PullRequestURL == "" || item.PullRequestNumber <= 0 || item.HeadSHA == "" || item.PullRequestMerged ||
		c == nil || c.Kind != ContinuationKindNeedsInput || c.RequestID == "" || c.RunID != item.RunID || c.Generation != item.Generation || c.BaseSHA == "" ||
		c.PullRequestURL != item.PullRequestURL || c.PullRequestNumber != item.PullRequestNumber || c.Stage != issuedomain.ContinuationStageResume ||
		item.Session == nil || item.Session.ID == "" || item.SessionID != item.Session.ID || !reflect.DeepEqual(item.Session, c.Session) || item.Workspace == nil || !reflect.DeepEqual(item.Workspace, c.Workspace) {
		return nil, fmt.Errorf("answered checks retry provenance is incomplete or inconsistent")
	}
	found := false
	for _, r := range q.Requests {
		if r == nil || r.IssueNumber != item.Number || r.RunID != item.RunID || r.Status != issuedomain.RequestStatusAnswered || r.Answer == "" || r.AnsweredAt == nil || r.AnsweredAt.IsZero() {
			return nil, fmt.Errorf("checks retry contains an unresolved or inconsistent request")
		}
		matches := 0
		for _, answer := range item.Answers {
			if answer.RequestID == r.ID && answer.Question == r.Question && answer.Answer == r.Answer && answer.AnsweredAt.Equal(*r.AnsweredAt) {
				matches++
			}
		}
		if matches != 1 {
			return nil, fmt.Errorf("checks retry answer receipt does not match its history")
		}
		if r.ID == c.RequestID {
			if found || r.CheckpointID != c.ID || r.ReleasedExecution == nil || r.ReleasedExecution.RunID != item.RunID || r.ReleasedExecution.Generation+1 != item.Generation {
				return nil, fmt.Errorf("checks retry does not follow the answered execution")
			}
			found = true
		}
	}
	if !found {
		return nil, fmt.Errorf("checks retry has no matching answered request")
	}
	return cloneIssue(item), nil
}

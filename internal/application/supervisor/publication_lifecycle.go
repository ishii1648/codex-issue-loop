package supervisor

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

func (l *Loop) requestResourceCorrection(ctx context.Context, current state.Issue, audit publication.Audit, detail string) error {
	requestID := state.NewID("req")
	checkpointID := state.NewID("checkpoint")
	requestedAt := l.now()
	question := fmt.Sprintf("Issue #%d has changes outside its declared resource claim. How should the existing worktree be corrected?", current.Number)
	decision, decisionErr := issuedomain.RequestResourceCorrection(current.Status, detail, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide resource correction request", decisionErr)
	}
	_, err := l.Store.Update("resource_claim_mismatch", current.Number, current.RunID, map[string]any{
		"reason": publication.ReasonResourceClaimMismatch, "audit": audit, "checkpoint_id": checkpointID,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID || item.Lease == nil || item.WorkerPID != 0 || item.WorkerPGID != 0 {
			return fmt.Errorf("Issue #%d no longer has a parkable publication boundary", current.Number)
		}
		owner := item.Lease.Owner
		if err := state.CaptureContinuationLease(item, owner, checkpointID, requestedAt); err != nil {
			return err
		}
		item.ResourcePark.Kind = state.ResourceParkKindNeedsInput
		item.ResourcePark.RequestID = requestID
		item.ResourcePark.Stage = issuedomain.ContinuationStageResume
		item.ResourcePark.Evidence = &state.ContinuationEvidence{
			Origin: "publisher", Phase: "resource_audit", Code: publication.ReasonResourceClaimMismatch,
			Status: string(issuedomain.StatusNeedsInput), ObservedAt: requestedAt,
		}
		if err := state.ApplyIssueTransition(item, decision.Transition); err != nil {
			return err
		}
		item.LastError = decision.LastError
		item.FailureKind = decision.FailureKind
		item.GitHubSync = decision.GitHubSync
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		s.PendingRequests[requestID] = &state.Request{
			ID: requestID, IssueNumber: current.Number, Question: question,
			Reason:      "Publication was refused before commit or push because actual_resources is not a subset of declared_resources.",
			Recommended: "revise_diff",
			Options: []state.Option{
				{ID: "revise_diff", Label: "Revise the diff"},
				{ID: "abandon", Label: "Abandon this work"},
			},
			AllowFreeText: true, RunID: current.RunID, ResourceParkID: checkpointID, ReleasedOwner: &owner,
			Status: issuedomain.RequestStatusPending, CreatedAt: requestedAt,
		}
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist resource claim mismatch", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

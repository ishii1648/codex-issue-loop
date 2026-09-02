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
	question := fmt.Sprintf("Issue #%d has changes outside its declared resource claim. How should the existing worktree be corrected?", current.Number)
	decision, decisionErr := issuedomain.RequestResourceCorrection(current.Status, detail, string(failure.Issue))
	if decisionErr != nil {
		return failure.Wrap(failure.Issue, "decide resource correction request", decisionErr)
	}
	_, err := l.Store.Update("resource_claim_mismatch", current.Number, current.RunID, map[string]any{
		"reason": publication.ReasonResourceClaimMismatch, "audit": audit,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
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
			AllowFreeText: true, ResumeStatus: issuedomain.StatusResumePending, Status: issuedomain.RequestStatusPending, CreatedAt: l.now(),
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

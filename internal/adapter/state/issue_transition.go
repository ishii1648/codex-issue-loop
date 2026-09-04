package state

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

// ApplyIssueTransition is the persistence commit boundary for a domain
// lifecycle decision. It fences the write to the status observed when the
// decision was made; ownership fields remain fenced by the surrounding
// transaction.
func ApplyIssueTransition(item *Issue, transition issuedomain.Transition) error {
	if item == nil {
		return fmt.Errorf("cannot apply Issue transition %s to a missing Issue", transition.Name)
	}
	if err := transition.ValidateCommit(item.Status); err != nil {
		return err
	}
	if transition.To.Terminal() && !transition.From.Terminal() && item.Suspension != nil && item.Suspension.Status == issuedomain.SuspensionResolved {
		item.Suspension = nil
	}
	item.Status = transition.To
	if transition.To == issuedomain.StatusCompleted {
		item.Continuation = nil
		item.Suspension = nil
	}
	return nil
}

// ApplyNotPlannedCancellation is the single persistence boundary used by
// startup, periodic, webhook, and safety-sweep reconciliation.
func ApplyNotPlannedCancellation(snapshot *Snapshot, issueNumber int, expected *Issue, now time.Time) (string, error) {
	if snapshot == nil || expected == nil || now.IsZero() {
		return "", fmt.Errorf("not planned cancellation requires snapshot, expected Issue, and time")
	}
	item := snapshot.Issues[strconv.Itoa(issueNumber)]
	if item == nil || expected.Number != issueNumber || item.Status != expected.Status || item.RunID != expected.RunID || item.Generation != expected.Generation {
		return "", fmt.Errorf("Issue #%d changed before not planned cancellation", issueNumber)
	}
	releaseResult := "not_present"
	if active := snapshot.ActiveExecution; active != nil && active.IssueNumber == issueNumber {
		if active.RunID != item.RunID || active.Generation != item.Generation {
			return "", fmt.Errorf("Issue #%d cannot be canceled because execution authority ownership is inconsistent", issueNumber)
		}
	}
	if item.WorkerPID != 0 || item.WorkerPGID != 0 {
		return "", fmt.Errorf("Issue #%d cannot be canceled while worker process identity is present", issueNumber)
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			return "", fmt.Errorf("Issue #%d cannot be canceled while an operator request is pending", issueNumber)
		}
	}
	effect := PendingEffect(snapshot, issueNumber)
	if effect != nil {
		if effect.RunID != item.RunID || effect.Kind != issuedomain.EffectMarkBlocked && effect.Kind != issuedomain.EffectMarkFailed {
			return "", fmt.Errorf("Issue #%d cannot be canceled with pending effect %q", issueNumber, effect.Kind)
		}
	}
	if !strings.EqualFold(item.GitHubStateReason, "NOT_PLANNED") {
		return "", fmt.Errorf("Issue #%d does not have authoritative NOT_PLANNED state reason", issueNumber)
	}
	previous := item.Status
	transition, err := issuedomain.ReconcileObservation(previous, issuedomain.StatusCanceled)
	if err != nil {
		return "", err
	}
	if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issueNumber {
		identity := ExecutionIdentity{RunID: item.RunID, Generation: item.Generation}
		if err := ReleaseExecution(snapshot, issueNumber, identity); err != nil {
			return "", err
		}
		releaseResult = "released"
	}
	if effect != nil {
		delete(snapshot.PendingEffects, strconv.Itoa(issueNumber))
	}
	item.GitHubStateReason = "NOT_PLANNED"
	if err := ApplyIssueTransition(item, transition); err != nil {
		return "", err
	}
	if item.Suspension != nil {
		item.Suspension.Status = issuedomain.SuspensionResolved
		item.Suspension.Resolution = issuedomain.ResolutionCancel
		item.Suspension.ResolvedAt = now.UTC()
	}
	item.Cancellation = &Cancellation{
		Source: "github_not_planned", GitHubStateReason: "NOT_PLANNED", PreviousStatus: previous,
		ExecutionReleaseResult: releaseResult, CanceledAt: now.UTC(),
	}
	item.LastError = ""
	item.RetryAfter = nil
	item.UpdatedAt = now.UTC()
	return releaseResult, nil
}

func CancelPendingRequests(snapshot *Snapshot, issueNumber int) {
	if snapshot == nil {
		return
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			request.Status = issuedomain.RequestStatusCanceled
		}
	}
}

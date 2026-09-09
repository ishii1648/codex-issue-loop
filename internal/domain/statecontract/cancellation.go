package statecontract

import (
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"strconv"
	"strings"
	"time"
)

func ValidateNotPlannedCancellation(snapshot *Snapshot, issueNumber int, expected *Issue, now time.Time) error {
	if snapshot == nil || expected == nil || now.IsZero() {
		return fmt.Errorf("not planned cancellation requires snapshot, expected Issue, and time")
	}
	item := snapshot.Issues[strconv.Itoa(issueNumber)]
	if item == nil || expected.Number != issueNumber || item.Status != expected.Status || item.RunID != expected.RunID || item.Generation != expected.Generation {
		return fmt.Errorf("Issue #%d changed before not planned cancellation", issueNumber)
	}
	if active := snapshot.ActiveExecution; active != nil && active.IssueNumber == issueNumber {
		if active.RunID != item.RunID || active.Generation != item.Generation {
			return fmt.Errorf("Issue #%d cannot be canceled because execution authority ownership is inconsistent", issueNumber)
		}
	}
	if item.WorkerPID != 0 || item.WorkerPGID != 0 {
		return fmt.Errorf("Issue #%d cannot be canceled while worker process identity is present", issueNumber)
	}
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issueNumber && request.Status == issuedomain.RequestStatusPending {
			return fmt.Errorf("Issue #%d cannot be canceled while an operator request is pending", issueNumber)
		}
	}
	effect := snapshot.PendingEffects[strconv.Itoa(issueNumber)]
	if effect != nil {
		if effect.RunID != item.RunID || effect.Kind != issuedomain.EffectMarkBlocked && effect.Kind != issuedomain.EffectMarkFailed {
			return fmt.Errorf("Issue #%d cannot be canceled with pending effect %q", issueNumber, effect.Kind)
		}
	}
	if !strings.EqualFold(item.GitHubStateReason, "NOT_PLANNED") {
		return fmt.Errorf("Issue #%d does not have authoritative NOT_PLANNED state reason", issueNumber)
	}
	return nil
}

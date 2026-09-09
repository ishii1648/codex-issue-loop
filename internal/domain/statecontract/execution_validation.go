package statecontract

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"reflect"
	"strings"
)

func validateExecutionState(snapshot Snapshot) error {
	var executing *Issue
	for _, issue := range snapshot.Issues {
		if issue == nil || !issue.Status.RequiresActiveExecution() {
			continue
		}
		if executing != nil {
			return fmt.Errorf("Issues #%d and #%d both claim the single active execution", executing.Number, issue.Number)
		}
		executing = issue
	}
	if executing == nil {
		if snapshot.ActiveExecution != nil {
			return fmt.Errorf("active execution has no executing Issue")
		}
		return nil
	}
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != executing.Number || active.RunID != executing.RunID ||
		active.Generation != executing.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return fmt.Errorf("Issue #%d does not match repository active execution", executing.Number)
	}
	if executing.Status == issuedomain.StatusLaunching && executing.WorkerPID == 0 && executing.Continuation != nil &&
		executing.Continuation.Kind == ContinuationKindNeedsInput &&
		(executing.LaunchSource == issuedomain.StatusResumePending || executing.LaunchSource == issuedomain.StatusRetryWait) {
		source, reason, ok := LegacyWorkerLaunchSource(&snapshot, executing)
		if !ok || source != executing.LaunchSource {
			if reason == "" {
				reason = fmt.Sprintf("evidence authorizes %s instead of %s", source, executing.LaunchSource)
			}
			return fmt.Errorf("Issue #%d launch source is inconsistent with answered continuation evidence: %s", executing.Number, reason)
		}
	}
	return nil
}
func LegacyWorkerLaunchSource(snapshot *Snapshot, issue *Issue) (issuedomain.Status, string, bool) {
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != issue.Number || active.RunID != issue.RunID ||
		active.Generation != issue.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return issuedomain.StatusUnset, "active execution does not match the Issue run and generation", false
	}

	checkpoint := issue.Continuation
	answeredEvidence := len(issue.Answers) > 0
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issue.Number && request.Status == issuedomain.RequestStatusAnswered {
			answeredEvidence = true
		}
	}
	if checkpoint == nil {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence has no continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind == "" && checkpoint.RequestID == "" {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence is not bound to the continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind != ContinuationKindNeedsInput || checkpoint.RequestID == "" || checkpoint.RunID != issue.RunID ||
		checkpoint.Generation == 0 || checkpoint.Generation >= issue.Generation || checkpoint.Generation+1 != issue.Generation {
		return issuedomain.StatusUnset, "needs-input continuation identity does not match the Issue run and generation", false
	}
	request := snapshot.PendingRequests[checkpoint.RequestID]
	if request == nil || request.ID != checkpoint.RequestID || request.IssueNumber != issue.Number ||
		request.CheckpointID != checkpoint.ID || request.RunID != issue.RunID || request.ReleasedExecution == nil ||
		request.ReleasedExecution.RunID != checkpoint.RunID || request.ReleasedExecution.Generation != checkpoint.Generation ||
		request.Status != issuedomain.RequestStatusAnswered ||
		strings.TrimSpace(request.Answer) == "" || request.AnsweredAt == nil || request.AnsweredAt.IsZero() {
		return issuedomain.StatusUnset, "answered request does not match the continuation identity", false
	}
	answerCount := 0
	for _, answer := range issue.Answers {
		if answer.RequestID != checkpoint.RequestID {
			continue
		}
		answerCount++
		if answer.Question != request.Question || answer.Answer != request.Answer || answer.AnsweredAt.IsZero() || !answer.AnsweredAt.Equal(*request.AnsweredAt) {
			return issuedomain.StatusUnset, "recorded answer does not match the answered request", false
		}
	}
	if answerCount != 1 {
		return issuedomain.StatusUnset, "answered request does not have exactly one matching answer record", false
	}
	return issuedomain.StatusResumePending, "", true
}
func ValidSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

const ContinuationKindNeedsInput = "needs_input"

func ValidateUnstartedConflictLaunch(snapshot *Snapshot, issue *Issue) error {
	if snapshot == nil || issue == nil || snapshot.ActiveExecution == nil {
		return fmt.Errorf("unstarted conflict launch evidence is incomplete")
	}
	active := snapshot.ActiveExecution
	if issue.Status != issuedomain.StatusLaunching || issue.LaunchSource != issuedomain.StatusResolvingConflict ||
		issue.WorkerPID != 0 || issue.WorkerPGID != 0 || active.IssueNumber != issue.Number ||
		active.RunID != issue.RunID || active.Generation != issue.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return fmt.Errorf("Issue #%d unstarted conflict launch execution identity is inconsistent", issue.Number)
	}
	checkpoint := issue.Continuation
	if checkpoint == nil || checkpoint.ID == "" || checkpoint.RunID != issue.RunID || checkpoint.Generation == 0 || checkpoint.Generation >= issue.Generation ||
		active.StartedAt.Before(checkpoint.CreatedAt) ||
		checkpoint.BaseSHA != active.BaseSHA || checkpoint.HeadSHA != issue.HeadSHA || checkpoint.PullRequestURL != issue.PullRequestURL ||
		checkpoint.PullRequestNumber != issue.PullRequestNumber || !reflect.DeepEqual(checkpoint.Workspace, issue.Workspace) {
		return fmt.Errorf("Issue #%d unstarted conflict launch continuation identity is inconsistent", issue.Number)
	}
	if checkpoint.Stage != issuedomain.ContinuationStageChecks && checkpoint.Stage != issuedomain.ContinuationStageConflict {
		return fmt.Errorf("Issue #%d unstarted conflict launch continuation stage is inconsistent", issue.Number)
	}
	recovery := issue.ConflictRecovery
	if recovery == nil || recovery.PullRequestURL == "" || recovery.PullRequestURL != issue.PullRequestURL ||
		recovery.PreviousBaseSHA != active.BaseSHA || recovery.TargetBaseSHA == "" || recovery.OriginalHeadSHA != issue.HeadSHA || len(recovery.ConflictFiles) == 0 {
		return fmt.Errorf("Issue #%d unstarted conflict launch recovery context is inconsistent", issue.Number)
	}
	return nil
}

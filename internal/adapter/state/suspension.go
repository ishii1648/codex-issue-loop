package state

import (
	"sort"
	"strconv"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func captureContinuation(issue *Issue, active ActiveExecution, now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if issue.Continuation == nil {
		issue.Continuation = &ContinuationCheckpoint{ID: NewID("checkpoint")}
	}
	checkpoint := issue.Continuation
	checkpoint.CreatedAt = now.UTC()
	checkpoint.RunID = active.RunID
	checkpoint.Generation = active.Generation
	checkpoint.BaseSHA = active.BaseSHA
	checkpoint.Workspace = cloneWorkspace(issue.Workspace)
	checkpoint.Session = cloneSession(issue.Session)
	checkpoint.HeadSHA = issue.HeadSHA
	checkpoint.PullRequestURL = issue.PullRequestURL
	checkpoint.PullRequestNumber = issue.PullRequestNumber
	if checkpoint.Stage.Validate() != nil {
		checkpoint.Stage = issuedomain.ContinuationStageForStatus(issue.Status)
	}
}

// finalizeLifecycleBoundaries releases repository execution authority whenever
// the lifecycle no longer executes a worker. Continuation evidence is kept on
// the Issue and never represents capacity ownership.
func finalizeLifecycleBoundaries(snapshot *Snapshot, now time.Time) {
	if snapshot == nil {
		return
	}
	if active := snapshot.ActiveExecution; active != nil {
		issue := snapshot.Issues[strconv.Itoa(active.IssueNumber)]
		if issue == nil || !issue.Status.RequiresActiveExecution() {
			if issue != nil && issue.Status != issuedomain.StatusCompleted && issue.Status != issuedomain.StatusCanceled {
				captureContinuation(issue, *active, now)
			}
			if issue != nil {
				issue.WorkerPID, issue.WorkerPGID = 0, 0
			}
			snapshot.ActiveExecution = nil
		}
	}
	for _, issue := range snapshot.Issues {
		if issue == nil || !issue.Status.Terminal() {
			continue
		}
		if issue.Status == issuedomain.StatusCompleted {
			issue.Continuation = nil
			issue.Suspension = nil
			continue
		}
		ensureTerminalSuspension(issue, now)
	}
}

func ensureTerminalSuspension(issue *Issue, now time.Time) {
	if issue == nil || !issue.Status.Terminal() || issue.Status == issuedomain.StatusCompleted || issue.Status == issuedomain.StatusCanceled || issue.Suspension != nil {
		return
	}
	reasonCode, recoverability := "terminal", issuedomain.RecoverabilityOperator
	if strings.TrimSpace(issue.FailureKind) != "" {
		reasonCode = strings.TrimSpace(issue.FailureKind)
	}
	missing := []string{}
	actions := []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}
	if issue.Worktree == "" || issue.Workspace == nil {
		missing = append(missing, "workspace")
	}
	if issue.PullRequestURL != "" {
		actions = append(actions, issuedomain.ResolutionAdoptPR, issuedomain.ResolutionRetryStage)
	}
	if issue.Continuation != nil {
		if issue.Continuation.Kind != "" {
			reasonCode = issue.Continuation.Kind
		}
		switch issue.Continuation.Stage {
		case issuedomain.ContinuationStagePublish, issuedomain.ContinuationStageChecks, issuedomain.ContinuationStageConflict:
			actions = append(actions, issuedomain.ResolutionRetryStage)
		default:
			actions = append(actions, issuedomain.ResolutionResume)
		}
		if (issue.Continuation.Stage == issuedomain.ContinuationStageResume || issue.Continuation.Stage == issuedomain.ContinuationStagePublish) &&
			strings.TrimSpace(issue.Continuation.WorktreeSHA256) == "" {
			missing = append(missing, "worktree_sha256")
			recoverability = issuedomain.RecoverabilityAmbiguous
			actions = []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}
		}
	}
	if len(missing) > 0 && issue.Worktree == "" {
		recoverability = issuedomain.RecoverabilityAmbiguous
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })
	actions = uniqueResolutionActions(actions)
	reason := strings.TrimSpace(issue.LastError)
	if reason == "" {
		reason = "terminal lifecycle boundary"
	}
	checkpointID := ""
	if issue.Continuation != nil {
		checkpointID = issue.Continuation.ID
	}
	status := issuedomain.SuspensionActive
	if recoverability == issuedomain.RecoverabilityAmbiguous {
		status = issuedomain.SuspensionQuarantined
	}
	issue.Suspension = &Suspension{
		ID: NewID("suspension"), Origin: "runtime", Status: status,
		ReasonCode: reasonCode, Recoverability: recoverability, Reason: reason,
		MissingEvidence: missing, AllowedActions: actions, CheckpointID: checkpointID, SuspendedAt: now.UTC(),
	}
}

func uniqueResolutionActions(actions []issuedomain.ResolutionAction) []issuedomain.ResolutionAction {
	result := actions[:0]
	for _, action := range actions {
		if len(result) == 0 || result[len(result)-1] != action {
			result = append(result, action)
		}
	}
	return result
}

func cloneWorkspace(workspace *WorkerWorkspace) *WorkerWorkspace {
	if workspace == nil {
		return nil
	}
	copy := *workspace
	return &copy
}

func cloneSession(session *WorkerSession) *WorkerSession {
	if session == nil {
		return nil
	}
	copy := *session
	return &copy
}

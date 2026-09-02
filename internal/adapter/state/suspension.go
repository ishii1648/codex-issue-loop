package state

import (
	"sort"
	"strings"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func captureContinuationCheckpoint(issue *Issue, lease ExecutionLease, now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	canonicalizeCheckpointLease(&lease)
	if issue.ResourcePark != nil {
		checkpoint := issue.ResourcePark
		checkpoint.Status = issuedomain.ResourceParkStatusParked
		checkpoint.OriginalLease = lease
		checkpoint.ParkedAt = now.UTC()
		checkpoint.ResumedAt = time.Time{}
		checkpoint.ResumeOwner = nil
		checkpoint.RunID = issue.RunID
		checkpoint.Workspace = cloneWorkspace(issue.Workspace)
		checkpoint.Session = cloneSession(issue.Session)
		checkpoint.HeadSHA = issue.HeadSHA
		checkpoint.PullRequestURL = issue.PullRequestURL
		checkpoint.PullRequestNumber = issue.PullRequestNumber
		if checkpoint.Stage.Validate() != nil {
			checkpoint.Stage = issuedomain.ContinuationStageForStatus(issue.Status)
		}
		return
	}
	checkpoint := &ContinuationCheckpoint{
		ID:                NewID("checkpoint"),
		Status:            issuedomain.ResourceParkStatusParked,
		OriginalLease:     lease,
		ParkedAt:          now.UTC(),
		RunID:             issue.RunID,
		Workspace:         cloneWorkspace(issue.Workspace),
		Session:           cloneSession(issue.Session),
		HeadSHA:           issue.HeadSHA,
		PullRequestURL:    issue.PullRequestURL,
		PullRequestNumber: issue.PullRequestNumber,
		Stage:             issuedomain.ContinuationStageForStatus(issue.Status),
	}
	issue.ResourcePark = checkpoint
}

// finalizeLifecycleBoundaries enforces the generic lease/checkpoint/suspension
// split after a transaction's domain transition and outcome fields are set.
func finalizeLifecycleBoundaries(snapshot *Snapshot, now time.Time) {
	if snapshot == nil {
		return
	}
	for _, item := range snapshot.Issues {
		if item == nil {
			continue
		}
		if !item.Status.Terminal() {
			continue
		}
		if item.Lease != nil {
			lease := *item.Lease
			lease.DeclaredResources = append([]string(nil), item.Lease.DeclaredResources...)
			lease.ResolvedResources = append([]string(nil), item.Lease.ResolvedResources...)
			lease.ActualResources = append([]string(nil), item.Lease.ActualResources...)
			if item.Status != issuedomain.StatusCompleted {
				captureContinuationCheckpoint(item, lease, now)
			}
			item.Lease = nil
		}
		if item.Status == issuedomain.StatusCompleted {
			pendingAdoptionSync := item.GitHubSync == issuedomain.GitHubSyncDone && item.Suspension != nil &&
				item.Suspension.Status == issuedomain.SuspensionResolved && item.Suspension.Resolution == issuedomain.ResolutionAdoptPR
			if !pendingAdoptionSync {
				item.ResourcePark = nil
				item.Suspension = nil
			}
			continue
		}
		ensureTerminalSuspension(item, now)
	}
}

func canonicalizeCheckpointLease(lease *ExecutionLease) {
	if lease == nil {
		return
	}
	if declared, err := normalizeResources(lease.DeclaredResources, true); err == nil {
		lease.DeclaredResources = declared
	}
	if resolved, err := normalizeResources(lease.ResolvedResources, false); err == nil {
		lease.ResolvedResources = resolved
	}
	if len(lease.ActualResources) > 0 {
		if actual, err := normalizeResources(lease.ActualResources, true); err == nil {
			lease.ActualResources = actual
		}
	}
}

func ensureTerminalSuspension(issue *Issue, now time.Time) {
	if issue == nil || !issue.Status.Terminal() || issue.Status == issuedomain.StatusCompleted {
		return
	}
	if issue.Suspension != nil {
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
	if issue.ResourcePark != nil {
		if issue.ResourcePark.Kind != "" {
			reasonCode = issue.ResourcePark.Kind
		}
		switch issue.ResourcePark.Stage {
		case issuedomain.ContinuationStagePublish, issuedomain.ContinuationStageChecks, issuedomain.ContinuationStageConflict:
			actions = append(actions, issuedomain.ResolutionRetryStage)
		default:
			actions = append(actions, issuedomain.ResolutionResume)
		}
		if (issue.ResourcePark.Stage == issuedomain.ContinuationStageResume || issue.ResourcePark.Stage == issuedomain.ContinuationStagePublish) &&
			strings.TrimSpace(issue.ResourcePark.WorktreeSHA256) == "" {
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
	if issue.ResourcePark != nil {
		checkpointID = issue.ResourcePark.ID
	}
	status := issuedomain.SuspensionActive
	if recoverability == issuedomain.RecoverabilityAmbiguous {
		status = issuedomain.SuspensionQuarantined
	}
	issue.Suspension = &Suspension{
		ID: NewID("suspension"), Origin: "runtime", Status: status,
		ReasonCode: reasonCode, Recoverability: recoverability, Reason: reason,
		MissingEvidence: missing, AllowedActions: actions, CheckpointID: checkpointID,
		SuspendedAt: now.UTC(),
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

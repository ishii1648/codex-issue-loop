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
		if checkpoint.Stage == issuedomain.StatusUnset || checkpoint.Stage.Terminal() {
			checkpoint.Stage = issue.Status
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
		Stage:             issue.Status,
	}
	issue.ResourcePark = checkpoint
}

func normalizeLifecycleBoundaries(snapshot *Snapshot, now time.Time) {
	if snapshot == nil {
		return
	}
	for _, item := range snapshot.Issues {
		if item == nil {
			continue
		}
		if !item.Status.Terminal() {
			resolveExecutingSuspension(item, now)
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
			item.ResourcePark = nil
			item.Suspension = nil
			continue
		}
		ensureTerminalSuspension(item, now)
	}
}

// resolveExecutingSuspension keeps the v5 boundary compatible with one-release
// transition callers that already reacquired an ExecutionLease before changing
// lifecycle state. A quarantined suspension is never inferred as resolved.
func resolveExecutingSuspension(issue *Issue, now time.Time) {
	if issue == nil || issue.Lease == nil || issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionActive {
		return
	}
	action := issuedomain.ResolutionRetryStage
	if issue.Status == issuedomain.StatusResumePending || issue.Status == issuedomain.StatusEnvironmentResumePending {
		action = issuedomain.ResolutionResume
	}
	if !containsResolutionAction(issue.Suspension.AllowedActions, action) {
		return
	}
	issue.Suspension.Status = issuedomain.SuspensionResolved
	issue.Suspension.Resolution = action
	issue.Suspension.ResolvedAt = now.UTC()
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
	missing := []string{}
	actions := []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}
	if issue.BlockedCause != nil {
		reasonCode = strings.TrimSpace(issue.BlockedCause.Kind)
		if issue.BlockedCause.Resumable {
			actions = append(actions, issuedomain.ResolutionResume)
		}
	}
	if issue.Worktree == "" || issue.Workspace == nil {
		missing = append(missing, "workspace")
	}
	if issue.PullRequestURL != "" {
		actions = append(actions, issuedomain.ResolutionAdoptPR, issuedomain.ResolutionRetryStage)
	}
	if issue.ResourcePark != nil {
		actions = append(actions, issuedomain.ResolutionResume)
	}
	if len(missing) > 0 && issue.Worktree == "" {
		recoverability = issuedomain.RecoverabilityAmbiguous
	}
	sort.Slice(actions, func(i, j int) bool { return actions[i] < actions[j] })
	actions = uniqueResolutionActions(actions)
	reason := strings.TrimSpace(issue.LastError)
	if issue.BlockedCause != nil && strings.TrimSpace(issue.BlockedCause.Reason) != "" {
		reason = strings.TrimSpace(issue.BlockedCause.Reason)
	}
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
		ID: NewID("suspension"), Status: status,
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

func containsResolutionAction(actions []issuedomain.ResolutionAction, target issuedomain.ResolutionAction) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
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

package statecontract

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"sort"
)

func (s Snapshot) Attention(untilIdle, autoMerge bool) (string, bool) {
	requests := make([]string, 0)
	for _, request := range s.Requests() {
		if request.Status == issuedomain.RequestStatusPending {
			requests = append(requests, request.ID)
		}
	}
	if len(requests) > 0 {
		sort.Strings(requests)
		return "needs_input", true
	}
	if len(s.HumanNeeds(autoMerge)) > 0 {
		return "needs_human", true
	}
	for _, issue := range s.Issues {
		if issue != nil && issue.Status == issuedomain.StatusBlocked {
			return "blocked", true
		}
	}
	if s.Supervisor.State == SupervisorStateBlocked || s.Supervisor.State == SupervisorStateStopped {
		return string(s.Supervisor.State), true
	}
	if untilIdle && s.Supervisor.State == SupervisorStatePolling {
		if len(s.PendingEffects) > 0 {
			return "", false
		}
		for _, issue := range s.Issues {
			if issue.Status.PreventsIdle() {
				return "", false
			}
		}
		return "idle", true
	}
	return "", false
}

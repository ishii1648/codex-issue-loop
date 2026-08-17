package app

import (
	"sort"

	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
)

type statusResult struct {
	Launchd         launchd.Status   `json:"launchd"`
	WorkerPool      workerPoolStatus `json:"worker_pool"`
	PendingRequests []*state.Request `json:"pending_requests"`
	State           state.Snapshot   `json:"state"`
	Broker          *brokerStatus    `json:"broker,omitempty"`
}

type brokerStatus struct {
	Launchd launchd.Status     `json:"launchd"`
	State   webhook.Status     `json:"state"`
	Sweep   webhook.SweepState `json:"repository_safety_sweep"`
}

type workerPoolStatus struct {
	Active    int                 `json:"active"`
	Limit     int                 `json:"limit"`
	Available int                 `json:"available"`
	Issues    []activeIssueStatus `json:"issues"`
}

type activeIssueStatus struct {
	IssueNumber int               `json:"issue_number"`
	RunID       string            `json:"run_id"`
	Phase       string            `json:"phase"`
	PID         int               `json:"pid,omitempty"`
	PGID        int               `json:"pgid,omitempty"`
	Slot        *int              `json:"slot,omitempty"`
	Resources   []string          `json:"resources"`
	Owner       *state.LeaseOwner `json:"resource_owner,omitempty"`
}

func buildStatus(launchStatus launchd.Status, snapshot state.Snapshot, limit int) statusResult {
	if limit < 1 {
		limit = 1
	}
	issues := make([]activeIssueStatus, 0)
	for _, issue := range snapshot.Issues {
		if issue == nil || !occupiesWorkerSlot(issue.Status) {
			continue
		}
		value := activeIssueStatus{
			IssueNumber: issue.Number, RunID: issue.RunID, Phase: issue.Status,
			PID: issue.WorkerPID, PGID: issue.WorkerPGID, Resources: []string{},
		}
		if issue.Lease != nil {
			slot := issue.Lease.Slot
			owner := issue.Lease.Owner
			value.Slot = &slot
			value.Resources = append([]string(nil), issue.Lease.ResolvedResources...)
			value.Owner = &owner
		}
		issues = append(issues, value)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].IssueNumber < issues[j].IssueNumber })
	requests := make([]*state.Request, 0)
	for _, request := range snapshot.PendingRequests {
		if request == nil || request.Status != "pending" {
			continue
		}
		copy := *request
		copy.Options = append([]state.Option(nil), request.Options...)
		requests = append(requests, &copy)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	available := limit - len(issues)
	if available < 0 {
		available = 0
	}
	return statusResult{
		Launchd:         launchStatus,
		WorkerPool:      workerPoolStatus{Active: len(issues), Limit: limit, Available: available, Issues: issues},
		PendingRequests: requests,
		State:           snapshot,
	}
}

func occupiesWorkerSlot(status string) bool {
	switch status {
	case "claiming", "claimed", "running", "resolving_conflict":
		return true
	default:
		return false
	}
}

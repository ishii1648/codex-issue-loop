package app

import (
	"fmt"
	"sort"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
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
	Queue   queueHealth        `json:"queue_health"`
}

type workerPoolStatus struct {
	Active    int                 `json:"active"`
	Limit     int                 `json:"limit"`
	Available int                 `json:"available"`
	Issues    []activeIssueStatus `json:"issues"`
}

type activeIssueStatus struct {
	IssueNumber int    `json:"issue_number"`
	RunID       string `json:"run_id"`
	Generation  uint64 `json:"generation"`
	Phase       string `json:"phase"`
	PID         int    `json:"pid,omitempty"`
	PGID        int    `json:"pgid,omitempty"`
}

func buildStatus(launchStatus launchd.Status, snapshot state.Snapshot, _ int) statusResult {
	issues := make([]activeIssueStatus, 0)
	if active := snapshot.ActiveExecution; active != nil {
		issue := snapshot.Issues[fmt.Sprint(active.IssueNumber)]
		if issue != nil {
			value := activeIssueStatus{
				IssueNumber: issue.Number, RunID: active.RunID, Generation: active.Generation,
				Phase: issue.Status.String(), PID: issue.WorkerPID, PGID: issue.WorkerPGID,
			}
			issues = append(issues, value)
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].IssueNumber < issues[j].IssueNumber })
	requests := make([]*state.Request, 0)
	for _, request := range snapshot.PendingRequests {
		if request == nil || request.Status != issuedomain.RequestStatusPending {
			continue
		}
		copy := *request
		copy.Options = append([]state.Option(nil), request.Options...)
		requests = append(requests, &copy)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	available := 1 - len(issues)
	return statusResult{
		Launchd: launchStatus, WorkerPool: workerPoolStatus{Active: len(issues), Limit: 1, Available: available, Issues: issues},
		PendingRequests: requests, State: snapshot,
	}
}

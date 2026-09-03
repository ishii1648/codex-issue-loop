package app

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/launchd"
)

func TestStatusSummarizesSingleActiveExecutionAndRequests(t *testing.T) {
	now := time.Now().UTC()
	snapshot := state.Snapshot{
		Supervisor: state.Supervisor{RateLimit: &state.RateLimit{
			Resource: "graphql", ObservedResetAt: now.Add(time.Hour),
			CooldownSource: "rest-rate-limit", SuppressedRetryCount: 17,
		}},
		ActiveExecution: &state.ActiveExecution{IssueNumber: 9, RunID: "run_9", Generation: 2, StartedAt: now},
		Issues: map[string]*state.Issue{
			"9": {
				Number: 9, RunID: "run_9", Status: issuedomain.StatusRunning, WorkerPID: 109, WorkerPGID: 109,
			},
			"4": {Number: 4, RunID: "run_4", Status: issuedomain.StatusNeedsInput},
		},
		PendingRequests: map[string]*state.Request{
			"req_z": {ID: "req_z", IssueNumber: 9, Status: issuedomain.RequestStatusAnswered},
			"req_b": {ID: "req_b", IssueNumber: 4, Status: issuedomain.RequestStatusPending},
		},
	}
	result := buildStatus(launchd.Status{Loaded: true}, snapshot, 3)
	if result.WorkerPool.Active != 1 || result.WorkerPool.Limit != 1 || result.WorkerPool.Available != 0 {
		t.Fatalf("pool=%+v", result.WorkerPool)
	}
	if len(result.WorkerPool.Issues) != 1 || result.WorkerPool.Issues[0].IssueNumber != 9 || result.WorkerPool.Issues[0].RunID != "run_9" || result.WorkerPool.Issues[0].Generation != 2 {
		t.Fatalf("issues=%+v", result.WorkerPool.Issues)
	}
	if len(result.PendingRequests) != 1 || result.PendingRequests[0].ID != "req_b" {
		t.Fatalf("requests=%+v", result.PendingRequests)
	}
	if result.State.Supervisor.RateLimit == nil || result.State.Supervisor.RateLimit.Resource != "graphql" || result.State.Supervisor.RateLimit.SuppressedRetryCount != 17 {
		t.Fatalf("rate_limit=%+v", result.State.Supervisor.RateLimit)
	}
}

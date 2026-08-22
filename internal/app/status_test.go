package app

import (
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestStatusSummarizesMultipleWorkersResourcesAndRequests(t *testing.T) {
	now := time.Now().UTC()
	snapshot := state.Snapshot{
		Supervisor: state.Supervisor{RateLimit: &state.RateLimit{
			Resource: "graphql", ObservedResetAt: now.Add(time.Hour),
			CooldownSource: "rest-rate-limit", SuppressedRetryCount: 17,
		}},
		Issues: map[string]*state.Issue{
			"9": {
				Number: 9, RunID: "run_9", Status: issuedomain.StatusRunning, WorkerPID: 109, WorkerPGID: 109,
				Lease: &state.ResourceLease{Owner: state.LeaseOwner{RunID: "run_9", Generation: 2}, Slot: 1, ResolvedResources: []string{"cli"}, ReservedAt: now},
			},
			"3": {
				Number: 3, RunID: "run_3", Status: issuedomain.StatusClaiming,
				Lease: &state.ResourceLease{Owner: state.LeaseOwner{RunID: "run_3", Generation: 1}, Slot: 0, ResolvedResources: []string{"reconcile"}, ReservedAt: now},
			},
			"4": {Number: 4, RunID: "run_4", Status: issuedomain.StatusNeedsInput},
			"5": {
				Number: 5, RunID: "run_5", Status: issuedomain.StatusBlocked, LeaseGeneration: 1,
				ResourcePark: &state.ResourceLeasePark{
					ID: "park_5", Status: issuedomain.ResourceParkStatusParked, ParkedAt: now,
					OriginalLease: state.ResourceLease{Owner: state.LeaseOwner{RunID: "run_5", Generation: 1}, Slot: 2, ResolvedResources: []string{state.RepositoryResource}, ReservedAt: now},
				},
			},
		},
		PendingRequests: map[string]*state.Request{
			"req_z": {ID: "req_z", IssueNumber: 9, Status: issuedomain.RequestStatusAnswered},
			"req_b": {ID: "req_b", IssueNumber: 4, Status: issuedomain.RequestStatusPending},
			"req_a": {ID: "req_a", IssueNumber: 3, Status: issuedomain.RequestStatusPending},
		},
	}
	result := buildStatus(launchd.Status{Loaded: true}, snapshot, 3)
	if result.WorkerPool.Active != 2 || result.WorkerPool.Limit != 3 || result.WorkerPool.Available != 1 {
		t.Fatalf("pool=%+v", result.WorkerPool)
	}
	if len(result.WorkerPool.Issues) != 2 || result.WorkerPool.Issues[0].IssueNumber != 3 || result.WorkerPool.Issues[0].Slot == nil || *result.WorkerPool.Issues[0].Slot != 0 || result.WorkerPool.Issues[1].Owner.RunID != "run_9" {
		t.Fatalf("issues=%+v", result.WorkerPool.Issues)
	}
	if len(result.PendingRequests) != 2 || result.PendingRequests[0].ID != "req_a" || result.PendingRequests[1].ID != "req_b" {
		t.Fatalf("requests=%+v", result.PendingRequests)
	}
	if len(result.ResourceAdmission.ResourceParks) != 1 || result.ResourceAdmission.ResourceParks[0].IssueNumber != 5 || len(result.ResourceAdmission.ClaimWaitingCandidates) != 1 {
		t.Fatalf("resource_admission=%+v", result.ResourceAdmission)
	}
	waiting := result.ResourceAdmission.ClaimWaitingCandidates[0]
	if len(waiting.BlockedBy) != 2 || waiting.BlockedBy[0].IssueNumber != 3 || waiting.BlockedBy[1].IssueNumber != 9 || waiting.BlockedBy[1].Reasons[0] != "resource_conflict" {
		t.Fatalf("claim waiting blockers=%+v", waiting.BlockedBy)
	}
	if result.State.Supervisor.RateLimit == nil || result.State.Supervisor.RateLimit.Resource != "graphql" || result.State.Supervisor.RateLimit.SuppressedRetryCount != 17 {
		t.Fatalf("rate_limit=%+v", result.State.Supervisor.RateLimit)
	}
}

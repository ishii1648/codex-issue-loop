package app

import (
	"sort"

	"github.com/ishii1648/codex-issue-loop/internal/launchd"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
)

type statusResult struct {
	Launchd           launchd.Status          `json:"launchd"`
	WorkerPool        workerPoolStatus        `json:"worker_pool"`
	ResourceAdmission resourceAdmissionStatus `json:"resource_admission"`
	PendingRequests   []*state.Request        `json:"pending_requests"`
	State             state.Snapshot          `json:"state"`
	Broker            *brokerStatus           `json:"broker,omitempty"`
}

type resourceAdmissionStatus struct {
	ResourceParks          []parkedClaimStatus `json:"resource_parks"`
	ClaimWaitingCandidates []parkedClaimStatus `json:"claim_waiting_candidates"`
}

type parkedClaimStatus struct {
	IssueNumber int               `json:"issue_number"`
	RunID       string            `json:"run_id"`
	ParkID      string            `json:"park_id"`
	Status      string            `json:"status"`
	Owner       state.LeaseOwner  `json:"released_owner"`
	ResumeOwner *state.LeaseOwner `json:"resume_owner,omitempty"`
	Slot        int               `json:"saved_slot"`
	Resources   []string          `json:"saved_resources"`
	BlockedBy   []resourceBlocker `json:"blocked_by"`
}

type resourceBlocker struct {
	IssueNumber int      `json:"issue_number"`
	Resources   []string `json:"resources"`
	Reasons     []string `json:"reasons"`
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
	parked := make([]parkedClaimStatus, 0)
	waiting := make([]parkedClaimStatus, 0)
	for _, issue := range snapshot.Issues {
		if issue == nil || issue.ResourcePark == nil {
			continue
		}
		claim := parkedClaimStatus{
			IssueNumber: issue.Number, RunID: issue.RunID, ParkID: issue.ResourcePark.ID, Status: issue.ResourcePark.Status,
			Owner: issue.ResourcePark.OriginalLease.Owner, ResumeOwner: issue.ResourcePark.ResumeOwner, Slot: issue.ResourcePark.OriginalLease.Slot,
			Resources: append([]string(nil), issue.ResourcePark.OriginalLease.ResolvedResources...), BlockedBy: []resourceBlocker{},
		}
		if issue.ResourcePark.Status == "parked" {
			blockers := map[int]*resourceBlocker{}
			occupiedSlots := 0
			for _, other := range snapshot.Issues {
				if other == nil || other.Number == issue.Number || other.Lease == nil {
					continue
				}
				if state.ResourcesConflict(claim.Resources, other.Lease.ResolvedResources) {
					blockers[other.Number] = &resourceBlocker{IssueNumber: other.Number, Resources: append([]string(nil), other.Lease.ResolvedResources...), Reasons: []string{"resource_conflict"}}
				}
				if occupiesWorkerSlot(other.Status) {
					occupiedSlots++
				}
			}
			if limit < 1 {
				limit = 1
			}
			if occupiedSlots >= limit {
				for _, other := range snapshot.Issues {
					if other == nil || other.Number == issue.Number || other.Lease == nil || !occupiesWorkerSlot(other.Status) {
						continue
					}
					blocker := blockers[other.Number]
					if blocker == nil {
						blocker = &resourceBlocker{IssueNumber: other.Number, Resources: append([]string(nil), other.Lease.ResolvedResources...)}
						blockers[other.Number] = blocker
					}
					blocker.Reasons = append(blocker.Reasons, "worker_slot")
				}
			}
			for _, blocker := range blockers {
				claim.BlockedBy = append(claim.BlockedBy, *blocker)
			}
		}
		sort.Slice(claim.BlockedBy, func(i, j int) bool { return claim.BlockedBy[i].IssueNumber < claim.BlockedBy[j].IssueNumber })
		parked = append(parked, claim)
		if len(claim.BlockedBy) > 0 {
			waiting = append(waiting, claim)
		}
	}
	sort.Slice(parked, func(i, j int) bool { return parked[i].IssueNumber < parked[j].IssueNumber })
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].IssueNumber < waiting[j].IssueNumber })
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
		Launchd:           launchStatus,
		WorkerPool:        workerPoolStatus{Active: len(issues), Limit: limit, Available: available, Issues: issues},
		ResourceAdmission: resourceAdmissionStatus{ResourceParks: parked, ClaimWaitingCandidates: waiting},
		PendingRequests:   requests,
		State:             snapshot,
	}
}

func occupiesWorkerSlot(status string) bool {
	switch status {
	case "claiming", "claimed", "running", "environment_resume_pending", "resolving_conflict":
		return true
	default:
		return false
	}
}

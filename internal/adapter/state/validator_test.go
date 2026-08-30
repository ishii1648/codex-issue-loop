package state

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
)

func validSnapshotForInvariantTest() Snapshot {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	return Snapshot{
		Version: CurrentVersion, SemanticContractVersion: statecontract.CurrentVersion,
		RepoID: "repo-deadbeef", RepoPath: "/tmp/repo",
		Supervisor: Supervisor{State: SupervisorStateStopped, UpdatedAt: now},
		Issues:     map[string]*Issue{"1": {Number: 1}}, PendingRequests: map[string]*Request{},
	}
}

func TestSnapshotValidateRejectsEveryCrossFieldInvariantClass(t *testing.T) {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "status and GitHub sync", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Status = issuedomain.StatusRunning
			snapshot.Issues["1"].GitHubSync = issuedomain.GitHubSyncDone
		}},
		{name: "run and lease generation", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.LeaseGeneration = issuedomain.StatusClaiming, "run_1", 2
			issue.Lease = &ResourceLease{Owner: LeaseOwner{RunID: "run_1", Generation: 1}, Slot: 0, ResolvedResources: []string{"state"}, ReservedAt: now}
		}},
		{name: "active lease and parked lease", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.LeaseGeneration = issuedomain.StatusBlocked, "run_1", 1
			lease := ResourceLease{Owner: LeaseOwner{RunID: "run_1", Generation: 1}, Slot: 0, ResolvedResources: []string{"state"}, ReservedAt: now}
			issue.Lease = &lease
			issue.ResourcePark = &ResourceLeasePark{ID: "park_1", Status: issuedomain.ResourceParkStatusParked, OriginalLease: lease, ParkedAt: now}
		}},
		{name: "worker slot conflict", mutate: func(snapshot *Snapshot) {
			for number := 1; number <= 2; number++ {
				key := string(rune('0' + number))
				runID := "run_" + key
				snapshot.Issues[key] = &Issue{Number: number, Status: issuedomain.StatusClaiming, RunID: runID, LeaseGeneration: 1,
					Lease: &ResourceLease{Owner: LeaseOwner{RunID: runID, Generation: 1}, Slot: 0, ResolvedResources: []string{key}, ReservedAt: now}}
			}
		}},
		{name: "pending request answer", mutate: func(snapshot *Snapshot) {
			snapshot.PendingRequests["req_1"] = &Request{ID: "req_1", IssueNumber: 1, Status: issuedomain.RequestStatusPending, Answer: "already answered"}
		}},
		{name: "workspace provenance", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.Attempts = issuedomain.StatusRunning, "run_1", 1
			issue.Worktree, issue.Branch = "/tmp/issue-1", "codex/issue-1"
			issue.Workspace = &WorkerWorkspace{Path: "/tmp/other", Branch: issue.Branch, RepoID: snapshot.RepoID, Repository: "owner/repo", GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: now}
		}},
		{name: "recovery substate", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.GitHubSync = issuedomain.StatusPublicationRecovery, issuedomain.GitHubSyncPublicationRecovery
		}},
		{name: "worker process lifecycle", mutate: func(snapshot *Snapshot) {
			issue := snapshot.Issues["1"]
			issue.Status, issue.RunID, issue.WorkerPID = issuedomain.StatusRunning, "run_1", 42
		}},
		{name: "Pull Request merge identity", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].PullRequestMerged = true
		}},
		{name: "retry and attempt counters", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["1"].Attempts = -1
		}},
		{name: "unknown semantic contract", mutate: func(snapshot *Snapshot) {
			snapshot.SemanticContractVersion++
		}},
		{name: "unknown schema", mutate: func(snapshot *Snapshot) {
			snapshot.Version++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := validSnapshotForInvariantTest()
			test.mutate(&snapshot)
			if err := snapshot.Validate(); err == nil {
				t.Fatal("invalid aggregate was accepted")
			}
		})
	}
}

func TestStoreUpdateRejectsInvalidAggregateWithoutPartialDurableWrite(t *testing.T) {
	store := newStore(t)
	beforeState, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("invalid", 1, "", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"] = &Issue{Number: 1, Attempts: -1}
		return nil
	}); err == nil {
		t.Fatal("invalid snapshot update succeeded")
	}
	afterState, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, afterState) {
		t.Fatal("invalid update changed the durable snapshot")
	}
	if _, err := os.Stat(store.EventsPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid update left an event log: %v", err)
	}
	if _, err := os.Stat(store.TransactionPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid update left a prepared transaction: %v", err)
	}
}

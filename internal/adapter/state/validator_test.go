package state

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
)

const (
	invalidIssueStatus           issuedomain.Status                = "invalid-test-status"
	invalidEffectKind            issuedomain.EffectKind            = "invalid-test-effect"
	invalidConflictAttemptStatus issuedomain.ConflictAttemptStatus = "invalid-test-conflict-attempt"
)

func validSnapshotForInvariantTest() Snapshot {
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	return Snapshot{
		Version: CurrentVersion,
		RepoID:  "repo-deadbeef", RepoPath: "/tmp/repo",
		Supervisor: Supervisor{State: SupervisorStateStopped, UpdatedAt: now},
		Issues:     map[string]*Issue{"1": {Number: 1}}, PendingEffects: map[string]*EffectIntent{}, QuarantinedIssues: map[string]*QuarantineRecord{},
		IntakeVerifications: map[string]*queuedomain.AuthorVerification{}, PendingRequests: map[string]*Request{},
	}
}

func TestStoreUpdateQuarantinesInvalidIssueAndAllowsFollowingIssue(t *testing.T) {
	store := newStore(t)
	snapshot, err := store.Update("invalid", 1, "run_1", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["1"] = &Issue{Number: 1, Attempts: -1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Issues["1"] != nil || snapshot.QuarantinedIssues["1"] == nil || snapshot.QuarantinedIssues["1"].ReasonCode != "issue_invariant_violation" {
		t.Fatalf("invalid Issue was not isolated: %+v", snapshot.QuarantinedIssues["1"])
	}
	snapshot, err = store.Update("following_issue", 2, "run_2", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["2"] = &Issue{Number: 2, Title: "following Issue"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Issues["2"] == nil || snapshot.QuarantinedIssues["1"] == nil || snapshot.Supervisor.State == SupervisorStateBlocked {
		t.Fatalf("following Issue did not progress independently: %+v", snapshot)
	}
}

func TestStoreUpdateRejectsRunningWithoutProcessIdentity(t *testing.T) {
	for _, issueNumber := range []int{0, 1} {
		t.Run(fmt.Sprint(issueNumber), func(t *testing.T) {
			store := newStore(t)
			before, identity, err := store.StartExecution(ExecutionStart{IssueNumber: 1, RunID: "run_1", StartedAt: time.Now().UTC()})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update("invalid_running", issueNumber, identity.RunID, nil, func(snapshot *Snapshot) error {
				snapshot.Issues["1"].Workspace = legacyAnsweredLaunchFixture().Issues["1"].Workspace
				snapshot.Issues["1"].Worktree = snapshot.Issues["1"].Workspace.Path
				snapshot.Issues["1"].Branch = snapshot.Issues["1"].Workspace.Branch
				snapshot.Issues["1"].Status = issuedomain.StatusRunning
				snapshot.Issues["1"].LaunchSource = issuedomain.StatusUnset
				return nil
			})
			if issueNumber == 0 {
				if err == nil || !strings.Contains(err.Error(), "running worker process identity is missing") {
					t.Fatalf("update error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			after, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if issueNumber == 0 {
				if !reflect.DeepEqual(before, after) {
					t.Fatal("rejected update changed durable state")
				}
			} else {
				record := after.QuarantinedIssues["1"]
				if after.Issues["1"] != nil || after.ActiveExecution != nil || record == nil ||
					record.RejectedStatus != issuedomain.StatusRunning || record.ReasonCode != "issue_invariant_violation" ||
					!strings.Contains(record.Reason, "running worker process identity is missing") {
					t.Fatalf("invalid running Issue was not quarantined: %+v", after)
				}
			}
		})
	}
}

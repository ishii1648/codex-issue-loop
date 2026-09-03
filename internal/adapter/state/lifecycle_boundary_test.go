package state

import (
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestApplyIssueTransitionFencesStaleDecision(t *testing.T) {
	item := &Issue{Number: 1, Status: issuedomain.StatusRunning}
	transition, err := issuedomain.NewTransition("complete", issuedomain.StatusRunning, issuedomain.StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = issuedomain.StatusRetryWait
	if err := ApplyIssueTransition(item, transition); err == nil {
		t.Fatal("stale transition was accepted")
	}
}

func TestLifecycleBoundaryReleasesExecutionAndRetainsCheckpointEvidence(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	_, identity, err := store.StartExecution(ExecutionStart{IssueNumber: 9, RunID: "run_9", StartedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Update("failed", 9, identity.RunID, nil, func(snapshot *Snapshot) error {
		item := snapshot.Issues["9"]
		if err := CaptureContinuation(snapshot, 9, identity, NewID("checkpoint"), now.Add(time.Minute)); err != nil {
			return err
		}
		item.Status = issuedomain.StatusFailed
		item.LastError = "fixture"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveExecution != nil || snapshot.Issues["9"].Continuation == nil {
		t.Fatalf("active=%+v continuation=%+v", snapshot.ActiveExecution, snapshot.Issues["9"].Continuation)
	}
}

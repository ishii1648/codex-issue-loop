package observe

import (
	"context"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestReconciliationFindsAttentionWithoutEvent(t *testing.T) {
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update("started", 0, "", nil, func(s *state.Snapshot) error { s.Supervisor.State = "running"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}) // Deliberately never receives: simulates a dropped event.
	eventErrors := make(chan error)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultCh := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := wait(ctx, store, 25*time.Millisecond, 0, false, wake, eventErrors)
		resultCh <- result
		errCh <- err
	}()
	time.Sleep(10 * time.Millisecond)
	_, err = store.Update("input_requested", 3, "run", nil, func(s *state.Snapshot) error {
		s.PendingRequests["req_1"] = &state.Request{ID: "req_1", IssueNumber: 3, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if result.Reason != "needs_input" {
		t.Fatalf("reason = %q", result.Reason)
	}
}

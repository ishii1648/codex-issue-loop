package observe

import (
	"context"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestFaultDroppedEventReconcilesAttention(t *testing.T) {
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

func TestFaultReadSubscribeReadRace(t *testing.T) {
	store := newWatchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := waitWithSubscribeHook(ctx, store, time.Hour, 0, false, func() {
		_, updateErr := store.Update("input_requested", 4, "run", nil, func(snapshot *state.Snapshot) error {
			snapshot.PendingRequests["req_race"] = &state.Request{ID: "req_race", IssueNumber: 4, Status: "pending"}
			return nil
		})
		if updateErr != nil {
			t.Fatal(updateErr)
		}
	})
	if err != nil || result.Reason != "needs_input" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFaultMultipleWatchConnectionsObserveSameRevision(t *testing.T) {
	store := newWatchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type outcome struct {
		result Result
		err    error
	}
	results := make(chan outcome, 2)
	for index := 0; index < 2; index++ {
		go func() {
			result, err := Wait(ctx, store, 20*time.Millisecond, 0, false)
			results <- outcome{result: result, err: err}
		}()
	}
	snapshot, err := store.Update("input_requested", 5, "run", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_multiple"] = &state.Request{ID: "req_multiple", IssueNumber: 5, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		outcome := <-results
		if outcome.err != nil || outcome.result.Reason != "needs_input" || outcome.result.Snapshot.StateRevision != snapshot.StateRevision {
			t.Fatalf("outcome=%+v revision=%d", outcome, snapshot.StateRevision)
		}
	}
}

func TestFaultDisconnectedEventChannelsFallBackToTimer(t *testing.T) {
	store := newWatchStore(t)
	wake := make(chan struct{})
	eventErrors := make(chan error)
	close(wake)
	close(eventErrors)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultCh := make(chan outcomeForTest, 1)
	go func() {
		result, err := wait(ctx, store, 20*time.Millisecond, 0, false, wake, eventErrors)
		resultCh <- outcomeForTest{result: result, err: err}
	}()
	_, err := store.Update("input_requested", 6, "run", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_disconnect"] = &state.Request{ID: "req_disconnect", IssueNumber: 6, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := <-resultCh
	if outcome.err != nil || outcome.result.Reason != "needs_input" {
		t.Fatalf("outcome=%+v", outcome)
	}
}

type outcomeForTest struct {
	result Result
	err    error
}

func newWatchStore(t *testing.T) state.Store {
	t.Helper()
	store := state.Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update("started", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

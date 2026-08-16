package observe

import (
	"context"
	"errors"
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

func TestWatchReturnsEveryPendingRequestInRequestIDOrder(t *testing.T) {
	store := newWatchStore(t)
	_, err := store.Update("input_requested", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_b"] = &state.Request{ID: "req_b", IssueNumber: 2, Status: "pending"}
		snapshot.PendingRequests["req_answered"] = &state.Request{ID: "req_answered", IssueNumber: 3, Status: "answered"}
		snapshot.PendingRequests["req_a"] = &state.Request{ID: "req_a", IssueNumber: 1, Status: "pending"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Wait(context.Background(), store, time.Second, 0, false)
	if err != nil || len(result.PendingRequests) != 2 || result.PendingRequests[0].ID != "req_a" || result.PendingRequests[1].ID != "req_b" {
		t.Fatalf("result=%+v err=%v", result, err)
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

func TestFaultWatcherSubscriptionFailureFallsBackToReconciliation(t *testing.T) {
	store := newWatchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultCh := make(chan outcomeForTest, 1)
	go func() {
		result, err := waitWithSubscription(ctx, store, 20*time.Millisecond, 0, false, func(context.Context, string) (eventSubscription, error) {
			return eventSubscription{}, errors.New("too many open files")
		}, nil)
		resultCh <- outcomeForTest{result: result, err: err}
	}()
	_, err := store.Update("input_requested", 7, "run", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_no_watcher"] = &state.Request{ID: "req_no_watcher", IssueNumber: 7, Status: "pending"}
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

func TestFSNotifyMultipleWatchersWakeAndCanReconnect(t *testing.T) {
	store := newWatchStore(t)
	waitForAttention := func(issueNumber int, requestID string, watchers int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		type outcome struct {
			result Result
			err    error
		}
		results := make(chan outcome, watchers)
		subscribed := make(chan struct{}, watchers)
		for index := 0; index < watchers; index++ {
			go func() {
				result, err := waitWithSubscribeHook(ctx, store, time.Hour, 0, false, func() { subscribed <- struct{}{} })
				results <- outcome{result: result, err: err}
			}()
		}
		for index := 0; index < watchers; index++ {
			<-subscribed
		}
		snapshot, err := store.Update("input_requested", issueNumber, "run", nil, func(snapshot *state.Snapshot) error {
			snapshot.PendingRequests[requestID] = &state.Request{ID: requestID, IssueNumber: issueNumber, Status: "pending"}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for index := 0; index < watchers; index++ {
			outcome := <-results
			if outcome.err != nil || outcome.result.Reason != "needs_input" || outcome.result.Snapshot.StateRevision != snapshot.StateRevision {
				t.Fatalf("outcome=%+v revision=%d", outcome, snapshot.StateRevision)
			}
		}
	}

	waitForAttention(8, "req_first", 2)
	if _, err := store.Update("answer_recorded", 8, "run", nil, func(snapshot *state.Snapshot) error {
		snapshot.PendingRequests["req_first"].Status = "answered"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitForAttention(9, "req_after_reconnect", 1)
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

package observe

import (
	"context"
	"errors"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func addWatchRequest(snapshot *state.Snapshot, id string, issueNumber int, status issuedomain.RequestStatus) {
	snapshot.Issues[fmt.Sprint(issueNumber)] = &state.Issue{Number: issueNumber, Status: issuedomain.StatusNeedsInput}
	request := &state.Request{ID: id, IssueNumber: issueNumber, Status: status}
	if status == issuedomain.RequestStatusAnswered {
		request.Answer = "answered"
	}
	snapshot.PendingRequests[id] = request
}

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
		addWatchRequest(s, "req_1", 3, issuedomain.RequestStatusPending)
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
		addWatchRequest(snapshot, "req_b", 2, issuedomain.RequestStatusPending)
		addWatchRequest(snapshot, "req_answered", 3, issuedomain.RequestStatusAnswered)
		addWatchRequest(snapshot, "req_a", 1, issuedomain.RequestStatusPending)
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

func TestWatchPreservesAnsweredGenericContinuation(t *testing.T) {
	store := newWatchStore(t)
	now := time.Date(2026, 8, 18, 8, 0, 0, 0, time.UTC)
	owner := state.ExecutionIdentity{RunID: "run_1", Generation: 1}
	_, err := store.Update("answer_recorded", 1, owner.RunID, nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = state.SupervisorStateStopped
		request := &state.Request{
			ID: "req_1", IssueNumber: 1, Question: "Continue?", RunID: owner.RunID, CheckpointID: "checkpoint_1",
			ReleasedExecution: &owner, Status: issuedomain.RequestStatusAnswered, Answer: "yes", CreatedAt: now, AnsweredAt: &now,
		}
		snapshot.PendingRequests[request.ID] = request
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, RunID: owner.RunID, Status: issuedomain.StatusResumePending, Generation: 1,
			Worktree: "/tmp/issue-1", Branch: "codex/issue-1",
			Workspace: &state.WorkerWorkspace{Path: "/tmp/issue-1", Branch: "codex/issue-1", RepoID: snapshot.RepoID,
				Repository: "owner/repo", GitCommonDir: snapshot.RepoPath + "/.git", MainCheckout: snapshot.RepoPath, CapturedAt: now},
			Continuation: &state.ContinuationCheckpoint{
				ID: "checkpoint_1", Kind: state.ContinuationKindNeedsInput, RequestID: request.ID, CreatedAt: now,
				RunID: owner.RunID, Generation: owner.Generation, BaseSHA: "base-1", Stage: issuedomain.ContinuationStageResume,
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Wait(context.Background(), store, time.Second, 0, false)
	if err != nil || result.Reason != "stopped" || len(result.PendingRequests) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	checkpoint := result.Snapshot.Issues["1"].Continuation
	if checkpoint == nil || checkpoint.RequestID != "req_1" || checkpoint.BaseSHA != "base-1" || result.Snapshot.PendingRequests["req_1"].Status != issuedomain.RequestStatusAnswered {
		t.Fatalf("watch lost continuation provenance: %+v", result)
	}
}

func TestFaultReadSubscribeReadRace(t *testing.T) {
	store := newWatchStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := waitWithSubscribeHook(ctx, store, time.Hour, 0, false, func() {
		_, updateErr := store.Update("input_requested", 4, "run", nil, func(snapshot *state.Snapshot) error {
			addWatchRequest(snapshot, "req_race", 4, issuedomain.RequestStatusPending)
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
		addWatchRequest(snapshot, "req_multiple", 5, issuedomain.RequestStatusPending)
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
		addWatchRequest(snapshot, "req_disconnect", 6, issuedomain.RequestStatusPending)
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
		addWatchRequest(snapshot, "req_no_watcher", 7, issuedomain.RequestStatusPending)
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
			addWatchRequest(snapshot, requestID, issueNumber, issuedomain.RequestStatusPending)
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
		snapshot.PendingRequests["req_first"].Status = issuedomain.RequestStatusAnswered
		snapshot.PendingRequests["req_first"].Answer = "answered"
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

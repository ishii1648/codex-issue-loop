package supervisor

import (
	"context"
	"io"
	"log"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/failure"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type numberedFakeGitHub struct{ *fakeGitHub }

func (f numberedFakeGitHub) Get(_ context.Context, cfg config.Config, number int) (gh.Issue, error) {
	return gh.Issue{Number: number, Title: "Test", State: "OPEN", Labels: append([]string(nil), cfg.GitHub.ReadyLabels...)}, nil
}

type blockingPoolWorker struct {
	mu       sync.Mutex
	active   int
	maximum  int
	started  chan int
	release  chan struct{}
	canceled chan int
}

func (w *blockingPoolWorker) Run(ctx context.Context, _ config.Config, _ gh.Issue, current state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	w.mu.Lock()
	w.active++
	if w.active > w.maximum {
		w.maximum = w.active
	}
	w.mu.Unlock()
	w.started <- current.Number
	select {
	case <-ctx.Done():
		if w.canceled != nil {
			w.canceled <- current.Number
		}
	case <-w.release:
	}
	w.mu.Lock()
	w.active--
	w.mu.Unlock()
	return worker.Result{
		Version: 1, Status: "retryable_failure", ExecutionProfile: "standard", Summary: "retry",
		Tests: []worker.Test{}, Retry: &worker.Retry{Reason: "retry"},
	}, nil
}

func TestSchedulerCancellationStopsAllWorkers(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	pool := &blockingPoolWorker{
		started: make(chan int, 1), release: make(chan struct{}, 1), canceled: make(chan int, 1),
	}
	loop.Worker = pool
	_, err := loop.Store.Update("scheduler_fixture", 1, "run_cancel", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Title: "Test", Status: "retry_wait", RunID: "run_cancel",
			Worktree: loop.Config.RepoPath, Attempts: 1, ExecutionProfile: "standard", UpdatedAt: loop.now(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.runScheduler(ctx, nil) }()
	if number := <-pool.started; number != 1 {
		t.Fatalf("started Issue=%d", number)
	}
	cancel()
	if number := <-pool.canceled; number != 1 {
		t.Fatalf("canceled Issue=%d", number)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil || snapshot.Supervisor.State != "stopped" {
		t.Fatalf("supervisor=%+v err=%v", snapshot.Supervisor, err)
	}
}

func TestSchedulerContinuesAfterNeedsInputWhenConfigured(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Queue.Concurrency = 2
	loop.Config.Queue.ContinueAfterNeedsInput = true
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	github.issue = gh.Issue{Number: 2, Title: "Next", Labels: loop.Config.GitHub.ReadyLabels}
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	pool := &blockingPoolWorker{started: make(chan int, 1), release: make(chan struct{}, 1)}
	loop.Worker = pool
	_, err := loop.Store.Update("needs_input_fixture", 1, "run_waiting", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{Number: 1, Status: "needs_input", RunID: "run_waiting", UpdatedAt: loop.now()}
		snapshot.PendingRequests["req_1"] = &state.Request{
			ID: "req_1", IssueNumber: 1, Question: "Choose?", Status: "pending", CreatedAt: loop.now(),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{
		loop: loop, events: make(chan schedulerEvent, 3), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if dispatched, err := s.schedule(ctx, true); err != nil || !dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, err)
	}
	if number := <-pool.started; number != 2 {
		t.Fatalf("started Issue=%d, want 2", number)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if lease := snapshot.Issues["2"].Lease; lease == nil || lease.BaseSHA != "base-sha" {
		t.Fatalf("dispatch base was not persisted before worker start: %+v", lease)
	}
	s.cancelAndDrain()
}

func (w *blockingPoolWorker) Resume(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started worker.Started) (worker.Result, error) {
	return w.Run(ctx, cfg, issue, current, prompt, started)
}

func TestSchedulerBoundsWorkersAndAdmitsAfterSlotRelease(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Queue.Concurrency = 2
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	pool := &blockingPoolWorker{started: make(chan int, 3), release: make(chan struct{}, 3)}
	loop.Worker = pool
	_, err := loop.Store.Update("scheduler_fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		for number, resource := range map[int]string{1: "one", 2: "two", 3: "three"} {
			runID := "run_" + resource
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, Title: "Test", Status: "retry_wait", RunID: runID,
				LeaseGeneration: 1, Lease: &state.ResourceLease{
					Owner: state.LeaseOwner{RunID: runID, Generation: 1}, Slot: 0,
					DeclaredResources: []string{}, ResolvedResources: []string{resource}, ReservedAt: loop.now(),
				},
				Worktree: loop.Config.RepoPath, Attempts: 1, ExecutionProfile: "standard", UpdatedAt: loop.now(),
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{
		loop: loop, events: make(chan schedulerEvent, 3), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatched, err := s.schedule(ctx, false)
	if err != nil || !dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, err)
	}
	first, second := <-pool.started, <-pool.started
	if first == second || len(s.active) != 2 {
		t.Fatalf("started=%d,%d active=%d", first, second, len(s.active))
	}
	select {
	case third := <-pool.started:
		t.Fatalf("worker %d exceeded concurrency before a slot was released", third)
	default:
	}

	pool.release <- struct{}{}
	event := <-s.events
	if err := s.handleEvent(event); err != nil {
		t.Fatal(err)
	}
	if _, err := s.schedule(ctx, false); err != nil {
		t.Fatal(err)
	}
	third := <-pool.started
	if third != 3 {
		t.Fatalf("next admitted Issue=%d, want 3", third)
	}
	s.cancelAndDrain()
	pool.mu.Lock()
	maximum := pool.maximum
	pool.mu.Unlock()
	if maximum != 2 {
		t.Fatalf("maximum active workers=%d, want 2", maximum)
	}
}

func TestSchedulerIssueFailureDoesNotCancelOtherWorker(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	_, cancelOne := context.WithCancel(context.Background())
	_, cancelTwo := context.WithCancel(context.Background())
	s := &scheduler{
		loop: loop, active: map[int]activeJob{
			1: {runID: "run_1", slot: 0, cancel: cancelOne},
			2: {runID: "run_2", slot: 1, cancel: cancelTwo},
		},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	err := s.handleEvent(schedulerEvent{
		Kind: schedulerJobFinished, IssueNumber: 1, RunID: "run_1",
		Err: failure.Wrap(failure.Transient, "worker", context.DeadlineExceeded),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, active := s.active[2]; !active || len(s.active) != 1 {
		t.Fatalf("unrelated active worker was removed: %+v", s.active)
	}
	if !s.issueRetry[1].After(loop.now()) {
		t.Fatalf("Issue-specific retry was not scheduled: %v", s.issueRetry)
	}
}

func TestWorkerProcessCallbackFencesRunAndPersistsProcessGroup(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, RunID: "run_current", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.recordWorkerPID(1, "run_stale")(1234); err == nil {
		t.Fatal("stale run callback was accepted")
	}
	if err := loop.recordWorkerPID(1, "run_current")(0); err == nil {
		t.Fatal("invalid PID callback was accepted")
	}
	if err := loop.recordWorkerPID(1, "run_current")(1234); err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Issues["1"].WorkerPID != 1234 || snapshot.Issues["1"].WorkerPGID != 1234 {
		t.Fatalf("process identity=%+v", snapshot.Issues["1"])
	}
}

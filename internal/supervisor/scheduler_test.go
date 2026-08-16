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

type barrierPoolWorker struct {
	started chan int
	release <-chan struct{}
	results map[int]worker.Result
	errors  map[int]error
}

func (w *barrierPoolWorker) run(ctx context.Context, current state.Issue) (worker.Result, error) {
	w.started <- current.Number
	select {
	case <-ctx.Done():
		return worker.Result{}, ctx.Err()
	case <-w.release:
		return w.results[current.Number], w.errors[current.Number]
	}
}

func (w *barrierPoolWorker) Run(ctx context.Context, _ config.Config, _ gh.Issue, current state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	return w.run(ctx, current)
}

func (w *barrierPoolWorker) Resume(ctx context.Context, _ config.Config, _ gh.Issue, current state.Issue, _ string, _ worker.Started) (worker.Result, error) {
	return w.run(ctx, current)
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

func TestFaultGracefulDrainWaitsForWorkerCheckpointWithoutCancellationOrNewDispatch(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Queue.Concurrency = 1
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	pool := &blockingPoolWorker{
		started: make(chan int, 2), release: make(chan struct{}, 1), canceled: make(chan int, 1),
	}
	loop.Worker = pool
	_, err := loop.Store.Update("drain_fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		for _, number := range []int{1, 2} {
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, Title: "Test", Status: "retry_wait", RunID: "run_" + strconv.Itoa(number),
				Worktree: loop.Config.RepoPath, Attempts: 1, ExecutionProfile: "standard", UpdatedAt: loop.now(),
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{
		loop: loop, events: make(chan schedulerEvent, 2), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if dispatched, scheduleErr := s.schedule(ctx, false); scheduleErr != nil || !dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, scheduleErr)
	}
	if number := <-pool.started; number != 1 {
		t.Fatalf("started Issue=%d, want 1", number)
	}
	beforeDrain, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	budgetAttempts := beforeDrain.Issues["1"].Attempts
	budgetContinuations := beforeDrain.Issues["1"].Continuations
	deadline := loop.now().Add(time.Minute)
	_, err = loop.Store.Update("drain_requested", 0, "drain_test", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "draining"
		snapshot.Supervisor.Drain = &state.Drain{ID: "drain_test", Operation: "stop", Status: "requested", RequestedAt: loop.now(), Deadline: deadline, RemainingActive: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if draining, completed, reconcileErr := s.reconcileDrain(); reconcileErr != nil || !draining || completed {
		t.Fatalf("draining=%v completed=%v err=%v", draining, completed, reconcileErr)
	}
	if dispatched, scheduleErr := s.schedule(ctx, true); scheduleErr != nil || dispatched {
		t.Fatalf("drain admitted new work: dispatched=%v err=%v", dispatched, scheduleErr)
	}
	select {
	case number := <-pool.canceled:
		t.Fatalf("graceful drain canceled Issue #%d", number)
	default:
	}
	pool.release <- struct{}{}
	event := <-s.events
	if err := s.handleEvent(event); err != nil {
		t.Fatal(err)
	}
	if err := s.recordDrainCheckpoint(event); err != nil {
		t.Fatal(err)
	}
	if draining, completed, reconcileErr := s.reconcileDrain(); reconcileErr != nil || !draining || !completed {
		t.Fatalf("final draining=%v completed=%v err=%v", draining, completed, reconcileErr)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Supervisor.State != "stopped" || snapshot.Supervisor.Drain == nil || snapshot.Supervisor.Drain.Status != "completed" {
		t.Fatalf("supervisor=%+v", snapshot.Supervisor)
	}
	if snapshot.Issues["1"].Attempts != budgetAttempts || snapshot.Issues["1"].Continuations != budgetContinuations {
		t.Fatalf("operator drain consumed worker budget: %+v", snapshot.Issues["1"])
	}
	select {
	case number := <-pool.started:
		t.Fatalf("Issue #%d started during drain", number)
	default:
	}
}

func TestFaultDrainTimeoutResumesSchedulingWithoutCancelingWorker(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Logger = log.New(io.Discard, "", 0)
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &scheduler{
		loop: loop, active: map[int]activeJob{1: {runID: "run_1", slot: 0, cancel: cancel}},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	past := loop.now().Add(-time.Second)
	_, err := loop.Store.Update("drain_requested", 0, "drain_timeout", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "draining"
		snapshot.Supervisor.Drain = &state.Drain{ID: "drain_timeout", Operation: "stop", Status: "draining", RequestedAt: past, Deadline: past, RemainingActive: 1}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	draining, completed, err := s.reconcileDrain()
	if err != nil || draining || completed {
		t.Fatalf("draining=%v completed=%v err=%v", draining, completed, err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil || snapshot.Supervisor.State != "running" || snapshot.Supervisor.Drain.Status != "timed_out" {
		t.Fatalf("supervisor=%+v err=%v", snapshot.Supervisor, err)
	}
}

func TestFaultGracefulDrainCheckpointsMultipleWorkersBeforeStopping(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Config.Queue.Concurrency = 2
	loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	pool := &blockingPoolWorker{started: make(chan int, 3), release: make(chan struct{}, 2), canceled: make(chan int, 2)}
	loop.Worker = pool
	_, err := loop.Store.Update("multi_drain_fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		for number := 1; number <= 3; number++ {
			snapshot.Issues[strconv.Itoa(number)] = &state.Issue{
				Number: number, Title: "Test", Status: "retry_wait", RunID: "run_" + strconv.Itoa(number),
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
	if dispatched, scheduleErr := s.schedule(ctx, false); scheduleErr != nil || !dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, scheduleErr)
	}
	first, second := <-pool.started, <-pool.started
	started := map[int]bool{first: true, second: true}
	if len(started) != 2 || len(s.active) != 2 {
		t.Fatalf("started=%v active=%v", started, s.active)
	}
	deadline := loop.now().Add(time.Minute)
	_, err = loop.Store.Update("drain_requested", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "draining"
		snapshot.Supervisor.Drain = &state.Drain{ID: "drain_multi", Operation: "restart", Status: "requested", RequestedAt: loop.now(), Deadline: deadline, RemainingActive: 2}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pool.release <- struct{}{}
	pool.release <- struct{}{}
	for range 2 {
		event := <-s.events
		if err := s.handleEvent(event); err != nil {
			t.Fatal(err)
		}
		if err := s.recordDrainCheckpoint(event); err != nil {
			t.Fatal(err)
		}
	}
	if draining, completed, reconcileErr := s.reconcileDrain(); reconcileErr != nil || !draining || !completed {
		t.Fatalf("draining=%v completed=%v err=%v", draining, completed, reconcileErr)
	}
	select {
	case number := <-pool.started:
		t.Fatalf("queued Issue #%d started during multi-worker drain", number)
	default:
	}
	select {
	case number := <-pool.canceled:
		t.Fatalf("Issue #%d was canceled by graceful drain", number)
	default:
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

func TestFaultSchedulerConcurrentResultBarrier(t *testing.T) {
	completed := func(summary string) worker.Result {
		return worker.Result{
			Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: summary,
			Tests: []worker.Test{}, Git: &worker.GitResult{},
		}
	}
	retryable := func(summary string) worker.Result {
		return worker.Result{
			Version: 1, Status: "retryable_failure", ExecutionProfile: "standard", Summary: summary,
			Tests: []worker.Test{}, Retry: &worker.Retry{Reason: summary},
		}
	}
	needsInput := worker.Result{
		Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "input required",
		Tests: []worker.Test{}, Question: &worker.Question{
			Text: "Choose a safe option", Reason: "The behavior is ambiguous", RecommendedOption: "safe",
			Options: []state.Option{{ID: "safe", Label: "Use safe option"}}, AllowFreeText: true,
		},
	}
	tests := []struct {
		name    string
		results map[int]worker.Result
		want    map[int]string
		leases  map[int]bool
		pending int
	}{
		{
			name:    "two workers complete together",
			results: map[int]worker.Result{1: completed("one done"), 2: completed("two done")},
			want:    map[int]string{1: "completed", 2: "completed"}, leases: map[int]bool{1: false, 2: false},
		},
		{
			name:    "two workers fail together",
			results: map[int]worker.Result{1: retryable("one retry"), 2: retryable("two retry")},
			want:    map[int]string{1: "retry_wait", 2: "retry_wait"}, leases: map[int]bool{1: true, 2: true},
		},
		{
			name:    "one worker needs input while the other completes",
			results: map[int]worker.Result{1: needsInput, 2: completed("two done")},
			want:    map[int]string{1: "needs_input", 2: "completed"}, leases: map[int]bool{1: true, 2: false}, pending: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop, github := testLoop(t, worker.Result{})
			loop.Config.Queue.Concurrency = 2
			loop.Clock = fixedClock{value: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)}
			loop.Random = fixedRandom(0.5)
			loop.Logger = log.New(io.Discard, "", 0)
			loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
			release := make(chan struct{})
			barrier := &barrierPoolWorker{started: make(chan int, 2), release: release, results: test.results, errors: map[int]error{}}
			loop.Worker = barrier
			for number, resource := range map[int]string{1: "one", 2: "two"} {
				runID := "run_" + resource
				_, owner, err := loop.Store.ReserveLease(state.LeaseReservation{
					IssueNumber: number, Title: "Test", RunID: runID, Slot: number - 1,
					ResolvedResources: []string{resource}, ReservedAt: loop.now(),
				})
				if err != nil {
					t.Fatal(err)
				}
				_, err = loop.Store.Update("resume_pending", number, runID, nil, func(snapshot *state.Snapshot) error {
					item := snapshot.Issues[strconv.Itoa(number)]
					item.Status = "resume_pending"
					item.Worktree = loop.Config.RepoPath
					item.ExecutionProfile = "standard"
					item.Lease.Owner = owner
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			s := &scheduler{
				loop: loop, events: make(chan schedulerEvent, 3), active: map[int]activeJob{},
				issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if dispatched, err := s.schedule(ctx, false); err != nil || !dispatched {
				t.Fatalf("dispatched=%v err=%v", dispatched, err)
			}
			started := map[int]bool{}
			for range 2 {
				select {
				case number := <-barrier.started:
					started[number] = true
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for both workers to reach the barrier")
				}
			}
			if len(started) != 2 || len(s.active) != 2 {
				t.Fatalf("started=%v active=%v", started, s.active)
			}
			close(release)
			for range 2 {
				select {
				case event := <-s.events:
					if err := s.handleEvent(event); err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for both scheduler results")
				}
			}
			if len(s.active) != 0 {
				t.Fatalf("active jobs remain: %v", s.active)
			}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			for number, want := range test.want {
				item := snapshot.Issues[strconv.Itoa(number)]
				if item.Status != want || (item.Lease != nil) != test.leases[number] {
					t.Fatalf("Issue #%d=%+v want_status=%s want_lease=%v", number, item, want, test.leases[number])
				}
			}
			if len(snapshot.PendingRequests) != test.pending {
				t.Fatalf("pending requests=%d want=%d", len(snapshot.PendingRequests), test.pending)
			}
		})
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

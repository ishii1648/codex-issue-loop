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
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type numberedFakeGitHub struct{ *fakeGitHub }

func (f numberedFakeGitHub) Get(_ context.Context, cfg config.Config, number int) (gh.Issue, error) {
	return gh.Issue{Number: number, Title: "Test", State: "OPEN", Labels: append([]string(nil), cfg.GitHub.ReadyLabels...)}, nil
}

type webhookFakeGitHub struct {
	*fakeGitHub
	listCalls        int
	restGets         int
	conditionalCalls int
	conditionalETags []string
}

func (f *webhookFakeGitHub) ListReadyConditional(_ context.Context, _ config.Config, etag, _ string) (gh.ConditionalQueueResult, error) {
	f.conditionalCalls++
	f.conditionalETags = append(f.conditionalETags, etag)
	if f.conditionalCalls == 1 {
		return gh.ConditionalQueueResult{Issues: []gh.Issue{f.issue}, StatusCode: 200, ETag: `W/"queue-v1"`, RateRemaining: "4999"}, nil
	}
	return gh.ConditionalQueueResult{StatusCode: 304, ETag: `W/"queue-v1"`, NotModified: true, RateRemaining: "4999"}, nil
}

func (f *webhookFakeGitHub) ListReady(ctx context.Context, cfg config.Config) ([]gh.Issue, error) {
	f.listCalls++
	return f.fakeGitHub.ListReady(ctx, cfg)
}

func (f *webhookFakeGitHub) GetREST(context.Context, config.Config, int) (gh.Issue, error) {
	f.restGets++
	return f.issue, nil
}

func (f *webhookFakeGitHub) InspectPullRequestREST(context.Context, config.Config, int, int, string) (gh.RemoteState, error) {
	f.inspectCalls++
	if f.remote != nil {
		return *f.remote, nil
	}
	return gh.RemoteState{Issue: f.issue}, nil
}

func TestWebhookMailboxClaimsReadyIssueWithoutQueuePolling(t *testing.T) {
	result := worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "standard", Summary: "done",
		Tests: []worker.Test{}, Git: &worker.GitResult{PullRequestURL: "https://example.test/owner/repo/pull/7"},
	}
	loop, baseGitHub := testLoop(t, result)
	loop.Config.Webhook.Mode = "webhook"
	loop.Config.Webhook.SafetySweepInterval.Duration = 15 * time.Minute
	github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
	loop.GitHub = github
	delivery := webhook.Delivery{
		Version: webhook.InboxVersion, DeliveryID: "delivery-labeled", Event: "issues", Action: "labeled",
		RepoID: loop.Store.RepoID, Repository: loop.Config.GitHub.Repo, IssueNumber: 1, AcceptedAt: time.Now().UTC(),
	}
	if err := webhook.EnqueueMailbox(loop.Store.Dir, delivery); err != nil {
		t.Fatal(err)
	}
	s := &scheduler{
		loop: loop, events: make(chan schedulerEvent, 2), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{}, pollAt: loop.now().Add(15 * time.Minute),
	}
	if dispatched, err := s.schedule(context.Background(), false); err != nil || !dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, err)
	}
	select {
	case event := <-s.events:
		if err := s.handleEvent(event); err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("webhook-started job did not finish")
	}
	if !github.claimed || github.restGets == 0 || github.listCalls != 0 {
		t.Fatalf("claimed=%v rest_gets=%d list_calls=%d", github.claimed, github.restGets, github.listCalls)
	}
	snapshot, err := loop.Store.Load()
	if err != nil || snapshot.Issues["1"] == nil || snapshot.Issues["1"].Status != "awaiting_checks" {
		t.Fatalf("issue=%+v err=%v", snapshot.Issues["1"], err)
	}
}

func TestWebhookSchedulerNeverPerformsQueueSweep(t *testing.T) {
	loop, baseGitHub := testLoop(t, worker.Result{})
	loop.Config.Webhook.Mode = "webhook"
	github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
	loop.GitHub = github
	s := &scheduler{
		loop: loop, events: make(chan schedulerEvent, 1), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
	}
	if dispatched, err := s.schedule(context.Background(), true); err != nil || dispatched {
		t.Fatalf("dispatched=%v err=%v", dispatched, err)
	}
	if github.listCalls != 0 || github.conditionalCalls != 0 {
		t.Fatalf("list=%d conditional=%d", github.listCalls, github.conditionalCalls)
	}
}

func TestWebhookTerminalStatesConvergeOnlyToRemoteTerminalAuthority(t *testing.T) {
	mergedAt := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		current    state.Issue
		delivery   webhook.Delivery
		remote     gh.RemoteState
		wantStatus string
		wantMerged bool
	}{
		{
			name: "completed observes merged PR",
			current: state.Issue{Number: 1, Status: "completed", RunID: "run-1", PullRequestNumber: 7,
				PullRequestURL: "https://example.test/owner/repo/pull/7"},
			delivery: webhook.Delivery{DeliveryID: "terminal-merged", Event: "pull_request", Action: "closed", PullRequestNumber: 7},
			remote: gh.RemoteState{Issue: gh.Issue{Number: 1, State: "open"}, PullRequests: []gh.PullRequest{{
				Number: 7, URL: "https://example.test/owner/repo/pull/7", State: "closed", MergedAt: &mergedAt,
			}}},
			wantStatus: "completed", wantMerged: true,
		},
		{
			name: "failed observes PR closed without merge",
			current: state.Issue{Number: 1, Status: "failed", RunID: "run-1", PullRequestNumber: 7,
				PullRequestURL: "https://example.test/owner/repo/pull/7"},
			delivery: webhook.Delivery{DeliveryID: "terminal-pr-closed", Event: "pull_request", Action: "closed", PullRequestNumber: 7},
			remote: gh.RemoteState{Issue: gh.Issue{Number: 1, State: "open"}, PullRequests: []gh.PullRequest{{
				Number: 7, URL: "https://example.test/owner/repo/pull/7", State: "closed",
			}}},
			wantStatus: "blocked",
		},
		{
			name:       "needs input observes Issue close",
			current:    state.Issue{Number: 1, Status: "needs_input", RunID: "run-1"},
			delivery:   webhook.Delivery{DeliveryID: "terminal-issue-closed", Event: "issues", Action: "closed", IssueNumber: 1},
			remote:     gh.RemoteState{Issue: gh.Issue{Number: 1, State: "closed"}},
			wantStatus: "blocked",
		},
		{
			name:       "blocked observes done label",
			current:    state.Issue{Number: 1, Status: "blocked", RunID: "run-1"},
			delivery:   webhook.Delivery{DeliveryID: "terminal-done", Event: "issues", Action: "labeled", IssueNumber: 1},
			remote:     gh.RemoteState{Issue: gh.Issue{Number: 1, State: "open", Labels: []string{"codex-loop:done"}}},
			wantStatus: "completed",
		},
		{
			name:       "failed label removal does not resume",
			current:    state.Issue{Number: 1, Status: "failed", RunID: "run-1"},
			delivery:   webhook.Delivery{DeliveryID: "terminal-unlabeled", Event: "issues", Action: "unlabeled", IssueNumber: 1},
			remote:     gh.RemoteState{Issue: gh.Issue{Number: 1, State: "open", Labels: []string{"codex-loop:ready"}}},
			wantStatus: "failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			loop, baseGitHub := testLoop(t, worker.Result{})
			loop.Config.Webhook.Mode = "webhook"
			github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
			github.remote = &test.remote
			github.issue = test.remote.Issue
			loop.GitHub = github
			_, err := loop.Store.Update("terminal_fixture", 1, test.current.RunID, nil, func(snapshot *state.Snapshot) error {
				value := test.current
				snapshot.Issues["1"] = &value
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			test.delivery.Version = webhook.InboxVersion
			test.delivery.RepoID = loop.Store.RepoID
			test.delivery.Repository = loop.Config.GitHub.Repo
			test.delivery.AcceptedAt = time.Now().UTC()
			if err := webhook.EnqueueMailbox(loop.Store.Dir, test.delivery); err != nil {
				t.Fatal(err)
			}
			s := &scheduler{loop: loop, events: make(chan schedulerEvent, 1), active: map[int]activeJob{}, issueRetry: map[int]time.Time{}, issueFails: map[int]int{}}
			snapshot, err := loop.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			candidates, acknowledged, err := s.processMailbox(context.Background(), snapshot)
			if err != nil || len(candidates) != 0 || len(acknowledged) != 1 {
				t.Fatalf("candidates=%v acknowledged=%v err=%v", candidates, acknowledged, err)
			}
			if err := webhook.AckMailbox(loop.Store.Dir, acknowledged); err != nil {
				t.Fatal(err)
			}
			snapshot, err = loop.Store.Load()
			if err != nil || snapshot.Issues["1"].Status != test.wantStatus || snapshot.Issues["1"].PullRequestMerged != test.wantMerged {
				t.Fatalf("issue=%+v err=%v", snapshot.Issues["1"], err)
			}
			remaining, err := webhook.ReadMailbox(loop.Store.Dir)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("remaining=%v err=%v", remaining, err)
			}
		})
	}
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

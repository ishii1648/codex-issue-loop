package supervisor

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/failure"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type numberedFakeGitHub struct{ *fakeGitHub }

func (f numberedFakeGitHub) Get(_ context.Context, cfg config.Config, number int) (gh.Issue, error) {
	return gh.Issue{Number: number, Title: "Test", State: "OPEN", Labels: append([]string(nil), cfg.GitHub.ReadyLabels...)}, nil
}

type countingGitHub struct {
	*fakeGitHub
	mu        sync.Mutex
	listCalls int
	called    chan struct{}
	empty     bool
}

type startupRateLimitGitHub struct {
	*fakeGitHub
	inspectErr error
}

type startupRateLimitObserverGitHub struct {
	*startupRateLimitGitHub
	status      gh.RateLimitStatus
	statusCalls int
}

func (f *startupRateLimitObserverGitHub) PrimaryRateLimitStatus(context.Context, string) (gh.RateLimitStatus, bool) {
	f.statusCalls++
	return f.status, true
}

func (f *startupRateLimitGitHub) Inspect(ctx context.Context, cfg config.Config, number int, branch string) (gh.RemoteState, error) {
	f.inspectCalls++
	if f.inspectErr != nil {
		return gh.RemoteState{}, f.inspectErr
	}
	return f.fakeGitHub.Inspect(ctx, cfg, number, branch)
}

func (f *countingGitHub) ListReady(ctx context.Context, cfg config.Config) ([]gh.Issue, error) {
	f.mu.Lock()
	f.listCalls++
	f.mu.Unlock()
	if f.called != nil {
		select {
		case f.called <- struct{}{}:
		default:
		}
	}
	if f.empty {
		return []gh.Issue{}, nil
	}
	return f.fakeGitHub.ListReady(ctx, cfg)
}

func (f *countingGitHub) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

type inertSchedulerTimer struct{ ch chan time.Time }

func (t inertSchedulerTimer) C() <-chan time.Time { return t.ch }
func (t inertSchedulerTimer) Stop() bool          { return true }

type inertSchedulerTimers struct{ created chan struct{} }

func (t inertSchedulerTimers) NewTimer(time.Duration) SchedulerTimer {
	select {
	case t.created <- struct{}{}:
	default:
	}
	return inertSchedulerTimer{ch: make(chan time.Time)}
}

func waitForTimers(t *testing.T, created <-chan struct{}, count int) {
	t.Helper()
	for range count {
		select {
		case <-created:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for scheduler timer")
		}
	}
}

func TestSchedulerFsnotifyWakeCannotBypassSupervisorRetryDeadline(t *testing.T) {
	loop, fake := testLoop(t, worker.Result{})
	loop.Logger = log.New(io.Discard, "", 0)
	now := time.Now().UTC()
	retryAt := now.Add(time.Hour)
	_, err := loop.Store.Update("retry_fixture", 0, "", nil, func(snapshot *state.Snapshot) error {
		snapshot.Supervisor.State = "retry_wait"
		snapshot.Supervisor.ConsecutiveFailures = 2
		snapshot.Supervisor.RetryAfter = &retryAt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &countingGitHub{fakeGitHub: fake}
	loop.GitHub = client
	created := make(chan struct{}, 8)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	wakes := make(chan fsnotify.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.runSchedulerEvents(ctx, wakes, nil) }()
	waitForTimers(t, created, 2)
	wakes <- fsnotify.Event{Name: loop.Store.StatePath(), Op: fsnotify.Write}
	waitForTimers(t, created, 2)
	if got := client.calls(); got != 0 {
		t.Fatalf("GitHub calls before retry deadline=%d, want 0", got)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Supervisor.ConsecutiveFailures != 2 {
		t.Fatalf("no-op wake reset failure counter: %+v", snapshot.Supervisor)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerCoalescesSelfGeneratedWakeBacklog(t *testing.T) {
	loop, fake := testLoop(t, worker.Result{})
	loop.Logger = log.New(io.Discard, "", 0)
	client := &countingGitHub{fakeGitHub: fake, called: make(chan struct{}, 1), empty: true}
	loop.GitHub = client
	created := make(chan struct{}, 8)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	wakes := make(chan fsnotify.Event, 1200)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.runSchedulerEvents(ctx, wakes, nil) }()
	select {
	case <-client.called:
	case <-time.After(5 * time.Second):
		t.Fatal("initial GitHub poll did not complete")
	}
	waitForTimers(t, created, 1)
	for range 1100 {
		wakes <- fsnotify.Event{Name: loop.Store.EventsPath(), Op: fsnotify.Write}
	}
	waitForTimers(t, created, 1)
	if got := client.calls(); got != 1 {
		t.Fatalf("GitHub polls after 1100 wake events=%d, want 1", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerIgnoresUnrelatedFsnotifyEventsWithoutRebuildingTimers(t *testing.T) {
	loop, fake := testLoop(t, worker.Result{})
	loop.Logger = log.New(io.Discard, "", 0)
	client := &countingGitHub{fakeGitHub: fake, called: make(chan struct{}, 1), empty: true}
	loop.GitHub = client
	created := make(chan struct{}, 8)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	wakes := make(chan fsnotify.Event, 1200)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.runSchedulerEvents(ctx, wakes, nil) }()
	select {
	case <-client.called:
	case <-time.After(5 * time.Second):
		t.Fatal("initial GitHub poll did not complete")
	}
	waitForTimers(t, created, 1)

	for range 1100 {
		wakes <- fsnotify.Event{Name: loop.Store.Dir, Op: fsnotify.Chmod}
	}
	select {
	case <-created:
		t.Fatal("unrelated fsnotify events rebuilt the scheduler timer")
	case <-time.After(250 * time.Millisecond):
	}
	if got := client.calls(); got != 1 {
		t.Fatalf("GitHub polls after unrelated wake events=%d, want 1", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSchedulerSharesPrimaryRateLimitCooldownAcrossRepositories(t *testing.T) {
	now := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(20 * time.Minute)
	shared := ratelimit.Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}

	first, firstGitHub := testLoop(t, worker.Result{})
	first.Clock = fixedClock{value: now}
	first.Logger = log.New(io.Discard, "", 0)
	first.Random = fixedRandom(0.5)
	first.RateLimits = shared
	firstGitHub.listErr = &gh.RateLimitError{
		Resource: "graphql", ResetAt: resetAt, Source: "rest-rate-limit", Err: errors.New("GraphQL: API rate limit exceeded"),
	}
	firstScheduler := &scheduler{
		loop: first, events: make(chan schedulerEvent, 2), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{}, pollAt: now,
	}
	if _, err := firstScheduler.schedule(context.Background(), true); err == nil {
		t.Fatal("primary rate limit was not returned")
	} else if fatal := firstScheduler.handleCycleError(err); fatal != nil {
		t.Fatal(fatal)
	}

	second, secondFake := testLoop(t, worker.Result{})
	second.Clock = fixedClock{value: now}
	second.Logger = log.New(io.Discard, "", 0)
	second.Random = fixedRandom(0.5)
	second.RateLimits = shared
	secondClient := &countingGitHub{fakeGitHub: secondFake, empty: true}
	second.GitHub = secondClient
	secondScheduler := &scheduler{
		loop: second, events: make(chan schedulerEvent, 2), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{}, pollAt: now,
	}
	if result, err := secondScheduler.schedule(context.Background(), true); err != nil || result.githubSucceeded {
		t.Fatalf("suppressed result=%+v err=%v", result, err)
	}
	if got := secondClient.calls(); got != 0 {
		t.Fatalf("second repository called GitHub during shared cooldown: %d", got)
	}
	snapshot, err := second.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Supervisor.RateLimit == nil || snapshot.Supervisor.RateLimit.Resource != "graphql" || snapshot.Supervisor.RateLimit.CooldownSource != "rest-rate-limit" || snapshot.Supervisor.RateLimit.SuppressedRetryCount != 1 {
		t.Fatalf("shared cooldown status=%+v", snapshot.Supervisor)
	}

	second.Clock = fixedClock{value: resetAt.Add(time.Second)}
	if result, err := secondScheduler.schedule(context.Background(), true); err != nil || !result.githubSucceeded {
		t.Fatalf("post-reset result=%+v err=%v", result, err)
	}
	if got := secondClient.calls(); got != 1 {
		t.Fatalf("post-reset GitHub calls=%d, want 1", got)
	}
}

func TestStartupReconciliationObservesRateLimitWithoutExiting(t *testing.T) {
	now := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(20 * time.Minute)
	loop, fake := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: now}
	loop.Logger = log.New(io.Discard, "", 0)
	loop.RateLimits = ratelimit.Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}
	client := &startupRateLimitGitHub{
		fakeGitHub: fake,
		inspectErr: &gh.RateLimitError{
			Resource: "graphql", ResetAt: resetAt, Source: "rest-rate-limit", Err: errors.New("GraphQL: API rate limit exceeded"),
		},
	}
	loop.GitHub = client
	_, err := loop.Store.Update("startup_fixture", 7, "run_7", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["7"] = &state.Issue{Number: 7, Status: "running", RunID: "run_7"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created := make(chan struct{}, 2)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.reconcileStartupWithRateLimit(ctx, snapshot) }()
	waitForTimers(t, created, 1)
	if client.inspectCalls != 1 {
		t.Fatalf("startup GitHub calls=%d, want 1", client.inspectCalls)
	}
	cooldown, active, err := loop.RateLimits.Current(now)
	if err != nil || !active || !cooldown.ResetAt.Equal(resetAt) {
		t.Fatalf("shared cooldown=%+v active=%v err=%v", cooldown, active, err)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Supervisor.RateLimit == nil || loaded.Supervisor.RetryAfter == nil || !loaded.Supervisor.RetryAfter.Equal(resetAt) {
		t.Fatalf("startup rate-limit state=%+v", loaded.Supervisor)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup reconciliation exit=%v, want context canceled", err)
	}
}

func TestStartupReconciliationUsesSharedCooldownBeforeGitHub(t *testing.T) {
	now := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	resetAt := now.Add(20 * time.Minute)
	loop, fake := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: now}
	loop.Logger = log.New(io.Discard, "", 0)
	loop.RateLimits = ratelimit.Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}
	if _, err := loop.RateLimits.Observe(ratelimit.Cooldown{Resource: "graphql", ResetAt: resetAt, Source: "rest-rate-limit"}, now); err != nil {
		t.Fatal(err)
	}
	client := &startupRateLimitGitHub{fakeGitHub: fake}
	loop.GitHub = client
	_, err := loop.Store.Update("startup_fixture", 7, "run_7", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["7"] = &state.Issue{Number: 7, Status: "blocked", RunID: "run_7"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created := make(chan struct{}, 2)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.reconcileStartupWithRateLimit(ctx, snapshot) }()
	waitForTimers(t, created, 1)
	if client.inspectCalls != 0 {
		t.Fatalf("startup called GitHub during shared cooldown: %d", client.inspectCalls)
	}
	cooldown, active, err := loop.RateLimits.Current(now)
	if err != nil || !active || cooldown.SuppressedRetryCount != 1 {
		t.Fatalf("shared cooldown=%+v active=%v err=%v", cooldown, active, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup reconciliation exit=%v, want context canceled", err)
	}
}

func TestStartupReconciliationShortensStaleCooldownWhenRESTHasRemaining(t *testing.T) {
	now := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	loop, fake := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: now}
	loop.Logger = log.New(io.Discard, "", 0)
	loop.RateLimits = ratelimit.Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}
	if _, err := loop.RateLimits.Observe(ratelimit.Cooldown{Resource: "graphql", ResetAt: now.Add(time.Hour), Source: "rest-rate-limit"}, now); err != nil {
		t.Fatal(err)
	}
	client := &startupRateLimitObserverGitHub{
		startupRateLimitGitHub: &startupRateLimitGitHub{fakeGitHub: fake},
		status:                 gh.RateLimitStatus{Resource: "graphql", ResetAt: now.Add(time.Hour), Remaining: 4930},
	}
	loop.GitHub = client
	_, err := loop.Store.Update("startup_fixture", 7, "run_7", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["7"] = &state.Issue{Number: 7, Status: "blocked", RunID: "run_7"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	created := make(chan struct{}, 2)
	loop.SchedulerTimers = inertSchedulerTimers{created: created}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- loop.reconcileStartupWithRateLimit(ctx, snapshot) }()
	waitForTimers(t, created, 1)
	if client.inspectCalls != 0 {
		t.Fatalf("startup called GitHub before recovered retry deadline: %d", client.inspectCalls)
	}
	cooldown, active, err := loop.RateLimits.Current(now)
	if err != nil || !active || !cooldown.ResetAt.Equal(now.Add(5*time.Second)) || cooldown.Source != "rest-rate-limit-recovered" {
		t.Fatalf("revalidated cooldown=%+v active=%v err=%v", cooldown, active, err)
	}
	if client.statusCalls != 1 {
		t.Fatalf("REST rate-limit status calls=%d, want 1", client.statusCalls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup reconciliation exit=%v, want context canceled", err)
	}
}

func TestStartupReconciliationDoesNotExtendRecoveredCooldown(t *testing.T) {
	now := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC)
	loop, fake := testLoop(t, worker.Result{})
	client := &startupRateLimitObserverGitHub{
		startupRateLimitGitHub: &startupRateLimitGitHub{fakeGitHub: fake},
		status:                 gh.RateLimitStatus{Resource: "graphql", ResetAt: now.Add(time.Hour), Remaining: 4930},
	}
	loop.GitHub = client
	want := ratelimit.Cooldown{Resource: "graphql", ResetAt: now.Add(5 * time.Second), Source: "rest-rate-limit-recovered"}
	got, err := loop.revalidateStartupCooldown(context.Background(), want, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !got.ResetAt.Equal(want.ResetAt) || got.Source != want.Source {
		t.Fatalf("recovered cooldown was changed: got=%+v want=%+v", got, want)
	}
	if client.statusCalls != 0 {
		t.Fatalf("recovered cooldown triggered REST revalidation: %d", client.statusCalls)
	}
}

func TestSchedulerTransientFailuresIncreaseWithoutNoOpRecovery(t *testing.T) {
	now := time.Date(2026, 8, 17, 1, 0, 0, 0, time.UTC)
	loop, _ := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: now}
	loop.Random = fixedRandom(0.5)
	loop.Logger = log.New(io.Discard, "", 0)
	s := &scheduler{loop: loop, active: map[int]activeJob{}, issueRetry: map[int]time.Time{}, issueFails: map[int]int{}}
	for attempt, wantDelay := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second} {
		if fatal := s.handleCycleError(failure.Wrap(failure.Transient, "poll", errors.New("temporary"))); fatal != nil {
			t.Fatalf("attempt %d: %v", attempt+1, fatal)
		}
		snapshot, err := loop.Store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Supervisor.ConsecutiveFailures != attempt+1 || snapshot.Supervisor.RetryAfter == nil || !snapshot.Supervisor.RetryAfter.Equal(now.Add(wantDelay)) {
			t.Fatalf("attempt %d supervisor=%+v", attempt+1, snapshot.Supervisor)
		}
		result, err := s.schedule(context.Background(), false)
		if err != nil || result.githubSucceeded {
			t.Fatalf("no-op attempt %d result=%+v err=%v", attempt+1, result, err)
		}
		if s.consecutiveFailures != attempt+1 {
			t.Fatalf("no-op reset in-memory failures: got=%d want=%d", s.consecutiveFailures, attempt+1)
		}
	}
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
	if result, err := s.schedule(context.Background(), false); err != nil || !result.dispatched {
		t.Fatalf("result=%+v err=%v", result, err)
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
	if result, err := s.schedule(context.Background(), true); err != nil || result.dispatched {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if github.listCalls != 0 || github.conditionalCalls != 0 {
		t.Fatalf("list=%d conditional=%d", github.listCalls, github.conditionalCalls)
	}
}

func TestTwoWebhookRepositorySchedulersMakeZeroQueueRequestsAcrossFakeHour(t *testing.T) {
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)
	loops := make([]*Loop, 0, 2)
	schedulers := make([]*scheduler, 0, 2)
	clients := make([]*webhookFakeGitHub, 0, 2)
	for _, interval := range []time.Duration{10 * time.Minute, 15 * time.Minute} {
		loop, baseGitHub := testLoop(t, worker.Result{})
		loop.Config.Webhook.Mode = "webhook"
		loop.Config.Webhook.SafetySweepInterval.Duration = interval
		loop.Clock = fixedClock{value: base}
		github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
		loop.GitHub = github
		loops = append(loops, loop)
		clients = append(clients, github)
		schedulers = append(schedulers, &scheduler{
			loop: loop, events: make(chan schedulerEvent, 1), active: map[int]activeJob{},
			issueRetry: map[int]time.Time{}, issueFails: map[int]int{},
		})
	}
	for minute := 0; minute <= 60; minute++ {
		for index, loop := range loops {
			loop.Clock = fixedClock{value: base.Add(time.Duration(minute) * time.Minute)}
			if result, err := schedulers[index].schedule(context.Background(), true); err != nil || result.dispatched {
				t.Fatalf("minute=%d repo=%d result=%+v err=%v", minute, index+1, result, err)
			}
		}
	}
	for index, github := range clients {
		if github.listCalls != 0 || github.conditionalCalls != 0 {
			t.Fatalf("repo=%d list=%d conditional=%d", index+1, github.listCalls, github.conditionalCalls)
		}
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

func TestSweepCollectionExitUsesTargetedAuthorityAndBlocksManualExclusion(t *testing.T) {
	loop, baseGitHub := testLoop(t, worker.Result{})
	loop.Config.Webhook.Mode = "webhook"
	github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
	github.issue = gh.Issue{
		Number: 1, State: "open",
		Labels: []string{loop.Config.GitHub.ReadyLabels[0], loop.Config.GitHub.ExcludeLabels[0]},
	}
	loop.GitHub = github
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run-1", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("running_fixture", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].Status = "running"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery := webhook.Delivery{
		Version: webhook.InboxVersion, DeliveryID: "sweep-exit-1", Event: "issues", Action: "collection_exited",
		RepoID: loop.Store.RepoID, Repository: loop.Config.GitHub.Repo, IssueNumber: 1, AcceptedAt: loop.now(),
	}
	if err := webhook.EnqueueMailbox(loop.Store.Dir, delivery); err != nil {
		t.Fatal(err)
	}
	s := &scheduler{loop: loop, events: make(chan schedulerEvent, 1), active: map[int]activeJob{}, issueRetry: map[int]time.Time{}, issueFails: map[int]int{}}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	candidates, acknowledged, err := s.processMailbox(context.Background(), snapshot)
	if err != nil || len(candidates) != 0 || len(acknowledged) != 1 || github.restGets != 1 {
		t.Fatalf("candidates=%v acknowledged=%v rest_gets=%d err=%v", candidates, acknowledged, github.restGets, err)
	}
	snapshot, err = loop.Store.Load()
	if err != nil || snapshot.Issues["1"].Status != "blocked" || snapshot.Issues["1"].Lease != nil {
		t.Fatalf("issue=%+v err=%v", snapshot.Issues["1"], err)
	}
}

func TestSweepCollectionExitDoesNotMisreadNormalClaimAsManualExclusion(t *testing.T) {
	loop, baseGitHub := testLoop(t, worker.Result{})
	loop.Config.Webhook.Mode = "webhook"
	github := &webhookFakeGitHub{fakeGitHub: baseGitHub}
	github.issue = gh.Issue{Number: 1, State: "open", Labels: []string{loop.Config.GitHub.RunningLabel}}
	loop.GitHub = github
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run-1", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("running_fixture", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].Status = "running"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery := webhook.Delivery{
		Version: webhook.InboxVersion, DeliveryID: "sweep-claimed-1", Event: "issues", Action: "collection_exited",
		RepoID: loop.Store.RepoID, Repository: loop.Config.GitHub.Repo, IssueNumber: 1, AcceptedAt: loop.now(),
	}
	if err := webhook.EnqueueMailbox(loop.Store.Dir, delivery); err != nil {
		t.Fatal(err)
	}
	s := &scheduler{loop: loop, events: make(chan schedulerEvent, 1), active: map[int]activeJob{}, issueRetry: map[int]time.Time{}, issueFails: map[int]int{}}
	snapshot, _ := loop.Store.Load()
	_, acknowledged, err := s.processMailbox(context.Background(), snapshot)
	if err != nil || len(acknowledged) != 1 || github.restGets != 1 {
		t.Fatalf("acknowledged=%v rest_gets=%d err=%v", acknowledged, github.restGets, err)
	}
	snapshot, err = loop.Store.Load()
	if err != nil || snapshot.Issues["1"].Status != "running" || snapshot.Issues["1"].Lease == nil {
		t.Fatalf("issue=%+v err=%v", snapshot.Issues["1"], err)
	}
}

func TestAuthoritativeCollectionExitFencesLateWorkerCompletion(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: 1, Title: "Test", RunID: "run-1", Slot: 0,
		ResolvedResources: []string{state.RepositoryResource}, ReservedAt: loop.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = loop.Store.Update("running_fixture", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"].Status = "running"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	workerState := *snapshot.Issues["1"]
	_, err = loop.Store.Update("webhook_terminal_reconciled", 1, "run-1", nil, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues["1"]
		if err := state.ReleaseIssueLease(item, item.Lease.Owner); err != nil {
			return err
		}
		item.Status = "blocked"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result := worker.Result{
		Version: 1, Status: "completed", ExecutionProfile: "extended", Summary: "late completion",
		Tests: []worker.Test{}, Git: &worker.GitResult{},
	}
	if err := loop.handleResult(context.Background(), gh.Issue{Number: 1}, workerState, result, nil); err != nil {
		t.Fatal(err)
	}
	snapshot, err = loop.Store.Load()
	if err != nil || snapshot.Issues["1"].Status != "blocked" || snapshot.Issues["1"].Lease != nil {
		t.Fatalf("late worker result changed authoritative state: issue=%+v err=%v", snapshot.Issues["1"], err)
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
	if result, err := s.schedule(ctx, true); err != nil || !result.dispatched {
		t.Fatalf("result=%+v err=%v", result, err)
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
	result, err := s.schedule(ctx, false)
	if err != nil || !result.dispatched {
		t.Fatalf("result=%+v err=%v", result, err)
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
			if result, err := s.schedule(ctx, false); err != nil || !result.dispatched {
				t.Fatalf("result=%+v err=%v", result, err)
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

package supervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/application/incidentloop"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

// SchedulerTimer and SchedulerTimerSource make deadline behavior deterministic
// in tests without making the durable Clock responsible for blocking.
type SchedulerTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type SchedulerTimerSource interface {
	NewTimer(time.Duration) SchedulerTimer
}

type systemSchedulerTimer struct{ timer *time.Timer }

func (t systemSchedulerTimer) C() <-chan time.Time { return t.timer.C }
func (t systemSchedulerTimer) Stop() bool          { return t.timer.Stop() }

type systemSchedulerTimers struct{}

func (systemSchedulerTimers) NewTimer(delay time.Duration) SchedulerTimer {
	return systemSchedulerTimer{timer: time.NewTimer(delay)}
}

type schedulerEventKind string

const schedulerJobFinished schedulerEventKind = "job_finished"

// schedulerEvent is the typed hand-off from a lifecycle goroutine. The run ID
// fences late results from a prior dispatch of the same Issue.
type schedulerEvent struct {
	Kind        schedulerEventKind
	IssueNumber int
	RunID       string
	Worked      bool
	Err         error
}

type activeJob struct {
	runID  string
	slot   int
	cancel context.CancelFunc
}

type scheduler struct {
	loop                *Loop
	runtimeRunID        string
	events              chan schedulerEvent
	active              map[int]activeJob
	issueRetry          map[int]time.Time
	issueFails          map[int]int
	terminalPoll        map[int]time.Time
	pollAt              time.Time
	cooldownUntil       time.Time
	lastSuppressedReset time.Time
	consecutiveFailures int
	rateLimitActive     bool
	lifecycleMu         sync.Mutex
}

type scheduleResult struct {
	dispatched      bool
	githubAttempted bool
	githubSucceeded bool
}

type lifecycleGateContextKey struct{}

func (l *Loop) runScheduler(ctx context.Context, watcher *fsnotify.Watcher) error {
	var watchEvents <-chan fsnotify.Event
	var watchErrors <-chan error
	if watcher != nil {
		watchEvents, watchErrors = watcher.Events, watcher.Errors
	}
	return l.runSchedulerEvents(ctx, watchEvents, watchErrors)
}

func (l *Loop) runSchedulerEvents(ctx context.Context, watchEvents <-chan fsnotify.Event, watchErrors <-chan error) error {
	l.enableRateLimitGate()
	current, err := l.Store.Load()
	if err != nil {
		return l.blockSupervisor(failure.Wrap(failure.Supervisor, "load scheduler state", err), failure.Supervisor, 1)
	}
	if current.Supervisor.State == state.SupervisorStateStarting {
		if _, err := l.Store.Update("supervisor_running", 0, "", nil, func(snapshot *state.Snapshot) error {
			if snapshot.Supervisor.State == state.SupervisorStateStarting {
				snapshot.Supervisor.State = state.SupervisorStateRunning
				snapshot.Supervisor.Message = ""
			}
			return nil
		}); err != nil {
			return l.blockSupervisor(failure.Wrap(failure.Supervisor, "mark scheduler running", err), failure.Supervisor, 1)
		}
	}
	concurrency := l.Config.Queue.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	s := &scheduler{
		loop: l, runtimeRunID: state.NewID("run"), events: make(chan schedulerEvent, concurrency+1), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{}, terminalPoll: map[int]time.Time{},
		consecutiveFailures: current.Supervisor.ConsecutiveFailures,
		rateLimitActive:     current.Supervisor.RateLimit != nil,
	}
	if !l.Config.Webhook.Enabled() {
		s.pollAt = l.now()
	}
	if current.Supervisor.RetryAfter != nil && (s.pollAt.IsZero() || current.Supervisor.RetryAfter.After(s.pollAt)) {
		s.pollAt = *current.Supervisor.RetryAfter
		s.cooldownUntil = *current.Supervisor.RetryAfter
	}

	pollCandidates := !l.Config.Webhook.Enabled() && !s.pollAt.After(l.now())
	trigger := "startup"
	coalescedWakeCount := 0
	for {
		cycleID := state.NewID("cycle")
		cycleStarted := l.now()
		var scheduledDeadline *time.Time
		if !s.pollAt.IsZero() {
			deadline := s.pollAt
			scheduledDeadline = &deadline
		}
		if err := l.recordIncidentSignal(incidentloop.Signal{
			RunID: s.runtimeRunID, CycleID: cycleID, Kind: "event", Name: "scheduler_cycle", Component: "scheduler", Phase: "poll",
			OutcomeCode: "started", ReasonCode: "scheduler_wake", Trigger: trigger,
			ScheduledDeadline: scheduledDeadline, AttemptAllowed: !s.pollAt.After(cycleStarted), CoalescedWakeCount: coalescedWakeCount,
		}); err != nil {
			return failure.Wrap(failure.Supervisor, "record scheduler cycle signal", err)
		}
		result, scheduleErr := s.schedule(ctx, pollCandidates)
		elapsed := l.now().Sub(cycleStarted)
		if elapsed < 0 {
			elapsed = 0
		}
		cycleOutcome := "observed"
		cycleReason := "scheduler_cycle_completed"
		if scheduleErr != nil {
			cycleOutcome = "failed"
			cycleReason = "scheduler_cycle_failed"
		}
		deadlineRemaining := int64(0)
		if scheduledDeadline != nil {
			deadlineRemaining = scheduledDeadline.Sub(l.now()).Milliseconds()
		}
		degradationThreshold := l.Config.IncidentAutomation.DegradationThreshold.Duration
		degraded := degradationThreshold > 0 && elapsed >= degradationThreshold
		if err := l.recordIncidentSignal(incidentloop.Signal{
			RunID: s.runtimeRunID, CycleID: cycleID, Kind: "event", Name: "operation_duration", Component: "scheduler", Phase: "poll",
			OutcomeCode: cycleOutcome, ReasonCode: cycleReason, OperationCode: "scheduler_cycle", ElapsedMS: elapsed.Milliseconds(), DeadlineRemainingMS: deadlineRemaining,
			ProgressStalled: degraded, ThresholdExceeded: degraded,
		}); err != nil {
			return failure.Wrap(failure.Supervisor, "record scheduler duration signal", err)
		}
		if result.githubAttempted {
			outcome, reason := "failed", "github_queue_poll_failed"
			if result.githubSucceeded {
				outcome, reason = "succeeded", "github_queue_polled"
			}
			if err := l.recordIncidentSignal(incidentloop.Signal{
				RunID: s.runtimeRunID, CycleID: cycleID, Kind: "event", Name: "external_attempt_completed", Component: "github", Phase: "poll",
				OutcomeCode: outcome, ReasonCode: reason, Provider: "github", OperationCode: "list_ready_issues",
			}); err != nil {
				return failure.Wrap(failure.Supervisor, "record GitHub attempt signal", err)
			}
		}
		if result.githubSucceeded {
			if err := l.recordIncidentSignal(incidentloop.Signal{
				RunID: s.runtimeRunID, CycleID: cycleID, EpisodeID: s.retryEpisodeID(), Kind: "status", Name: "progress", Component: "scheduler", Phase: "poll",
				OutcomeCode: "succeeded", ReasonCode: "github_queue_progress", ProgressKind: "github_poll",
			}); err != nil {
				return failure.Wrap(failure.Supervisor, "record scheduler progress signal", err)
			}
		}
		pollCandidates = false
		trigger = "schedule"
		coalescedWakeCount = 0
		if scheduleErr != nil {
			if fatal := s.handleCycleError(scheduleErr); fatal != nil {
				s.cancelAndDrain()
				return fatal
			}
		} else if result.githubSucceeded && (s.consecutiveFailures > 0 || s.rateLimitActive) {
			if resetErr := l.resetSupervisorFailures(s.consecutiveFailures); resetErr != nil {
				s.cancelAndDrain()
				return BlockedError{Err: failure.Wrap(failure.Supervisor, "reset supervisor failure counter", resetErr)}
			}
			s.consecutiveFailures = 0
			s.rateLimitActive = false
			s.cooldownUntil = time.Time{}
			if l.Config.Webhook.Enabled() {
				s.pollAt = time.Time{}
			}
		}
		var pollTimer, retryTimer, reconciliationTimer SchedulerTimer
		var pollTimerC, retryTimerC, reconciliationTimerC <-chan time.Time
		if !s.pollAt.IsZero() {
			pollTimer = l.newSchedulerTimer(until(l.now(), s.pollAt))
			pollTimerC = pollTimer.C()
		}
		if delay, ok := s.nextRetryDelay(); ok {
			retryTimer = l.newSchedulerTimer(delay)
			retryTimerC = retryTimer.C()
		}
		if l.Config.Webhook.Enabled() {
			delay := l.jitter(l.Config.Watch.ReconcileInterval.Duration, l.Config.Watch.ReconcileJitter, 0)
			reconciliationTimer = l.newSchedulerTimer(delay)
			reconciliationTimerC = reconciliationTimer.C()
		}
	waitForWake:
		for {
			select {
			case <-ctx.Done():
				stopSchedulerTimers(pollTimer, retryTimer, reconciliationTimer)
				s.cancelAndDrain()
				_, _ = l.Store.Update("supervisor_stopped", 0, "", map[string]string{"reason": ctx.Err().Error()}, func(snapshot *state.Snapshot) error {
					snapshot.Supervisor.State = state.SupervisorStateStopped
					snapshot.Supervisor.PID = 0
					snapshot.Supervisor.Message = ctx.Err().Error()
					return nil
				})
				return nil
			case event := <-s.events:
				stopSchedulerTimers(pollTimer, retryTimer, reconciliationTimer)
				if eventErr := s.handleEvent(event); eventErr != nil {
					if fatal := s.handleCycleError(eventErr); fatal != nil {
						s.cancelAndDrain()
						return fatal
					}
				}
				// A freed slot immediately admits the next candidate instead of
				// waiting for the regular GitHub poll interval.
				if !l.Config.Webhook.Enabled() {
					s.pollAt = l.now()
					pollCandidates = true
				}
				trigger = "worker_finished"
				break waitForWake
			case <-pollTimerC:
				stopSchedulerTimers(retryTimer, reconciliationTimer)
				if l.Config.Webhook.Enabled() {
					// Webhook schedulers have no periodic GitHub queue timer. A non-zero
					// pollAt in this mode is only a shared supervisor cooldown deadline.
					s.pollAt = time.Time{}
					pollCandidates = false
				} else {
					pollCandidates = true
				}
				trigger = "poll_timer"
				break waitForWake
			case <-retryTimerC:
				stopSchedulerTimers(pollTimer, reconciliationTimer)
				trigger = "retry_timer"
				break waitForWake
			case <-reconciliationTimerC:
				stopSchedulerTimers(pollTimer, retryTimer)
				trigger = "reconciliation_timer"
				break waitForWake
			case event, ok := <-watchEvents:
				if !ok {
					watchEvents, watchErrors = nil, nil
					continue
				}
				base := filepath.Base(event.Name)
				if filepath.Dir(event.Name) != webhook.MailboxDir(l.Store.Dir) && base != "state.json" && base != "events.jsonl" && base != "delivery-maintenance.wake" {
					continue
				}
				stopSchedulerTimers(pollTimer, retryTimer, reconciliationTimer)
				watchEvents, watchErrors, coalescedWakeCount = coalesceWatchEventsCount(watchEvents, watchErrors)
				if !l.Config.Webhook.Enabled() {
					pollCandidates = s.hasFreeSlot()
				}
				if filepath.Dir(event.Name) == webhook.MailboxDir(l.Store.Dir) {
					trigger = "webhook"
				} else {
					trigger = "fsnotify"
				}
				break waitForWake
			case _, ok := <-watchErrors:
				if !ok {
					watchErrors = nil
				}
				// Timers remain authoritative when fsnotify reports an error.
			}
		}
	}
}

func stopSchedulerTimer(timer SchedulerTimer) {
	if timer != nil {
		timer.Stop()
	}
}

func stopSchedulerTimers(timers ...SchedulerTimer) {
	for _, timer := range timers {
		stopSchedulerTimer(timer)
	}
}

func (l *Loop) newSchedulerTimer(delay time.Duration) SchedulerTimer {
	if delay < 0 {
		delay = 0
	}
	timers := l.SchedulerTimers
	if timers == nil {
		timers = systemSchedulerTimers{}
	}
	return timers.NewTimer(delay)
}

func until(now, deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return 24 * time.Hour
	}
	if !deadline.After(now) {
		return 0
	}
	return deadline.Sub(now)
}

func coalesceWatchEvents(events <-chan fsnotify.Event, watchErrors <-chan error) (<-chan fsnotify.Event, <-chan error) {
	events, watchErrors, _ = coalesceWatchEventsCount(events, watchErrors)
	return events, watchErrors
}

func coalesceWatchEventsCount(events <-chan fsnotify.Event, watchErrors <-chan error) (<-chan fsnotify.Event, <-chan error, int) {
	count := 0
	for range 4096 {
		select {
		case _, ok := <-events:
			if !ok {
				return nil, nil, count
			}
			count++
		case _, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
			}
		default:
			return events, watchErrors, count
		}
	}
	return events, watchErrors, count
}

func (s *scheduler) schedule(ctx context.Context, pollCandidates bool) (scheduleResult, error) {
	if s.loop.Config.Webhook.Enabled() {
		// Defense in depth: even a stale timer or direct test invocation cannot
		// make a repository scheduler query the shared GitHub ready queue.
		pollCandidates = false
	}
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	result := scheduleResult{}
	snapshot, err := s.loop.Store.Load()
	if err != nil {
		return result, failure.Wrap(failure.Supervisor, "load durable state", err)
	}
	if err := s.preflight(snapshot); err != nil {
		return result, err
	}
	if s.loop.maintenanceRequested() {
		maintenanceState := state.SupervisorStateMaintenance
		if len(s.active) > 0 {
			maintenanceState = state.SupervisorStateDraining
		}
		if snapshot.Supervisor.State != maintenanceState || snapshot.Supervisor.Message != "host delivery maintenance fence is active" {
			_, err = s.loop.Store.Update("delivery_maintenance_observed", 0, "", map[string]string{"state": string(maintenanceState)}, func(current *state.Snapshot) error {
				current.Supervisor.State = maintenanceState
				current.Supervisor.Message = "host delivery maintenance fence is active"
				return nil
			})
		}
		return result, err
	}
	now := s.loop.now()
	if snapshot.Supervisor.RetryAfter != nil && snapshot.Supervisor.RetryAfter.After(s.pollAt) {
		s.pollAt = *snapshot.Supervisor.RetryAfter
	}
	if snapshot.Supervisor.RetryAfter != nil && snapshot.Supervisor.RetryAfter.After(now) {
		s.cooldownUntil = *snapshot.Supervisor.RetryAfter
		return result, nil
	}
	if s.pollAt.After(now) {
		pollCandidates = false
	}
	cooldown, active, err := s.loop.RateLimits.Current(now)
	if err != nil {
		return result, failure.Wrap(failure.Supervisor, "load shared GitHub rate-limit cooldown", err)
	}
	if active {
		s.cooldownUntil = cooldown.ResetAt
		if cooldown.ResetAt.After(s.pollAt) {
			s.pollAt = cooldown.ResetAt
		}
		dueIssues := len(pendingIssues(snapshot, now, s.loop.Config.Queue.Concurrency)) > 0
		if (dueIssues || pollCandidates && s.hasFreeSlot()) && !cooldown.ResetAt.Equal(s.lastSuppressedReset) {
			updated, stillActive, suppressErr := s.loop.RateLimits.Suppress(now)
			if suppressErr != nil {
				return result, failure.Wrap(failure.Supervisor, "record shared GitHub rate-limit suppression", suppressErr)
			}
			if stillActive {
				s.lastSuppressedReset = updated.ResetAt
				s.rateLimitActive = true
				if recordErr := s.loop.recordRateLimitSuppressed(updated); recordErr != nil {
					return result, failure.Wrap(failure.Supervisor, "persist GitHub rate-limit suppression", recordErr)
				}
			}
		}
		return result, nil
	}
	s.cooldownUntil = time.Time{}
	s.lastSuppressedReset = time.Time{}
	mailboxCandidates, mailboxBatch, err := s.processMailbox(ctx, snapshot)
	if err != nil {
		if len(mailboxBatch) > 0 {
			if ackErr := webhook.AckMailbox(s.loop.Store.Dir, mailboxBatch); ackErr != nil {
				return result, failure.Wrap(failure.Supervisor, "ack safely coalesced webhook mailbox after reconciliation failure", ackErr)
			}
		}
		return result, failure.Wrap(failure.Supervisor, "read webhook mailbox", err)
	}
	if len(mailboxBatch) > 0 {
		snapshot, err = s.loop.Store.Load()
		if err != nil {
			return result, failure.Wrap(failure.Supervisor, "reload webhook-routed state", err)
		}
	}

	for _, current := range pendingIssues(snapshot, now, s.loop.Config.Queue.Concurrency) {
		if _, running := s.active[current.Number]; running || s.retryPending(current.Number) {
			continue
		}
		slot := -1
		if issueDispatchesWorker(current, state.PendingEffect(&snapshot, current.Number) != nil) {
			var ok bool
			slot, ok = s.freeSlot()
			if !ok {
				continue
			}
		} else if s.hasMaintenanceJob() {
			continue
		}
		s.dispatch(ctx, current.Number, current.RunID, slot, func(jobCtx context.Context) error {
			return s.loop.processExisting(jobCtx, current)
		})
		result.dispatched = true
	}

	if len(mailboxCandidates) > 0 && s.hasFreeSlot() {
		selected, ok, selectErr := s.selectReady(ctx, mailboxCandidates, snapshot)
		if selectErr != nil {
			return result, failure.Wrap(failure.Supervisor, "select webhook Issue admission", selectErr)
		}
		if ok {
			slot, slotOK := s.freeSlot()
			if slotOK {
				runID := state.NewID("run")
				s.dispatch(ctx, selected.Number, runID, slot, func(jobCtx context.Context) error {
					return s.loop.startIssue(jobCtx, selected, runID)
				})
				result.dispatched = true
			}
		}
	}
	if len(mailboxBatch) > 0 {
		if err := webhook.AckMailbox(s.loop.Store.Dir, mailboxBatch); err != nil {
			return result, failure.Wrap(failure.Supervisor, "ack webhook mailbox", err)
		}
	}

	if !pollCandidates {
		return result, s.markPollingIfIdle(snapshot, "")
	}
	if s.dispatchTerminalPullRequestReconciliation(ctx, snapshot) {
		result.dispatched = true
	}
	if !s.hasFreeSlot() {
		s.pollAt = s.loop.now().Add(s.pollDelay())
		return result, nil
	}
	issues, err := s.listReady(ctx)
	result.githubAttempted = true
	if err != nil {
		return result, failure.Wrap(failure.Transient, "poll GitHub Issue queue", err)
	}
	result.githubSucceeded = true
	s.pollAt = s.loop.now().Add(s.pollDelay())
	selected, ok, selectErr := s.selectReady(ctx, issues, snapshot)
	if selectErr != nil {
		return result, failure.Wrap(failure.Supervisor, "select Issue admission", selectErr)
	}
	if !ok {
		return result, s.markPollingIfIdle(snapshot, "")
	}
	slot, ok := s.freeSlot()
	if !ok {
		return result, nil
	}
	runID := state.NewID("run")
	s.dispatch(ctx, selected.Number, runID, slot, func(jobCtx context.Context) error {
		return s.loop.startIssue(jobCtx, selected, runID)
	})
	result.dispatched = true
	return result, nil
}

func (s *scheduler) handleCycleError(cause error) error {
	now := s.loop.now()
	if observed, limited := cooldownFromError(cause, now); limited {
		cooldown, err := s.loop.RateLimits.Observe(observed, now)
		if err != nil {
			return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "persist shared GitHub rate-limit cooldown", err), failure.Supervisor, s.consecutiveFailures+1)
		}
		s.consecutiveFailures++
		s.rateLimitActive = true
		s.cooldownUntil = cooldown.ResetAt
		s.pollAt = cooldown.ResetAt
		if err := s.recordRetrySignals(cause, "github_rate_limit", s.consecutiveFailures, cooldown.ResetAt, false); err != nil {
			return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "record rate-limit incident signal", err), failure.Supervisor, s.consecutiveFailures)
		}
		if err := s.loop.recordIncidentSignal(incidentloop.Signal{
			RunID: s.runtimeRunID, EpisodeID: s.retryEpisodeID(), Kind: "event", Name: "external_attempt_completed", Component: "github", Phase: "poll",
			OutcomeCode: "rate_limited", ReasonCode: "github_rate_limit", Provider: "github", OperationCode: "list_ready_issues",
			RateLimitResource: cooldown.Resource, ResetAt: &cooldown.ResetAt,
		}); err != nil {
			return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "record rate-limit attempt signal", err), failure.Supervisor, s.consecutiveFailures)
		}
		s.loop.Logger.Printf("GitHub %s primary rate limit reached; suppressing requests until %s (source=%s)", cooldown.Resource, cooldown.ResetAt.Format(time.RFC3339), cooldown.Source)
		if err := s.loop.recordSupervisorRateLimit(cause, s.consecutiveFailures, cooldown); err != nil {
			return BlockedError{Err: failure.Wrap(failure.Supervisor, "persist supervisor rate-limit cooldown", err)}
		}
		return nil
	}
	kind := failure.KindOf(cause)
	if kind == failure.Supervisor {
		if err := s.loop.recordFailureSignal("supervisor", "poll", 0, s.runtimeRunID, s.retryEpisodeID(), "supervisor_failure", cause, s.consecutiveFailures+1, nil, true); err != nil {
			return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "record supervisor failure signal", err), failure.Supervisor, s.consecutiveFailures+1)
		}
		return s.loop.blockSupervisor(cause, kind, s.consecutiveFailures+1)
	}
	s.consecutiveFailures++
	s.loop.Logger.Printf("scheduler cycle failed (%d, %s): %v", s.consecutiveFailures, kind, cause)
	if s.consecutiveFailures >= 5 {
		if err := s.loop.recordFailureSignal("scheduler", "poll", 0, s.runtimeRunID, s.retryEpisodeID(), "scheduler_retry_exhausted", cause, s.consecutiveFailures, nil, true); err != nil {
			return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "record retry exhaustion signal", err), failure.Supervisor, s.consecutiveFailures)
		}
		return s.loop.blockSupervisor(cause, kind, s.consecutiveFailures)
	}
	delay := s.loop.retryDelay(s.consecutiveFailures)
	retryAt := now.Add(delay)
	if err := s.recordRetrySignals(cause, "scheduler_cycle_failed", s.consecutiveFailures, retryAt, false); err != nil {
		return s.loop.blockSupervisor(failure.Wrap(failure.Supervisor, "record scheduler retry signal", err), failure.Supervisor, s.consecutiveFailures)
	}
	if err := s.loop.recordSupervisorRetry(cause, kind, s.consecutiveFailures, delay); err != nil {
		return BlockedError{Err: failure.Wrap(failure.Supervisor, "persist supervisor retry", err)}
	}
	s.pollAt = retryAt
	s.cooldownUntil = s.pollAt
	return nil
}

func (s *scheduler) retryEpisodeID() string {
	return "supervisor-retry-" + incidentloop.OpaqueScopeID(s.loop.Store.RepoID)
}

func (s *scheduler) recordRetrySignals(cause error, code string, attempt int, retryAt time.Time, human bool) error {
	episodeID := s.retryEpisodeID()
	if err := s.loop.recordFailureSignal("scheduler", "poll", 0, s.runtimeRunID, episodeID, code, cause, attempt, &retryAt, human); err != nil {
		return err
	}
	return s.loop.recordIncidentSignal(incidentloop.Signal{
		RunID: s.runtimeRunID, EpisodeID: episodeID, Kind: "event", Name: "retry_episode", Component: "scheduler", Phase: "poll",
		OutcomeCode: "retrying", ReasonCode: code, FailureKind: string(failure.KindOf(cause)), FailureCode: code,
		ScopeKind: "repository", Attempt: attempt, RetryAt: &retryAt,
	})
}

func (s *scheduler) pollInterval() time.Duration {
	return s.loop.Config.Queue.PollInterval.Duration
}

func (s *scheduler) pollDelay() time.Duration {
	interval := s.pollInterval()
	return s.loop.pollDelay(interval)
}

func (s *scheduler) listReady(ctx context.Context) ([]gh.Issue, error) {
	return s.loop.GitHub.ListReady(ctx, s.loop.Config)
}

func (s *scheduler) processMailbox(ctx context.Context, snapshot state.Snapshot) ([]gh.Issue, []webhook.Delivery, error) {
	if !s.loop.Config.Webhook.Enabled() {
		return nil, nil, nil
	}
	deliveries, err := webhook.ReadMailbox(s.loop.Store.Dir)
	if err != nil || len(deliveries) == 0 {
		return nil, deliveries, err
	}
	candidates := make([]gh.Issue, 0)
	acknowledged := make([]webhook.Delivery, 0, len(deliveries))
	issueNumbers := make([]int, len(deliveries))
	newestByIssue := map[int]int{}
	for index, delivery := range deliveries {
		number := mailboxIssueNumber(snapshot, delivery)
		issueNumbers[index] = number
		if number > 0 {
			newestByIssue[number] = index
		}
	}
	for index, number := range issueNumbers {
		if number > 0 && newestByIssue[number] != index {
			acknowledged = append(acknowledged, deliveries[index])
		}
	}
	for index := len(deliveries) - 1; index >= 0; index-- {
		delivery := deliveries[index]
		number := issueNumbers[index]
		if number == 0 {
			acknowledged = append(acknowledged, delivery)
			continue
		}
		if newestByIssue[number] != index {
			continue
		}
		if local := snapshot.Issues[fmt.Sprint(number)]; local != nil {
			if delivery.Event == "issues" && delivery.Action == "collection_exited" {
				converged, reconcileErr := s.loop.reconcileCollectionExit(ctx, *local, delivery)
				if reconcileErr != nil {
					return nil, acknowledged, reconcileErr
				}
				if converged {
					if job, active := s.active[number]; active && job.runID == local.RunID {
						job.cancel()
					}
				}
				acknowledged = append(acknowledged, delivery)
				continue
			}
			if local.Status.TerminalForWebhook() {
				handled, reconcileErr := s.loop.reconcileTerminalWebhook(ctx, *local, delivery)
				if reconcileErr != nil {
					return nil, acknowledged, reconcileErr
				}
				if handled {
					acknowledged = append(acknowledged, delivery)
				}
				continue
			}
			if !local.Status.WebhookRoutable() && state.PendingEffect(&snapshot, local.Number) == nil {
				acknowledged = append(acknowledged, delivery)
				continue
			}
			_, updateErr := s.loop.Store.Update("webhook_scheduler_wake", number, local.RunID, map[string]any{
				"delivery_id": delivery.DeliveryID, "event": delivery.Event, "action": delivery.Action,
			}, func(current *state.Snapshot) error {
				item := current.Issues[fmt.Sprint(number)]
				if item != nil {
					now := s.loop.now()
					item.RetryAfter = &now
					if delivery.HeadSHA != "" {
						item.HeadSHA = delivery.HeadSHA
					}
					if delivery.PullRequestNumber != 0 {
						item.PullRequestNumber = delivery.PullRequestNumber
					}
				}
				return nil
			})
			if updateErr != nil {
				return nil, acknowledged, updateErr
			}
			acknowledged = append(acknowledged, delivery)
			continue
		}
		if delivery.Event != "issues" && delivery.Event != "issue_comment" {
			acknowledged = append(acknowledged, delivery)
			continue
		}
		candidate, getErr := s.loop.getIssue(ctx, number)
		if getErr != nil {
			return nil, acknowledged, getErr
		}
		if gh.EligibleIssue(candidate, s.loop.Config.GitHub) {
			candidates = append(candidates, candidate)
			// Keep the delivery until the durable local Issue exists. A crash
			// between selection and execution start can then replay safely.
			// Older deliveries for the same Issue are acknowledged because this
			// newest intent is sufficient to repeat the authoritative read.
		} else {
			acknowledged = append(acknowledged, delivery)
		}
	}
	return candidates, acknowledged, nil
}

func mailboxIssueNumber(snapshot state.Snapshot, delivery webhook.Delivery) int {
	if delivery.IssueNumber > 0 {
		return delivery.IssueNumber
	}
	if delivery.PullRequestNumber != 0 {
		for _, issue := range snapshot.Issues {
			if issue != nil && (issue.PullRequestNumber == delivery.PullRequestNumber || strings.HasSuffix(issue.PullRequestURL, "/pull/"+fmt.Sprint(delivery.PullRequestNumber))) {
				return issue.Number
			}
		}
	}
	if delivery.HeadSHA != "" {
		for _, issue := range snapshot.Issues {
			if issue != nil && issue.HeadSHA == delivery.HeadSHA {
				return issue.Number
			}
		}
	}
	return 0
}

// dispatchTerminalPullRequestReconciliation checks at most one terminal Issue
// per queue poll. Per-Issue cooldowns keep sticky/manual blocks from consuming
// GitHub API budget every cycle while still allowing all candidates to make
// progress without restarting the repository supervisor.
func (s *scheduler) dispatchTerminalPullRequestReconciliation(ctx context.Context, snapshot state.Snapshot) bool {
	if s.hasMaintenanceJob() {
		return false
	}
	if s.terminalPoll == nil {
		s.terminalPoll = map[int]time.Time{}
	}
	now := s.loop.now()
	candidates := make([]state.Issue, 0)
	for _, issue := range snapshot.Issues {
		if issue == nil || !terminalReconciliationCandidate(*issue) {
			continue
		}
		if _, active := s.active[issue.Number]; active {
			continue
		}
		if s.retryPending(issue.Number) {
			continue
		}
		if next := s.terminalPoll[issue.Number]; next.After(now) {
			continue
		}
		candidates = append(candidates, *issue)
	}
	if len(candidates) == 0 {
		return false
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := s.terminalPoll[candidates[i].Number], s.terminalPoll[candidates[j].Number]
		if !left.Equal(right) {
			return left.Before(right)
		}
		return candidates[i].Number < candidates[j].Number
	})
	current := candidates[0]
	delay := 5 * s.loop.Config.Queue.PollInterval.Duration
	if delay < 5*time.Minute {
		delay = 5 * time.Minute
	}
	s.terminalPoll[current.Number] = now.Add(delay)
	s.dispatch(ctx, current.Number, current.RunID, -1, func(jobCtx context.Context) error {
		return s.loop.reconcileTerminalPullRequest(jobCtx, current)
	})
	return true
}

func hasPendingRequests(snapshot state.Snapshot) bool {
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.Status == issuedomain.RequestStatusPending {
			return true
		}
	}
	return false
}

func (s *scheduler) selectReady(ctx context.Context, issues []gh.Issue, snapshot state.Snapshot) (gh.Issue, bool, error) {
	if s.workerCount() != 0 || snapshot.ActiveExecution != nil {
		return gh.Issue{}, false, nil
	}
	candidates := append([]gh.Issue(nil), issues...)
	gh.OrderIssues(candidates, s.loop.Config.Queue)
	for _, candidate := range candidates {
		if !gh.EligibleIssue(candidate, s.loop.Config.GitHub) {
			continue
		}
		if snapshot.QuarantinedIssues[fmt.Sprint(candidate.Number)] != nil {
			continue
		}
		if current := snapshot.Issues[fmt.Sprint(candidate.Number)]; current != nil && current.Status.IneligibleForAdmission() {
			continue
		}
		if _, running := s.active[candidate.Number]; running {
			continue
		}
		verification, err := s.loop.GitHub.VerifyIssueAuthor(ctx, s.loop.Config, candidate)
		if err != nil {
			if !state.SameAuthorDecision(snapshot.IntakeVerifications[fmt.Sprint(candidate.Number)], verification) {
				if recordErr := s.loop.Store.RecordAuthorVerification(candidate.Number, verification); recordErr != nil {
					return gh.Issue{}, false, failure.Wrap(failure.Supervisor, "record Issue author verification", recordErr)
				}
			}
			s.loop.Logger.Printf("Issue #%d author verification unavailable; skipping candidate: %v", candidate.Number, err)
			continue
		}
		if !verification.Trusted {
			if !state.SameAuthorDecision(snapshot.IntakeVerifications[fmt.Sprint(candidate.Number)], verification) {
				if recordErr := s.loop.Store.RecordAuthorVerification(candidate.Number, verification); recordErr != nil {
					return gh.Issue{}, false, failure.Wrap(failure.Supervisor, "record Issue author verification", recordErr)
				}
			}
			s.loop.Logger.Printf("Issue #%d author rejected; skipping candidate (reason=%s)", candidate.Number, verification.Reason)
			continue
		}
		return candidate, true, nil
	}
	return gh.Issue{}, false, nil
}

func (s *scheduler) preflight(snapshot state.Snapshot) error {
	diskAvailable := s.loop.DiskAvailable
	if diskAvailable == nil {
		diskAvailable = retention.AvailableBytes
	}
	available, err := diskAvailable(s.loop.Store.Dir)
	if err != nil {
		return failure.Wrap(failure.Supervisor, "inspect log storage capacity", err)
	}
	reserve := uint64(s.loop.Config.Logs.RotateBytes * 2)
	if available < reserve {
		return failure.Wrap(failure.Supervisor, "log storage safety reserve exhausted", fmt.Errorf("available=%d required=%d", available, reserve))
	}
	if err := s.loop.pruneRunLogs(snapshot); err != nil {
		return failure.Wrap(failure.Supervisor, "prune worker run logs", err)
	}
	return nil
}

func (s *scheduler) dispatch(ctx context.Context, number int, runID string, slot int, run func(context.Context) error) {
	jobCtx, cancel := context.WithCancel(ctx)
	s.active[number] = activeJob{runID: runID, slot: slot, cancel: cancel}
	delete(s.issueRetry, number)
	go func() {
		s.lifecycleMu.Lock()
		err := run(context.WithValue(jobCtx, lifecycleGateContextKey{}, &s.lifecycleMu))
		s.lifecycleMu.Unlock()
		s.events <- schedulerEvent{Kind: schedulerJobFinished, IssueNumber: number, RunID: runID, Worked: true, Err: err}
	}()
}

func (l *Loop) runWorker(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started worker.Started) (worker.Result, error) {
	return withWorkerSlot(ctx, func() (worker.Result, error) {
		validated, err := l.validateWorkerLaunch(ctx, cfg, current)
		if err != nil {
			return worker.Result{}, err
		}
		cfg.RepoPath = validated.CanonicalCWD
		return l.Worker.Run(ctx, cfg, issue, current, prompt, started)
	})
}

func (l *Loop) resumeWorker(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started worker.Started) (worker.Result, error) {
	return withWorkerSlot(ctx, func() (worker.Result, error) {
		validated, err := l.validateWorkerLaunch(ctx, cfg, current)
		if err != nil {
			return worker.Result{}, err
		}
		cfg.RepoPath = validated.CanonicalCWD
		return l.Worker.Resume(ctx, cfg, issue, current, prompt, started)
	})
}

func withWorkerSlot(ctx context.Context, run func() (worker.Result, error)) (worker.Result, error) {
	gate, _ := ctx.Value(lifecycleGateContextKey{}).(*sync.Mutex)
	if gate == nil {
		return run()
	}
	gate.Unlock()
	defer gate.Lock()
	return run()
}

func (s *scheduler) handleEvent(event schedulerEvent) error {
	if event.Kind != schedulerJobFinished {
		return failure.Wrap(failure.Supervisor, "handle scheduler event", fmt.Errorf("unknown event kind %q", event.Kind))
	}
	job, ok := s.active[event.IssueNumber]
	if !ok || job.runID != event.RunID {
		return failure.Wrap(failure.Supervisor, "handle scheduler event", fmt.Errorf("stale event for Issue #%d run %s", event.IssueNumber, event.RunID))
	}
	job.cancel()
	delete(s.active, event.IssueNumber)
	if event.Err == nil {
		delete(s.issueFails, event.IssueNumber)
		return nil
	}
	if _, limited := cooldownFromError(event.Err, s.loop.now()); limited {
		return event.Err
	}
	if failure.KindOf(event.Err) == failure.Supervisor {
		return event.Err
	}
	s.issueFails[event.IssueNumber]++
	delay := s.loop.retryDelay(s.issueFails[event.IssueNumber])
	s.issueRetry[event.IssueNumber] = s.loop.now().Add(delay)
	s.loop.Logger.Printf("Issue #%d lifecycle failed without stopping other workers; retrying in %s: %v", event.IssueNumber, delay, event.Err)
	return nil
}

func (s *scheduler) cancelAndDrain() {
	for _, job := range s.active {
		job.cancel()
	}
	for len(s.active) > 0 {
		event := <-s.events
		if job, ok := s.active[event.IssueNumber]; ok && job.runID == event.RunID {
			delete(s.active, event.IssueNumber)
		}
	}
}

func (s *scheduler) retryPending(number int) bool {
	deadline, ok := s.issueRetry[number]
	return ok && deadline.After(s.loop.now())
}

func (s *scheduler) freeSlot() (int, bool) {
	limit := s.loop.Config.Queue.Concurrency
	if limit < 1 {
		limit = 1
	}
	used := make(map[int]bool, len(s.active))
	for _, job := range s.active {
		if job.slot >= 0 {
			used[job.slot] = true
		}
	}
	for slot := 0; slot < limit; slot++ {
		if !used[slot] {
			return slot, true
		}
	}
	return -1, false
}

func (s *scheduler) hasFreeSlot() bool {
	_, ok := s.freeSlot()
	return ok
}

func (s *scheduler) workerCount() int {
	count := 0
	for _, job := range s.active {
		if job.slot >= 0 {
			count++
		}
	}
	return count
}

func (s *scheduler) hasMaintenanceJob() bool {
	for _, job := range s.active {
		if job.slot < 0 {
			return true
		}
	}
	return false
}

func (s *scheduler) nextRetryDelay() (time.Duration, bool) {
	now := s.loop.now()
	if s.cooldownUntil.After(now) {
		return s.cooldownUntil.Sub(now), true
	}
	deadline := time.Time{}
	for number, value := range s.issueRetry {
		if _, active := s.active[number]; !active && (deadline.IsZero() || value.Before(deadline)) {
			deadline = value
		}
	}
	if snapshot, err := s.loop.Store.Load(); err == nil {
		for _, issue := range snapshot.Issues {
			if issue != nil && issue.RetryAfter != nil {
				if _, active := s.active[issue.Number]; !active && (deadline.IsZero() || issue.RetryAfter.Before(deadline)) {
					deadline = *issue.RetryAfter
				}
			}
		}
	}
	if deadline.IsZero() {
		return 0, false
	}
	return until(now, deadline), true
}

func (s *scheduler) markPollingIfIdle(snapshot state.Snapshot, message string) error {
	if len(s.active) != 0 || (snapshot.Supervisor.State == state.SupervisorStatePolling && snapshot.Supervisor.Message == message) {
		return nil
	}
	return s.loop.markPolling(message)
}

func pendingIssues(snapshot state.Snapshot, now time.Time, _ int) []state.Issue {
	result := make([]state.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		if issue == nil || !pendingIssue(*issue, state.PendingEffect(&snapshot, issue.Number) != nil, now) {
			continue
		}
		result = append(result, *issue)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result
}

func pendingIssue(issue state.Issue, effectPending bool, now time.Time) bool {
	if !issue.Status.DispatchPending(effectPending) {
		return false
	}
	return issue.RetryAfter == nil || !issue.RetryAfter.After(now)
}

func issueDispatchesWorker(issue state.Issue, effectPending bool) bool {
	return issue.Status.DispatchesWorkerWhile(effectPending)
}

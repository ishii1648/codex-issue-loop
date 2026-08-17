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
	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/failure"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
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
	events              chan schedulerEvent
	active              map[int]activeJob
	issueRetry          map[int]time.Time
	issueFails          map[int]int
	pollAt              time.Time
	cooldownUntil       time.Time
	lastSuppressedReset time.Time
	consecutiveFailures int
	rateLimitActive     bool
	lifecycleMu         sync.Mutex
}

type scheduleResult struct {
	dispatched      bool
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
	concurrency := l.Config.Queue.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	s := &scheduler{
		loop: l, events: make(chan schedulerEvent, concurrency+1), active: map[int]activeJob{},
		issueRetry: map[int]time.Time{}, issueFails: map[int]int{}, pollAt: l.now(),
		consecutiveFailures: current.Supervisor.ConsecutiveFailures,
		rateLimitActive:     current.Supervisor.RateLimit != nil,
	}
	if current.Supervisor.RetryAfter != nil && current.Supervisor.RetryAfter.After(s.pollAt) {
		s.pollAt = *current.Supervisor.RetryAfter
		s.cooldownUntil = *current.Supervisor.RetryAfter
	}

	pollCandidates := !s.pollAt.After(l.now())
	for {
		result, scheduleErr := s.schedule(ctx, pollCandidates)
		pollCandidates = false
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
		}
		pollTimer := l.newSchedulerTimer(until(l.now(), s.pollAt))
		retryTimer := l.newSchedulerTimer(s.nextRetryDelay())
		select {
		case <-ctx.Done():
			pollTimer.Stop()
			retryTimer.Stop()
			s.cancelAndDrain()
			_, _ = l.Store.Update("supervisor_stopped", 0, "", map[string]string{"reason": ctx.Err().Error()}, func(snapshot *state.Snapshot) error {
				snapshot.Supervisor.State = "stopped"
				snapshot.Supervisor.PID = 0
				snapshot.Supervisor.Message = ctx.Err().Error()
				return nil
			})
			return nil
		case event := <-s.events:
			pollTimer.Stop()
			retryTimer.Stop()
			if eventErr := s.handleEvent(event); eventErr != nil {
				if fatal := s.handleCycleError(eventErr); fatal != nil {
					s.cancelAndDrain()
					return fatal
				}
			}
		case <-pollTimer.C():
			retryTimer.Stop()
			pollCandidates = true
		case <-retryTimer.C():
			pollTimer.Stop()
		case event, ok := <-watchEvents:
			pollTimer.Stop()
			retryTimer.Stop()
			if !ok {
				watchEvents, watchErrors = nil, nil
				continue
			}
			base := filepath.Base(event.Name)
			if base != "state.json" && base != "events.jsonl" {
				continue
			}
			watchEvents, watchErrors = coalesceWatchEvents(watchEvents, watchErrors)
		case _, ok := <-watchErrors:
			pollTimer.Stop()
			retryTimer.Stop()
			if !ok {
				watchErrors = nil
			}
			// Timers remain authoritative when fsnotify reports an error.
		}
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
	for range 4096 {
		select {
		case _, ok := <-events:
			if !ok {
				return nil, nil
			}
		case _, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
			}
		default:
			return events, watchErrors
		}
	}
	return events, watchErrors
}

func (s *scheduler) schedule(ctx context.Context, pollCandidates bool) (scheduleResult, error) {
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
		dueIssues := len(pendingIssues(snapshot, now)) > 0
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

	for _, current := range pendingIssues(snapshot, now) {
		if _, running := s.active[current.Number]; running || s.retryPending(current.Number) {
			continue
		}
		slot := -1
		if issueUsesWorkerSlot(current) {
			var ok bool
			slot, ok = s.freeSlot()
			if !ok {
				continue
			}
			if current.Lease != nil && current.Lease.Slot != slot {
				if _, err := s.loop.Store.AssignLeaseSlot(current.Number, current.Lease.Owner, slot); err != nil {
					return result, failure.Wrap(failure.Supervisor, "assign worker slot", err)
				}
				current.Lease.Slot = slot
			}
		} else if s.hasMaintenanceJob() {
			continue
		}
		s.dispatch(ctx, current.Number, current.RunID, slot, func(jobCtx context.Context) error {
			return s.loop.processExisting(jobCtx, current)
		})
		result.dispatched = true
	}

	if !pollCandidates {
		return result, nil
	}
	if !s.hasFreeSlot() {
		s.pollAt = s.loop.now().Add(s.loop.pollDelay(s.loop.Config.Queue.PollInterval.Duration))
		return result, nil
	}
	if !s.loop.Config.Queue.ContinueAfterNeedsInput {
		if hasPendingRequests(snapshot) {
			s.pollAt = s.loop.now().Add(s.loop.pollDelay(s.loop.Config.Queue.PollInterval.Duration))
			return result, s.markPollingIfIdle(snapshot, "waiting for user input")
		}
	}
	issues, err := s.loop.GitHub.ListReady(ctx, s.loop.Config)
	if err != nil {
		return result, failure.Wrap(failure.Transient, "poll GitHub Issue queue", err)
	}
	result.githubSucceeded = true
	s.pollAt = s.loop.now().Add(s.loop.pollDelay(s.loop.Config.Queue.PollInterval.Duration))
	selected, evaluation, ok, selectErr := s.selectReady(ctx, issues, snapshot)
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
		return s.loop.startIssueAtSlotWithResources(jobCtx, selected, runID, slot, evaluation.DeclaredResources, evaluation.Resources)
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
		s.loop.Logger.Printf("GitHub %s primary rate limit reached; suppressing requests until %s (source=%s)", cooldown.Resource, cooldown.ResetAt.Format(time.RFC3339), cooldown.Source)
		if err := s.loop.recordSupervisorRateLimit(cause, s.consecutiveFailures, cooldown); err != nil {
			return BlockedError{Err: failure.Wrap(failure.Supervisor, "persist supervisor rate-limit cooldown", err)}
		}
		return nil
	}
	kind := failure.KindOf(cause)
	if kind == failure.Supervisor {
		return s.loop.blockSupervisor(cause, kind, s.consecutiveFailures+1)
	}
	s.consecutiveFailures++
	s.loop.Logger.Printf("scheduler cycle failed (%d, %s): %v", s.consecutiveFailures, kind, cause)
	if s.consecutiveFailures >= 5 {
		return s.loop.blockSupervisor(cause, kind, s.consecutiveFailures)
	}
	delay := s.loop.retryDelay(s.consecutiveFailures)
	if err := s.loop.recordSupervisorRetry(cause, kind, s.consecutiveFailures, delay); err != nil {
		return BlockedError{Err: failure.Wrap(failure.Supervisor, "persist supervisor retry", err)}
	}
	s.pollAt = now.Add(delay)
	s.cooldownUntil = s.pollAt
	return nil
}

func hasPendingRequests(snapshot state.Snapshot) bool {
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.Status == "pending" {
			return true
		}
	}
	return false
}

func (s *scheduler) selectReady(ctx context.Context, issues []gh.Issue, snapshot state.Snapshot) (gh.Issue, admission.Evaluation, bool, error) {
	candidates := make([]admission.Candidate, 0, len(issues))
	byNumber := make(map[int]gh.Issue, len(issues))
	for _, issue := range issues {
		candidates = append(candidates, admission.Candidate{
			Number: issue.Number, CreatedAt: issue.CreatedAt,
			Labels: append([]string(nil), issue.Labels...), Body: issue.Body,
		})
		byNumber[issue.Number] = issue
	}
	settings := s.loop.Config.AdmissionSettings()
	dependencies := map[int]admission.DependencyState{}
	for _, candidate := range candidates {
		evaluation, err := admission.EvaluateCandidate(settings, candidate)
		if err != nil {
			return gh.Issue{}, admission.Evaluation{}, false, err
		}
		for _, number := range evaluation.Dependencies {
			if _, exists := dependencies[number]; exists {
				continue
			}
			if local := snapshot.Issues[fmt.Sprint(number)]; local != nil {
				dependencies[number] = admission.DependencyState{
					Exists: true, Accessible: true,
					Closed:                   local.Status == "completed",
					LocalCompleted:           local.Status == "completed",
					PullRequestMergeRecorded: local.PullRequestURL == "" || local.PullRequestMerged,
					KnownOpenOrUnmergedPR:    local.PullRequestURL != "" && !local.PullRequestMerged,
				}
				continue
			}
			remote, getErr := s.loop.GitHub.Get(ctx, s.loop.Config, number)
			if _, limited := gh.AsRateLimit(getErr); limited {
				return gh.Issue{}, admission.Evaluation{}, false, getErr
			}
			if getErr == nil {
				dependencies[number] = admission.DependencyState{
					Exists: true, Accessible: true, Closed: strings.EqualFold(remote.State, "closed"),
				}
			}
		}
	}
	active := make([]admission.Lease, 0, len(snapshot.Issues))
	activeLeaseNumbers := map[int]bool{}
	ineligible := map[int]string{}
	for _, issue := range snapshot.Issues {
		if issue == nil {
			continue
		}
		if issue.Lease != nil {
			active = append(active, admission.Lease{
				IssueNumber: issue.Number,
				Resources:   append([]string(nil), issue.Lease.ResolvedResources...),
			})
			activeLeaseNumbers[issue.Number] = true
		}
		switch issue.Status {
		case "running", "claimed", "needs_input", "completed", "blocked", "resolving_conflict":
			ineligible[issue.Number] = issue.Status
		}
	}
	for number := range s.active {
		ineligible[number] = "running"
		if !activeLeaseNumbers[number] {
			// Before the write-ahead reservation becomes visible, treat a
			// dispatched claim conservatively as repository-wide.
			active = append(active, admission.Lease{IssueNumber: number, Resources: []string{admission.RepositoryResource}})
		}
	}
	concurrency := s.loop.Config.Queue.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	settings.Concurrency = concurrency
	result, err := admission.Select(admission.Input{
		Settings: settings,
		Queue: admission.Queue{
			Order:          s.loop.Config.Queue.Order,
			PriorityLabels: append([]string(nil), s.loop.Config.Queue.PriorityLabels...),
		},
		Candidates: candidates, Active: active, OccupiedSlots: s.workerCount(), Dependencies: dependencies,
		Ineligible: ineligible,
	})
	if err != nil || len(result.Selected) == 0 {
		return gh.Issue{}, admission.Evaluation{}, false, err
	}
	return byNumber[result.Selected[0].Candidate.Number], result.Selected[0], true, nil
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
		return l.Worker.Run(ctx, cfg, issue, current, prompt, started)
	})
}

func (l *Loop) resumeWorker(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started worker.Started) (worker.Result, error) {
	return withWorkerSlot(ctx, func() (worker.Result, error) {
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

func (s *scheduler) nextRetryDelay() time.Duration {
	now := s.loop.now()
	if s.cooldownUntil.After(now) {
		return s.cooldownUntil.Sub(now)
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
	return until(now, deadline)
}

func (s *scheduler) markPollingIfIdle(snapshot state.Snapshot, message string) error {
	if len(s.active) != 0 || (snapshot.Supervisor.State == "polling" && snapshot.Supervisor.Message == message) {
		return nil
	}
	return s.loop.markPolling(message)
}

func pendingIssues(snapshot state.Snapshot, now time.Time) []state.Issue {
	result := make([]state.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		if issue == nil || !pendingIssue(*issue, now) {
			continue
		}
		result = append(result, *issue)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result
}

func pendingIssue(issue state.Issue, now time.Time) bool {
	if issue.Status != "claiming" && issue.Status != "resume_pending" && issue.Status != "environment_resume_pending" && issue.Status != "retry_wait" && issue.Status != "awaiting_checks" && issue.Status != "awaiting_merge" && issue.Status != "resolving_conflict" && issue.GitHubSync == "" {
		return false
	}
	return issue.RetryAfter == nil || !issue.RetryAfter.After(now)
}

func issueUsesWorkerSlot(issue state.Issue) bool {
	if issue.GitHubSync != "" {
		return false
	}
	switch issue.Status {
	case "awaiting_checks", "awaiting_merge":
		return false
	default:
		return true
	}
}

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
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
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
			if l.Config.Webhook.Enabled() {
				s.pollAt = time.Time{}
			}
		}
		var pollTimer, retryTimer SchedulerTimer
		var pollTimerC, retryTimerC <-chan time.Time
		if !s.pollAt.IsZero() {
			pollTimer = l.newSchedulerTimer(until(l.now(), s.pollAt))
			pollTimerC = pollTimer.C()
		}
		if delay, ok := s.nextRetryDelay(); ok {
			retryTimer = l.newSchedulerTimer(delay)
			retryTimerC = retryTimer.C()
		}
	waitForWake:
		for {
			select {
			case <-ctx.Done():
				stopSchedulerTimer(pollTimer)
				stopSchedulerTimer(retryTimer)
				s.cancelAndDrain()
				_, _ = l.Store.Update("supervisor_stopped", 0, "", map[string]string{"reason": ctx.Err().Error()}, func(snapshot *state.Snapshot) error {
					snapshot.Supervisor.State = "stopped"
					snapshot.Supervisor.PID = 0
					snapshot.Supervisor.Message = ctx.Err().Error()
					return nil
				})
				return nil
			case event := <-s.events:
				stopSchedulerTimer(pollTimer)
				stopSchedulerTimer(retryTimer)
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
				break waitForWake
			case <-pollTimerC:
				stopSchedulerTimer(retryTimer)
				if l.Config.Webhook.Enabled() {
					// Webhook schedulers have no periodic GitHub queue timer. A non-zero
					// pollAt in this mode is only a shared supervisor cooldown deadline.
					s.pollAt = time.Time{}
					pollCandidates = false
				} else {
					pollCandidates = true
				}
				break waitForWake
			case <-retryTimerC:
				stopSchedulerTimer(pollTimer)
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
				stopSchedulerTimer(pollTimer)
				stopSchedulerTimer(retryTimer)
				watchEvents, watchErrors = coalesceWatchEvents(watchEvents, watchErrors)
				if !l.Config.Webhook.Enabled() {
					pollCandidates = s.hasFreeSlot()
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
		maintenanceState := "maintenance"
		if len(s.active) > 0 {
			maintenanceState = "draining"
		}
		if snapshot.Supervisor.State != maintenanceState || snapshot.Supervisor.Message != "host delivery maintenance fence is active" {
			_, err = s.loop.Store.Update("delivery_maintenance_observed", 0, "", map[string]string{"state": maintenanceState}, func(current *state.Snapshot) error {
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
		return result, failure.Wrap(failure.Supervisor, "read webhook mailbox", err)
	}
	if len(mailboxBatch) > 0 {
		// Reload RetryAfter changes made while routing active lifecycle events.
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

	if len(mailboxCandidates) > 0 && s.hasFreeSlot() {
		selected, _, ok, selectErr := s.selectReady(ctx, mailboxCandidates, snapshot)
		if selectErr != nil {
			return result, failure.Wrap(failure.Supervisor, "select webhook Issue admission", selectErr)
		}
		if ok {
			slot, slotOK := s.freeSlot()
			if slotOK {
				runID := state.NewID("run")
				s.dispatch(ctx, selected.Number, runID, slot, func(jobCtx context.Context) error {
					return s.loop.startIssueAtSlotWithResources(jobCtx, selected, runID, slot)
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
		return result, nil
	}
	if s.dispatchTerminalPullRequestReconciliation(ctx, snapshot) {
		result.dispatched = true
	}
	if !s.hasFreeSlot() {
		s.pollAt = s.loop.now().Add(s.pollDelay())
		return result, nil
	}
	if !s.loop.Config.Queue.ContinueAfterNeedsInput {
		if hasPendingRequests(snapshot) {
			s.pollAt = s.loop.now().Add(s.pollDelay())
			return result, s.markPollingIfIdle(snapshot, "waiting for user input")
		}
	}
	issues, err := s.listReady(ctx)
	if err != nil {
		return result, failure.Wrap(failure.Transient, "poll GitHub Issue queue", err)
	}
	result.githubSucceeded = true
	s.pollAt = s.loop.now().Add(s.pollDelay())
	selected, _, ok, selectErr := s.selectReady(ctx, issues, snapshot)
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
		return s.loop.startIssueAtSlotWithResources(jobCtx, selected, runID, slot)
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
	seen := map[int]bool{}
	processed := map[int]bool{}
	for index := len(deliveries) - 1; index >= 0; index-- {
		delivery := deliveries[index]
		number := delivery.IssueNumber
		if number == 0 && delivery.PullRequestNumber != 0 {
			for _, issue := range snapshot.Issues {
				if issue != nil && (issue.PullRequestNumber == delivery.PullRequestNumber || strings.HasSuffix(issue.PullRequestURL, "/pull/"+fmt.Sprint(delivery.PullRequestNumber))) {
					number = issue.Number
					break
				}
			}
		}
		if number == 0 && delivery.HeadSHA != "" {
			for _, issue := range snapshot.Issues {
				if issue != nil && issue.HeadSHA == delivery.HeadSHA {
					number = issue.Number
					break
				}
			}
		}
		if number == 0 {
			continue
		}
		if seen[number] {
			if processed[number] {
				acknowledged = append(acknowledged, delivery)
			}
			continue
		}
		seen[number] = true
		if local := snapshot.Issues[fmt.Sprint(number)]; local != nil {
			if delivery.Event == "issues" && delivery.Action == "collection_exited" {
				converged, reconcileErr := s.loop.reconcileCollectionExit(ctx, *local, delivery)
				if reconcileErr != nil {
					return nil, nil, reconcileErr
				}
				if converged {
					if job, active := s.active[number]; active && job.runID == local.RunID {
						job.cancel()
					}
				}
				acknowledged = append(acknowledged, delivery)
				processed[number] = true
				continue
			}
			if terminalWebhookStatus(local.Status) {
				handled, reconcileErr := s.loop.reconcileTerminalWebhook(ctx, *local, delivery)
				if reconcileErr != nil {
					return nil, nil, reconcileErr
				}
				if handled {
					acknowledged = append(acknowledged, delivery)
					processed[number] = true
				}
				continue
			}
			if !webhookRoutableStatus(local.Status) && local.GitHubSync == "" {
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
				return nil, nil, updateErr
			}
			acknowledged = append(acknowledged, delivery)
			processed[number] = true
			continue
		}
		if delivery.Event != "issues" && delivery.Event != "issue_comment" {
			continue
		}
		candidate, getErr := s.loop.getIssue(ctx, number)
		if getErr != nil {
			return nil, nil, getErr
		}
		if gh.EligibleIssue(candidate, s.loop.Config.GitHub) {
			candidates = append(candidates, candidate)
			// Keep the delivery until the durable local Issue exists. A crash
			// between admission and lease reservation can then replay safely.
		} else {
			acknowledged = append(acknowledged, delivery)
			processed[number] = true
		}
	}
	return candidates, acknowledged, nil
}

func terminalWebhookStatus(status string) bool {
	switch status {
	case "blocked", "failed", "needs_input", "completed":
		return true
	default:
		return false
	}
}

func webhookRoutableStatus(status string) bool {
	switch status {
	case "claiming", "claimed", "running", "answer_claim_waiting", "resume_pending", "environment_resume_pending", "pull_request_checks_recovery_pending", "retry_wait", "needs_input", "awaiting_checks", "awaiting_merge", "resolving_conflict":
		return true
	default:
		return false
	}
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
		if issue == nil || !terminalPullRequestCandidate(*issue) {
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
			remote, getErr := s.loop.getIssue(ctx, number)
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
		case "running", "claimed", "needs_input", "answer_claim_waiting", "resume_pending", "completed", "blocked", "resolving_conflict":
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
	if len(s.active) != 0 || (snapshot.Supervisor.State == "polling" && snapshot.Supervisor.Message == message) {
		return nil
	}
	return s.loop.markPolling(message)
}

func pendingIssues(snapshot state.Snapshot, now time.Time, concurrency int) []state.Issue {
	result := make([]state.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		if issue == nil || !pendingIssue(*issue, now) || (issue.Status == "answer_claim_waiting" && !answeredClaimAvailable(snapshot, *issue, concurrency)) {
			continue
		}
		result = append(result, *issue)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result
}

func pendingIssue(issue state.Issue, now time.Time) bool {
	if issue.Status != "claiming" && issue.Status != "answer_claim_waiting" && issue.Status != "resume_pending" && issue.Status != "environment_resume_pending" && issue.Status != "publication_recovery_pending" && issue.Status != "pull_request_checks_recovery_pending" && issue.Status != "retry_wait" && issue.Status != "awaiting_checks" && issue.Status != "awaiting_merge" && issue.Status != "resolving_conflict" && issue.GitHubSync == "" {
		return false
	}
	return issue.RetryAfter == nil || !issue.RetryAfter.After(now)
}

func answeredClaimAvailable(snapshot state.Snapshot, issue state.Issue, concurrency int) bool {
	if issue.ResourcePark == nil || issue.ResourcePark.Kind != state.ResourceParkKindNeedsInput || issue.ResourcePark.Status != "parked" || issue.Lease != nil {
		return false
	}
	copy := snapshot
	if _, ok := availableSnapshotLeaseSlot(&copy, concurrency, issue.ResourcePark.OriginalLease.Slot, issue.Number); !ok {
		return false
	}
	for _, other := range snapshot.Issues {
		if other == nil || other.Number == issue.Number || other.Lease == nil {
			continue
		}
		if state.ResourcesConflict(issue.ResourcePark.OriginalLease.ResolvedResources, other.Lease.ResolvedResources) {
			return false
		}
	}
	return true
}

func issueUsesWorkerSlot(issue state.Issue) bool {
	if issue.GitHubSync != "" {
		return false
	}
	switch issue.Status {
	case "awaiting_checks", "awaiting_merge", "publication_recovery_pending", "pull_request_checks_recovery_pending":
		return false
	default:
		return true
	}
}

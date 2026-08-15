package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/failure"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type WorktreeManager interface {
	Ensure(context.Context, config.Config, string, int, string) (worktree.Result, error)
	Inspect(context.Context, config.Config, string, string) (worktree.Inspection, error)
}

type Publisher interface {
	Publish(context.Context, config.Config, gh.Issue, string, string, string) (worker.GitResult, error)
}

type ProcessInspector interface {
	Alive(pid int) bool
}

type NotificationDispatcher interface {
	Dispatch(context.Context) error
}

type Loop struct {
	Config        config.Config
	Store         state.Store
	GitHub        gh.Client
	Worktrees     WorktreeManager
	Worker        worker.Runner
	Publisher     Publisher
	Processes     ProcessInspector
	Clock         Clock
	Random        RandomSource
	Logger        *log.Logger
	DiskAvailable func(string) (uint64, error)
	Notifications NotificationDispatcher
}

type BlockedError struct{ Err error }

func (e BlockedError) Error() string { return "supervisor blocked: " + e.Err.Error() }
func (e BlockedError) Unwrap() error { return e.Err }

func (l *Loop) Run(ctx context.Context) error {
	lock, err := l.Store.AcquireSupervisorLock()
	if err != nil {
		return err
	}
	defer state.ReleaseSupervisorLock(lock)
	if l.Logger == nil {
		l.Logger = log.New(os.Stderr, "agent-loop: ", log.LstdFlags|log.LUTC)
	}
	snapshot, err := l.Store.Load()
	if err != nil {
		return BlockedError{Err: fmt.Errorf("validate durable state: %w", err)}
	}
	if snapshot.Recovery != nil && snapshot.Recovery.Status == "blocked" {
		return BlockedError{Err: fmt.Errorf("durable state recovery blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)}
	}
	if err := l.reconcileStartup(ctx, snapshot); err != nil {
		return err
	}
	watcher, watchErr := fsnotify.NewWatcher()
	if watchErr == nil {
		watchErr = watcher.Add(l.Store.Dir)
	}
	if watchErr != nil {
		if watcher != nil {
			_ = watcher.Close()
			watcher = nil
		}
		l.Logger.Printf("state wake events unavailable; using reconciliation polling: %v", watchErr)
	} else {
		defer watcher.Close()
	}
	now := l.now()
	_, err = l.Store.Update("supervisor_started", 0, "", nil, func(s *state.Snapshot) error {
		s.Supervisor.State = "starting"
		s.Supervisor.PID = os.Getpid()
		s.Supervisor.StartedAt = now
		s.Supervisor.Message = ""
		return nil
	})
	if err != nil {
		return err
	}

	// Persisted failures survive launchd restarts. Only a successful cycle resets
	// the counter, so repeatedly crashing between retries cannot avoid the limit.
	consecutiveFailures := snapshot.Supervisor.ConsecutiveFailures
	if consecutiveFailures > 0 && snapshot.Supervisor.RetryAfter != nil && snapshot.Supervisor.RetryAfter.After(l.now()) {
		l.waitForDelay(ctx, snapshot.Supervisor.RetryAfter.Sub(l.now()))
	}
	for {
		if err := ctx.Err(); err != nil {
			_, _ = l.Store.Update("supervisor_stopped", 0, "", map[string]string{"reason": err.Error()}, func(s *state.Snapshot) error {
				s.Supervisor.State = "stopped"
				s.Supervisor.PID = 0
				s.Supervisor.Message = err.Error()
				return nil
			})
			return nil
		}
		l.dispatchNotifications(ctx)
		worked, err := l.RunOnce(ctx)
		l.dispatchNotifications(ctx)
		if err != nil {
			kind := failure.KindOf(err)
			if kind == failure.Supervisor {
				return l.blockSupervisor(err, kind, consecutiveFailures+1)
			}
			consecutiveFailures++
			l.Logger.Printf("cycle failed (%d, %s): %v", consecutiveFailures, kind, err)
			if consecutiveFailures >= 5 {
				return l.blockSupervisor(err, kind, consecutiveFailures)
			}
			delay := l.retryDelay(consecutiveFailures)
			if recordErr := l.recordSupervisorRetry(err, kind, consecutiveFailures, delay); recordErr != nil {
				return BlockedError{Err: failure.Wrap(failure.Supervisor, "persist supervisor retry", recordErr)}
			}
			// State files written while recording the retry are not a reason to
			// bypass backoff. Retry waits are timer-only and remain cancellable.
			l.waitForDelay(ctx, delay)
			continue
		} else {
			if consecutiveFailures > 0 {
				if resetErr := l.resetSupervisorFailures(consecutiveFailures); resetErr != nil {
					return BlockedError{Err: failure.Wrap(failure.Supervisor, "reset supervisor failure counter", resetErr)}
				}
			}
			consecutiveFailures = 0
		}

		delay := l.pollDelay(l.Config.Queue.PollInterval.Duration)
		if worked {
			delay = time.Second
		}
		l.waitForWork(ctx, delay, watcher)
	}
}

func (l *Loop) waitForDelay(ctx context.Context, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (l *Loop) waitForWork(ctx context.Context, delay time.Duration, watcher *fsnotify.Watcher) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var events <-chan fsnotify.Event
	var watchErrors <-chan error
	if watcher != nil {
		events, watchErrors = watcher.Events, watcher.Errors
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			return
		case <-watchErrors:
			// Durable timer reconciliation remains active.
		case event, ok := <-events:
			if !ok {
				events = nil
				watchErrors = nil
				continue
			}
			base := filepath.Base(event.Name)
			if base != "state.json" && base != "events.jsonl" {
				continue
			}
			snapshot, err := l.Store.Load()
			if err != nil {
				continue
			}
			if nextPending(snapshot, l.now()) != nil {
				return
			}
		}
	}
}

func (l *Loop) RunOnce(ctx context.Context) (bool, error) {
	snapshot, err := l.Store.Load()
	if err != nil {
		return false, failure.Wrap(failure.Supervisor, "load durable state", err)
	}
	diskAvailable := l.DiskAvailable
	if diskAvailable == nil {
		diskAvailable = retention.AvailableBytes
	}
	available, err := diskAvailable(l.Store.Dir)
	if err != nil {
		return false, failure.Wrap(failure.Supervisor, "inspect log storage capacity", err)
	}
	reserve := uint64(l.Config.Logs.RotateBytes * 2)
	if available < reserve {
		return false, failure.Wrap(failure.Supervisor, "log storage safety reserve exhausted", fmt.Errorf("available=%d required=%d", available, reserve))
	}
	if err := l.pruneRunLogs(snapshot); err != nil {
		return false, failure.Wrap(failure.Supervisor, "prune worker run logs", err)
	}
	if issueState := nextPending(snapshot, l.now()); issueState != nil {
		return true, l.processExisting(ctx, *issueState)
	}
	if !l.Config.Queue.ContinueAfterNeedsInput {
		if _, attention := snapshot.Attention(false); attention {
			return false, l.markPolling("waiting for user input")
		}
	}
	issues, err := l.GitHub.ListReady(ctx, l.Config)
	if err != nil {
		return false, failure.Wrap(failure.Transient, "poll GitHub Issue queue", err)
	}
	statuses := map[string]string{}
	for number, issue := range snapshot.Issues {
		statuses[number] = issue.Status
	}
	selected, ok := gh.SelectReady(issues, statuses)
	if !ok {
		return false, l.markPolling("")
	}
	return true, l.startIssue(ctx, selected)
}

func (l *Loop) pruneRunLogs(snapshot state.Snapshot) error {
	exclude := map[string]bool{}
	for _, issue := range snapshot.Issues {
		switch issue.Status {
		case "claiming", "claimed", "running", "resume_pending", "retry_wait", "needs_input":
			if issue.RunID != "" {
				exclude[issue.RunID] = true
			}
		}
	}
	removed, err := retention.PruneRunDirs(
		filepath.Join(l.Store.Dir, "runs"), exclude,
		l.Config.Logs.WorkerRunMaxAge.Duration, l.Config.Logs.WorkerRunMaxCount, l.now(),
	)
	if err != nil || len(removed) == 0 {
		return err
	}
	_, err = l.Store.Update("worker_logs_pruned", 0, "", map[string]any{"run_ids": removed}, func(*state.Snapshot) error { return nil })
	return err
}

func nextPending(snapshot state.Snapshot, now time.Time) *state.Issue {
	var selected *state.Issue
	for _, issue := range snapshot.Issues {
		if issue.Status != "claiming" && issue.Status != "resume_pending" && issue.Status != "retry_wait" && issue.GitHubSync == "" {
			continue
		}
		if issue.RetryAfter != nil && issue.RetryAfter.After(now) {
			continue
		}
		if selected == nil || issue.Number < selected.Number {
			copy := *issue
			selected = &copy
		}
	}
	return selected
}

func (l *Loop) startIssue(ctx context.Context, issue gh.Issue) error {
	latest, err := l.GitHub.Get(ctx, l.Config, issue.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh GitHub Issue before claim", err)
	}
	if !gh.Eligible(latest.Labels, l.Config.GitHub) {
		return nil
	}
	issue = latest
	runID := state.NewID("run")
	now := l.now()
	_, err = l.Store.Update("claim_started", issue.Number, runID, map[string]any{"title": issue.Title}, func(s *state.Snapshot) error {
		s.Supervisor.State = "running"
		s.Issues[strconv.Itoa(issue.Number)] = &state.Issue{
			Number: issue.Number, Title: issue.Title, Status: "claiming", RunID: runID,
			Attempts: 1, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist claim start", err)
	}
	return l.claimAndRun(ctx, issue, runID)
}

func (l *Loop) claimAndRun(ctx context.Context, issue gh.Issue, runID string) error {
	if err := l.GitHub.Claim(ctx, l.Config, issue, runID); err != nil {
		return failure.Wrap(failure.Transient, "claim GitHub Issue", err)
	}
	_, err := l.Store.Update("issue_claimed", issue.Number, runID, map[string]any{"title": issue.Title}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared while claiming", issue.Number)
		}
		item.Status, item.UpdatedAt = "claimed", l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist claimed Issue", err)
	}
	return l.prepareAndRun(ctx, issue, runID)
}

func (l *Loop) prepareAndRun(ctx context.Context, issue gh.Issue, runID string) error {
	wt, err := l.Worktrees.Ensure(ctx, l.Config, l.Store.RepoID, issue.Number, issue.Title)
	if err != nil {
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "prepare Issue worktree", err), false)
	}
	_, err = l.Store.Update("worker_started", issue.Number, runID, map[string]string{"worktree": wt.Path, "branch": wt.Branch}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.Worktree, item.Branch, item.UpdatedAt = "running", wt.Path, wt.Branch, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker start", err)
	}
	current, err := l.issueState(issue.Number)
	if err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = wt.Path
	result, runErr := l.Worker.Run(ctx, workerCfg, issue, current, "", l.recordWorkerPID(issue.Number, current.RunID))
	return l.handleResult(ctx, issue, current, result, runErr)
}

func (l *Loop) processExisting(ctx context.Context, current state.Issue) error {
	if current.GitHubSync != "" {
		return l.syncGitHub(ctx, current)
	}
	issue, err := l.GitHub.Get(ctx, l.Config, current.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh existing GitHub Issue", err)
	}
	if current.Status == "claiming" {
		return l.claimAndRun(ctx, issue, current.RunID)
	}
	if current.Status == "resume_pending" {
		if err := l.GitHub.MarkRunning(ctx, l.Config, current.Number); err != nil {
			return failure.Wrap(failure.Transient, "mark resumed Issue running", err)
		}
		current.Status = "running"
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "user_answer_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status = "running"
			item.RetryAfter = nil
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist answer resume", err)
		}
		workerCfg := l.Config
		workerCfg.RepoPath = current.Worktree
		instruction := "Continue after the user's recorded answer. Implement the decision, verify the work, and return the schema-conforming result."
		var result worker.Result
		if current.SessionID != "" {
			result, err = l.Worker.Resume(ctx, workerCfg, issue, current, worker.BuildContinuationPrompt(current, instruction), l.recordWorkerPID(current.Number, current.RunID))
		} else {
			result, err = l.Worker.Run(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
		}
		return l.handleResult(ctx, issue, current, result, err)
	}

	current.RetryAfter = nil
	current.Status = "running"
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	var result worker.Result
	if current.ExecutionProfile == "extended" && current.SessionID != "" && current.Continuations < l.maxContinuations() {
		current.Continuations++
		_, err = l.Store.Update("worker_continuation_started", current.Number, current.RunID, map[string]int{"continuation": current.Continuations}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status = "running"
			item.Continuations = current.Continuations
			item.RetryAfter = nil
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist worker continuation", err)
		}
		result, err = l.Worker.Resume(ctx, workerCfg, issue, current, "Continue the implementation from the previous run. Resolve the retry reason, run verification, and return the schema-conforming final result.", l.recordWorkerPID(current.Number, current.RunID))
	} else {
		current.Attempts++
		current.RunID = state.NewID("run")
		current.SessionID = ""
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]int{"attempt": current.Attempts}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status, item.RunID, item.Attempts, item.SessionID = "running", current.RunID, current.Attempts, ""
			item.RetryAfter = nil
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist worker retry start", err)
		}
		result, err = l.Worker.Run(ctx, workerCfg, issue, current, "Retry the Issue after the previous recoverable failure. Inspect the existing worktree first and preserve valid work.", l.recordWorkerPID(current.Number, current.RunID))
	}
	return l.handleResult(ctx, issue, current, result, err)
}

func (l *Loop) handleResult(ctx context.Context, issue gh.Issue, current state.Issue, result worker.Result, runErr error) error {
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	if current.ExecutionProfile == "extended" {
		profile = "extended"
	}
	_, err := l.Store.Update("worker_preflight_completed", issue.Number, current.RunID, map[string]string{"execution_profile": profile}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.ExecutionProfile = profile
		item.WorkerPID = 0
		if result.SessionID != "" {
			item.SessionID = result.SessionID
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker result", err)
	}
	current, err = l.issueState(issue.Number)
	if err != nil {
		return err
	}
	if runErr != nil {
		return l.scheduleRetry(ctx, current, runErr.Error())
	}
	switch result.Status {
	case "completed":
		if l.Publisher != nil {
			published, publishErr := l.Publisher.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, result.Summary)
			if publishErr != nil {
				return l.scheduleRetry(ctx, current, "publish completed work: "+publishErr.Error())
			}
			result.Git = &published
		}
		if result.Git == nil {
			return l.scheduleRetry(ctx, current, "completed work has not been published")
		}
		prURL := ""
		if result.Git != nil {
			prURL = result.Git.PullRequestURL
		}
		_, err := l.Store.Update("issue_completed", issue.Number, current.RunID, result, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			item.Status, item.PullRequestURL, item.LastError, item.SessionID = "completed", prURL, "", ""
			item.FailureKind = ""
			item.GitHubSync = "done"
			item.RetryAfter, item.UpdatedAt = nil, l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Issue completion", err)
		}
		updated, stateErr := l.issueState(issue.Number)
		if stateErr != nil {
			return stateErr
		}
		return l.syncGitHub(ctx, updated)
	case "needs_input":
		requestID := state.NewID("req")
		q := result.Question
		_, err := l.Store.Update("input_requested", issue.Number, current.RunID, q, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			item.Status, item.UpdatedAt = "needs_input", l.now()
			item.FailureKind = ""
			item.GitHubSync = "needs_input"
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: issue.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				Status: "pending", CreatedAt: l.now(),
			}
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist input request", err)
		}
		updated, stateErr := l.issueState(issue.Number)
		if stateErr != nil {
			return stateErr
		}
		return l.syncGitHub(ctx, updated)
	case "retryable_failure":
		reason := result.Summary
		if result.Retry != nil && result.Retry.Reason != "" {
			reason = result.Retry.Reason
		}
		return l.scheduleRetry(ctx, current, reason)
	case "blocked":
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "worker blocked", errors.New(result.Summary)), true)
	default:
		return l.scheduleRetry(ctx, current, "worker returned an unknown status")
	}
}

func (l *Loop) recordWorkerPID(number int, runID string) worker.Started {
	return func(pid int) error {
		_, err := l.Store.Update("worker_process_started", number, runID, map[string]int{"pid": pid}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(number)]
			if item == nil || item.RunID != runID {
				return fmt.Errorf("Issue #%d run %s is no longer active", number, runID)
			}
			item.WorkerPID = pid
			item.UpdatedAt = l.now()
			return nil
		})
		return err
	}
}

func (l *Loop) scheduleRetry(ctx context.Context, issue state.Issue, reason string) error {
	canContinue := issue.ExecutionProfile == "extended" && issue.SessionID != "" && issue.Continuations < l.maxContinuations()
	canRetry := issue.Attempts < l.Config.Queue.MaxAttempts
	if !canContinue && !canRetry {
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "worker retry limit reached", errors.New(reason)), false)
	}
	delay := l.retryDelay(issue.Attempts + issue.Continuations)
	retryAt := l.now().Add(delay)
	_, err := l.Store.Update("retry_scheduled", issue.Number, issue.RunID, map[string]any{
		"failure_kind": failure.Transient, "reason": reason, "retry_at": retryAt, "delay": delay.String(),
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.LastError, item.RetryAfter = "retry_wait", reason, &retryAt
		item.FailureKind = string(failure.Transient)
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist Issue retry", err)
}

func (l *Loop) failIssue(ctx context.Context, number int, cause error, blocked bool) error {
	status := "failed"
	if blocked {
		status = "blocked"
	}
	current, _ := l.issueState(number)
	kind := failure.KindOf(cause)
	_, err := l.Store.Update("issue_"+status, number, current.RunID, map[string]string{"error": cause.Error(), "failure_kind": string(kind)}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil {
			item = &state.Issue{Number: number}
			s.Issues[strconv.Itoa(number)] = item
		}
		item.Status, item.LastError, item.SessionID = status, cause.Error(), ""
		item.FailureKind = string(kind)
		item.GitHubSync = status
		item.RetryAfter, item.UpdatedAt = nil, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Issue failure", err)
	}
	updated, stateErr := l.issueState(number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) syncGitHub(ctx context.Context, issue state.Issue) error {
	var err error
	switch issue.GitHubSync {
	case "done":
		err = l.GitHub.MarkDone(ctx, l.Config, issue.Number, issue.PullRequestURL)
	case "needs_input":
		snapshot, loadErr := l.Store.Load()
		if loadErr != nil {
			return loadErr
		}
		var pending *state.Request
		for _, request := range snapshot.PendingRequests {
			if request.IssueNumber == issue.Number && request.Status == "pending" {
				pending = request
				break
			}
		}
		if pending == nil {
			return fmt.Errorf("Issue #%d has no pending request to sync", issue.Number)
		}
		err = l.GitHub.MarkNeedsInput(ctx, l.Config, issue.Number, pending.ID, pending.Question)
	case "failed", "blocked":
		err = l.GitHub.MarkFailed(ctx, l.Config, issue.Number, issue.LastError, issue.GitHubSync == "blocked")
	default:
		return fmt.Errorf("unknown GitHub sync state %q", issue.GitHubSync)
	}
	if err != nil {
		return failure.Wrap(failure.Transient, "sync GitHub Issue state", err)
	}
	_, err = l.Store.Update("github_state_synced", issue.Number, issue.RunID, map[string]string{"state": issue.GitHubSync}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub sync", issue.Number)
		}
		if item.GitHubSync == issue.GitHubSync {
			item.GitHubSync = ""
		}
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist GitHub synchronization", err)
}

func (l *Loop) issueState(number int) (state.Issue, error) {
	snapshot, err := l.Store.Load()
	if err != nil {
		return state.Issue{}, err
	}
	item := snapshot.Issues[strconv.Itoa(number)]
	if item == nil {
		return state.Issue{}, fmt.Errorf("Issue #%d is missing from state", number)
	}
	return *item, nil
}

func (l *Loop) markPolling(message string) error {
	_, err := l.Store.Update("supervisor_polling", 0, "", map[string]string{"message": message}, func(s *state.Snapshot) error {
		s.Supervisor.State, s.Supervisor.Message = "polling", message
		return nil
	})
	return err
}

func (l *Loop) recordSupervisorRetry(cause error, kind failure.Kind, consecutive int, delay time.Duration) error {
	retryAt := l.now().Add(delay)
	_, err := l.Store.Update("supervisor_retry_scheduled", 0, "", map[string]any{
		"failure_kind": kind, "reason": cause.Error(), "consecutive_failures": consecutive,
		"retry_at": retryAt, "delay": delay.String(),
	}, func(s *state.Snapshot) error {
		s.Supervisor.State = "retry_wait"
		s.Supervisor.Message = cause.Error()
		s.Supervisor.FailureKind = string(kind)
		s.Supervisor.ConsecutiveFailures = consecutive
		s.Supervisor.RetryAfter = &retryAt
		return nil
	})
	return err
}

func (l *Loop) resetSupervisorFailures(previous int) error {
	_, err := l.Store.Update("supervisor_recovered", 0, "", map[string]int{"previous_consecutive_failures": previous}, func(s *state.Snapshot) error {
		s.Supervisor.FailureKind = ""
		s.Supervisor.ConsecutiveFailures = 0
		s.Supervisor.RetryAfter = nil
		return nil
	})
	return err
}

func (l *Loop) blockSupervisor(cause error, kind failure.Kind, consecutive int) error {
	_, _ = l.Store.Update("supervisor_blocked", 0, "", map[string]any{
		"error": cause.Error(), "failure_kind": kind, "consecutive_failures": consecutive,
	}, func(s *state.Snapshot) error {
		s.Supervisor.State = "blocked"
		s.Supervisor.Message = cause.Error()
		s.Supervisor.FailureKind = string(kind)
		s.Supervisor.ConsecutiveFailures = consecutive
		s.Supervisor.RetryAfter = nil
		return nil
	})
	l.dispatchNotifications(context.Background())
	return BlockedError{Err: cause}
}

func (l *Loop) dispatchNotifications(ctx context.Context) {
	if l.Notifications == nil {
		return
	}
	if err := l.Notifications.Dispatch(ctx); err != nil {
		l.Logger.Printf("notification delivery failed without stopping supervisor: %v", err)
	}
}

func (l *Loop) maxContinuations() int {
	return l.Config.Worker.Profiles["extended"].MaxContinuations
}

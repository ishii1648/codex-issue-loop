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
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type WorktreeManager interface {
	Ensure(context.Context, config.Config, string, int, string) (worktree.Result, error)
}

type Loop struct {
	Config    config.Config
	Store     state.Store
	GitHub    gh.Client
	Worktrees WorktreeManager
	Worker    worker.Runner
	Logger    *log.Logger
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
	now := time.Now().UTC()
	_, err = l.Store.Update("supervisor_started", 0, "", nil, func(s *state.Snapshot) error {
		s.Supervisor.State = "starting"
		s.Supervisor.PID = os.Getpid()
		s.Supervisor.StartedAt = now
		s.Supervisor.Message = ""
		for _, issue := range s.Issues {
			if issue.Status == "running" || issue.Status == "claimed" {
				issue.Status = "retry_wait"
				issue.LastError = "supervisor restarted while Issue was active"
				retryAt := now
				issue.RetryAfter = &retryAt
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	consecutiveFailures := 0
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
		worked, err := l.RunOnce(ctx)
		if err != nil {
			consecutiveFailures++
			l.Logger.Printf("cycle failed (%d): %v", consecutiveFailures, err)
			if consecutiveFailures >= 5 {
				_, _ = l.Store.Update("supervisor_blocked", 0, "", map[string]string{"error": err.Error()}, func(s *state.Snapshot) error {
					s.Supervisor.State = "blocked"
					s.Supervisor.Message = err.Error()
					return nil
				})
				return BlockedError{Err: err}
			}
		} else {
			consecutiveFailures = 0
		}

		delay := l.Config.Queue.PollInterval.Duration
		if worked {
			delay = time.Second
		}
		if consecutiveFailures > 0 {
			delay = retryDelay(consecutiveFailures)
		}
		l.waitForWork(ctx, delay, watcher)
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
			if nextPending(snapshot) != nil {
				return
			}
		}
	}
}

func (l *Loop) RunOnce(ctx context.Context) (bool, error) {
	snapshot, err := l.Store.Load()
	if err != nil {
		return false, err
	}
	if issueState := nextPending(snapshot); issueState != nil {
		return true, l.processExisting(ctx, *issueState)
	}
	if !l.Config.Queue.ContinueAfterNeedsInput {
		if _, attention := snapshot.Attention(false); attention {
			return false, l.markPolling("waiting for user input")
		}
	}
	issues, err := l.GitHub.ListReady(ctx, l.Config)
	if err != nil {
		return false, err
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

func nextPending(snapshot state.Snapshot) *state.Issue {
	var selected *state.Issue
	for _, issue := range snapshot.Issues {
		if issue.Status != "claiming" && issue.Status != "resume_pending" && issue.Status != "retry_wait" && issue.GitHubSync == "" {
			continue
		}
		if issue.RetryAfter != nil && issue.RetryAfter.After(time.Now()) {
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
		return err
	}
	if !gh.Eligible(latest.Labels, l.Config.GitHub) {
		return nil
	}
	issue = latest
	runID := state.NewID("run")
	now := time.Now().UTC()
	_, err = l.Store.Update("claim_started", issue.Number, runID, map[string]any{"title": issue.Title}, func(s *state.Snapshot) error {
		s.Supervisor.State = "running"
		s.Issues[strconv.Itoa(issue.Number)] = &state.Issue{
			Number: issue.Number, Title: issue.Title, Status: "claiming", RunID: runID,
			Attempts: 1, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return err
	}
	return l.claimAndRun(ctx, issue, runID)
}

func (l *Loop) claimAndRun(ctx context.Context, issue gh.Issue, runID string) error {
	if err := l.GitHub.Claim(ctx, l.Config, issue, runID); err != nil {
		return err
	}
	_, err := l.Store.Update("issue_claimed", issue.Number, runID, map[string]any{"title": issue.Title}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared while claiming", issue.Number)
		}
		item.Status, item.UpdatedAt = "claimed", time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	return l.prepareAndRun(ctx, issue, runID)
}

func (l *Loop) prepareAndRun(ctx context.Context, issue gh.Issue, runID string) error {
	wt, err := l.Worktrees.Ensure(ctx, l.Config, l.Store.RepoID, issue.Number, issue.Title)
	if err != nil {
		return l.failIssue(ctx, issue.Number, err, false)
	}
	_, err = l.Store.Update("worker_started", issue.Number, runID, map[string]string{"worktree": wt.Path, "branch": wt.Branch}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.Worktree, item.Branch, item.UpdatedAt = "running", wt.Path, wt.Branch, time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	current, err := l.issueState(issue.Number)
	if err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = wt.Path
	result, runErr := l.Worker.Run(ctx, workerCfg, issue, current, "")
	return l.handleResult(ctx, issue, current, result, runErr)
}

func (l *Loop) processExisting(ctx context.Context, current state.Issue) error {
	if current.GitHubSync != "" {
		return l.syncGitHub(ctx, current)
	}
	issue, err := l.GitHub.Get(ctx, l.Config, current.Number)
	if err != nil {
		return err
	}
	if current.Status == "claiming" {
		return l.claimAndRun(ctx, issue, current.RunID)
	}
	if current.Status == "resume_pending" {
		if err := l.GitHub.MarkRunning(ctx, l.Config, current.Number); err != nil {
			return err
		}
		current.Status = "running"
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "user_answer_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status = "running"
			item.RetryAfter = nil
			item.UpdatedAt = time.Now().UTC()
			return nil
		})
		if err != nil {
			return err
		}
		workerCfg := l.Config
		workerCfg.RepoPath = current.Worktree
		instruction := "Continue after the user's recorded answer. Implement the decision, verify the work, and return the schema-conforming result."
		var result worker.Result
		if current.SessionID != "" {
			result, err = l.Worker.Resume(ctx, workerCfg, issue, current, worker.BuildContinuationPrompt(current, instruction))
		} else {
			result, err = l.Worker.Run(ctx, workerCfg, issue, current, instruction)
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
			return err
		}
		result, err = l.Worker.Resume(ctx, workerCfg, issue, current, "Continue the implementation from the previous run. Resolve the retry reason, run verification, and return the schema-conforming final result.")
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
			return err
		}
		result, err = l.Worker.Run(ctx, workerCfg, issue, current, "Retry the Issue after the previous recoverable failure. Inspect the existing worktree first and preserve valid work.")
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
		if result.SessionID != "" {
			item.SessionID = result.SessionID
		}
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
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
		prURL := ""
		if result.Git != nil {
			prURL = result.Git.PullRequestURL
		}
		_, err := l.Store.Update("issue_completed", issue.Number, current.RunID, result, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			item.Status, item.PullRequestURL, item.LastError, item.SessionID = "completed", prURL, "", ""
			item.GitHubSync = "done"
			item.RetryAfter, item.UpdatedAt = nil, time.Now().UTC()
			return nil
		})
		if err != nil {
			return err
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
			item.Status, item.UpdatedAt = "needs_input", time.Now().UTC()
			item.GitHubSync = "needs_input"
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: issue.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				Status: "pending", CreatedAt: time.Now().UTC(),
			}
			return nil
		})
		if err != nil {
			return err
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
		return l.failIssue(ctx, issue.Number, errors.New(result.Summary), true)
	default:
		return l.scheduleRetry(ctx, current, "worker returned an unknown status")
	}
}

func (l *Loop) scheduleRetry(ctx context.Context, issue state.Issue, reason string) error {
	canContinue := issue.ExecutionProfile == "extended" && issue.SessionID != "" && issue.Continuations < l.maxContinuations()
	canRetry := issue.Attempts < l.Config.Queue.MaxAttempts
	if !canContinue && !canRetry {
		return l.failIssue(ctx, issue.Number, fmt.Errorf("retry limit reached: %s", reason), false)
	}
	delay := retryDelay(issue.Attempts + issue.Continuations)
	retryAt := time.Now().UTC().Add(delay)
	_, err := l.Store.Update("retry_scheduled", issue.Number, issue.RunID, map[string]any{"reason": reason, "retry_at": retryAt}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.LastError, item.RetryAfter = "retry_wait", reason, &retryAt
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	return err
}

func (l *Loop) failIssue(ctx context.Context, number int, cause error, blocked bool) error {
	status := "failed"
	if blocked {
		status = "blocked"
	}
	current, _ := l.issueState(number)
	_, err := l.Store.Update("issue_"+status, number, current.RunID, map[string]string{"error": cause.Error()}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil {
			item = &state.Issue{Number: number}
			s.Issues[strconv.Itoa(number)] = item
		}
		item.Status, item.LastError, item.SessionID = status, cause.Error(), ""
		item.GitHubSync = status
		item.RetryAfter, item.UpdatedAt = nil, time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
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
		return err
	}
	_, err = l.Store.Update("github_state_synced", issue.Number, issue.RunID, map[string]string{"state": issue.GitHubSync}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		if item == nil {
			return fmt.Errorf("Issue #%d disappeared during GitHub sync", issue.Number)
		}
		if item.GitHubSync == issue.GitHubSync {
			item.GitHubSync = ""
		}
		item.UpdatedAt = time.Now().UTC()
		return nil
	})
	return err
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

func (l *Loop) maxContinuations() int {
	return l.Config.Worker.Profiles["extended"].MaxContinuations
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 5 * time.Second
	for i := 1; i < attempt && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

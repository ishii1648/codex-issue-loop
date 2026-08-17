package supervisor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/conflict"
	"github.com/ishii1648/codex-issue-loop/internal/failure"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type WorktreeManager interface {
	Ensure(context.Context, config.Config, string, int, string) (worktree.Result, error)
	Inspect(context.Context, config.Config, string, string) (worktree.Inspection, error)
	ContentDigest(context.Context, string) (string, error)
}

type Publisher interface {
	Publish(context.Context, config.Config, gh.Issue, string, string, string, string, string, []string) (worker.GitResult, publication.Audit, error)
}

type ConflictResolver interface {
	Prepare(context.Context, config.Config, string, string, *state.ConflictRecovery) (conflict.Preparation, error)
	Publish(context.Context, config.Config, gh.Issue, string, string, state.ConflictRecovery, []worker.Test) (worker.GitResult, error)
}

type ProcessInspector interface {
	Alive(pid int) bool
}

type Loop struct {
	Config          config.Config
	Store           state.Store
	GitHub          gh.Client
	Worktrees       WorktreeManager
	Worker          worker.Runner
	WorkerIdentity  worker.Identity
	Publisher       Publisher
	Conflicts       ConflictResolver
	Processes       ProcessInspector
	Clock           Clock
	SchedulerTimers SchedulerTimerSource
	RateLimits      ratelimit.Store
	Random          RandomSource
	Logger          *log.Logger
	DiskAvailable   func(string) (uint64, error)
	publicationMu   sync.Mutex
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
	if err := l.reconcileStartupWithRateLimit(ctx, snapshot); err != nil {
		return err
	}
	watcher, watchErr := fsnotify.NewWatcher()
	if watchErr == nil {
		watchErr = watcher.Add(l.Store.Dir)
		if watchErr == nil && l.Config.Webhook.Enabled() {
			mailbox := webhook.MailboxDir(l.Store.Dir)
			if mkdirErr := os.MkdirAll(mailbox, 0o700); mkdirErr != nil {
				watchErr = mkdirErr
			} else {
				watchErr = watcher.Add(mailbox)
			}
		}
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

	return l.runScheduler(ctx, watcher)
}

func (l *Loop) waitForDelay(ctx context.Context, delay time.Duration) {
	timer := l.newSchedulerTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C():
	}
}

// reconcileStartupWithRateLimit keeps launchd from turning a primary quota
// exhaustion during startup reconciliation into a process restart loop. The
// same managed-root cooldown is shared with the scheduler and every registered
// repository, so only the first observed failure reaches GitHub before reset.
func (l *Loop) reconcileStartupWithRateLimit(ctx context.Context, snapshot state.Snapshot) error {
	l.enableRateLimitGate()
	consecutive := snapshot.Supervisor.ConsecutiveFailures
	for {
		now := l.now()
		cooldown, active, err := l.RateLimits.Current(now)
		if err != nil {
			return failure.Wrap(failure.Supervisor, "load startup GitHub rate-limit cooldown", err)
		}
		if active {
			cooldown, err = l.revalidateStartupCooldown(ctx, cooldown, now)
			if err != nil {
				return failure.Wrap(failure.Supervisor, "revalidate startup GitHub rate-limit cooldown", err)
			}
			updated, stillActive, suppressErr := l.RateLimits.Suppress(now)
			if suppressErr != nil {
				return failure.Wrap(failure.Supervisor, "record startup GitHub rate-limit suppression", suppressErr)
			}
			if stillActive {
				if err := l.recordRateLimitSuppressed(updated); err != nil {
					return failure.Wrap(failure.Supervisor, "persist startup GitHub rate-limit suppression", err)
				}
				l.Logger.Printf("startup reconciliation suppressed by shared GitHub %s cooldown until %s (source=%s)", updated.Resource, updated.ResetAt.Format(time.RFC3339), updated.Source)
				l.waitForDelay(ctx, until(now, updated.ResetAt))
				if err := ctx.Err(); err != nil {
					return err
				}
				snapshot, err = l.Store.Load()
				if err != nil {
					return failure.Wrap(failure.Supervisor, "reload startup reconciliation state", err)
				}
				continue
			}
		}

		reconcileErr := l.reconcileStartup(ctx, snapshot)
		if reconcileErr == nil {
			return nil
		}
		observed, limited := cooldownFromError(reconcileErr, now)
		if !limited {
			return reconcileErr
		}
		cooldown, err = l.RateLimits.Observe(observed, now)
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist startup GitHub rate-limit cooldown", err)
		}
		consecutive++
		if err := l.recordSupervisorRateLimit(reconcileErr, consecutive, cooldown); err != nil {
			return failure.Wrap(failure.Supervisor, "persist startup supervisor rate-limit cooldown", err)
		}
		l.Logger.Printf("GitHub %s primary rate limit reached during startup reconciliation; suppressing requests until %s (source=%s)", cooldown.Resource, cooldown.ResetAt.Format(time.RFC3339), cooldown.Source)
		l.waitForDelay(ctx, until(now, cooldown.ResetAt))
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot, err = l.Store.Load()
		if err != nil {
			return failure.Wrap(failure.Supervisor, "reload startup reconciliation state", err)
		}
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
	l.enableRateLimitGate()
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
	if queueBlockedByPullRequest(snapshot, l.Config.Completion.AutoMerge) {
		return false, l.markPolling("waiting for Pull Request checks or merge")
	}
	if !l.Config.Queue.ContinueAfterNeedsInput {
		if hasPendingRequests(snapshot) {
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
	selected, ok := gh.SelectReady(issues, statuses, l.Config.Queue)
	if !ok {
		return false, l.markPolling("")
	}
	return true, l.startIssue(ctx, selected)
}

func (l *Loop) pruneRunLogs(snapshot state.Snapshot) error {
	exclude := map[string]bool{}
	for _, issue := range snapshot.Issues {
		switch issue.Status {
		case "claiming", "claimed", "running", "resume_pending", "environment_resume_pending", "publication_recovery_pending", "retry_wait", "needs_input", "awaiting_checks", "awaiting_merge", "resolving_conflict":
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
		if issue.Status != "claiming" && issue.Status != "resume_pending" && issue.Status != "environment_resume_pending" && issue.Status != "publication_recovery_pending" && issue.Status != "retry_wait" && issue.Status != "awaiting_checks" && issue.Status != "awaiting_merge" && issue.Status != "resolving_conflict" && issue.GitHubSync == "" {
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

func queueBlockedByPullRequest(snapshot state.Snapshot, autoMerge bool) bool {
	for _, issue := range snapshot.Issues {
		if issue.Status == "awaiting_checks" || issue.Status == "resolving_conflict" || (autoMerge && issue.Status == "awaiting_merge") {
			return true
		}
	}
	return false
}

func (l *Loop) startIssue(ctx context.Context, issue gh.Issue) error {
	return l.startIssueAtSlot(ctx, issue, state.NewID("run"), 0)
}

func (l *Loop) startIssueAtSlot(ctx context.Context, issue gh.Issue, runID string, slot int) error {
	evaluation, err := admission.EvaluateCandidate(l.Config.AdmissionSettings(), admission.Candidate{
		Number: issue.Number, CreatedAt: issue.CreatedAt, Labels: issue.Labels, Body: issue.Body,
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "evaluate Issue resource claim", err)
	}
	return l.startIssueAtSlotWithResources(ctx, issue, runID, slot, evaluation.DeclaredResources, evaluation.Resources)
}

func (l *Loop) startIssueAtSlotWithResources(ctx context.Context, issue gh.Issue, runID string, slot int, declared, resolved []string) error {
	latest, err := l.getIssue(ctx, issue.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh GitHub Issue before claim", err)
	}
	if !gh.EligibleIssue(latest, l.Config.GitHub) {
		return nil
	}
	issue = latest
	now := l.now()
	_, _, err = l.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: issue.Number, Title: issue.Title, RunID: runID, Slot: slot,
		DeclaredResources: declared, ResolvedResources: resolved, BaseSHA: localBaseSHA(ctx, l.Config), ReservedAt: now,
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist claim start", err)
	}
	return l.claimAndRun(ctx, issue, runID)
}

func (l *Loop) getIssue(ctx context.Context, number int) (gh.Issue, error) {
	if l.Config.Webhook.Enabled() {
		if targeted, ok := l.GitHub.(gh.TargetedRESTClient); ok {
			return targeted.GetREST(ctx, l.Config, number)
		}
	}
	return l.GitHub.Get(ctx, l.Config, number)
}

func (l *Loop) inspectIssue(ctx context.Context, current state.Issue) (gh.RemoteState, error) {
	if l.Config.Webhook.Enabled() {
		if targeted, ok := l.GitHub.(gh.TargetedRESTClient); ok {
			prNumber := current.PullRequestNumber
			if prNumber == 0 {
				prNumber = pullRequestNumber(current.PullRequestURL)
			}
			if prNumber > 0 {
				return targeted.InspectPullRequestREST(ctx, l.Config, current.Number, prNumber, current.HeadSHA)
			}
			issue, err := targeted.GetREST(ctx, l.Config, current.Number)
			return gh.RemoteState{Issue: issue}, err
		}
	}
	return l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
}

func pullRequestNumber(value string) int {
	parts := strings.Split(strings.TrimRight(value, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] != "pull" {
		return 0
	}
	number, _ := strconv.Atoi(parts[len(parts)-1])
	return number
}

func localBaseSHA(ctx context.Context, cfg config.Config) string {
	for _, ref := range []string{"refs/remotes/origin/" + cfg.Git.BaseBranch, "HEAD"} {
		out, err := exec.CommandContext(ctx, "git", "-C", cfg.RepoPath, "rev-parse", "--verify", ref+"^{commit}").Output()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
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
	_, err = l.Store.Update("worker_started", issue.Number, runID, map[string]any{"worktree": wt.Path, "branch": wt.Branch, "identity": l.WorkerIdentity}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.Worktree, item.Branch, item.UpdatedAt = "running", wt.Path, wt.Branch, l.now()
		item.WorkerIdentity = stateIdentity(l.WorkerIdentity)
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
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, "", l.recordWorkerPID(issue.Number, current.RunID))
	return l.handleResult(ctx, issue, current, result, runErr)
}

func (l *Loop) processExisting(ctx context.Context, current state.Issue) error {
	if current.GitHubSync != "" {
		return l.syncGitHub(ctx, current)
	}
	if current.Status == "awaiting_checks" || current.Status == "awaiting_merge" {
		return l.processPullRequest(ctx, current)
	}
	if current.Status == "resolving_conflict" {
		return l.processConflictRecovery(ctx, current)
	}
	if current.Status == "publication_recovery_pending" {
		return l.processPublicationRecovery(ctx, current)
	}
	issue, err := l.getIssue(ctx, current.Number)
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
		if l.canResume(current) {
			result, err = l.resumeWorker(ctx, workerCfg, issue, current, worker.BuildContinuationPrompt(current, instruction), l.recordWorkerPID(current.Number, current.RunID))
		} else {
			if current.SessionID != "" {
				instruction = "The saved session belongs to a different worker backend. Start a fresh session in the existing worktree and use durable state.\n\n" + instruction
			}
			result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
		}
		return l.handleResult(ctx, issue, current, result, err)
	}
	if current.Status == "environment_resume_pending" {
		current.Status = "running"
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "environment_block_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if item == nil || item.Status != "environment_resume_pending" || item.GitHubSync != "" {
				return fmt.Errorf("Issue #%d environment resume is no longer pending", current.Number)
			}
			item.Status = "running"
			item.RetryAfter = nil
			if item.EnvironmentResume != nil {
				item.EnvironmentResume.Status = "running"
			}
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist environment-blocked resume", err)
		}
		workerCfg := l.Config
		workerCfg.RepoPath = current.Worktree
		instruction := "The operator confirmed that the external environment prerequisite is resolved. Continue in the existing worktree, preserve all valid dirty changes and prior metadata, rerun the blocked verification, and return the schema-conforming result."
		if current.BlockedCause != nil && current.BlockedCause.Reason != "" {
			instruction += " Previous environment block: " + current.BlockedCause.Reason
		}
		var result worker.Result
		if l.canResume(current) {
			result, err = l.resumeWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
		} else {
			result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
		}
		return l.handleResult(ctx, issue, current, result, err)
	}

	current.RetryAfter = nil
	current.Status = "running"
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	var result worker.Result
	if current.ExecutionProfile == "extended" && l.canResume(current) && current.Continuations < l.maxContinuations() {
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
		instruction := "Continue the implementation from the previous run. Resolve the retry reason, run verification, and return the schema-conforming final result."
		if current.LastError != "" {
			instruction += " Retry reason: " + current.LastError
		}
		result, err = l.resumeWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
	} else {
		previousOwner := state.LeaseOwner{}
		if current.Lease != nil {
			previousOwner = current.Lease.Owner
		}
		current.Attempts++
		current.RunID = state.NewID("run")
		current.SessionID = ""
		current.Session = nil
		payload := map[string]any{"attempt": current.Attempts}
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			owner, transferErr := state.TransferIssueLease(item, previousOwner, current.RunID)
			if transferErr != nil {
				return transferErr
			}
			if owner != (state.LeaseOwner{}) {
				payload["lease_owner"] = owner
			}
			item.Status, item.RunID, item.Attempts, item.SessionID = "running", current.RunID, current.Attempts, ""
			item.Session = nil
			item.Goal = nil
			item.RetryAfter = nil
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist worker retry start", err)
		}
		current, err = l.issueState(current.Number)
		if err != nil {
			return err
		}
		instruction := "Retry the Issue after the previous recoverable failure. Inspect the existing worktree first and preserve valid work."
		if current.LastError != "" {
			instruction += " Retry reason: " + current.LastError
		}
		result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current.Number, current.RunID))
	}
	return l.handleResult(ctx, issue, current, result, err)
}

func (l *Loop) reconcileTerminalPullRequest(ctx context.Context, scheduled state.Issue) error {
	current, err := l.issueState(scheduled.Number)
	if err != nil {
		return failure.Wrap(failure.Supervisor, "refresh terminal Issue state", err)
	}
	if !terminalPullRequestCandidate(current) || current.RunID != scheduled.RunID {
		return nil
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return failure.Wrap(failure.Transient, "reconcile terminal Pull Request", err)
	}
	decision, ok := l.decideTerminalPullRequestReconciliation(current, remote)
	if !ok {
		return nil
	}
	if decision.status == "completed" && decision.prMerged {
		return l.completeIssue(ctx, current, decision.pullRequest, map[string]any{
			"source": "periodic_terminal_reconciliation", "reason": decision.reason,
			"pull_requests": remote.PullRequests,
		})
	}
	_, err = l.Store.Update("terminal_pull_request_reconciled", current.Number, current.RunID, map[string]any{
		"status": current.Status, "reason": decision.reason, "pull_requests": remote.PullRequests,
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID || !terminalPullRequestCandidate(*item) {
			return nil
		}
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist terminal Pull Request reconciliation", err)
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
		if item == nil || item.RunID != current.RunID || terminalWebhookStatus(item.Status) {
			return errWorkerResultSuperseded
		}
		item.ExecutionProfile = profile
		item.WorkerPID = 0
		item.WorkerPGID = 0
		// A fresh worker result supersedes any publisher provenance from an
		// earlier worker attempt. A new publication failure is recorded below
		// only if this completed result reaches that boundary again.
		item.PublicationFailure = nil
		if result.SessionID != "" {
			item.SessionID = result.SessionID
			backend := result.Identity.Backend
			if backend == "" {
				backend = l.Config.Worker.Backend
			}
			if backend == "" {
				backend = "codex"
			}
			item.Session = &state.WorkerSession{Backend: backend, ID: result.SessionID}
		}
		identity := result.Identity
		if identity.Backend == "" {
			identity = l.WorkerIdentity
		}
		item.WorkerIdentity = stateIdentity(identity)
		if result.Goal != nil {
			item.Goal = stateGoal(result.Goal)
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if errors.Is(err, errWorkerResultSuperseded) {
		return nil
	}
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
			baseSHA := ""
			declared := append([]string(nil), current.DeclaredResources...)
			if current.Lease != nil {
				baseSHA = current.Lease.BaseSHA
				if len(declared) == 0 {
					declared = append([]string(nil), current.Lease.DeclaredResources...)
				}
			}
			l.publicationMu.Lock()
			published, audit, publishErr := l.Publisher.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, current.PullRequestURL, result.Summary, baseSHA, declared)
			l.publicationMu.Unlock()
			_, auditErr := l.Store.Update("publication_audited", issue.Number, current.RunID, audit, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(issue.Number)]
				auditCopy := audit
				item.PublicationAudit = &auditCopy
				item.DeclaredResources = append([]string(nil), audit.DeclaredResources...)
				item.ActualResources = append([]string(nil), audit.ActualResources...)
				if item.Lease != nil {
					item.Lease.ActualResources = append([]string(nil), audit.ActualResources...)
				}
				item.UpdatedAt = l.now()
				return nil
			})
			if auditErr != nil {
				return failure.Wrap(failure.Supervisor, "persist publication resource audit", auditErr)
			}
			if publishErr != nil {
				var mismatch publication.ClaimMismatchError
				if errors.As(publishErr, &mismatch) {
					return l.requestResourceCorrection(ctx, current, audit, publishErr.Error())
				}
				if provenanceErr := l.recordPublicationFailure(current, publishErr); provenanceErr != nil {
					return provenanceErr
				}
				return l.schedulePublicationRetry(ctx, current, "publish completed work: "+publishErr.Error())
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
		if prURL == "" {
			return l.completeIssue(ctx, current, prURL, result)
		}
		_, err := l.Store.Update("pull_request_checks_pending", issue.Number, current.RunID, result, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			item.Status, item.PullRequestURL, item.LastError = "awaiting_checks", prURL, ""
			item.PullRequestNumber = pullRequestNumber(prURL)
			item.PullRequestMerged = false
			item.FailureKind = ""
			item.GitHubSync = ""
			item.RetryAfter, item.UpdatedAt = nil, l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Pull Request check wait", err)
		}
		return nil
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
		return l.blockWorkerEnvironment(ctx, issue.Number, result.Summary)
	default:
		return l.scheduleRetry(ctx, current, "worker returned an unknown status")
	}
}

func (l *Loop) recordPublicationFailure(current state.Issue, cause error) error {
	provenance := publication.ClassifyFailure(cause, l.now())
	_, err := l.Store.Update("publication_failed", current.Number, current.RunID, &provenance, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID {
			return fmt.Errorf("Issue #%d run changed while recording publication failure", current.Number)
		}
		if item.Lease != nil {
			provenance.DeclaredResources = append([]string(nil), item.Lease.DeclaredResources...)
			provenance.ResolvedResources = append([]string(nil), item.Lease.ResolvedResources...)
			provenance.ActualResources = append([]string(nil), item.Lease.ActualResources...)
		}
		item.PublicationFailure = &provenance
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist publication failure provenance", err)
}

func (l *Loop) processPublicationRecovery(ctx context.Context, current state.Issue) error {
	recovery := current.PublicationRecovery
	if recovery == nil || recovery.ID == "" || !state.ValidID(current.RunID, "run_") || current.Lease == nil || current.Lease.BaseSHA == "" || l.Publisher == nil || l.Worktrees == nil {
		return l.failPublicationRecovery(ctx, current, "publication recovery metadata or durable base SHA is missing")
	}
	runningAttempt := publicationRecoveryAttemptRunning(recovery, recovery.Attempts)
	if (recovery.Status == "publishing") != runningAttempt {
		return l.failPublicationRecovery(ctx, current, "publication recovery attempt history is inconsistent")
	}
	if recovery.Attempts >= recovery.MaxAttempts && !runningAttempt {
		return l.failPublicationRecovery(ctx, current, "publication recovery budget is exhausted")
	}
	inspection, err := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if err != nil {
		return l.failPublicationRecovery(ctx, current, "inspect saved publication worktree: "+err.Error())
	}
	if !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists || inspection.Head == "" {
		return l.failPublicationRecovery(ctx, current, "saved publication worktree or branch is invalid")
	}
	if inspection.RemoteBranchExists && !inspection.RemoteConsistent {
		return l.failPublicationRecovery(ctx, current, "saved local and remote branch histories diverged")
	}
	if recovery.Attempts == 0 && inspection.Head != recovery.ExpectedHeadSHA {
		return l.failPublicationRecovery(ctx, current, "saved publication worktree HEAD changed after operator validation")
	}
	if recovery.Attempts == 0 {
		digest, digestErr := l.Worktrees.ContentDigest(ctx, current.Worktree)
		if digestErr != nil || digest != recovery.WorktreeSHA256 {
			return l.failPublicationRecovery(ctx, current, "saved publication worktree content changed after operator validation")
		}
	}
	result, resultBytes, err := worker.LoadLatestCompletedResult(filepath.Join(l.Store.Dir, "runs", current.RunID))
	if err != nil {
		return l.failPublicationRecovery(ctx, current, "saved completed worker result is unavailable: "+err.Error())
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(resultBytes))
	if digest != recovery.ResultSHA256 || result.Summary != recovery.Summary {
		return l.failPublicationRecovery(ctx, current, "saved completed worker result changed after operator validation")
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh Issue and Pull Requests for publication recovery", err)
	}
	if remoteErr := validatePublicationRecoveryRemote(l.Config, current, remote, inspection, recovery.Attempts > 0); remoteErr != nil {
		return l.failPublicationRecovery(ctx, current, remoteErr.Error())
	}
	issue := remote.Issue
	savedPRURL := current.PullRequestURL
	if savedPRURL == "" && len(remote.PullRequests) == 1 {
		savedPRURL = remote.PullRequests[0].URL
	}

	now := l.now()
	resumingAttempt := runningAttempt
	attemptNumber := recovery.Attempts + 1
	eventType := "publication_recovery_attempt_started"
	if resumingAttempt {
		attemptNumber = recovery.Attempts
		eventType = "publication_recovery_attempt_resumed"
	}
	_, err = l.Store.Update(eventType, current.Number, current.RunID, map[string]any{
		"recovery_id": recovery.ID, "generation": recovery.Generation, "attempt": attemptNumber,
		"resumed": resumingAttempt, "pull_request_url": savedPRURL,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.Status != "publication_recovery_pending" || item.GitHubSync != "" || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recovery.ID || item.PublicationRecovery.Attempts != recovery.Attempts {
			return fmt.Errorf("Issue #%d publication recovery changed before attempt", current.Number)
		}
		if !resumingAttempt {
			item.PublicationRecovery.Attempts = attemptNumber
			item.PublicationRecovery.History = append(item.PublicationRecovery.History, state.PublicationRecoveryAttempt{
				Number: attemptNumber, Generation: recovery.Generation, Status: "running", StartedAt: now,
			})
		}
		if item.PullRequestURL == "" && savedPRURL != "" {
			item.PullRequestURL = savedPRURL
		}
		item.PublicationRecovery.Status = "publishing"
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery attempt", err)
	}
	declared := append([]string(nil), current.DeclaredResources...)
	l.publicationMu.Lock()
	published, audit, publishErr := l.Publisher.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, savedPRURL, recovery.Summary, current.Lease.BaseSHA, declared)
	l.publicationMu.Unlock()
	_, auditErr := l.Store.Update("publication_audited", current.Number, current.RunID, audit, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		auditCopy := audit
		item.PublicationAudit = &auditCopy
		item.ActualResources = append([]string(nil), audit.ActualResources...)
		if item.Lease != nil {
			item.Lease.ActualResources = append([]string(nil), audit.ActualResources...)
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if auditErr != nil {
		return failure.Wrap(failure.Supervisor, "persist recovered publication audit", auditErr)
	}
	if publishErr != nil {
		if ctx.Err() != nil {
			// Keep the write-ahead attempt running. A restarted supervisor resumes
			// this same attempt without consuming another budget entry.
			return nil
		}
		return l.finishPublicationRecoveryFailure(ctx, current, recovery.ID, attemptNumber, publishErr)
	}
	result.Git = &published
	_, err = l.Store.Update("publication_recovery_succeeded", current.Number, current.RunID, result, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recovery.ID {
			return fmt.Errorf("Issue #%d publication recovery disappeared", current.Number)
		}
		finishPublicationRecoveryAttempt(item, attemptNumber, "succeeded", "", l.now())
		item.PublicationRecovery.Status = "succeeded"
		item.LastError = ""
		item.FailureKind = ""
		item.RetryAfter = nil
		if published.PullRequestURL == "" {
			if item.Lease != nil {
				if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
					return releaseErr
				}
			}
			item.Status = "completed"
			item.PullRequestURL = ""
			item.PullRequestMerged = false
			item.SessionID = ""
			item.Session = nil
			item.GitHubSync = "done"
		} else {
			item.Status = "awaiting_checks"
			item.PullRequestURL = published.PullRequestURL
			item.PullRequestMerged = false
			item.GitHubSync = ""
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist recovered publication success", err)
	}
	if published.PullRequestURL == "" {
		updated, stateErr := l.issueState(current.Number)
		if stateErr != nil {
			return stateErr
		}
		return l.syncGitHub(ctx, updated)
	}
	return nil
}

func (l *Loop) finishPublicationRecoveryFailure(ctx context.Context, current state.Issue, recoveryID string, attempt int, cause error) error {
	provenance := publication.ClassifyFailure(cause, l.now())
	terminal := current.PublicationRecovery != nil && attempt >= current.PublicationRecovery.MaxAttempts
	var mismatch publication.PullRequestMismatchError
	var claimMismatch publication.ClaimMismatchError
	var formatter publication.FormatterError
	if errors.As(cause, &mismatch) || errors.As(cause, &claimMismatch) || (errors.As(cause, &formatter) && formatter.Code == "path_unsafe") {
		terminal = true
	}
	discoveredPRURL, discoveredOpenPRs, inspectedPRs := l.discoverOpenPublicationPullRequests(ctx, current)
	retainLease := current.PullRequestURL != "" || discoveredOpenPRs > 0 || !inspectedPRs
	retryAt := l.now().Add(l.retryDelay(attempt))
	_, err := l.Store.Update("publication_recovery_attempt_failed", current.Number, current.RunID, map[string]any{
		"recovery_id": recoveryID, "attempt": attempt, "failure": provenance, "terminal": terminal,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.PublicationRecovery == nil || item.PublicationRecovery.ID != recoveryID {
			return fmt.Errorf("Issue #%d publication recovery disappeared", current.Number)
		}
		finishPublicationRecoveryAttempt(item, attempt, "failed", cause.Error(), l.now())
		item.PublicationFailure = &provenance
		item.LastError = "publication recovery: " + cause.Error()
		item.FailureKind = string(failure.Issue)
		if item.PullRequestURL == "" && discoveredOpenPRs == 1 {
			item.PullRequestURL = discoveredPRURL
		}
		if terminal {
			if !retainLease && item.PullRequestURL == "" && item.Lease != nil {
				if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
					return releaseErr
				}
			}
			item.Status = "failed"
			item.GitHubSync = "failed"
			item.RetryAfter = nil
			item.PublicationRecovery.Status = "failed"
		} else {
			item.Status = "publication_recovery_pending"
			item.RetryAfter = &retryAt
			item.PublicationRecovery.Status = "retry_wait"
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery failure", err)
	}
	if !terminal {
		return nil
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) failPublicationRecovery(ctx context.Context, current state.Issue, reason string) error {
	provenance := publication.FailureProvenance{
		Origin: publication.FailureOriginPublisher, Phase: publication.FailurePhasePublication,
		Code: "recovery_validation_failed", Recoverable: false, Reason: reason, FailedAt: l.now(),
	}
	discoveredPRURL, discoveredOpenPRs, inspectedPRs := l.discoverOpenPublicationPullRequests(ctx, current)
	retainLease := current.PullRequestURL != "" || discoveredOpenPRs > 0 || !inspectedPRs
	_, err := l.Store.Update("publication_recovery_refused", current.Number, current.RunID, provenance, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if !retainLease && item.PullRequestURL == "" && item.Lease != nil {
			if releaseErr := state.ReleaseIssueLease(item, item.Lease.Owner); releaseErr != nil {
				return releaseErr
			}
		}
		if item.PullRequestURL == "" && discoveredOpenPRs == 1 {
			item.PullRequestURL = discoveredPRURL
		}
		item.Status = "failed"
		item.LastError = "publication recovery refused: " + reason
		item.FailureKind = string(failure.Issue)
		item.PublicationFailure = &provenance
		item.GitHubSync = "failed"
		item.RetryAfter = nil
		if item.PublicationRecovery != nil {
			item.PublicationRecovery.Status = "failed"
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist publication recovery refusal", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) discoverOpenPublicationPullRequests(ctx context.Context, current state.Issue) (string, int, bool) {
	if l.GitHub == nil {
		return "", 0, false
	}
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return "", 0, false
	}
	url := ""
	count := 0
	for _, pr := range remote.PullRequests {
		if pr.MergedAt == nil && strings.EqualFold(pr.State, "open") {
			count++
			if count == 1 {
				url = pr.URL
			} else {
				url = ""
			}
		}
	}
	return url, count, true
}

func finishPublicationRecoveryAttempt(issue *state.Issue, number int, status, reason string, finished time.Time) {
	for index := len(issue.PublicationRecovery.History) - 1; index >= 0; index-- {
		attempt := &issue.PublicationRecovery.History[index]
		if attempt.Number == number && attempt.Status == "running" {
			attempt.Status = status
			attempt.Reason = reason
			attempt.FinishedAt = finished
			return
		}
	}
}

func publicationRecoveryAttemptRunning(recovery *state.PublicationRecovery, number int) bool {
	if recovery == nil || number < 1 {
		return false
	}
	for index := len(recovery.History) - 1; index >= 0; index-- {
		attempt := recovery.History[index]
		if attempt.Number == number {
			return attempt.Status == "running" && attempt.FinishedAt.IsZero()
		}
	}
	return false
}

func validatePublicationRecoveryRemote(cfg config.Config, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection, allowDiscoveredPR bool) error {
	if !strings.EqualFold(remote.Issue.State, "open") {
		return fmt.Errorf("GitHub Issue closed before recovered publication")
	}
	labels := map[string]bool{}
	for _, label := range remote.Issue.Labels {
		labels[strings.ToLower(label)] = true
	}
	if !labels[strings.ToLower(cfg.GitHub.RunningLabel)] || labels[strings.ToLower(cfg.GitHub.FailedLabel)] || labels[strings.ToLower(cfg.GitHub.DoneLabel)] || labels[strings.ToLower(cfg.GitHub.NeedsInputLabel)] {
		return fmt.Errorf("GitHub labels changed after publication recovery confirmation")
	}
	for _, label := range append(append([]string(nil), cfg.GitHub.ReadyLabels...), cfg.GitHub.ExcludeLabels...) {
		if labels[strings.ToLower(label)] {
			return fmt.Errorf("GitHub label %q excludes publication recovery", label)
		}
	}
	if current.PullRequestURL == "" {
		if len(remote.PullRequests) == 0 {
			return nil
		}
		if len(remote.PullRequests) == 1 && allowDiscoveredPR {
			pr := remote.PullRequests[0]
			if strings.EqualFold(pr.State, "open") && pr.MergedAt == nil && pr.HeadRefName == current.Branch && pr.BaseRefName == cfg.Git.BaseBranch && inspection.RemoteBranchExists {
				return nil
			}
		}
		return fmt.Errorf("a Pull Request appeared after publication recovery confirmation")
	}
	if len(remote.PullRequests) != 1 {
		return fmt.Errorf("saved Pull Request count changed after publication recovery confirmation")
	}
	pr := remote.PullRequests[0]
	if pr.URL != current.PullRequestURL || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil || pr.HeadRefName != current.Branch || pr.BaseRefName != cfg.Git.BaseBranch || !inspection.RemoteBranchExists {
		return fmt.Errorf("saved Pull Request changed after publication recovery confirmation")
	}
	return nil
}

var errWorkerResultSuperseded = errors.New("worker result superseded by authoritative state")

func stateGoal(goal *worker.Goal) *state.WorkerGoal {
	if goal == nil {
		return nil
	}
	return &state.WorkerGoal{
		ThreadID: goal.ThreadID, Objective: goal.Objective, Status: goal.Status,
		TokenBudget: goal.TokenBudget, TimeBudgetSeconds: goal.TimeBudgetSeconds,
		TokensUsed: goal.TokensUsed, TimeUsedSeconds: goal.TimeUsedSeconds,
		InputTokens: goal.InputTokens, CachedInputTokens: goal.CachedInputTokens,
		OutputTokens: goal.OutputTokens, UpdatedAt: goal.UpdatedAt,
	}
}

func (l *Loop) requestResourceCorrection(ctx context.Context, current state.Issue, audit publication.Audit, detail string) error {
	requestID := state.NewID("req")
	question := fmt.Sprintf("Issue #%d has changes outside its declared resource claim. How should the existing worktree be corrected?", current.Number)
	_, err := l.Store.Update("resource_claim_mismatch", current.Number, current.RunID, map[string]any{
		"reason": publication.ReasonResourceClaimMismatch, "audit": audit,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.Status = "needs_input"
		item.LastError = detail
		item.FailureKind = string(failure.Issue)
		item.GitHubSync = "needs_input"
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		s.PendingRequests[requestID] = &state.Request{
			ID: requestID, IssueNumber: current.Number, Question: question,
			Reason:      "Publication was refused before commit or push because actual_resources is not a subset of declared_resources.",
			Recommended: "revise_diff",
			Options: []state.Option{
				{ID: "revise_diff", Label: "Revise the diff"},
				{ID: "abandon", Label: "Abandon this work"},
			},
			AllowFreeText: true, Status: "pending", CreatedAt: l.now(),
		}
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist resource claim mismatch", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) canResume(current state.Issue) bool {
	if current.SessionID == "" {
		return false
	}
	backend := l.Config.Worker.Backend
	if backend == "" {
		backend = "codex"
	}
	if current.Session == nil {
		return backend == "codex"
	}
	return current.Session.ID == current.SessionID && current.Session.Backend == backend
}

func stateIdentity(identity worker.Identity) state.WorkerIdentity {
	return state.WorkerIdentity{Backend: identity.Backend, RuntimeVersion: identity.RuntimeVersion, Provider: identity.Provider,
		RequestedModel: identity.RequestedModel, ResolvedModel: identity.ResolvedModel, Variant: identity.Variant}
}

func (l *Loop) processPullRequest(ctx context.Context, current state.Issue) error {
	remote, err := l.inspectIssue(ctx, current)
	if err != nil {
		return failure.Wrap(failure.Transient, "inspect Pull Request lifecycle", err)
	}
	var selected *gh.PullRequest
	for index := range remote.PullRequests {
		candidate := &remote.PullRequests[index]
		if candidate.MergedAt != nil && (candidate.URL == current.PullRequestURL || current.PullRequestURL == "") {
			return l.completeIssue(ctx, current, candidate.URL, nil)
		}
	}
	if len(remote.PullRequests) > 1 {
		return l.blockPullRequestLifecycle(ctx, current, remote.PullRequests[0].URL, "multiple Pull Requests exist for the saved branch")
	}
	for index := range remote.PullRequests {
		candidate := &remote.PullRequests[index]
		if candidate.URL == current.PullRequestURL || (current.PullRequestURL == "" && candidate.HeadRefName == current.Branch) {
			selected = candidate
			break
		}
	}
	if selected == nil {
		inspection, inspectErr := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "inspect Pull Request worktree", inspectErr)
		}
		if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
			return l.blockPullRequestLifecycle(ctx, current, current.PullRequestURL, "saved Pull Request branch or worktree disappeared")
		}
		return l.schedulePullRequestPoll(current, "Pull Request is not visible yet")
	}
	if selected.MergedAt != nil {
		return l.completeIssue(ctx, current, selected.URL, nil)
	}
	if selected.HeadSHA != "" && current.HeadSHA != selected.HeadSHA {
		_, err := l.Store.Update("pull_request_head_observed", current.Number, current.RunID, map[string]string{"head_sha": selected.HeadSHA}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.HeadSHA = selected.HeadSHA
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist Pull Request head", err)
		}
		current.HeadSHA = selected.HeadSHA
	}
	if !strings.EqualFold(selected.State, "open") {
		return l.blockPullRequestLifecycle(ctx, current, selected.URL, "Pull Request was closed without merge")
	}
	inspection, inspectErr := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if inspectErr != nil {
		return failure.Wrap(failure.Transient, "inspect Pull Request worktree", inspectErr)
	}
	if !inspection.Exists || !inspection.Valid || !inspection.LocalBranchExists || !inspection.RemoteBranchExists {
		return l.blockPullRequestLifecycle(ctx, current, selected.URL, "open Pull Request branch or worktree disappeared")
	}
	if current.Status == "awaiting_merge" && !l.Config.Completion.AutoMerge {
		return l.schedulePullRequestPoll(current, "waiting for Pull Request merge")
	}
	if l.Config.Completion.AutoMerge {
		// Merge-state mutations take precedence over checks: checks for a behind
		// head are stale, while a dirty head may never receive a check run at all.
		// Every mutation returns immediately so one poll cannot both update or
		// recover a branch and also ready or merge the Pull Request.
		switch strings.ToLower(selected.MergeStateStatus) {
		case "behind":
			if err := l.GitHub.UpdatePullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "update Pull Request branch", err)
			}
			return l.schedulePullRequestPoll(current, "Pull Request branch updated; waiting for checks")
		case "dirty":
			return l.beginConflictRecovery(ctx, current, *selected)
		case "unknown", "unstable":
			return l.schedulePullRequestPoll(current, "waiting for stable Pull Request merge state")
		}
	}
	switch selected.ChecksStatus {
	case "pending", "":
		return l.schedulePullRequestPoll(current, "waiting for Pull Request checks")
	case "failure":
		return l.scheduleRetry(ctx, current, "Pull Request checks failed: "+selected.URL)
	case "success":
		if selected.IsDraft {
			if err := l.GitHub.ReadyPullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "mark Pull Request ready", err)
			}
		}
		if l.Config.Completion.AutoMerge {
			if err := l.GitHub.MergePullRequest(ctx, l.Config, selected.URL); err != nil {
				return failure.Wrap(failure.Transient, "merge Pull Request", err)
			}
		}
		_, err := l.Store.Update("pull_request_ready", current.Number, current.RunID, map[string]any{
			"pull_request_url": selected.URL, "auto_merge": l.Config.Completion.AutoMerge,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status = "awaiting_merge"
			item.PullRequestURL = selected.URL
			item.PullRequestNumber = selected.Number
			item.LastError = ""
			item.FailureKind = ""
			interval := l.Config.Queue.PollInterval.Duration
			if l.Config.Webhook.Enabled() {
				interval = l.Config.Webhook.SafetySweepInterval.Duration
			}
			item.RetryAfter = deadlinePointer(l.now().Add(interval))
			item.UpdatedAt = l.now()
			return nil
		})
		return failure.Wrap(failure.Supervisor, "persist Pull Request ready state", err)
	default:
		return failure.Wrap(failure.Supervisor, "inspect Pull Request checks", fmt.Errorf("unknown check status %q", selected.ChecksStatus))
	}
}

func (l *Loop) blockPullRequestLifecycle(ctx context.Context, current state.Issue, prURL, reason string) error {
	cause := "Pull Request lifecycle: " + reason
	_, err := l.Store.Update("pull_request_lifecycle_blocked", current.Number, current.RunID, map[string]any{
		"reason": reason, "pull_request_url": prURL,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.Status = "blocked"
		item.PullRequestURL = prURL
		item.LastError = cause
		item.FailureKind = string(failure.Issue)
		item.GitHubSync = "blocked"
		item.RetryAfter = nil
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Pull Request lifecycle attention", err)
	}
	updated, err := l.issueState(current.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
}

func (l *Loop) beginConflictRecovery(ctx context.Context, current state.Issue, pr gh.PullRequest) error {
	if l.Conflicts == nil {
		return l.failIssue(ctx, current.Number, failure.Wrap(failure.Issue, "Pull Request conflict recovery", errors.New("conflict recovery manager is unavailable")), true)
	}
	// A newly observed dirty PR starts by fetching the latest base even when a
	// previous recovery record is retained for audit. Only resolving_conflict
	// resumes the exact recorded MERGE_HEAD.
	preparation, err := l.Conflicts.Prepare(ctx, l.Config, current.Worktree, current.Branch, nil)
	if err != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(err, &fatal) {
			return l.failConflictRecovery(ctx, current, err.Error())
		}
		return failure.Wrap(failure.Transient, "prepare Pull Request conflict recovery", err)
	}
	baseUpdates := 1
	attempts := 0
	history := []state.ConflictAttempt(nil)
	verification := []state.ConflictVerification(nil)
	startedAt := l.now()
	if previous := current.ConflictRecovery; previous != nil {
		baseUpdates = previous.BaseUpdates
		history = append(history, previous.History...)
		startedAt = previous.StartedAt
		if startedAt.IsZero() {
			startedAt = l.now()
		}
		if previous.TargetBaseSHA == preparation.TargetBaseSHA {
			attempts = previous.Attempts
			verification = append(verification, previous.Verification...)
		} else {
			baseUpdates++
		}
	}
	recovery := &state.ConflictRecovery{
		PullRequestURL: pr.URL, PreviousBaseSHA: preparation.PreviousBaseSHA,
		TargetBaseSHA: preparation.TargetBaseSHA, OriginalHeadSHA: preparation.OriginalHeadSHA,
		ConflictFiles: append([]string(nil), preparation.ConflictFiles...), AllowedPaths: append([]string(nil), preparation.AllowedPaths...),
		Attempts: attempts, BaseUpdates: baseUpdates, History: history,
		OriginalDiff: preparation.OriginalDiff, BaseCommits: preparation.BaseCommits,
		ConflictContent: preparation.ConflictContent, StartedAt: startedAt, UpdatedAt: l.now(),
		Verification: verification,
	}
	if baseUpdates > l.Config.ConflictRecovery.MaxBaseUpdates {
		_, persistErr := l.Store.Update("conflict_recovery_base_budget_exceeded", current.Number, current.RunID, map[string]any{
			"target_base_sha": recovery.TargetBaseSHA, "base_updates": baseUpdates,
			"conflict_files": recovery.ConflictFiles,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.ConflictRecovery = recovery
			item.UpdatedAt = l.now()
			return nil
		})
		if persistErr != nil {
			return failure.Wrap(failure.Supervisor, "persist conflict recovery base budget", persistErr)
		}
		current, _ = l.issueState(current.Number)
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("base update budget exceeded (%d > %d)", baseUpdates, l.Config.ConflictRecovery.MaxBaseUpdates))
	}
	_, err = l.Store.Update("conflict_recovery_prepared", current.Number, current.RunID, map[string]any{
		"pull_request_url": pr.URL, "previous_base_sha": recovery.PreviousBaseSHA,
		"target_base_sha": recovery.TargetBaseSHA, "conflict_files": recovery.ConflictFiles,
		"attempts": recovery.Attempts, "base_updates": recovery.BaseUpdates,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.Status = "resolving_conflict"
		item.ConflictRecovery = recovery
		item.PullRequestURL = pr.URL
		item.LastError = "Pull Request conflict recovery prepared"
		item.FailureKind = ""
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict recovery preparation", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	if preparation.Published {
		return l.finishConflictPublication(current, preparation.Commit)
	}
	if preparation.Resolved {
		issue, getErr := l.getIssue(ctx, current.Number)
		if getErr != nil {
			return failure.Wrap(failure.Transient, "refresh Issue for mechanical conflict recovery", getErr)
		}
		return l.publishConflictRecovery(ctx, issue, current, nil)
	}
	return l.processConflictRecovery(ctx, current)
}

func (l *Loop) processConflictRecovery(ctx context.Context, current state.Issue) error {
	if current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, "durable conflict recovery context is missing")
	}
	if l.Conflicts == nil {
		return l.failConflictRecovery(ctx, current, "conflict recovery manager is unavailable")
	}
	preparation, prepareErr := l.Conflicts.Prepare(ctx, l.Config, current.Worktree, current.Branch, current.ConflictRecovery)
	if prepareErr != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(prepareErr, &fatal) {
			return l.failConflictRecovery(ctx, current, prepareErr.Error())
		}
		return failure.Wrap(failure.Transient, "resume Pull Request conflict recovery", prepareErr)
	}
	if preparation.Published {
		return l.finishConflictPublication(current, preparation.Commit)
	}
	if preparation.Resolved && (len(current.ConflictRecovery.ConflictFiles) == 0 || conflictVerificationGreen(current.ConflictRecovery.Verification)) {
		issue, getErr := l.getIssue(ctx, current.Number)
		if getErr != nil {
			return failure.Wrap(failure.Transient, "refresh Issue for resolved conflict publication", getErr)
		}
		return l.publishConflictRecovery(ctx, issue, current, conflictTests(current.ConflictRecovery.Verification))
	}
	if current.ConflictRecovery.Attempts >= l.Config.ConflictRecovery.MaxAttemptsPerBase {
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("recovery budget exhausted for base %s after %d attempts", current.ConflictRecovery.TargetBaseSHA, current.ConflictRecovery.Attempts))
	}
	issue, err := l.getIssue(ctx, current.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh Issue for conflict recovery", err)
	}
	runID := state.NewID("conflict")
	previousOwner := state.LeaseOwner{}
	if current.Lease != nil {
		previousOwner = current.Lease.Owner
	}
	attemptNumber := len(current.ConflictRecovery.History) + 1
	payload := map[string]any{
		"attempt": current.ConflictRecovery.Attempts + 1, "base_sha": current.ConflictRecovery.TargetBaseSHA,
		"conflict_files": current.ConflictRecovery.ConflictFiles,
	}
	_, err = l.Store.Update("conflict_recovery_attempt_started", current.Number, runID, payload, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		owner, transferErr := state.TransferIssueLease(item, previousOwner, runID)
		if transferErr != nil {
			return transferErr
		}
		if owner != (state.LeaseOwner{}) {
			payload["lease_owner"] = owner
		}
		item.Status, item.RunID, item.WorkerPID, item.WorkerPGID = "resolving_conflict", runID, 0, 0
		item.SessionID, item.Session = "", nil
		item.RetryAfter = nil
		item.WorkerIdentity = stateIdentity(l.WorkerIdentity)
		item.ConflictRecovery.Attempts++
		item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
			Number: attemptNumber, BaseSHA: item.ConflictRecovery.TargetBaseSHA, Status: "running",
			ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...), StartedAt: l.now(),
		})
		item.ConflictRecovery.UpdatedAt = l.now()
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict recovery attempt", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, worker.BuildConflictPrompt(current), l.recordWorkerPID(current.Number, runID))
	return l.handleConflictResult(ctx, issue, current, result, runErr)
}

func (l *Loop) handleConflictResult(ctx context.Context, issue gh.Issue, current state.Issue, result worker.Result, runErr error) error {
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	_, err := l.Store.Update("conflict_recovery_worker_completed", current.Number, current.RunID, map[string]any{
		"status": result.Status, "summary": result.Summary, "execution_profile": profile, "tests": result.Tests,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.ExecutionProfile = profile
		if result.Status == "completed" && item.ConflictRecovery != nil {
			item.ConflictRecovery.Verification = make([]state.ConflictVerification, 0, len(result.Tests))
			for _, test := range result.Tests {
				item.ConflictRecovery.Verification = append(item.ConflictRecovery.Verification, state.ConflictVerification{Command: test.Command, Result: test.Result})
			}
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist conflict worker result", err)
	}
	current, err = l.issueState(current.Number)
	if err != nil {
		return err
	}
	if runErr != nil {
		return l.scheduleConflictRetry(ctx, current, runErr.Error())
	}
	switch result.Status {
	case "completed":
		if result.Git != nil {
			return l.failConflictRecovery(ctx, current, "conflict worker crossed the publication boundary and returned Git publication data")
		}
		return l.publishConflictRecovery(ctx, issue, current, result.Tests)
	case "needs_input":
		requestID := state.NewID("req")
		_, err := l.Store.Update("input_requested", current.Number, current.RunID, result.Question, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.Status, item.GitHubSync, item.UpdatedAt = "needs_input", "needs_input", l.now()
			finishConflictAttempt(item, "needs_input", result.Summary, l.now())
			q := result.Question
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: current.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				ResumeStatus: "resolving_conflict", Status: "pending", CreatedAt: l.now(),
			}
			return nil
		})
		if err != nil {
			return failure.Wrap(failure.Supervisor, "persist conflict recovery input request", err)
		}
		updated, _ := l.issueState(current.Number)
		return l.syncGitHub(ctx, updated)
	case "retryable_failure":
		reason := result.Summary
		if result.Retry != nil && result.Retry.Reason != "" {
			reason = result.Retry.Reason
		}
		return l.scheduleConflictRetry(ctx, current, reason)
	case "blocked":
		return l.failConflictRecovery(ctx, current, "worker reported a non-recoverable conflict: "+result.Summary)
	default:
		return l.scheduleConflictRetry(ctx, current, "conflict worker returned an unknown status")
	}
}

func (l *Loop) publishConflictRecovery(ctx context.Context, issue gh.Issue, current state.Issue, tests []worker.Test) error {
	if l.Conflicts == nil || current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, "conflict recovery publisher is unavailable")
	}
	l.publicationMu.Lock()
	published, err := l.Conflicts.Publish(ctx, l.Config, issue, current.Worktree, current.Branch, *current.ConflictRecovery, tests)
	l.publicationMu.Unlock()
	if err != nil {
		var fatal conflict.NonRecoverableError
		if errors.As(err, &fatal) {
			return l.failConflictRecovery(ctx, current, err.Error())
		}
		return l.scheduleConflictRetry(ctx, current, "publish resolved conflict: "+err.Error())
	}
	return l.finishConflictPublication(current, published.Commit)
}

func (l *Loop) finishConflictPublication(current state.Issue, commit string) error {
	retryAt := l.now().Add(l.Config.Queue.PollInterval.Duration)
	_, err := l.Store.Update("conflict_recovery_published", current.Number, current.RunID, map[string]any{
		"pull_request_url": current.PullRequestURL, "commit": commit,
		"target_base_sha": current.ConflictRecovery.TargetBaseSHA,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.Status = "awaiting_checks"
		item.LastError = ""
		item.FailureKind = ""
		item.RetryAfter = &retryAt
		item.ConflictRecovery.LastReason = "published; waiting for CI revalidation"
		item.ConflictRecovery.UpdatedAt = l.now()
		finishConflictAttempt(item, "completed", "published as "+commit, l.now())
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist conflict recovery publication", err)
}

func (l *Loop) scheduleConflictRetry(ctx context.Context, current state.Issue, reason string) error {
	if current.ConflictRecovery == nil {
		return l.failConflictRecovery(ctx, current, reason)
	}
	hasRunningAttempt := len(current.ConflictRecovery.History) > 0 && current.ConflictRecovery.History[len(current.ConflictRecovery.History)-1].Status == "running"
	effectiveAttempts := current.ConflictRecovery.Attempts
	if !hasRunningAttempt {
		effectiveAttempts++ // preparation/publication retry without a worker invocation
	}
	if effectiveAttempts >= l.Config.ConflictRecovery.MaxAttemptsPerBase {
		if !hasRunningAttempt {
			_, _ = l.Store.Update("conflict_recovery_budget_consumed", current.Number, current.RunID, map[string]any{"reason": reason, "attempts": effectiveAttempts}, func(s *state.Snapshot) error {
				item := s.Issues[strconv.Itoa(current.Number)]
				item.ConflictRecovery.Attempts = effectiveAttempts
				item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
					Number: len(item.ConflictRecovery.History) + 1, BaseSHA: item.ConflictRecovery.TargetBaseSHA,
					Status: "retryable_failure", Reason: reason, StartedAt: l.now(), FinishedAt: l.now(),
					ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...),
				})
				return nil
			})
			current, _ = l.issueState(current.Number)
		}
		return l.failConflictRecovery(ctx, current, fmt.Sprintf("%s; recovery budget exhausted for base %s after %d attempts", reason, current.ConflictRecovery.TargetBaseSHA, effectiveAttempts))
	}
	delay := l.retryDelay(effectiveAttempts)
	retryAt := l.now().Add(delay)
	_, err := l.Store.Update("conflict_recovery_retry_scheduled", current.Number, current.RunID, map[string]any{
		"reason": reason, "base_sha": current.ConflictRecovery.TargetBaseSHA,
		"attempts": current.ConflictRecovery.Attempts, "retry_at": retryAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.Status, item.LastError, item.RetryAfter = "resolving_conflict", reason, &retryAt
		item.FailureKind = string(failure.Transient)
		if !hasRunningAttempt {
			item.ConflictRecovery.Attempts = effectiveAttempts
			item.ConflictRecovery.History = append(item.ConflictRecovery.History, state.ConflictAttempt{
				Number: len(item.ConflictRecovery.History) + 1, BaseSHA: item.ConflictRecovery.TargetBaseSHA,
				Status: "retryable_failure", Reason: reason, StartedAt: l.now(), FinishedAt: l.now(),
				ConflictFiles: append([]string(nil), item.ConflictRecovery.ConflictFiles...),
			})
		}
		item.ConflictRecovery.LastReason = reason
		item.ConflictRecovery.UpdatedAt = l.now()
		finishConflictAttempt(item, "retryable_failure", reason, l.now())
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist conflict recovery retry", err)
}

func (l *Loop) failConflictRecovery(ctx context.Context, current state.Issue, reason string) error {
	if current.ConflictRecovery != nil {
		_, _ = l.Store.Update("conflict_recovery_exhausted", current.Number, current.RunID, map[string]any{
			"reason": reason, "attempts": current.ConflictRecovery.Attempts,
			"base_updates": current.ConflictRecovery.BaseUpdates, "conflict_files": current.ConflictRecovery.ConflictFiles,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			item.ConflictRecovery.LastReason = reason
			item.ConflictRecovery.UpdatedAt = l.now()
			finishConflictAttempt(item, "blocked", reason, l.now())
			return nil
		})
		current, _ = l.issueState(current.Number)
	}
	detail := conflictFailureDetail(l.Config.RepoPath, current, reason)
	return l.failIssue(ctx, current.Number, failure.Wrap(failure.Issue, "Pull Request conflict recovery", errors.New(detail)), true)
}

func finishConflictAttempt(item *state.Issue, status, reason string, now time.Time) {
	if item == nil || item.ConflictRecovery == nil || len(item.ConflictRecovery.History) == 0 {
		return
	}
	attempt := &item.ConflictRecovery.History[len(item.ConflictRecovery.History)-1]
	if attempt.Status == "running" {
		attempt.Status, attempt.Reason, attempt.FinishedAt = status, reason, now
	}
}

func conflictFailureDetail(repoPath string, current state.Issue, reason string) string {
	recovery := current.ConflictRecovery
	if recovery == nil {
		return fmt.Sprintf("%s. Recommended recovery: inspect status and run agent-loop retry --repo %q --issue %d after repairing the worktree.", reason, repoPath, current.Number)
	}
	baseHistory := make([]string, 0, len(recovery.History))
	for _, attempt := range recovery.History {
		baseHistory = append(baseHistory, attempt.BaseSHA)
	}
	baseHistory = append(baseHistory, recovery.TargetBaseSHA)
	return fmt.Sprintf("%s. Attempts: %d; base SHA history: %s; conflict files: %s; last reason: %s. Recommended recovery: inspect the saved worktree and run agent-loop retry --repo %q --issue %d.",
		reason, recovery.Attempts, strings.Join(uniqueStrings(baseHistory), ", "), strings.Join(recovery.ConflictFiles, ", "), recovery.LastReason, repoPath, current.Number)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func conflictTests(values []state.ConflictVerification) []worker.Test {
	result := make([]worker.Test, 0, len(values))
	for _, value := range values {
		result = append(result, worker.Test{Command: value.Command, Result: value.Result})
	}
	return result
}

func conflictVerificationGreen(values []state.ConflictVerification) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		result := strings.ToLower(strings.TrimSpace(value.Result))
		green := strings.TrimSpace(value.Command) != "" && (result == "ok" || strings.HasPrefix(result, "ok ") ||
			strings.HasPrefix(result, "pass") || strings.HasPrefix(result, "success") ||
			strings.HasPrefix(result, "green") || strings.Contains(result, "exit 0"))
		if !green {
			return false
		}
	}
	return true
}

func (l *Loop) schedulePullRequestPoll(current state.Issue, reason string) error {
	interval := l.Config.Queue.PollInterval.Duration
	if l.Config.Webhook.Enabled() {
		interval = l.Config.Webhook.SafetySweepInterval.Duration
	}
	retryAt := l.now().Add(interval)
	_, err := l.Store.Update("pull_request_poll_scheduled", current.Number, current.RunID, map[string]any{
		"status": current.Status, "reason": reason, "retry_at": retryAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		item.LastError = reason
		item.RetryAfter = &retryAt
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist Pull Request poll", err)
}

func (l *Loop) completeIssue(ctx context.Context, current state.Issue, prURL string, payload any) error {
	owner := state.LeaseOwner{}
	if current.Lease != nil {
		owner = current.Lease.Owner
	}
	_, err := l.Store.Update("issue_completed", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if err := state.ReleaseIssueLease(item, owner); err != nil {
			return err
		}
		item.Status, item.PullRequestURL, item.LastError, item.SessionID = "completed", prURL, "", ""
		item.Session = nil
		item.PullRequestMerged = prURL != ""
		item.FailureKind = ""
		item.GitHubSync = "done"
		item.RetryAfter, item.UpdatedAt = nil, l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Issue completion", err)
	}
	updated, stateErr := l.issueState(current.Number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
}

func deadlinePointer(value time.Time) *time.Time { return &value }

func (l *Loop) recordWorkerPID(number int, runID string) worker.Started {
	return func(pid int) error {
		if pid <= 0 {
			return fmt.Errorf("Issue #%d run %s reported invalid worker PID %d", number, runID, pid)
		}
		_, err := l.Store.Update("worker_process_started", number, runID, map[string]int{"pid": pid, "pgid": pid}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(number)]
			if item == nil || item.RunID != runID {
				return fmt.Errorf("Issue #%d run %s is no longer active", number, runID)
			}
			if item.Lease != nil && item.Lease.Owner.RunID != runID {
				return fmt.Errorf("Issue #%d run %s does not own its resource lease", number, runID)
			}
			item.WorkerPID = pid
			// All worker backends start their command with Setpgid=true, making
			// the process PID the process-group ID used for cancellation.
			item.WorkerPGID = pid
			item.UpdatedAt = l.now()
			return nil
		})
		return err
	}
}

func (l *Loop) scheduleRetry(ctx context.Context, issue state.Issue, reason string) error {
	canContinue := issue.ExecutionProfile == "extended" && l.canResume(issue) && issue.Continuations < l.maxContinuations()
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

// schedulePublicationRetry deliberately ignores worker continuation budget.
// A publisher failure did not invalidate the completed worker result, and
// resuming that worker would blur implementation retries with publication
// retries. Before the worker-attempt budget is exhausted the existing retry
// path may start a fresh validation run; at the terminal boundary failIssue
// retains typed recoverable provenance and the completed session for the
// operator-only publication recovery transaction.
func (l *Loop) schedulePublicationRetry(ctx context.Context, issue state.Issue, reason string) error {
	if issue.Attempts >= l.Config.Queue.MaxAttempts {
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "worker retry limit reached", errors.New(reason)), false)
	}
	delay := l.retryDelay(issue.Attempts)
	retryAt := l.now().Add(delay)
	_, err := l.Store.Update("publication_retry_scheduled", issue.Number, issue.RunID, map[string]any{
		"failure_kind": failure.Transient, "reason": reason, "retry_at": retryAt, "delay": delay.String(),
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.LastError, item.RetryAfter = "retry_wait", reason, &retryAt
		item.FailureKind = string(failure.Transient)
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist publication retry", err)
}

func (l *Loop) failIssue(ctx context.Context, number int, cause error, blocked bool) error {
	status := "failed"
	if blocked {
		status = "blocked"
	}
	current, _ := l.issueState(number)
	owner := state.LeaseOwner{}
	if current.Lease != nil {
		owner = current.Lease.Owner
	}
	kind := failure.KindOf(cause)
	_, err := l.Store.Update("issue_"+status, number, current.RunID, map[string]string{"error": cause.Error(), "failure_kind": string(kind)}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil {
			item = &state.Issue{Number: number}
			s.Issues[strconv.Itoa(number)] = item
		}
		publicationRecoverable := item.PublicationFailure != nil && item.PublicationFailure.Origin == publication.FailureOriginPublisher &&
			item.PublicationFailure.Phase == publication.FailurePhasePrePublication && item.PublicationFailure.Recoverable
		// An open or previously published Pull Request keeps the lease until
		// reconciliation confirms merge or explicit abandonment. A terminal
		// pre-publication failure releases it so the queue remains live; the
		// recovery command reacquires resources transactionally.
		if item.PullRequestURL == "" {
			if err := state.ReleaseIssueLease(item, owner); err != nil {
				return err
			}
		}
		item.Status, item.LastError = status, cause.Error()
		if !publicationRecoverable {
			item.SessionID = ""
			item.Session = nil
		}
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

func (l *Loop) blockWorkerEnvironment(ctx context.Context, number int, reason string) error {
	if strings.TrimSpace(reason) == "" {
		reason = "worker reported an unresolved environment prerequisite"
	}
	current, err := l.issueState(number)
	if err != nil {
		return err
	}
	cause := failure.Wrap(failure.Issue, "worker blocked", errors.New(reason))
	_, err = l.Store.Update("issue_blocked", number, current.RunID, map[string]string{
		"error": cause.Error(), "failure_kind": string(failure.Issue), "blocked_origin": "worker", "blocked_kind": "environment",
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil || item.RunID != current.RunID {
			return fmt.Errorf("Issue #%d run changed while recording worker block", number)
		}
		// Keep the lease, session, Goal, answers, worktree, branch, dirty files,
		// and resource metadata. They are the continuation boundary used by the
		// explicit operator resume operation.
		item.Status = "blocked"
		item.LastError = cause.Error()
		item.FailureKind = string(failure.Issue)
		item.GitHubSync = "blocked"
		item.BlockedCause = &state.BlockedCause{
			Origin: "worker", Kind: "environment", Resumable: true,
			Reason: reason, BlockedAt: l.now(),
		}
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist worker environment block", err)
	}
	updated, err := l.issueState(number)
	if err != nil {
		return err
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
	case "conflict_retry":
		recoveryID := issue.RunID
		if issue.ConflictRecovery != nil {
			if issue.ConflictRecovery.RetryID != "" {
				recoveryID = issue.ConflictRecovery.RetryID
			} else if issue.ConflictRecovery.TargetBaseSHA != "" {
				recoveryID = issue.ConflictRecovery.TargetBaseSHA
			}
		}
		err = l.GitHub.MarkConflictRetry(ctx, l.Config, issue.Number, recoveryID)
	case "environment_resume":
		resumeID := issue.RunID
		if issue.EnvironmentResume != nil && issue.EnvironmentResume.ID != "" {
			resumeID = issue.EnvironmentResume.ID
		}
		if resumer, ok := l.GitHub.(interface {
			MarkEnvironmentResume(context.Context, config.Config, int, string) error
		}); ok {
			err = resumer.MarkEnvironmentResume(ctx, l.Config, issue.Number, resumeID)
		} else {
			err = l.GitHub.MarkRunning(ctx, l.Config, issue.Number)
		}
	case "publication_recovery":
		recoveryID := issue.RunID
		if issue.PublicationRecovery != nil && issue.PublicationRecovery.ID != "" {
			recoveryID = issue.PublicationRecovery.ID
		}
		if resumer, ok := l.GitHub.(interface {
			MarkPublicationRecovery(context.Context, config.Config, int, string) error
		}); ok {
			err = resumer.MarkPublicationRecovery(ctx, l.Config, issue.Number, recoveryID)
		} else {
			err = l.GitHub.MarkRunning(ctx, l.Config, issue.Number)
		}
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
			if issue.GitHubSync == "environment_resume" && item.EnvironmentResume != nil {
				item.EnvironmentResume.Status = "github_synced"
			}
			if issue.GitHubSync == "publication_recovery" && item.PublicationRecovery != nil {
				item.PublicationRecovery.Status = "github_synced"
			}
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

func rateLimitState(cooldown ratelimit.Cooldown) *state.RateLimit {
	return &state.RateLimit{
		Resource: cooldown.Resource, ObservedResetAt: cooldown.ResetAt,
		CooldownSource: cooldown.Source, SuppressedRetryCount: cooldown.SuppressedRetryCount,
	}
}

func (l *Loop) recordSupervisorRateLimit(cause error, consecutive int, cooldown ratelimit.Cooldown) error {
	_, err := l.Store.Update("supervisor_rate_limit_cooldown", 0, "", map[string]any{
		"resource": cooldown.Resource, "observed_reset_at": cooldown.ResetAt,
		"cooldown_source": cooldown.Source, "suppressed_retry_count": cooldown.SuppressedRetryCount,
	}, func(s *state.Snapshot) error {
		s.Supervisor.State = "retry_wait"
		s.Supervisor.Message = cause.Error()
		s.Supervisor.FailureKind = string(failure.Transient)
		s.Supervisor.ConsecutiveFailures = consecutive
		s.Supervisor.RetryAfter = &cooldown.ResetAt
		s.Supervisor.RateLimit = rateLimitState(cooldown)
		return nil
	})
	return err
}

func (l *Loop) recordRateLimitSuppressed(cooldown ratelimit.Cooldown) error {
	_, err := l.Store.Update("supervisor_rate_limit_retry_suppressed", 0, "", map[string]any{
		"resource": cooldown.Resource, "observed_reset_at": cooldown.ResetAt,
		"cooldown_source": cooldown.Source, "suppressed_retry_count": cooldown.SuppressedRetryCount,
	}, func(s *state.Snapshot) error {
		s.Supervisor.State = "retry_wait"
		s.Supervisor.Message = fmt.Sprintf("GitHub %s primary rate-limit cooldown until %s", cooldown.Resource, cooldown.ResetAt.Format(time.RFC3339))
		s.Supervisor.FailureKind = string(failure.Transient)
		s.Supervisor.RetryAfter = &cooldown.ResetAt
		s.Supervisor.RateLimit = rateLimitState(cooldown)
		return nil
	})
	return err
}

func (l *Loop) resetSupervisorFailures(previous int) error {
	_, err := l.Store.Update("supervisor_recovered", 0, "", map[string]int{"previous_consecutive_failures": previous}, func(s *state.Snapshot) error {
		s.Supervisor.FailureKind = ""
		s.Supervisor.ConsecutiveFailures = 0
		s.Supervisor.RetryAfter = nil
		s.Supervisor.RateLimit = nil
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
	return BlockedError{Err: cause}
}

func (l *Loop) maxContinuations() int {
	return l.Config.Worker.Profiles["extended"].MaxContinuations
}

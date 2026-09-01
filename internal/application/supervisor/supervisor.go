package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/webhook"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
	"github.com/ishii1648/codex-issue-loop/internal/application/conflict"
	"github.com/ishii1648/codex-issue-loop/internal/domain/admission"
	"github.com/ishii1648/codex-issue-loop/internal/domain/capability"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
	"github.com/ishii1648/codex-issue-loop/internal/platform/ratelimit"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

type WorktreeManager interface {
	Ensure(context.Context, config.Config, string, int, string) (worktree.Result, error)
	Inspect(context.Context, config.Config, string, string) (worktree.Inspection, error)
	ValidateLaunch(context.Context, config.Config, string, string) (worktree.LaunchValidation, error)
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
	// Delivery fences stop new lifecycle dispatch while allowing active workers
	// to reach their normal durable checkpoint without signals. The host fence
	// coordinates legacy all-repository delivery; the repository fence isolates
	// an exact per-repository assignment transaction.
	MaintenanceFencePath           string
	RepositoryMaintenanceFencePath string
	publicationMu                  sync.Mutex
}

type BlockedError struct{ Err error }

func (e BlockedError) Error() string { return "supervisor blocked: " + e.Err.Error() }
func (e BlockedError) Unwrap() error { return e.Err }

type workerWorkspaceError struct {
	expected   string
	validation worktree.LaunchValidation
	cause      error
}

func (e *workerWorkspaceError) Error() string {
	return fmt.Sprintf("worker workspace validation failed for %s: %v", e.expected, e.cause)
}

func (e *workerWorkspaceError) Unwrap() error { return e.cause }

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
	if err := state.ValidateSemanticContract(snapshot); err != nil {
		return BlockedError{Err: fmt.Errorf("validate durable state semantic compatibility before startup: %w; run agent-loop migrate --json", err)}
	}
	if snapshot.Recovery != nil && snapshot.Recovery.Status == state.RecoveryStateBlocked {
		return BlockedError{Err: fmt.Errorf("durable state recovery blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)}
	}
	if !l.maintenanceRequested() {
		if err := l.reconcileStartupWithRateLimit(ctx, snapshot); err != nil {
			return err
		}
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
		s.Supervisor.State = state.SupervisorStateStarting
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

func (l *Loop) maintenanceRequested() bool {
	for _, path := range []string{l.MaintenanceFencePath, l.RepositoryMaintenanceFencePath} {
		if path == "" {
			continue
		}
		_, err := os.Lstat(path)
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
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
	selector := &scheduler{loop: l, active: map[int]activeJob{}}
	selected, _, ok, err := selector.selectReady(ctx, issues, snapshot)
	if err != nil {
		return false, failure.Wrap(failure.Supervisor, "select Issue admission", err)
	}
	if !ok {
		return false, l.markPolling("")
	}
	return true, l.startIssueAtSlotWithResources(ctx, selected, state.NewID("run"), 0)
}

func (l *Loop) pruneRunLogs(snapshot state.Snapshot) error {
	exclude := map[string]bool{}
	for _, issue := range snapshot.Issues {
		if issue.Status.RetainsRunLogs() {
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
		if !issue.Status.DispatchPending(issue.GitHubSync) {
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
		if issue.Status.BlocksQueueForPullRequest(autoMerge) {
			return true
		}
	}
	return false
}

func (l *Loop) startIssueAtSlotWithResources(ctx context.Context, issue gh.Issue, runID string, slot int) error {
	latest, err := l.getIssue(ctx, issue.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh GitHub Issue before claim", err)
	}
	if !gh.EligibleIssue(latest, l.Config.GitHub) {
		return nil
	}
	issue = latest
	// Re-evaluate the authoritative body immediately before the first durable
	// write. A metadata edit between queue collection and dispatch must not
	// reserve a lease or mutate GitHub under stale capability assumptions.
	evaluation, err := admission.EvaluateCandidate(l.Config.AdmissionSettings(), admission.Candidate{
		Number: issue.Number, CreatedAt: issue.CreatedAt, Labels: issue.Labels, Body: issue.Body,
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "evaluate refreshed Issue admission", err)
	}
	if !evaluation.Capability.Compatible {
		return nil
	}
	now := l.now()
	_, _, err = l.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: issue.Number, Title: issue.Title, RunID: runID, Slot: slot,
		DeclaredResources: evaluation.DeclaredResources, ResolvedResources: evaluation.Resources, BaseSHA: localBaseSHA(ctx, l.Config), ReservedAt: now,
		CapabilityRequirements: evaluation.Capability.Requirements, WorkerCapabilities: evaluation.Capability.Provided,
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

func (l *Loop) processExisting(ctx context.Context, current state.Issue) error {
	if current.CapabilityRequirements != nil && current.Status.RequiresCapabilityRecheck() {
		capabilityEvaluation := capability.EvaluateRequirement(current.CapabilityRequirements, l.Config.WorkerCapabilityProfiles())
		if !capabilityEvaluation.Compatible {
			codes := make([]string, 0, len(capabilityEvaluation.Mismatches))
			for _, mismatch := range capabilityEvaluation.Mismatches {
				codes = append(codes, mismatch.Code)
			}
			sort.Strings(codes)
			return failure.Wrap(failure.Issue, "revalidate persisted Issue capability", fmt.Errorf("%s", strings.Join(codes, ",")))
		}
	}
	if current.GitHubSync.Pending() {
		return l.syncGitHub(ctx, current)
	}
	if current.Status == issuedomain.StatusAwaitingChecks || current.Status == issuedomain.StatusAwaitingMerge {
		return l.processPullRequest(ctx, current)
	}
	if current.Status == issuedomain.StatusResolvingConflict {
		return l.processConflictRecovery(ctx, current)
	}
	if current.Status == issuedomain.StatusPublicationRecovery {
		return l.processPublicationRecovery(ctx, current)
	}
	if current.Status == issuedomain.StatusChecksRecovery {
		return failure.Wrap(failure.Issue, "route Pull Request checks recovery", fmt.Errorf("Issue #%d checks recovery has no pending GitHub synchronization", current.Number))
	}
	issue, err := l.getIssue(ctx, current.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh existing GitHub Issue", err)
	}
	if current.Status == issuedomain.StatusClaiming {
		return l.claimAndRun(ctx, issue, current.RunID)
	}
	if current.Status == issuedomain.StatusAnswerClaimWaiting {
		return l.reacquireAnsweredClaim(ctx, issue, current)
	}
	if current.Status == issuedomain.StatusResumePending {
		resumeTransition, transitionErr := issuedomain.StartAnsweredResume(current.Status)
		if transitionErr != nil {
			return failure.Wrap(failure.Issue, "decide answered resume start", transitionErr)
		}
		if current.ResourcePark != nil && current.ResourcePark.Kind == state.ResourceParkKindNeedsInput {
			if reason := l.answeredResumeRemoteMismatch(issue, current); reason != "" {
				return l.rejectAnsweredContinuation(current, reason)
			}
		}
		if err := l.GitHub.MarkRunning(ctx, l.Config, current.Number); err != nil {
			return failure.Wrap(failure.Transient, "mark resumed Issue running", err)
		}
		if err := state.ApplyIssueTransition(&current, resumeTransition); err != nil {
			return err
		}
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "user_answer_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if item == nil || item.Status != issuedomain.StatusResumePending || item.GitHubSync != issuedomain.GitHubSyncNone || item.Lease == nil {
				return fmt.Errorf("Issue #%d answered continuation is no longer pending", current.Number)
			}
			if err := state.ApplyIssueTransition(item, resumeTransition); err != nil {
				return err
			}
			item.RetryAfter = nil
			if item.ResourcePark != nil && item.ResourcePark.Kind == state.ResourceParkKindNeedsInput && item.ResourcePark.Status == issuedomain.ResourceParkStatusResuming {
				item.ResourcePark.Status = issuedomain.ResourceParkStatusResumed
			}
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
			result, err = l.resumeWorker(ctx, workerCfg, issue, current, worker.BuildContinuationPrompt(current, instruction), l.recordWorkerPID(current))
		} else {
			if current.SessionID != "" {
				instruction = "The saved session belongs to a different worker backend. Start a fresh session in the existing worktree and use durable state.\n\n" + instruction
			}
			result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current))
		}
		return l.handleResult(ctx, issue, current, result, err)
	}
	if current.Status == issuedomain.StatusEnvironmentResumePending {
		resumeTransition, transitionErr := issuedomain.StartEnvironmentResume(current.Status)
		if transitionErr != nil {
			return failure.Wrap(failure.Issue, "decide environment resume start", transitionErr)
		}
		if err := state.ApplyIssueTransition(&current, resumeTransition); err != nil {
			return err
		}
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "environment_block_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if item == nil || item.Status != issuedomain.StatusEnvironmentResumePending || item.GitHubSync != issuedomain.GitHubSyncNone {
				return fmt.Errorf("Issue #%d environment resume is no longer pending", current.Number)
			}
			if err := state.ApplyIssueTransition(item, resumeTransition); err != nil {
				return err
			}
			item.RetryAfter = nil
			if item.EnvironmentResume != nil {
				item.EnvironmentResume.Status = issuedomain.EnvironmentResumeStatusRunning
			}
			if item.ResourcePark != nil && item.ResourcePark.Status == issuedomain.ResourceParkStatusResuming {
				item.ResourcePark.Status = issuedomain.ResourceParkStatusResumed
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
		previousReason := ""
		if current.EnvironmentResume != nil {
			previousReason = current.EnvironmentResume.PreviousReason
		}
		if previousReason == "" && current.BlockedCause != nil {
			previousReason = current.BlockedCause.Reason
		}
		if previousReason != "" {
			instruction += " Previous environment block: " + previousReason
		}
		var result worker.Result
		if l.canResume(current) {
			result, err = l.resumeWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current))
		} else {
			result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current))
		}
		return l.handleResult(ctx, issue, current, result, err)
	}

	retryTransition, transitionErr := issuedomain.StartRetry(current.Status)
	if transitionErr != nil {
		return failure.Wrap(failure.Issue, "decide retry start", transitionErr)
	}
	current.RetryAfter = nil
	if err := state.ApplyIssueTransition(&current, retryTransition); err != nil {
		return err
	}
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	var result worker.Result
	if l.retryBudget(current).Decide() == issuedomain.RetryContinuation {
		current.Continuations++
		_, err = l.Store.Update("worker_continuation_started", current.Number, current.RunID, map[string]int{"continuation": current.Continuations}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if err := state.ApplyIssueTransition(item, retryTransition); err != nil {
				return err
			}
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
		result, err = l.resumeWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current))
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
			if err := state.ApplyIssueTransition(item, retryTransition); err != nil {
				return err
			}
			item.RunID, item.Attempts, item.SessionID = current.RunID, current.Attempts, ""
			item.Session = nil
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
		result, err = l.runWorker(ctx, workerCfg, issue, current, instruction, l.recordWorkerPID(current))
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
	if decision.status == issuedomain.StatusCompleted && decision.prMerged {
		var merged gh.PullRequest
		for _, candidate := range remote.PullRequests {
			if candidate.URL == decision.pullRequest {
				merged = candidate
				break
			}
		}
		return l.completeIssue(ctx, current, merged, map[string]any{
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

var errAnsweredClaimWaiting = errors.New("answered needs-input claim is still waiting")

var errWorkerResultSuperseded = errors.New("worker result superseded by authoritative state")

func deadlinePointer(value time.Time) *time.Time { return &value }

var errAnsweredWorkspaceSyncConverged = errors.New("answered workspace recovery GitHub synchronization already converged")

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
		s.Supervisor.State, s.Supervisor.Message = state.SupervisorStatePolling, message
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
		s.Supervisor.State = state.SupervisorStateRetryWait
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
		s.Supervisor.State = state.SupervisorStateRetryWait
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
		s.Supervisor.State = state.SupervisorStateRetryWait
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
		s.Supervisor.State = state.SupervisorStateBlocked
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

func (l *Loop) retryBudget(issue state.Issue) issuedomain.RetryBudget {
	return issuedomain.RetryBudget{
		Extended: issue.ExecutionProfile == "extended", Resumable: l.canResume(issue),
		Attempts: issue.Attempts, MaxAttempts: l.Config.Queue.MaxAttempts,
		Continuations: issue.Continuations, MaxContinuations: l.maxContinuations(),
	}
}

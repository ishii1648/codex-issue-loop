package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	"github.com/ishii1648/codex-issue-loop/internal/application/incidentloop"
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
	Publish(context.Context, config.Config, gh.Issue, string, string, string, string, string) (worker.GitResult, publication.Audit, error)
}

type ConflictResolver interface {
	Prepare(context.Context, config.Config, string, string, *state.ConflictRecovery) (conflict.Preparation, error)
	Publish(context.Context, config.Config, gh.Issue, string, string, state.ConflictRecovery, []worker.Test) (worker.GitResult, error)
}

type ProcessInspector interface {
	Alive(pid int) bool
}

type Loop struct {
	Config             config.Config
	Store              state.Store
	GitHub             gh.Client
	Worktrees          WorktreeManager
	Worker             worker.Runner
	WorkerIdentity     worker.Identity
	Publisher          Publisher
	Conflicts          ConflictResolver
	Processes          ProcessInspector
	Clock              Clock
	SchedulerTimers    SchedulerTimerSource
	RateLimits         ratelimit.Store
	Random             RandomSource
	Logger             *log.Logger
	DiskAvailable      func(string) (uint64, error)
	IncidentSignals    incidentSignalRecorder
	IncidentAutomation incidentAutomationRunner
	ReleaseVersion     string
	ReleaseCommit      string
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
	if err := l.recordRuntimeIdentity(); err != nil {
		return failure.Wrap(failure.Supervisor, "record runtime identity signal", err)
	}
	if watchErr != nil {
		if err := l.recordIncidentSignal(incidentloop.Signal{
			Kind: "event", Name: "host_diagnostic", Component: "host", Phase: "startup",
			OutcomeCode: "observed", ReasonCode: "fsnotify_unavailable", FailureKind: "supervisor", FailureCode: "fsnotify_unavailable", Resumable: true,
		}); err != nil {
			return failure.Wrap(failure.Supervisor, "record host diagnostic signal", err)
		}
	}

	return l.runWithIncidentAutomation(ctx, func(runCtx context.Context) error { return l.runScheduler(runCtx, watcher) })
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
	issues, err := l.GitHub.ListReady(ctx, l.Config)
	if err != nil {
		return false, failure.Wrap(failure.Transient, "poll GitHub Issue queue", err)
	}
	selector := &scheduler{loop: l, active: map[int]activeJob{}}
	selected, ok, err := selector.selectReady(ctx, issues, snapshot)
	if err != nil {
		return false, failure.Wrap(failure.Supervisor, "select Issue admission", err)
	}
	if !ok {
		return false, l.markPolling("")
	}
	return true, l.startIssue(ctx, selected, state.NewID("run"))
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
		if !issue.Status.DispatchPending(state.PendingEffect(&snapshot, issue.Number) != nil) {
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

func (l *Loop) startIssue(ctx context.Context, issue gh.Issue, runID string) error {
	latest, err := l.getIssue(ctx, issue.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh GitHub Issue before claim", err)
	}
	if !gh.EligibleIssue(latest, l.Config.GitHub) {
		return nil
	}
	issue = latest
	verification, err := l.GitHub.VerifyIssueAuthor(ctx, l.Config, issue)
	if err != nil {
		if verification.Reason != "" {
			_ = l.Store.RecordAuthorVerification(issue.Number, verification)
		}
		return failure.Wrap(failure.Issue, "verify refreshed Issue author", err)
	}
	if !verification.Trusted {
		if recordErr := l.Store.RecordAuthorVerification(issue.Number, verification); recordErr != nil {
			return failure.Wrap(failure.Supervisor, "record refreshed Issue author verification", recordErr)
		}
		l.Logger.Printf("Issue #%d author rejected before worker start (reason=%s)", issue.Number, verification.Reason)
		return nil
	}
	now := l.now()
	_, _, err = l.Store.StartExecution(state.ExecutionStart{
		IssueNumber: issue.Number, Title: issue.Title, RunID: runID,
		BaseSHA: localBaseSHA(ctx, l.Config), StartedAt: now,
		AuthorVerification: &verification,
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

func (l *Loop) inspectReconciliationIssue(ctx context.Context, current state.Issue) (gh.RemoteState, error) {
	if terminalReconciliationCandidate(current) && current.Branch != "" {
		// Cancellation must observe the complete zero/one/multiple PR set for
		// the saved branch. The webhook-optimized targeted read proves only one
		// PR identity and therefore cannot authorize this terminal transition.
		return l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	}
	return l.inspectIssue(ctx, current)
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
	snapshot, err := l.Store.Load()
	if err != nil {
		return err
	}
	if state.PendingEffect(&snapshot, current.Number) != nil {
		return l.syncGitHub(ctx, current)
	}
	if current.Status == issuedomain.StatusAwaitingChecks || current.Status == issuedomain.StatusAwaitingMerge {
		return l.processPullRequest(ctx, current)
	}
	pendingStatus := current.Status
	if pendingStatus == issuedomain.StatusResumePending || pendingStatus == issuedomain.StatusRetryWait || pendingStatus == issuedomain.StatusResolvingConflict {
		resumed, err := l.ensurePendingExecution(current)
		if err != nil {
			return err
		}
		current = resumed
	}
	if pendingStatus == issuedomain.StatusResolvingConflict {
		return l.processConflictRecovery(ctx, current)
	}
	issue, err := l.getIssue(ctx, current.Number)
	if err != nil {
		return failure.Wrap(failure.Transient, "refresh existing GitHub Issue", err)
	}
	if current.Status == issuedomain.StatusClaiming {
		return l.claimAndRun(ctx, issue, current.RunID)
	}
	if pendingStatus == issuedomain.StatusResumePending {
		if current.Continuation != nil && current.Continuation.Stage == issuedomain.ContinuationStagePublish {
			return l.processPublicationCheckpoint(ctx, current)
		}
		if current.Continuation != nil && current.Continuation.Kind == state.ContinuationKindNeedsInput {
			if reason := l.answeredResumeRemoteMismatch(issue, current); reason != "" {
				return l.rejectAnsweredContinuation(current, reason)
			}
		}
		if err := l.GitHub.MarkRunning(ctx, l.Config, current.Number); err != nil {
			return failure.Wrap(failure.Transient, "mark resumed Issue running", err)
		}
		current.RetryAfter = nil
		workerCfg := l.Config
		workerCfg.RepoPath = current.Worktree
		instruction := "Continue from the operator-resolved checkpoint in the existing worktree, preserve valid work and prior metadata, rerun the blocked verification, and return the schema-conforming result."
		if current.Continuation != nil && current.Continuation.Kind == state.ContinuationKindNeedsInput {
			instruction = "Continue after the user's recorded answer. Implement the decision, verify the work, and return the schema-conforming result."
		} else if current.Suspension != nil && current.Suspension.Reason != "" {
			instruction += " Previous block: " + current.Suspension.Reason
		}
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
	current.RetryAfter = nil
	workerCfg := l.Config
	workerCfg.RepoPath = current.Worktree
	var result worker.Result
	if l.retryBudget(current).Decide() == issuedomain.RetryContinuation {
		current.Continuations++
		_, err = l.Store.Update("worker_continuation_started", current.Number, current.RunID, map[string]int{"continuation": current.Continuations}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			identity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
			if item == nil || item.Status != issuedomain.StatusRunning || !state.OwnsActiveExecution(s, current.Number, identity) {
				return fmt.Errorf("Issue #%d retry execution changed before worker continuation", current.Number)
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
		previousIdentity := state.ExecutionIdentity{RunID: current.RunID, Generation: current.Generation}
		current.Attempts++
		current.RunID = state.NewID("run")
		current.SessionID = ""
		current.Session = nil
		payload := map[string]any{"attempt": current.Attempts}
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, payload, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			identity, transferErr := state.TransferExecution(s, current.Number, previousIdentity, current.RunID, l.now())
			if transferErr != nil {
				return transferErr
			}
			payload["execution_identity"] = identity
			if item.Status != issuedomain.StatusRunning {
				return fmt.Errorf("Issue #%d retry execution is not running", current.Number)
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

func (l *Loop) ensurePendingExecution(current state.Issue) (state.Issue, error) {
	snapshot, err := l.Store.Load()
	if err != nil {
		return state.Issue{}, failure.Wrap(failure.Supervisor, "load pending execution", err)
	}
	if active := snapshot.ActiveExecution; active != nil {
		if active.IssueNumber == current.Number && active.RunID == current.RunID && active.Generation == current.Generation {
			return current, nil
		}
		return state.Issue{}, failure.Wrap(failure.Transient, "acquire pending execution", fmt.Errorf("active execution belongs to Issue #%d", active.IssueNumber))
	}
	if current.Continuation == nil {
		return state.Issue{}, failure.Wrap(failure.Issue, "acquire pending execution", fmt.Errorf("Issue #%d has no continuation checkpoint", current.Number))
	}
	_, err = l.Store.Update("execution_resumed", current.Number, current.RunID, map[string]any{"checkpoint_id": current.Continuation.ID}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID || item.Generation != current.Generation || item.Continuation == nil || item.Continuation.ID != current.Continuation.ID {
			return fmt.Errorf("Issue #%d continuation changed before execution resume", current.Number)
		}
		if _, resumeErr := state.ResumeContinuation(snapshot, item.Number, item.Continuation.ID, l.now()); resumeErr != nil {
			return resumeErr
		}
		var transition issuedomain.Transition
		var transitionErr error
		switch item.Status {
		case issuedomain.StatusResumePending:
			transition, transitionErr = issuedomain.StartAnsweredResume(item.Status)
		case issuedomain.StatusRetryWait:
			transition, transitionErr = issuedomain.StartRetry(item.Status)
		case issuedomain.StatusResolvingConflict:
			return nil
		default:
			return fmt.Errorf("Issue #%d status %s cannot resume execution", item.Number, item.Status)
		}
		if transitionErr != nil {
			return transitionErr
		}
		if err := state.ApplyIssueTransition(item, transition); err != nil {
			return err
		}
		item.RetryAfter = nil
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return state.Issue{}, failure.Wrap(failure.Issue, "resume pending execution", err)
	}
	return l.issueState(current.Number)
}

func (l *Loop) reconcileTerminalPullRequest(ctx context.Context, scheduled state.Issue) error {
	snapshot, err := l.Store.Load()
	if err != nil {
		return failure.Wrap(failure.Supervisor, "refresh terminal Issue state", err)
	}
	item := snapshot.Issues[strconv.Itoa(scheduled.Number)]
	if item == nil || !terminalReconciliationCandidate(*item) || item.RunID != scheduled.RunID {
		return nil
	}
	current := *item
	remote, err := l.GitHub.Inspect(ctx, l.Config, current.Number, current.Branch)
	if err != nil {
		return failure.Wrap(failure.Transient, "reconcile terminal Pull Request", err)
	}
	considered, canceled, err := l.reconcileNotPlannedCancellation(snapshot, current, remote, "periodic_reconciliation", nil)
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist not planned cancellation", err)
	}
	if canceled {
		return nil
	}
	if considered {
		currentState, observation := l.reconciliationInputs(snapshot, current, remote, worktree.Inspection{})
		decision, _ := issuedomain.DecideNotPlannedCancellation(currentState, observation)
		_, err = l.Store.Update("terminal_issue_reconciled", current.Number, current.RunID, map[string]any{
			"status": current.Status, "reason": decision.Reason, "github_state_reason": remote.Issue.StateReason,
		}, func(latest *state.Snapshot) error {
			latestItem := latest.Issues[strconv.Itoa(current.Number)]
			if latestItem == nil || !reflect.DeepEqual(latestItem, &current) {
				return nil
			}
			latestItem.GitHubStateReason = remote.Issue.StateReason
			latestItem.UpdatedAt = l.now()
			return nil
		})
		return failure.Wrap(failure.Supervisor, "persist refused not planned cancellation", err)
	}
	if !terminalPullRequestCandidate(current) {
		_, err = l.Store.Update("terminal_issue_reconciled", current.Number, current.RunID, map[string]any{
			"status": current.Status, "reason": "terminal Issue remains sticky", "github_state_reason": remote.Issue.StateReason,
		}, func(latest *state.Snapshot) error {
			latestItem := latest.Issues[strconv.Itoa(current.Number)]
			if latestItem != nil && reflect.DeepEqual(latestItem, &current) {
				latestItem.GitHubStateReason = remote.Issue.StateReason
				latestItem.UpdatedAt = l.now()
			}
			return nil
		})
		return failure.Wrap(failure.Supervisor, "persist terminal Issue reconciliation", err)
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

var errWorkerResultSuperseded = errors.New("worker result superseded by authoritative state")

func deadlinePointer(value time.Time) *time.Time { return &value }

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

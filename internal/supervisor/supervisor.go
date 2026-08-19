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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/capability"
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
	publicationMu   sync.Mutex
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
	selector := &scheduler{loop: l, active: map[int]activeJob{}}
	selected, evaluation, ok, err := selector.selectReady(ctx, issues, snapshot)
	if err != nil {
		return false, failure.Wrap(failure.Supervisor, "select Issue admission", err)
	}
	if !ok {
		return false, l.markPolling("")
	}
	return true, l.startIssueAtSlotWithResources(ctx, selected, state.NewID("run"), 0, evaluation.DeclaredResources, evaluation.Resources)
}

func (l *Loop) pruneRunLogs(snapshot state.Snapshot) error {
	exclude := map[string]bool{}
	for _, issue := range snapshot.Issues {
		switch issue.Status {
		case "claiming", "claimed", "running", "answer_claim_waiting", "resume_pending", "environment_resume_pending", "publication_recovery_pending", "pull_request_checks_recovery_pending", "retry_wait", "needs_input", "awaiting_checks", "awaiting_merge", "resolving_conflict":
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
		if issue.Status != "claiming" && issue.Status != "answer_claim_waiting" && issue.Status != "resume_pending" && issue.Status != "environment_resume_pending" && issue.Status != "publication_recovery_pending" && issue.Status != "pull_request_checks_recovery_pending" && issue.Status != "retry_wait" && issue.Status != "awaiting_checks" && issue.Status != "awaiting_merge" && issue.Status != "resolving_conflict" && issue.GitHubSync == "" {
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
	declared, resolved = evaluation.DeclaredResources, evaluation.Resources
	now := l.now()
	_, _, err = l.Store.ReserveLease(state.LeaseReservation{
		IssueNumber: issue.Number, Title: issue.Title, RunID: runID, Slot: slot,
		DeclaredResources: declared, ResolvedResources: resolved, BaseSHA: localBaseSHA(ctx, l.Config), ReservedAt: now,
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
	launch, err := l.Worktrees.ValidateLaunch(ctx, l.Config, wt.Path, wt.Branch)
	if err != nil || !launch.Valid {
		if err == nil {
			err = fmt.Errorf("worktree validator did not establish a valid launch boundary")
		}
		return l.failIssue(ctx, issue.Number, failure.Wrap(failure.Issue, "validate initial Issue worktree", err), false)
	}
	workspace := state.WorkerWorkspace{
		Path: launch.CanonicalCWD, Branch: launch.Branch,
		RepoID: l.Store.RepoID, Repository: l.Config.GitHub.Repo, RepositoryID: l.Config.GitHub.RepositoryID,
		GitCommonDir: launch.CommonDir, MainCheckout: launch.MainCheckout, CapturedAt: l.now(),
	}
	_, err = l.Store.Update("worker_started", issue.Number, runID, map[string]any{
		"worktree": wt.Path, "branch": wt.Branch, "identity": l.WorkerIdentity,
		"expected_cwd": launch.CanonicalCWD, "workspace_validation": launch,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(issue.Number)]
		item.Status, item.Worktree, item.Branch, item.UpdatedAt = "running", launch.CanonicalCWD, wt.Branch, l.now()
		item.Workspace = &workspace
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
	workerCfg.RepoPath = workspace.Path
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, "", l.recordWorkerPID(current))
	return l.handleResult(ctx, issue, current, result, runErr)
}

func workerCapabilityRecheckStatus(status string) bool {
	switch status {
	case "claiming", "answer_claim_waiting", "resume_pending", "environment_resume_pending", "retry_wait":
		return true
	default:
		return false
	}
}

func (l *Loop) processExisting(ctx context.Context, current state.Issue) error {
	if current.CapabilityRequirements != nil && workerCapabilityRecheckStatus(current.Status) {
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
	if current.Status == "answer_claim_waiting" {
		return l.reacquireAnsweredClaim(ctx, issue, current)
	}
	if current.Status == "resume_pending" {
		if current.ResourcePark != nil && current.ResourcePark.Kind == state.ResourceParkKindNeedsInput {
			if reason := l.answeredResumeRemoteMismatch(issue, current); reason != "" {
				return l.rejectAnsweredContinuation(current, reason)
			}
		}
		if err := l.GitHub.MarkRunning(ctx, l.Config, current.Number); err != nil {
			return failure.Wrap(failure.Transient, "mark resumed Issue running", err)
		}
		current.Status = "running"
		current.RetryAfter = nil
		_, err = l.Store.Update("worker_started", current.Number, current.RunID, map[string]string{"mode": "user_answer_resume"}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(current.Number)]
			if item == nil || item.Status != "resume_pending" || item.GitHubSync != "" || item.Lease == nil {
				return fmt.Errorf("Issue #%d answered continuation is no longer pending", current.Number)
			}
			item.Status = "running"
			item.RetryAfter = nil
			if item.ResourcePark != nil && item.ResourcePark.Kind == state.ResourceParkKindNeedsInput && item.ResourcePark.Status == "resuming" {
				item.ResourcePark.Status = "resumed"
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
			if item.ResourcePark != nil && item.ResourcePark.Status == "resuming" {
				item.ResourcePark.Status = "resumed"
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
	var workspaceErr *workerWorkspaceError
	if errors.As(runErr, &workspaceErr) {
		return l.blockWorkerWorkspace(ctx, current, workspaceErr)
	}
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	if current.ExecutionProfile == "extended" {
		profile = "extended"
	}
	if current.CapabilityRequirements != nil {
		profile = current.CapabilityRequirements.Profile
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
		parkID := state.NewID("park")
		parkedAt := l.now()
		owner := state.LeaseOwner{}
		if current.Lease != nil {
			owner = current.Lease.Owner
		}
		_, err := l.Store.Update("input_requested", issue.Number, current.RunID, map[string]any{
			"question": q, "request_id": requestID, "resource_park_id": parkID,
			"released_owner": owner, "parked_at": parkedAt,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(issue.Number)]
			if item == nil || item.RunID != current.RunID || item.WorkerPID != 0 || item.WorkerPGID != 0 ||
				item.Lease == nil || item.Lease.Owner != owner || item.ConflictRecovery != nil {
				return fmt.Errorf("Issue #%d no longer has a parkable needs-input worker boundary", issue.Number)
			}
			if err := state.ParkIssueLease(item, owner, parkID, parkedAt); err != nil {
				return err
			}
			item.ResourcePark.Kind = state.ResourceParkKindNeedsInput
			item.ResourcePark.RequestID = requestID
			item.Status, item.UpdatedAt = "needs_input", l.now()
			item.FailureKind = ""
			item.GitHubSync = "needs_input"
			s.PendingRequests[requestID] = &state.Request{
				ID: requestID, IssueNumber: issue.Number, Question: q.Text, Reason: q.Reason,
				Recommended: q.RecommendedOption, Options: q.Options, AllowFreeText: q.AllowFreeText,
				RunID: current.RunID, ResourceParkID: parkID, ReleasedOwner: &owner,
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

func (l *Loop) answeredResumeRemoteMismatch(issue gh.Issue, current state.Issue) string {
	labels := labelSet(issue.Labels)
	needsInput := labels[l.Config.GitHub.NeedsInputLabel]
	running := labels[l.Config.GitHub.RunningLabel]
	if !strings.EqualFold(issue.State, "open") {
		return "GitHub Issue is no longer open"
	}
	if needsInput == running {
		return "GitHub needs-input/running labels are ambiguous"
	}
	if labels[l.Config.GitHub.DoneLabel] || labels[l.Config.GitHub.FailedLabel] ||
		hasAnyLabel(labels, append(append([]string{}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
		return "GitHub Issue has an incompatible manual, terminal, or ready label"
	}
	if current.PullRequestURL != "" {
		return "answered continuation unexpectedly has a saved Pull Request"
	}
	return ""
}

func (l *Loop) rejectAnsweredContinuation(current state.Issue, reason string) error {
	now := l.now()
	_, err := l.Store.Update("answered_resume_rejected", current.Number, current.RunID, map[string]string{"reason": reason}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.Status != "resume_pending" || item.RunID != current.RunID || item.Lease == nil || current.Lease == nil || item.Lease.Owner != current.Lease.Owner {
			return fmt.Errorf("Issue #%d answered continuation changed before rejection", current.Number)
		}
		if item.ResourcePark == nil || item.ResourcePark.Kind != state.ResourceParkKindNeedsInput || item.ResourcePark.Status != "resuming" {
			return fmt.Errorf("Issue #%d answered continuation park is inconsistent", current.Number)
		}
		item.ResourcePark.Status = "resumed"
		if err := state.ReleaseIssueLease(item, current.Lease.Owner); err != nil {
			return err
		}
		item.Status = "blocked"
		item.LastError = "answered continuation rejected: " + reason
		item.FailureKind = string(failure.Issue)
		item.BlockedCause = &state.BlockedCause{Origin: "supervisor", Kind: "answer_resume", Resumable: false, Reason: reason, BlockedAt: now}
		item.RetryAfter = nil
		item.UpdatedAt = now
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist rejected answered continuation", err)
}

var errAnsweredClaimWaiting = errors.New("answered needs-input claim is still waiting")

func (l *Loop) reacquireAnsweredClaim(ctx context.Context, remoteIssue gh.Issue, current state.Issue) error {
	if current.WorkerPID != 0 || current.WorkerPGID != 0 || current.Lease != nil || current.ResourcePark == nil ||
		current.ResourcePark.Kind != state.ResourceParkKindNeedsInput || current.ResourcePark.Status != "parked" || current.PullRequestURL != "" {
		return nil
	}
	if current.CapabilityRequirements != nil && !capability.EvaluateRequirement(current.CapabilityRequirements, l.Config.WorkerCapabilityProfiles()).Compatible {
		return nil
	}
	labels := labelSet(remoteIssue.Labels)
	if !strings.EqualFold(remoteIssue.State, "open") || !labels[l.Config.GitHub.NeedsInputLabel] || labels[l.Config.GitHub.RunningLabel] ||
		labels[l.Config.GitHub.DoneLabel] || labels[l.Config.GitHub.FailedLabel] ||
		hasAnyLabel(labels, append(append([]string{}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
		return nil
	}
	if l.Worktrees == nil || current.Worktree == "" || current.Branch == "" {
		return nil
	}
	inspection, err := l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
	if err != nil || !inspection.Exists || !inspection.Valid || inspection.Branch != current.Branch || !inspection.LocalBranchExists {
		return nil
	}
	now := l.now()
	_, err = l.Store.Update("answered_claim_acquired", current.Number, current.RunID, map[string]any{
		"request_id": current.ResourcePark.RequestID, "resource_park_id": current.ResourcePark.ID,
	}, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.Status != "answer_claim_waiting" || item.RunID != current.RunID || item.WorkerPID != 0 || item.WorkerPGID != 0 || item.Lease != nil {
			return errAnsweredClaimWaiting
		}
		request := snapshot.PendingRequests[item.ResourcePark.RequestID]
		if request == nil || request.Status != "answered" {
			return fmt.Errorf("Issue #%d answered request is missing", current.Number)
		}
		if err := state.ValidateNeedsInputPark(item, request); err != nil {
			return err
		}
		slot, ok := availableSnapshotLeaseSlot(snapshot, l.Config.Queue.Concurrency, item.ResourcePark.OriginalLease.Slot, item.Number)
		if !ok {
			return errAnsweredClaimWaiting
		}
		if _, resumeErr := state.ResumeParkedLease(snapshot, item.Number, item.ResourcePark.ID, slot, now); resumeErr != nil {
			var conflict state.LeaseConflictError
			if errors.As(resumeErr, &conflict) {
				return errAnsweredClaimWaiting
			}
			return resumeErr
		}
		item.Status = "resume_pending"
		item.RetryAfter = nil
		item.UpdatedAt = now
		return nil
	})
	if errors.Is(err, errAnsweredClaimWaiting) {
		return nil
	}
	return failure.Wrap(failure.Supervisor, "reacquire answered needs-input claim", err)
}

func availableSnapshotLeaseSlot(snapshot *state.Snapshot, limit, preferred, issueNumber int) (int, bool) {
	if limit < 1 {
		limit = 1
	}
	used := map[int]bool{}
	for _, other := range snapshot.Issues {
		if other == nil || other.Number == issueNumber || other.Lease == nil || !durableWorkerSlotOccupied(other.Status) {
			continue
		}
		used[other.Lease.Slot] = true
	}
	if preferred >= 0 && preferred < limit && !used[preferred] {
		return preferred, true
	}
	for slot := 0; slot < limit; slot++ {
		if !used[slot] {
			return slot, true
		}
	}
	return -1, false
}

func durableWorkerSlotOccupied(status string) bool {
	switch status {
	case "claiming", "claimed", "running", "resume_pending", "environment_resume_pending", "resolving_conflict":
		return true
	default:
		return false
	}
}

func (l *Loop) blockWorkerWorkspace(ctx context.Context, expected state.Issue, validationErr *workerWorkspaceError) error {
	reason := validationErr.Error()
	payload := map[string]any{
		"expected_cwd": validationErr.expected,
		"validation":   validationErr.validation,
		"error":        reason,
		"run_id":       expected.RunID,
	}
	_, err := l.Store.Update("worker_workspace_rejected", expected.Number, expected.RunID, payload, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(expected.Number)]
		if item == nil || item.RunID != expected.RunID {
			return fmt.Errorf("Issue #%d run changed while rejecting worker workspace", expected.Number)
		}
		item.Status = "blocked"
		item.LastError = reason
		item.FailureKind = string(failure.Issue)
		item.GitHubSync = "blocked"
		item.WorkerPID = 0
		item.WorkerPGID = 0
		item.RetryAfter = nil
		item.BlockedCause = &state.BlockedCause{
			Origin: "supervisor", Kind: "worker_workspace", Resumable: false,
			Reason: reason, BlockedAt: l.now(),
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist rejected worker workspace", err)
	}
	updated, err := l.issueState(expected.Number)
	if err != nil {
		return err
	}
	return l.syncGitHub(ctx, updated)
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
			AllowFreeText: true, ResumeStatus: "resume_pending", Status: "pending", CreatedAt: l.now(),
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
		return l.schedulePullRequestChecksRetry(ctx, current, *selected)
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

func (l *Loop) schedulePullRequestChecksRetry(ctx context.Context, issue state.Issue, pr gh.PullRequest) error {
	reason := "Pull Request checks failed: " + pr.URL
	canContinue := issue.ExecutionProfile == "extended" && l.canResume(issue) && issue.Continuations < l.maxContinuations()
	canRetry := issue.Attempts < l.Config.Queue.MaxAttempts
	if canContinue || canRetry {
		return l.scheduleRetry(ctx, issue, reason)
	}
	return l.failPullRequestChecks(ctx, issue, pr, reason)
}

func (l *Loop) failPullRequestChecks(ctx context.Context, current state.Issue, pr gh.PullRequest, reason string) error {
	cause := failure.Wrap(failure.Issue, "worker retry limit reached", errors.New(reason))
	if current.PullRequestURL == "" || current.Branch == "" || pr.URL != current.PullRequestURL ||
		pr.HeadRefName != current.Branch || pr.HeadSHA == "" {
		return l.failIssue(ctx, current.Number, cause, false)
	}
	now := l.now()
	_, err := l.Store.Update("pull_request_checks_retry_exhausted", current.Number, current.RunID, map[string]any{
		"pull_request_url": pr.URL, "pull_request_number": pr.Number, "head_sha": pr.HeadSHA,
		"checks_status": pr.ChecksStatus, "attempts": current.Attempts, "continuations": current.Continuations,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(current.Number)]
		if item == nil || item.RunID != current.RunID || item.PullRequestURL != pr.URL || item.Branch != pr.HeadRefName {
			return fmt.Errorf("Issue #%d changed while recording Pull Request checks exhaustion", current.Number)
		}
		item.Status = "failed"
		item.LastError = cause.Error()
		item.FailureKind = string(failure.Issue)
		item.GitHubSync = "failed"
		item.RetryAfter = nil
		item.SessionID = ""
		item.Session = nil
		item.HeadSHA = pr.HeadSHA
		item.PullRequestNumber = pr.Number
		item.PullRequestChecksFailure = &state.PullRequestChecksFailure{
			Origin: state.ChecksFailureOriginPullRequest, Phase: state.ChecksFailurePhaseRequired,
			Code: state.ChecksFailureCodeRetryExhausted, Recoverable: true,
			PullRequestURL: pr.URL, PullRequestNumber: pr.Number, Branch: pr.HeadRefName,
			HeadSHA: pr.HeadSHA, ChecksStatus: "failure", RetryExhausted: true, FailedAt: now,
		}
		item.UpdatedAt = now
		return nil
	})
	if err != nil {
		return failure.Wrap(failure.Supervisor, "persist Pull Request checks exhaustion", err)
	}
	updated, stateErr := l.issueState(current.Number)
	if stateErr != nil {
		return stateErr
	}
	return l.syncGitHub(ctx, updated)
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
	result, runErr := l.runWorker(ctx, workerCfg, issue, current, worker.BuildConflictPrompt(current), l.recordWorkerPID(current))
	return l.handleConflictResult(ctx, issue, current, result, runErr)
}

func (l *Loop) handleConflictResult(ctx context.Context, issue gh.Issue, current state.Issue, result worker.Result, runErr error) error {
	var workspaceErr *workerWorkspaceError
	if errors.As(runErr, &workspaceErr) {
		return l.blockWorkerWorkspace(ctx, current, workspaceErr)
	}
	profile := result.ExecutionProfile
	if profile == "" {
		profile = "extended"
	}
	if current.CapabilityRequirements != nil {
		profile = current.CapabilityRequirements.Profile
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

func (l *Loop) validateWorkerLaunch(ctx context.Context, cfg config.Config, expected state.Issue) (worktree.LaunchValidation, error) {
	validation := worktree.LaunchValidation{ExpectedCWD: cfg.RepoPath, Checks: map[string]bool{}}
	fresh, err := l.issueState(expected.Number)
	if err != nil {
		return validation, &workerWorkspaceError{expected: cfg.RepoPath, validation: validation, cause: err}
	}
	fail := func(cause error) (worktree.LaunchValidation, error) {
		return validation, &workerWorkspaceError{expected: cfg.RepoPath, validation: validation, cause: cause}
	}
	if fresh.RunID == "" || fresh.RunID != expected.RunID {
		return fail(fmt.Errorf("run changed from %q to %q", expected.RunID, fresh.RunID))
	}
	validation.Checks["run_id"] = true
	if fresh.CapabilityRequirements != nil {
		capabilityEvaluation := capability.EvaluateRequirement(fresh.CapabilityRequirements, l.Config.WorkerCapabilityProfiles())
		if !capabilityEvaluation.Compatible {
			codes := make([]string, 0, len(capabilityEvaluation.Mismatches))
			for _, mismatch := range capabilityEvaluation.Mismatches {
				codes = append(codes, mismatch.Code)
			}
			sort.Strings(codes)
			return fail(fmt.Errorf("worker capability predicate failed: %s", strings.Join(codes, ",")))
		}
		validation.Checks["worker_capabilities"] = true
	}
	if fresh.SessionID != expected.SessionID {
		return fail(fmt.Errorf("session changed before spawn"))
	}
	validation.Checks["session_id"] = true
	if fresh.Worktree == "" || fresh.Worktree != expected.Worktree || fresh.Worktree != cfg.RepoPath {
		return fail(fmt.Errorf("saved worktree path changed before spawn"))
	}
	validation.Checks["saved_path"] = true
	if fresh.Branch == "" || fresh.Branch != expected.Branch {
		return fail(fmt.Errorf("saved worktree branch changed before spawn"))
	}
	validation.Checks["saved_branch_state"] = true
	if fresh.Lease == nil || expected.Lease == nil {
		return fail(fmt.Errorf("active resource lease is missing"))
	}
	if fresh.LeaseGeneration == 0 || fresh.LeaseGeneration != expected.LeaseGeneration || fresh.Lease.Owner != expected.Lease.Owner ||
		fresh.Lease.Owner.RunID != fresh.RunID || fresh.Lease.Owner.Generation != fresh.LeaseGeneration {
		return fail(fmt.Errorf("resource lease owner generation changed before spawn"))
	}
	validation.Checks["lease_owner_generation"] = true

	local, inspectErr := l.Worktrees.ValidateLaunch(ctx, l.Config, fresh.Worktree, fresh.Branch)
	for name, passed := range local.Checks {
		validation.Checks[name] = passed
	}
	validation.CanonicalCWD = local.CanonicalCWD
	validation.TopLevel = local.TopLevel
	validation.Branch = local.Branch
	validation.CommonDir = local.CommonDir
	validation.MainCheckout = local.MainCheckout
	if inspectErr != nil {
		return fail(inspectErr)
	}
	validation.Valid = local.Valid
	if !validation.Valid {
		return fail(fmt.Errorf("worktree validator did not establish a valid launch boundary"))
	}

	workspace := fresh.Workspace
	if workspace == nil {
		return fail(fmt.Errorf("saved workspace provenance is missing"))
	} else if !workspace.Matches(validation.CanonicalCWD, validation.Branch, l.Store.RepoID, l.Config.GitHub.Repo,
		l.Config.GitHub.RepositoryID, validation.CommonDir, validation.MainCheckout) {
		return fail(fmt.Errorf("saved workspace provenance does not match the launch target"))
	}
	validation.Checks["saved_provenance"] = true

	payload := map[string]any{
		"expected_cwd": validation.CanonicalCWD, "validation": validation,
		"run_id": fresh.RunID, "session_present": fresh.SessionID != "", "lease_owner": fresh.Lease.Owner,
	}
	_, err = l.Store.Update("worker_workspace_validated", fresh.Number, fresh.RunID, payload, func(snapshot *state.Snapshot) error {
		item := snapshot.Issues[strconv.Itoa(fresh.Number)]
		if item == nil || item.RunID != fresh.RunID || item.Worktree != fresh.Worktree || item.Branch != fresh.Branch || item.SessionID != fresh.SessionID ||
			item.Lease == nil || item.Lease.Owner != fresh.Lease.Owner || item.LeaseGeneration != fresh.LeaseGeneration {
			return fmt.Errorf("Issue #%d launch provenance changed during validation", fresh.Number)
		}
		if item.Workspace == nil || *item.Workspace != *workspace {
			return fmt.Errorf("Issue #%d workspace provenance changed during validation", fresh.Number)
		}
		item.UpdatedAt = l.now()
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return validation, nil
}

func (l *Loop) recordWorkerPID(expected state.Issue) worker.Started {
	return func(start worker.ProcessStart) error {
		number, runID := expected.Number, expected.RunID
		validation := worktree.LaunchValidation{
			ExpectedCWD: start.ExpectedCWD, CanonicalCWD: start.ActualCWD,
			Checks: map[string]bool{"spawn_cwd": start.ExpectedCWD != "" && start.ActualCWD == start.ExpectedCWD},
		}
		fail := func(cause error) error {
			return &workerWorkspaceError{expected: start.ExpectedCWD, validation: validation, cause: cause}
		}
		if start.PID <= 0 || start.PGID != start.PID {
			return fail(fmt.Errorf("Issue #%d run %s reported invalid worker process identity", number, runID))
		}
		if start.ExpectedCWD == "" || start.ActualCWD != start.ExpectedCWD {
			return fail(fmt.Errorf("Issue #%d run %s spawned with cwd %q, expected %q", number, runID, start.ActualCWD, start.ExpectedCWD))
		}
		_, err := l.Store.Update("worker_process_started", number, runID, start, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(number)]
			if item == nil || item.RunID != runID {
				return fmt.Errorf("Issue #%d run %s is no longer active", number, runID)
			}
			if expected.Lease == nil || item.Lease == nil || item.Lease.Owner != expected.Lease.Owner || item.LeaseGeneration != expected.LeaseGeneration {
				return fmt.Errorf("Issue #%d run %s resource lease owner generation changed before process audit", number, runID)
			}
			if item.Workspace == nil || item.Workspace.Path != start.ExpectedCWD {
				return fmt.Errorf("Issue #%d run %s has no matching saved workspace provenance", number, runID)
			}
			item.WorkerPID = start.PID
			// All worker backends start their command with Setpgid=true, making
			// the process PID the process-group ID used for cancellation.
			item.WorkerPGID = start.PGID
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return fail(err)
		}
		return nil
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
	parkID := state.NewID("park")
	parkedAt := l.now()
	owner := state.LeaseOwner{}
	if current.Lease != nil {
		owner = current.Lease.Owner
	}
	_, err = l.Store.Update("issue_blocked", number, current.RunID, map[string]any{
		"error": cause.Error(), "failure_kind": string(failure.Issue), "blocked_origin": "worker", "blocked_kind": "environment",
		"resource_park_id": parkID, "released_owner": owner, "parked_at": parkedAt,
	}, func(s *state.Snapshot) error {
		item := s.Issues[strconv.Itoa(number)]
		if item == nil || item.RunID != current.RunID {
			return fmt.Errorf("Issue #%d run changed while recording worker block", number)
		}
		if item.WorkerPID != 0 || item.WorkerPGID != 0 {
			return fmt.Errorf("Issue #%d worker process identity still exists while parking resources", number)
		}
		if item.Lease == nil || item.Lease.Owner != owner {
			return fmt.Errorf("Issue #%d does not own a consistent resource lease to park", number)
		}
		// Move the full lease into a non-admitting park record. Session, Goal,
		// answers, worktree, branch, dirty files, and Issue resource metadata stay
		// untouched as the continuation boundary.
		if err := state.ParkIssueLease(item, owner, parkID, parkedAt); err != nil {
			return err
		}
		item.ResourcePark.Kind = state.ResourceParkKindEnvironmentBlock
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
		if adoption := issue.MergedPullRequestAdoption; adoption != nil && adoption.Status == "github_sync_pending" {
			branch := adoption.Branch
			if branch == "" {
				branch = issue.Branch
			}
			remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, branch)
			if inspectErr != nil {
				return failure.Wrap(failure.Transient, "reinspect adopted merged Pull Request", inspectErr)
			}
			if _, validateErr := gh.ValidateMergedPullRequestAdoption(l.Config, remote, gh.MergedPullRequestAdoptionExpectation{
				IssueNumber: issue.Number, PreviousStatus: adoption.PreviousStatus, Branch: branch,
				BaseBranch: adoption.BaseBranch, HeadSHA: adoption.HeadSHA, PullRequestURL: adoption.PullRequestURL,
				PullRequestNumber: adoption.PullRequestNumber, MergeCommitSHA: adoption.MergeSHA, AllowDone: true,
			}); validateErr != nil {
				return validateErr
			}
		}
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
	case "pull_request_checks_recovery":
		if issue.PullRequestChecksRecovery == nil {
			return fmt.Errorf("Issue #%d Pull Request checks recovery metadata is missing", issue.Number)
		}
		remote, inspectErr := l.GitHub.Inspect(ctx, l.Config, issue.Number, issue.Branch)
		if inspectErr != nil {
			return failure.Wrap(failure.Transient, "reinspect Pull Request checks recovery", inspectErr)
		}
		if validateErr := l.validatePullRequestChecksRecoverySync(issue, remote); validateErr != nil {
			return validateErr
		}
		err = l.GitHub.MarkPullRequestChecksRecovery(ctx, l.Config, issue.Number, issue.PullRequestChecksRecovery.ID)
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
			if issue.GitHubSync == "done" && item.MergedPullRequestAdoption != nil && item.MergedPullRequestAdoption.Status == "github_sync_pending" {
				item.MergedPullRequestAdoption.Status = "synced"
			}
			if issue.GitHubSync == "environment_resume" && item.EnvironmentResume != nil {
				item.EnvironmentResume.Status = "github_synced"
			}
			if issue.GitHubSync == "publication_recovery" && item.PublicationRecovery != nil {
				item.PublicationRecovery.Status = "github_synced"
			}
			if issue.GitHubSync == "pull_request_checks_recovery" && item.PullRequestChecksRecovery != nil {
				now := l.now()
				item.Status = "awaiting_checks"
				item.FailureKind = ""
				item.LastError = ""
				item.PullRequestChecksRecovery.Status = "resumed"
				item.RetryAfter = &now
			}
		}
		item.UpdatedAt = l.now()
		return nil
	})
	return failure.Wrap(failure.Supervisor, "persist GitHub synchronization", err)
}

func (l *Loop) validatePullRequestChecksRecoverySync(issue state.Issue, remote gh.RemoteState) error {
	labels := labelSet(remote.Issue.Labels)
	failed := labels[l.Config.GitHub.FailedLabel]
	running := labels[l.Config.GitHub.RunningLabel]
	if !strings.EqualFold(remote.Issue.State, "open") || failed == running ||
		hasAnyLabel(labels, append(append([]string{l.Config.GitHub.DoneLabel, l.Config.GitHub.NeedsInputLabel}, l.Config.GitHub.ReadyLabels...), l.Config.GitHub.ExcludeLabels...)) {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: authoritative Issue labels changed")
	}
	if len(remote.PullRequests) != 1 || issue.PullRequestChecksRecovery == nil {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: saved Pull Request count changed")
	}
	pr := remote.PullRequests[0]
	if pr.URL != issue.PullRequestURL || pr.Number != issue.PullRequestNumber || !strings.EqualFold(pr.State, "open") || pr.MergedAt != nil ||
		pr.HeadRefName != issue.Branch || pr.BaseRefName != l.Config.Git.BaseBranch || !strings.EqualFold(pr.HeadRepository, l.Config.GitHub.Repo) || pr.HeadSHA != issue.PullRequestChecksRecovery.NewHeadSHA ||
		(pr.ChecksStatus != "pending" && pr.ChecksStatus != "success") {
		return fmt.Errorf("refuse Pull Request checks recovery synchronization: authoritative Pull Request, head, or checks changed")
	}
	return nil
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

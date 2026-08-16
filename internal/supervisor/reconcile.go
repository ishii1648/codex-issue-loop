package supervisor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type osProcessInspector struct{}

func (osProcessInspector) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

type reconciliationDecision struct {
	status      string
	lastError   string
	branch      string
	pullRequest string
	githubSync  string
	retryAt     *time.Time
	workerPID   int
	prMerged    bool
	markRunning bool
	reason      string
}

func (l *Loop) reconcileStartup(ctx context.Context, snapshot state.Snapshot) error {
	numbers := make([]int, 0, len(snapshot.Issues))
	for _, item := range snapshot.Issues {
		if item.Status == "completed" && item.GitHubSync == "" && (item.PullRequestURL == "" || item.PullRequestMerged) {
			continue
		}
		numbers = append(numbers, item.Number)
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		current := snapshot.Issues[strconv.Itoa(number)]
		if current == nil {
			continue
		}
		remote, err := l.GitHub.Inspect(ctx, l.Config, number, current.Branch)
		if err != nil {
			return fmt.Errorf("reconcile GitHub state for Issue #%d: %w", number, err)
		}
		inspection := worktree.Inspection{}
		if current.Worktree != "" {
			inspection, err = l.Worktrees.Inspect(ctx, l.Config, current.Worktree, current.Branch)
			if err != nil {
				return fmt.Errorf("reconcile worktree for Issue #%d: %w", number, err)
			}
		}
		decision := l.decideReconciliation(snapshot, *current, remote, inspection)
		if decision.markRunning {
			if err := l.GitHub.MarkRunning(ctx, l.Config, number); err != nil {
				return fmt.Errorf("repair running label for Issue #%d: %w", number, err)
			}
		}
		_, err = l.Store.Update("startup_reconciled", number, current.RunID, map[string]any{
			"previous_status": current.Status,
			"status":          decision.status,
			"reason":          decision.reason,
			"worktree":        inspection,
			"pull_requests":   remote.PullRequests,
		}, func(s *state.Snapshot) error {
			item := s.Issues[strconv.Itoa(number)]
			if item == nil {
				return fmt.Errorf("Issue #%d disappeared during startup reconciliation", number)
			}
			item.Status = decision.status
			item.LastError = decision.lastError
			item.Branch = decision.branch
			item.PullRequestURL = decision.pullRequest
			item.PullRequestMerged = decision.prMerged
			item.GitHubSync = decision.githubSync
			item.RetryAfter = decision.retryAt
			item.WorkerPID = decision.workerPID
			item.UpdatedAt = l.now()
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *Loop) decideReconciliation(snapshot state.Snapshot, current state.Issue, remote gh.RemoteState, inspection worktree.Inspection) reconciliationDecision {
	decision := reconciliationDecision{
		status: current.Status, lastError: current.LastError, branch: current.Branch,
		pullRequest: current.PullRequestURL, githubSync: current.GitHubSync,
		retryAt: current.RetryAfter, workerPID: current.WorkerPID, prMerged: current.PullRequestMerged,
		reason: "state already converged",
	}
	labels := labelSet(remote.Issue.Labels)
	ready := hasAnyLabel(labels, l.Config.GitHub.ReadyLabels)
	running := labels[l.Config.GitHub.RunningLabel]
	needsInput := labels[l.Config.GitHub.NeedsInputLabel]
	done := labels[l.Config.GitHub.DoneLabel]
	failed := labels[l.Config.GitHub.FailedLabel]
	excluded := hasAnyLabel(labels, l.Config.GitHub.ExcludeLabels)
	if current.Status == "completed" && strings.EqualFold(remote.Issue.State, "open") {
		open := []gh.PullRequest{}
		for _, pr := range remote.PullRequests {
			if pr.MergedAt == nil && strings.EqualFold(pr.State, "open") {
				open = append(open, pr)
			}
		}
		if len(open) == 1 {
			decision.status = "awaiting_merge"
			if open[0].IsDraft {
				decision.status = "awaiting_checks"
			}
			decision.pullRequest = open[0].URL
			decision.prMerged = false
			decision.githubSync = ""
			decision.retryAt = nil
			decision.workerPID = 0
			decision.markRunning = true
			decision.reason = "legacy completed Issue with open Pull Request returned to lifecycle monitoring"
			return decision
		}
	}

	if done {
		decision.status, decision.lastError = "completed", ""
		for _, pr := range remote.PullRequests {
			if pr.MergedAt != nil {
				decision.pullRequest = pr.URL
				decision.prMerged = true
				break
			}
		}
		if current.GitHubSync == "done" && !hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") {
			decision.githubSync = "done"
		} else {
			decision.githubSync = ""
		}
		decision.workerPID, decision.retryAt, decision.reason = 0, nil, "GitHub done label is authoritative"
		return decision
	}
	if excluded {
		if current.GitHubSync == "blocked" {
			decision.status, decision.workerPID, decision.retryAt = "blocked", 0, nil
			if hasComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)) {
				decision.githubSync = ""
			}
			decision.reason = "partially synchronized blocked state recovered"
			return decision
		}
		return blockDecision(decision, "GitHub exclusion label was applied manually")
	}
	if failed {
		decision.status, decision.workerPID, decision.retryAt = "failed", 0, nil
		if current.GitHubSync == "failed" && !hasComment(remote.Issue.Comments, fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", current.Number)) {
			decision.githubSync = "failed"
		} else {
			decision.githubSync = ""
		}
		decision.reason = "GitHub failed label is authoritative"
		return decision
	}
	if ready && (running || needsInput) {
		return blockDecision(decision, "GitHub labels contain conflicting ready and active states")
	}

	openPRs := []gh.PullRequest{}
	var mergedPR *gh.PullRequest
	var closedPR *gh.PullRequest
	for index := range remote.PullRequests {
		pr := &remote.PullRequests[index]
		if pr.MergedAt != nil {
			if mergedPR == nil {
				mergedPR = pr
			}
			continue
		}
		if strings.EqualFold(pr.State, "open") {
			openPRs = append(openPRs, *pr)
		} else if closedPR == nil {
			closedPR = pr
		}
	}
	if mergedPR != nil {
		decision.status, decision.lastError, decision.pullRequest = "completed", "", mergedPR.URL
		decision.prMerged = true
		decision.workerPID, decision.retryAt, decision.reason = 0, nil, "merged Pull Request discovered"
		if hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") && done {
			decision.githubSync = ""
		} else {
			decision.githubSync = "done"
		}
		return decision
	}
	if len(openPRs) > 1 {
		return blockDecision(decision, "multiple open Pull Requests target the saved branch")
	}
	if len(openPRs) == 1 {
		decision.pullRequest = openPRs[0].URL
		if decision.branch == "" {
			decision.branch = openPRs[0].HeadRefName
		}
	} else if closedPR != nil {
		decision.pullRequest = closedPR.URL
		return blockDecision(decision, "Pull Request was closed without merge")
	}
	if strings.EqualFold(remote.Issue.State, "closed") {
		return blockDecision(decision, "GitHub Issue was closed without a done label or merged Pull Request")
	}

	if current.Worktree != "" {
		if !inspection.Exists || !inspection.Valid {
			return blockDecision(decision, "saved worktree is missing or invalid")
		}
		if current.Branch != "" && inspection.Branch != current.Branch {
			return blockDecision(decision, fmt.Sprintf("worktree branch changed from %s to %s", current.Branch, inspection.Branch))
		}
		if current.Branch != "" && !inspection.LocalBranchExists {
			return blockDecision(decision, "saved local branch is missing")
		}
		if len(openPRs) == 1 && !inspection.RemoteBranchExists {
			return blockDecision(decision, "open Pull Request head branch is missing from origin")
		}
	} else if len(openPRs) == 1 {
		return blockDecision(decision, "open Pull Request exists but the saved worktree is missing")
	}

	processes := l.Processes
	if processes == nil {
		processes = osProcessInspector{}
	}
	if current.WorkerPID > 0 && processes.Alive(current.WorkerPID) {
		return blockDecision(decision, fmt.Sprintf("saved worker PID %d is still alive", current.WorkerPID))
	}
	decision.workerPID = 0

	if current.GitHubSync == "needs_input" {
		if request := pendingRequest(snapshot, current.Number); request != nil && needsInput && hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:request:"+request.ID+" -->") {
			decision.githubSync = ""
		}
	}
	if current.GitHubSync == "done" && done && hasComment(remote.Issue.Comments, "<!-- codex-issue-loop:done -->") {
		decision.githubSync = ""
	}

	switch current.Status {
	case "claiming":
		if running && !ready {
			now := l.now()
			decision.status, decision.retryAt = "retry_wait", &now
			decision.lastError, decision.reason = "claim completed before supervisor restart", "write-ahead claim converged from GitHub"
		} else if ready && !running {
			decision.reason = "claim did not reach GitHub and will be retried idempotently"
		} else if !ready && !running {
			return blockDecision(decision, "claim labels were removed manually")
		}
	case "running", "claimed":
		now := l.now()
		decision.status, decision.retryAt = "retry_wait", &now
		decision.lastError, decision.reason = "worker disappeared before supervisor restart", "dead worker scheduled for retry"
		if !running && inspection.Valid {
			decision.markRunning = true
		}
	case "retry_wait":
		decision.reason = "existing retry schedule preserved"
		if !running && inspection.Valid {
			decision.markRunning = true
		}
	case "resume_pending":
		decision.reason = "recorded answer remains pending for resume"
	case "needs_input":
		decision.reason = "unanswered request remains sticky"
		if !needsInput {
			decision.githubSync = "needs_input"
		}
	}
	if len(openPRs) == 1 && (current.Status == "running" || current.Status == "claimed") {
		decision.reason = "open Pull Request discovered and dead worker scheduled for retry"
	}
	return decision
}

func blockDecision(decision reconciliationDecision, reason string) reconciliationDecision {
	decision.status = "blocked"
	decision.lastError = "startup reconciliation blocked: " + reason
	decision.githubSync = ""
	decision.retryAt = nil
	decision.workerPID = 0
	decision.reason = reason
	return decision
}

func labelSet(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		set[label] = true
	}
	return set
}

func hasAnyLabel(labels map[string]bool, candidates []string) bool {
	for _, candidate := range candidates {
		if labels[candidate] {
			return true
		}
	}
	return false
}

func hasComment(comments []string, marker string) bool {
	for _, comment := range comments {
		if strings.Contains(comment, marker) {
			return true
		}
	}
	return false
}

func pendingRequest(snapshot state.Snapshot, issueNumber int) *state.Request {
	for _, request := range snapshot.PendingRequests {
		if request.IssueNumber == issueNumber && request.Status == "pending" {
			return request
		}
	}
	return nil
}

var _ ProcessInspector = osProcessInspector{}

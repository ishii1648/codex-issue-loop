package lifecycle

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worktree"
)

type RemoteInspector interface {
	Inspect(context.Context, config.Config, int, string) (gh.RemoteState, error)
}

type Manager struct {
	Worktrees worktree.Manager
	Remote    RemoteInspector
	Now       func() time.Time
}

type WorktreeSafety struct {
	Dirty              bool     `json:"dirty"`
	UnpushedCommits    bool     `json:"unpushed_commits"`
	OpenPullRequests   []string `json:"open_pull_requests,omitempty"`
	UnansweredRequests []string `json:"unanswered_requests,omitempty"`
}

type Entry struct {
	IssueNumber       int            `json:"issue_number"`
	Status            string         `json:"status"`
	Path              string         `json:"path"`
	Branch            string         `json:"branch,omitempty"`
	UpdatedAt         time.Time      `json:"updated_at,omitempty"`
	MaxAge            string         `json:"max_age"`
	Age               string         `json:"age"`
	Eligible          bool           `json:"eligible"`
	Action            string         `json:"action"`
	Reasons           []string       `json:"reasons"`
	Safety            WorktreeSafety `json:"safety"`
	Recoverable       bool           `json:"recoverable"`
	RecoverySources   []string       `json:"recovery_sources,omitempty"`
	PurgeConfirmation string         `json:"purge_confirmation"`
}

type Result struct {
	Repository string  `json:"repository"`
	RepoID     string  `json:"repo_id"`
	Operation  string  `json:"operation"`
	Applied    bool    `json:"applied"`
	Entries    []Entry `json:"entries"`
}

func ConfirmationToken(repoID string, issueNumber int) string {
	return fmt.Sprintf("%s:issue-%d", repoID, issueNumber)
}

func (m Manager) Plan(ctx context.Context, cfg config.Config, repoID string, snapshot state.Snapshot) (Result, error) {
	if m.Remote == nil {
		return Result{}, fmt.Errorf("remote inspector is required for safe worktree cleanup")
	}
	now := time.Now().UTC()
	if m.Now != nil {
		now = m.Now().UTC()
	}
	result := Result{Repository: cfg.GitHub.Repo, RepoID: repoID, Operation: "cleanup", Entries: []Entry{}}
	issues := make([]*state.Issue, 0, len(snapshot.Issues))
	for _, issue := range snapshot.Issues {
		if issue != nil && issue.Worktree != "" {
			issues = append(issues, issue)
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	for _, issue := range issues {
		entry, err := m.inspect(ctx, cfg, repoID, snapshot, issue, now)
		if err != nil {
			return result, fmt.Errorf("inspect Issue #%d worktree: %w", issue.Number, err)
		}
		result.Entries = append(result.Entries, entry)
	}
	return result, nil
}

func (m Manager) Cleanup(ctx context.Context, cfg config.Config, repoID string, store state.Store, snapshot state.Snapshot) (Result, error) {
	result, err := m.Plan(ctx, cfg, repoID, snapshot)
	if err != nil {
		return result, err
	}
	result.Applied = true
	for index := range result.Entries {
		entry := &result.Entries[index]
		if !entry.Eligible {
			continue
		}
		if err := m.remove(ctx, cfg, store, entry, false); err != nil {
			return result, err
		}
		entry.Action = "removed"
	}
	if err := m.Worktrees.Prune(ctx, cfg); err != nil {
		return result, err
	}
	return result, nil
}

func (m Manager) Purge(ctx context.Context, cfg config.Config, repoID string, store state.Store, snapshot state.Snapshot, issueNumber int, confirmation string) (Result, error) {
	expected := ConfirmationToken(repoID, issueNumber)
	if confirmation != expected {
		return Result{}, fmt.Errorf("purge requires --confirm %q", expected)
	}
	plan, err := m.Plan(ctx, cfg, repoID, snapshot)
	if err != nil {
		return plan, err
	}
	plan.Operation = "purge"
	plan.Applied = true
	for index := range plan.Entries {
		entry := &plan.Entries[index]
		if entry.IssueNumber != issueNumber {
			continue
		}
		if err := m.remove(ctx, cfg, store, entry, true); err != nil {
			return plan, err
		}
		entry.Eligible = true
		entry.Action = "purged"
		entry.Reasons = []string{"explicitly_confirmed_purge"}
		plan.Entries = []Entry{*entry}
		return plan, nil
	}
	return plan, fmt.Errorf("Issue #%d has no retained worktree", issueNumber)
}

func (m Manager) inspect(ctx context.Context, cfg config.Config, repoID string, snapshot state.Snapshot, issue *state.Issue, now time.Time) (Entry, error) {
	maxAge, policyReason := maxAgeForStatus(cfg.Worktrees, issue.Status)
	age := time.Duration(0)
	if !issue.UpdatedAt.IsZero() && now.After(issue.UpdatedAt) {
		age = now.Sub(issue.UpdatedAt)
	}
	entry := Entry{
		IssueNumber: issue.Number, Status: issue.Status, Path: issue.Worktree, Branch: issue.Branch,
		UpdatedAt: issue.UpdatedAt, MaxAge: maxAge.String(), Age: age.Round(time.Second).String(),
		Action: "retain", PurgeConfirmation: ConfirmationToken(repoID, issue.Number),
	}
	inspection, err := m.Worktrees.Inspect(ctx, cfg, issue.Worktree, issue.Branch)
	if err != nil {
		return entry, err
	}
	if !inspection.Exists || !inspection.Valid {
		entry.Reasons = append(entry.Reasons, "worktree_missing_or_invalid")
		return entry, nil
	}
	entry.Safety.Dirty = inspection.Dirty
	entry.Safety.UnpushedCommits = inspection.UnpushedCommits
	if inspection.LocalBranchExists {
		entry.RecoverySources = append(entry.RecoverySources, "local_branch")
	}
	if inspection.RemoteBranchExists {
		entry.RecoverySources = append(entry.RecoverySources, "remote_branch")
	}
	if m.Remote != nil {
		remote, remoteErr := m.Remote.Inspect(ctx, cfg, issue.Number, issue.Branch)
		if remoteErr != nil {
			return entry, remoteErr
		}
		for _, pr := range remote.PullRequests {
			if strings.EqualFold(pr.State, "open") && pr.MergedAt == nil {
				entry.Safety.OpenPullRequests = append(entry.Safety.OpenPullRequests, pr.URL)
			}
		}
	}
	sort.Strings(entry.Safety.OpenPullRequests)
	for id, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issue.Number && request.Status == "pending" {
			entry.Safety.UnansweredRequests = append(entry.Safety.UnansweredRequests, id)
		}
	}
	sort.Strings(entry.Safety.UnansweredRequests)
	entry.Recoverable = !inspection.Dirty && len(entry.RecoverySources) > 0
	if policyReason != "" {
		entry.Reasons = append(entry.Reasons, policyReason)
	} else if issue.UpdatedAt.IsZero() {
		entry.Reasons = append(entry.Reasons, "updated_at_missing")
	} else if age < maxAge {
		entry.Reasons = append(entry.Reasons, "retention_period_not_expired")
	}
	if inspection.Dirty {
		entry.Reasons = append(entry.Reasons, "dirty_worktree")
	}
	if inspection.UnpushedCommits {
		entry.Reasons = append(entry.Reasons, "unpushed_commits")
	}
	if len(entry.Safety.OpenPullRequests) > 0 {
		entry.Reasons = append(entry.Reasons, "open_pull_request")
	}
	if len(entry.Safety.UnansweredRequests) > 0 {
		entry.Reasons = append(entry.Reasons, "unanswered_request")
	}
	if len(entry.Reasons) == 0 {
		entry.Eligible = true
		entry.Action = "would_remove"
		entry.Reasons = []string{"retention_period_expired"}
	}
	return entry, nil
}

func maxAgeForStatus(policy config.Worktrees, status string) (time.Duration, string) {
	var age time.Duration
	switch status {
	case "completed":
		age = policy.CompletedMaxAge.Duration
	case "failed":
		age = policy.FailedMaxAge.Duration
	case "blocked":
		age = policy.BlockedMaxAge.Duration
	case "needs_input", "resume_pending":
		age = policy.NeedsInputMaxAge.Duration
	default:
		return 0, "non_terminal_status"
	}
	if age == 0 {
		return 0, "status_retained_indefinitely"
	}
	return age, ""
}

func (m Manager) remove(ctx context.Context, cfg config.Config, store state.Store, entry *Entry, force bool) error {
	eventPrefix := "worktree_cleanup"
	if force {
		eventPrefix = "worktree_purge"
	}
	payload := map[string]any{
		"path": entry.Path, "branch": entry.Branch, "recoverable": entry.Recoverable,
		"recovery_sources": entry.RecoverySources, "safety": entry.Safety,
	}
	if _, err := store.Update(eventPrefix+"_started", entry.IssueNumber, "", payload, func(*state.Snapshot) error { return nil }); err != nil {
		return fmt.Errorf("record worktree removal intent: %w", err)
	}
	if err := m.Worktrees.Remove(ctx, cfg, entry.Path, force); err != nil {
		_, _ = store.Update(eventPrefix+"_failed", entry.IssueNumber, "", map[string]any{"path": entry.Path, "error": err.Error()}, func(*state.Snapshot) error { return nil })
		return err
	}
	doneEvent := "worktree_cleaned"
	if force {
		doneEvent = "worktree_purged"
	}
	_, err := store.Update(doneEvent, entry.IssueNumber, "", payload, func(snapshot *state.Snapshot) error {
		if issue := snapshot.Issues[fmt.Sprint(entry.IssueNumber)]; issue != nil && issue.Worktree == entry.Path {
			issue.Worktree = ""
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("record worktree removal: %w", err)
	}
	return nil
}

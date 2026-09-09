package github

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/githubqueue"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

type Issue = githubqueue.Issue

type PullRequest struct {
	Number           int
	URL              string
	State            string
	IsDraft          bool
	MergedAt         *time.Time
	HeadRefName      string
	BaseRefName      string
	MergeStateStatus string
	ChecksStatus     string
	ReviewDecision   string
	HeadSHA          string
	MergeSHA         string
	MergeCommitSHA   string
	HeadRepository   string
}

type checkRollup struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

type RemoteState struct {
	Issue        Issue
	PullRequests []PullRequest
}

type Client interface {
	ListReady(context.Context, config.Config) ([]Issue, error)
	Get(context.Context, config.Config, int) (Issue, error)
	VerifyIssueAuthor(context.Context, config.Config, Issue) (AuthorVerification, error)
	Inspect(context.Context, config.Config, int, string) (RemoteState, error)
	Claim(context.Context, config.Config, Issue, string) error
	MarkNeedsInput(context.Context, config.Config, int, string, string) error
	MarkDone(context.Context, config.Config, int, string) error
	MarkFailed(context.Context, config.Config, int, string, bool) error
	MarkRunning(context.Context, config.Config, int) error
	ReconcileIssue(context.Context, config.Config, int, issuedomain.Status, bool) error
	MarkConflictRetry(context.Context, config.Config, int, string) error
	ReadyPullRequest(context.Context, config.Config, string) error
	UpdatePullRequest(context.Context, config.Config, string) error
	MergePullRequest(context.Context, config.Config, string) error
}

type CLI struct {
	Path    string
	Secrets []string
}

type rawIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"createdAt"`
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Comments []struct {
		Body string `json:"body"`
	} `json:"comments"`
	Author struct {
		Login string `json:"login"`
		IsBot bool   `json:"is_bot"`
	} `json:"author"`
}

func (c CLI) ListReady(ctx context.Context, cfg config.Config) ([]Issue, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	// gh paginates internally up to the requested limit. Keep this above the
	// MVP's original 100 so large queues are not silently truncated.
	args := []string{"issue", "list", "--repo", cfg.GitHub.Repo, "--state", "open", "--limit", "1000", "--json", "number,title,body,url,createdAt,state,stateReason,labels,assignees,milestone,author"}
	// GitHub's AND label filtering matches Eligible's requirement that all
	// ready labels be present, avoiding GraphQL node cost for unrelated Issues.
	for _, label := range cfg.GitHub.ReadyLabels {
		args = append(args, "--label", label)
	}
	if cfg.GitHub.Assignee != "" {
		args = append(args, "--assignee", cfg.GitHub.Assignee)
	}
	if cfg.GitHub.Milestone != "" {
		args = append(args, "--milestone", cfg.GitHub.Milestone)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return nil, c.commandError(ctx, path, "list GitHub Issues", err, out)
	}
	var raw []rawIssue
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode GitHub Issues: %w", err)
	}
	issues := make([]Issue, 0, len(raw))
	for _, item := range raw {
		labels := make([]string, 0, len(item.Labels))
		for _, label := range item.Labels {
			labels = append(labels, label.Name)
		}
		if !Eligible(labels, cfg.GitHub) {
			continue
		}
		assignees := make([]string, 0, len(item.Assignees))
		for _, assignee := range item.Assignees {
			assignees = append(assignees, assignee.Login)
		}
		milestone := ""
		if item.Milestone != nil {
			milestone = item.Milestone.Title
		}
		issues = append(issues, NormalizeIssue(Issue{
			Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL, CreatedAt: item.CreatedAt,
			State: item.State, StateReason: item.StateReason, Labels: labels, Assignees: assignees, Milestone: milestone, AuthorLogin: item.Author.Login,
			AuthorType: authorType(item.Author.IsBot),
		}))
	}
	OrderIssues(issues, cfg.Queue)
	return issues, nil
}

func (c CLI) Get(ctx context.Context, cfg config.Config, number int) (Issue, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "issue", "view", fmt.Sprint(number), "--repo", cfg.GitHub.Repo, "--json", "number,title,body,url,createdAt,state,stateReason,labels,assignees,milestone,comments,author").CombinedOutput()
	if err != nil {
		return Issue{}, c.commandError(ctx, path, fmt.Sprintf("get GitHub Issue #%d", number), err, out)
	}
	var item rawIssue
	if err := json.Unmarshal(out, &item); err != nil {
		return Issue{}, fmt.Errorf("decode GitHub Issue #%d: %w", number, err)
	}
	labels := make([]string, 0, len(item.Labels))
	for _, label := range item.Labels {
		labels = append(labels, label.Name)
	}
	assignees := make([]string, 0, len(item.Assignees))
	for _, assignee := range item.Assignees {
		assignees = append(assignees, assignee.Login)
	}
	milestone := ""
	if item.Milestone != nil {
		milestone = item.Milestone.Title
	}
	comments := make([]string, 0, len(item.Comments))
	for _, comment := range item.Comments {
		comments = append(comments, comment.Body)
	}
	return NormalizeIssue(Issue{Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL, CreatedAt: item.CreatedAt, Labels: labels, Assignees: assignees, Milestone: milestone, Comments: comments, State: item.State, StateReason: item.StateReason, AuthorLogin: item.Author.Login, AuthorType: authorType(item.Author.IsBot)}), nil
}

func authorType(bot bool) string {
	if bot {
		return "Bot"
	}
	return "User"
}

func (c CLI) Inspect(ctx context.Context, cfg config.Config, number int, branch string) (RemoteState, error) {
	issue, err := c.Get(ctx, cfg, number)
	if err != nil {
		return RemoteState{}, err
	}
	state := RemoteState{Issue: issue}
	if branch == "" {
		return state, nil
	}
	path := c.Path
	if path == "" {
		path = "gh"
	}
	// Reconciliation only distinguishes zero, one, or multiple Pull Requests.
	// Fetching two is sufficient to detect the unsafe multiple-PR case and
	// avoids requesting 100 expensive statusCheckRollup nodes every poll.
	out, err := exec.CommandContext(ctx, path, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "all", "--head", branch, "--limit", "2", "--json", "number,url,state,isDraft,mergedAt,headRefName,baseRefName,headRefOid,mergeCommit,headRepository,headRepositoryOwner,mergeStateStatus,reviewDecision,statusCheckRollup").CombinedOutput()
	if err != nil {
		return RemoteState{}, c.commandError(ctx, path, fmt.Sprintf("inspect Pull Requests for branch %s", branch), err, out)
	}
	var raw []struct {
		Number      int        `json:"number"`
		URL         string     `json:"url"`
		State       string     `json:"state"`
		IsDraft     bool       `json:"isDraft"`
		MergedAt    *time.Time `json:"mergedAt"`
		HeadRefName string     `json:"headRefName"`
		BaseRefName string     `json:"baseRefName"`
		HeadRefOID  string     `json:"headRefOid"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		PullRequestHeadRepository
		MergeStateStatus  string        `json:"mergeStateStatus"`
		ReviewDecision    string        `json:"reviewDecision"`
		StatusCheckRollup []checkRollup `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return RemoteState{}, fmt.Errorf("decode Pull Requests for branch %s: %w", branch, err)
	}
	for _, item := range raw {
		mergeCommitSHA := ""
		if item.MergeCommit != nil {
			mergeCommitSHA = item.MergeCommit.OID
		}
		state.PullRequests = append(state.PullRequests, PullRequest{
			Number: item.Number, URL: item.URL, State: item.State, IsDraft: item.IsDraft,
			MergedAt: item.MergedAt, HeadRefName: item.HeadRefName,
			BaseRefName:      item.BaseRefName,
			HeadSHA:          item.HeadRefOID,
			MergeStateStatus: item.MergeStateStatus,
			ChecksStatus:     pullRequestChecksStatus(item.MergeStateStatus, item.StatusCheckRollup),
			ReviewDecision:   item.ReviewDecision,
			MergeSHA:         mergeCommitSHA,
			MergeCommitSHA:   mergeCommitSHA,
			HeadRepository:   item.PullRequestHeadRepository.FullName(),
		})
	}
	sort.Slice(state.PullRequests, func(i, j int) bool { return state.PullRequests[i].Number > state.PullRequests[j].Number })
	return state, nil
}

// ListMergedPullRequests returns repository-local merged Pull Requests without
// status checks. Recovery callers use the complete identity set to validate
// historical terminal records in one bounded GitHub query.
func (c CLI) ListMergedPullRequests(ctx context.Context, cfg config.Config) ([]PullRequest, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "merged", "--limit", "1000", "--json", "number,url,state,mergedAt,headRefName,baseRefName,headRefOid,mergeCommit,headRepository,headRepositoryOwner").CombinedOutput()
	if err != nil {
		return nil, c.commandError(ctx, path, "list merged Pull Requests", err, out)
	}
	var raw []struct {
		Number      int        `json:"number"`
		URL         string     `json:"url"`
		State       string     `json:"state"`
		MergedAt    *time.Time `json:"mergedAt"`
		HeadRefName string     `json:"headRefName"`
		BaseRefName string     `json:"baseRefName"`
		HeadRefOID  string     `json:"headRefOid"`
		MergeCommit *struct {
			OID string `json:"oid"`
		} `json:"mergeCommit"`
		PullRequestHeadRepository
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode merged Pull Requests: %w", err)
	}
	result := make([]PullRequest, 0, len(raw))
	for _, item := range raw {
		mergeSHA := ""
		if item.MergeCommit != nil {
			mergeSHA = item.MergeCommit.OID
		}
		result = append(result, PullRequest{
			Number: item.Number, URL: item.URL, State: item.State, MergedAt: item.MergedAt,
			HeadRefName: item.HeadRefName, BaseRefName: item.BaseRefName, HeadSHA: item.HeadRefOID,
			MergeSHA: mergeSHA, MergeCommitSHA: mergeSHA, HeadRepository: item.PullRequestHeadRepository.FullName(),
		})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Number < result[j].Number })
	return result, nil
}

func pullRequestChecksStatus(mergeState string, checks []checkRollup) string {
	if len(checks) == 0 {
		if strings.EqualFold(mergeState, "CLEAN") {
			return "success"
		}
		return "pending"
	}
	result := "success"
	for _, check := range checks {
		status := strings.ToUpper(check.Status)
		conclusion := strings.ToUpper(check.Conclusion)
		state := strings.ToUpper(check.State)
		if state != "" {
			switch state {
			case "SUCCESS", "EXPECTED":
			case "PENDING":
				result = "pending"
			default:
				return "failure"
			}
			continue
		}
		if status != "COMPLETED" {
			result = "pending"
			continue
		}
		switch conclusion {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
		default:
			return "failure"
		}
	}
	return result
}

func (c CLI) Claim(ctx context.Context, cfg config.Config, issue Issue, runID string) error {
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[labelKey(label)] = true
	}
	add := []string{}
	if !labels[labelKey(cfg.GitHub.RunningLabel)] {
		add = append(add, cfg.GitHub.RunningLabel)
	}
	remove := []string{}
	for _, label := range cfg.GitHub.ReadyLabels {
		if labels[labelKey(label)] {
			remove = append(remove, label)
		}
	}
	if len(add) > 0 || len(remove) > 0 {
		if err := c.editLabels(ctx, cfg.GitHub.Repo, issue.Number, add, remove); err != nil {
			return err
		}
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:claim:%s -->", runID)
	body := fmt.Sprintf("%s\nSupervisor picked up this issue. Preparing the workspace before starting the worker (run `%s`).", marker, runID)
	return c.ensureComment(ctx, cfg.GitHub.Repo, issue.Number, marker, body)
}

func (c CLI) MarkNeedsInput(ctx context.Context, cfg config.Config, number int, requestID, question string) error {
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.NeedsInputLabel}, []string{cfg.GitHub.RunningLabel}); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:request:%s -->", requestID)
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, marker+"\nInput required: "+redact.StringWithSecrets(question, c.Secrets))
}

func (c CLI) MarkDone(ctx context.Context, cfg config.Config, number int, prURL string) error {
	remove := []string{cfg.GitHub.RunningLabel, cfg.GitHub.NeedsInputLabel, cfg.GitHub.FailedLabel}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			remove = append(remove, label)
		}
	}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.DoneLabel}, remove); err != nil {
		return err
	}
	marker := "<!-- codex-issue-loop:done -->"
	body := marker + "\nCompleted by `codex-issue-loop`."
	if prURL != "" {
		body += "\n\nPull request: " + prURL
	}
	if err := c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, body); err != nil {
		return err
	}
	if cfg.Completion.CloseIssue {
		path := c.Path
		if path == "" {
			path = "gh"
		}
		out, err := exec.CommandContext(ctx, path, "issue", "close", fmt.Sprint(number), "--repo", cfg.GitHub.Repo).CombinedOutput()
		if err != nil {
			return c.commandError(ctx, path, fmt.Sprintf("close Issue #%d", number), err, out)
		}
	}
	return nil
}

func (c CLI) MarkFailed(ctx context.Context, cfg config.Config, number int, reason string, blocked bool) error {
	label := cfg.GitHub.FailedLabel
	if blocked {
		for _, candidate := range cfg.GitHub.ExcludeLabels {
			if strings.EqualFold(candidate, "blocked") {
				label = candidate
				break
			}
		}
	}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{label}, []string{cfg.GitHub.RunningLabel}); err != nil {
		return err
	}
	baseMarker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", number)
	digest := sha256.Sum256([]byte(reason))
	idempotencyMarker := fmt.Sprintf("<!-- codex-issue-loop:failure:%x -->", digest[:8])
	body := baseMarker + "\n" + idempotencyMarker + "\nAutomation stopped: " + redact.StringWithSecrets(reason, c.Secrets)
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, idempotencyMarker, body)
}

func (c CLI) MarkRunning(ctx context.Context, cfg config.Config, number int) error {
	remove := []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			remove = append(remove, label)
		}
	}
	return c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, remove)
}

func (c CLI) MarkConflictRetry(ctx context.Context, cfg config.Config, number int, recoveryID string) error {
	remove := []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			remove = append(remove, label)
		}
	}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, remove); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:conflict-retry:%s -->", recoveryID)
	body := marker + "\nPull Request conflict recovery was explicitly resumed using durable state."
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, body)
}

func (c CLI) ReadyPullRequest(ctx context.Context, cfg config.Config, prURL string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "pr", "ready", prURL, "--repo", cfg.GitHub.Repo).CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, "mark Pull Request ready", err, out)
	}
	return nil
}

func (c CLI) UpdatePullRequest(ctx context.Context, cfg config.Config, prURL string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "pr", "update-branch", prURL, "--repo", cfg.GitHub.Repo).CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, "update Pull Request branch", err, out)
	}
	return nil
}

func (c CLI) MergePullRequest(ctx context.Context, cfg config.Config, prURL string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "pr", "merge", prURL, "--repo", cfg.GitHub.Repo, "--squash").CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, "merge Pull Request", err, out)
	}
	return nil
}

func (c CLI) editLabels(ctx context.Context, repo string, number int, add, remove []string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	args := []string{"issue", "edit", fmt.Sprint(number), "--repo", repo}
	for _, label := range add {
		if label != "" {
			args = append(args, "--add-label", label)
		}
	}
	for _, label := range remove {
		if label != "" {
			args = append(args, "--remove-label", label)
		}
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, fmt.Sprintf("update Issue #%d labels", number), err, out)
	}
	return nil
}

func (c CLI) ensureComment(ctx context.Context, repo string, number int, marker, body string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	view, err := exec.CommandContext(ctx, path, "issue", "view", fmt.Sprint(number), "--repo", repo, "--json", "comments", "--jq", ".comments[].body").CombinedOutput()
	if err == nil && strings.Contains(string(view), marker) {
		return nil
	}
	if err != nil {
		if _, _, _, limited := primaryRateLimit(view); limited {
			return c.commandError(ctx, path, fmt.Sprintf("inspect comments on Issue #%d", number), err, view)
		}
	}
	out, err := exec.CommandContext(ctx, path, "issue", "comment", fmt.Sprint(number), "--repo", repo, "--body", body).CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, fmt.Sprintf("comment on Issue #%d", number), err, out)
	}
	return nil
}

func (c CLI) safe(data []byte) string {
	return strings.TrimSpace(redact.StringWithSecrets(string(data), c.Secrets))
}

var OrderIssues = githubqueue.OrderIssues

const maxIssueTitleBytes = githubqueue.MaxIssueTitleBytes

const maxIssueBodyBytes = githubqueue.MaxIssueBodyBytes

const maxIssueComments = githubqueue.MaxIssueComments

const maxCommentBytes = githubqueue.MaxCommentBytes

var Eligible = githubqueue.Eligible

var EligibleIssue = githubqueue.EligibleIssue

var NormalizeIssue = githubqueue.NormalizeIssue

var safeText = githubqueue.SafeText

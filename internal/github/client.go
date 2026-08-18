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
	"unicode/utf8"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
)

type Issue struct {
	Number    int
	Title     string
	Body      string
	URL       string
	CreatedAt time.Time
	Labels    []string
	Assignees []string
	Milestone string
	Comments  []string
	State     string
}

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
	Inspect(context.Context, config.Config, int, string) (RemoteState, error)
	Claim(context.Context, config.Config, Issue, string) error
	MarkNeedsInput(context.Context, config.Config, int, string, string) error
	MarkDone(context.Context, config.Config, int, string) error
	MarkFailed(context.Context, config.Config, int, string, bool) error
	MarkRunning(context.Context, config.Config, int) error
	MarkConflictRetry(context.Context, config.Config, int, string) error
	MarkPullRequestChecksRecovery(context.Context, config.Config, int, string) error
	ReadyPullRequest(context.Context, config.Config, string) error
	UpdatePullRequest(context.Context, config.Config, string) error
	MergePullRequest(context.Context, config.Config, string) error
}

type CLI struct {
	Path    string
	Secrets []string
}

const (
	maxIssueTitleBytes = 512
	maxIssueBodyBytes  = 64 * 1024
	maxIssueComments   = 20
	maxCommentBytes    = 8 * 1024
)

type rawIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	State     string    `json:"state"`
	Labels    []struct {
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
}

func (c CLI) ListReady(ctx context.Context, cfg config.Config) ([]Issue, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	// gh paginates internally up to the requested limit. Keep this above the
	// MVP's original 100 so large queues are not silently truncated.
	args := []string{"issue", "list", "--repo", cfg.GitHub.Repo, "--state", "open", "--limit", "1000", "--json", "number,title,body,url,createdAt,labels,assignees,milestone"}
	// A single configured ready label can be pushed into GitHub's query
	// without changing the local has-any-label eligibility contract. This
	// avoids paying GraphQL node cost for every unrelated open Issue on each
	// reconciliation poll. Multiple ready labels retain local OR filtering.
	if len(cfg.GitHub.ReadyLabels) == 1 {
		args = append(args, "--label", cfg.GitHub.ReadyLabels[0])
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
			Labels: labels, Assignees: assignees, Milestone: milestone,
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
	out, err := exec.CommandContext(ctx, path, "issue", "view", fmt.Sprint(number), "--repo", cfg.GitHub.Repo, "--json", "number,title,body,url,createdAt,state,labels,assignees,milestone,comments").CombinedOutput()
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
	return NormalizeIssue(Issue{Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL, CreatedAt: item.CreatedAt, Labels: labels, Assignees: assignees, Milestone: milestone, Comments: comments, State: item.State}), nil
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
	out, err := exec.CommandContext(ctx, path, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "all", "--head", branch, "--limit", "2", "--json", "number,url,state,isDraft,mergedAt,headRefName,baseRefName,headRefOid,mergeCommit,headRepository,headRepositoryOwner,mergeStateStatus,statusCheckRollup").CombinedOutput()
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
			MergeSHA:         mergeCommitSHA,
			MergeCommitSHA:   mergeCommitSHA,
			HeadRepository:   item.PullRequestHeadRepository.FullName(),
		})
	}
	sort.Slice(state.PullRequests, func(i, j int) bool { return state.PullRequests[i].Number > state.PullRequests[j].Number })
	return state, nil
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

func Eligible(labels []string, cfg config.GitHub) bool {
	set := map[string]bool{}
	for _, label := range labels {
		set[label] = true
	}
	for _, label := range cfg.ReadyLabels {
		if !set[label] {
			return false
		}
	}
	for _, label := range cfg.ExcludeLabels {
		if set[label] {
			return false
		}
	}
	for _, label := range []string{cfg.RunningLabel, cfg.NeedsInputLabel, cfg.FailedLabel, cfg.DoneLabel} {
		if label != "" && set[label] {
			return false
		}
	}
	return true
}

func EligibleIssue(issue Issue, cfg config.GitHub) bool {
	if !Eligible(issue.Labels, cfg) {
		return false
	}
	if cfg.Assignee != "" {
		matched := false
		for _, assignee := range issue.Assignees {
			matched = matched || strings.EqualFold(assignee, cfg.Assignee)
		}
		if !matched {
			return false
		}
	}
	return cfg.Milestone == "" || issue.Milestone == cfg.Milestone
}

func (c CLI) Claim(ctx context.Context, cfg config.Config, issue Issue, runID string) error {
	labels := map[string]bool{}
	for _, label := range issue.Labels {
		labels[label] = true
	}
	add := []string{}
	if !labels[cfg.GitHub.RunningLabel] {
		add = append(add, cfg.GitHub.RunningLabel)
	}
	remove := []string{}
	for _, label := range cfg.GitHub.ReadyLabels {
		if labels[label] {
			remove = append(remove, label)
		}
	}
	if len(add) > 0 || len(remove) > 0 {
		if err := c.editLabels(ctx, cfg.GitHub.Repo, issue.Number, add, remove); err != nil {
			return err
		}
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:claim:%s -->", runID)
	body := fmt.Sprintf("%s\nClaimed by `codex-issue-loop` (run `%s`).", marker, runID)
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
	return c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel})
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

// MarkEnvironmentResume removes only supervisor-owned terminal labels. Manual
// exclusions such as do-not-automate are never removed by this operation.
func (c CLI) MarkEnvironmentResume(ctx context.Context, cfg config.Config, number int, resumeID string) error {
	remove := []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	for _, label := range cfg.GitHub.ExcludeLabels {
		if strings.EqualFold(label, "blocked") {
			remove = append(remove, label)
		}
	}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, remove); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:environment-resume:%s -->", resumeID)
	body := marker + "\nEnvironment-blocked worker execution was explicitly resumed using the existing worktree and durable state."
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, body)
}

// MarkPublicationRecovery removes only non-exclusion supervisor state labels.
// In particular, a concurrently added blocked/do-not-automate label is never
// removed. The operation is idempotent through labels and the durable marker.
func (c CLI) MarkPublicationRecovery(ctx context.Context, cfg config.Config, number int, recoveryID string) error {
	remove := []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, remove); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:publication-recovery:%s -->", recoveryID)
	body := marker + "\nPre-publication failure recovery was explicitly resumed using the existing worktree and durable state."
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, body)
}

// MarkPullRequestChecksRecovery returns only supervisor-owned failed state to
// running. Manual/security exclusion labels are intentionally never removed.
func (c CLI) MarkPullRequestChecksRecovery(ctx context.Context, cfg config.Config, number int, recoveryID string) error {
	remove := []string{cfg.GitHub.NeedsInputLabel, cfg.GitHub.DoneLabel, cfg.GitHub.FailedLabel}
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, remove); err != nil {
		return err
	}
	marker := fmt.Sprintf("<!-- codex-issue-loop:checks-recovery:%s -->", recoveryID)
	body := marker + "\nExternally repaired Pull Request checks were explicitly returned to the existing lifecycle."
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

func NormalizeIssue(issue Issue) Issue {
	issue.Title = safeText(issue.Title, maxIssueTitleBytes)
	issue.Body = safeText(issue.Body, maxIssueBodyBytes)
	issue.URL = safeText(issue.URL, 2048)
	if len(issue.Comments) > maxIssueComments {
		issue.Comments = issue.Comments[len(issue.Comments)-maxIssueComments:]
	}
	for index := range issue.Comments {
		issue.Comments[index] = safeText(issue.Comments[index], maxCommentBytes)
	}
	return issue
}

func safeText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= ' ' && r != 0x7f {
			return r
		}
		return -1
	}, value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "\n[TRUNCATED]"
}

func OrderIssues(issues []Issue, queue config.Queue) {
	priorityRank := make(map[string]int, len(queue.PriorityLabels))
	for index, label := range queue.PriorityLabels {
		priorityRank[strings.ToLower(label)] = index
	}
	rank := func(issue Issue) int {
		result := len(priorityRank)
		for _, label := range issue.Labels {
			if candidate, ok := priorityRank[strings.ToLower(label)]; ok && candidate < result {
				result = candidate
			}
		}
		return result
	}
	createdBefore := func(left, right Issue) bool {
		if left.CreatedAt.IsZero() != right.CreatedAt.IsZero() {
			return !left.CreatedAt.IsZero()
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.Number < right.Number
	}
	sort.SliceStable(issues, func(i, j int) bool {
		switch queue.Order {
		case "created_at_asc":
			return createdBefore(issues[i], issues[j])
		case "priority_then_created_at":
			left, right := rank(issues[i]), rank(issues[j])
			if left != right {
				return left < right
			}
			return createdBefore(issues[i], issues[j])
		default:
			return issues[i].Number < issues[j].Number
		}
	})
}

func SelectReady(issues []Issue, snapshotIssues map[string]string, queue config.Queue) (Issue, bool) {
	candidates := make([]admission.Candidate, 0, len(issues))
	byNumber := make(map[int]Issue, len(issues))
	for _, issue := range issues {
		candidates = append(candidates, admission.Candidate{
			Number: issue.Number, CreatedAt: issue.CreatedAt, Labels: append([]string(nil), issue.Labels...), Body: issue.Body,
		})
		byNumber[issue.Number] = issue
	}
	ineligible := map[int]string{}
	for _, issue := range issues {
		status := snapshotIssues[fmt.Sprint(issue.Number)]
		if status == "running" || status == "claimed" || status == "needs_input" || status == "answer_claim_waiting" || status == "resume_pending" || status == "completed" || status == "blocked" || status == "resolving_conflict" {
			ineligible[issue.Number] = status
		}
	}
	concurrency := queue.Concurrency
	if concurrency < 1 {
		// Unit callers historically supplied only the ordering fields. Loaded
		// schema-v2 configurations are validated as concurrency 1.
		concurrency = 1
	}
	result, err := admission.Select(admission.Input{
		Settings:   admission.Settings{Concurrency: concurrency, MetadataVersion: 1, Legacy: true},
		Queue:      admission.Queue{Order: queue.Order, PriorityLabels: append([]string(nil), queue.PriorityLabels...)},
		Candidates: candidates,
		Ineligible: ineligible,
	})
	if err != nil || len(result.Selected) == 0 {
		return Issue{}, false
	}
	return byNumber[result.Selected[0].Candidate.Number], true
}

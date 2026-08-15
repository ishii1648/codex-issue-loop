package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type Issue struct {
	Number    int
	Title     string
	Body      string
	URL       string
	Labels    []string
	Assignees []string
	Milestone string
	Comments  []string
	State     string
}

type PullRequest struct {
	Number      int
	URL         string
	State       string
	IsDraft     bool
	MergedAt    *time.Time
	HeadRefName string
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
}

type CLI struct {
	Path string
}

type rawIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
	State  string `json:"state"`
	Labels []struct {
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
	args := []string{"issue", "list", "--repo", cfg.GitHub.Repo, "--state", "open", "--limit", "100", "--json", "number,title,body,url,labels,assignees,milestone"}
	if cfg.GitHub.Assignee != "" {
		args = append(args, "--assignee", cfg.GitHub.Assignee)
	}
	if cfg.GitHub.Milestone != "" {
		args = append(args, "--milestone", cfg.GitHub.Milestone)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("list GitHub Issues: %w: %s", err, strings.TrimSpace(string(out)))
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
		issues = append(issues, Issue{
			Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL,
			Labels: labels, Assignees: assignees, Milestone: milestone,
		})
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	return issues, nil
}

func (c CLI) Get(ctx context.Context, cfg config.Config, number int) (Issue, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "issue", "view", fmt.Sprint(number), "--repo", cfg.GitHub.Repo, "--json", "number,title,body,url,state,labels,assignees,milestone,comments").CombinedOutput()
	if err != nil {
		return Issue{}, fmt.Errorf("get GitHub Issue #%d: %w: %s", number, err, strings.TrimSpace(string(out)))
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
	return Issue{Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL, Labels: labels, Assignees: assignees, Milestone: milestone, Comments: comments, State: item.State}, nil
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
	out, err := exec.CommandContext(ctx, path, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "all", "--head", branch, "--limit", "100", "--json", "number,url,state,isDraft,mergedAt,headRefName").CombinedOutput()
	if err != nil {
		return RemoteState{}, fmt.Errorf("inspect Pull Requests for branch %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	var raw []struct {
		Number      int        `json:"number"`
		URL         string     `json:"url"`
		State       string     `json:"state"`
		IsDraft     bool       `json:"isDraft"`
		MergedAt    *time.Time `json:"mergedAt"`
		HeadRefName string     `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return RemoteState{}, fmt.Errorf("decode Pull Requests for branch %s: %w", branch, err)
	}
	for _, item := range raw {
		state.PullRequests = append(state.PullRequests, PullRequest{
			Number: item.Number, URL: item.URL, State: item.State, IsDraft: item.IsDraft,
			MergedAt: item.MergedAt, HeadRefName: item.HeadRefName,
		})
	}
	sort.Slice(state.PullRequests, func(i, j int) bool { return state.PullRequests[i].Number > state.PullRequests[j].Number })
	return state, nil
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
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, marker+"\nInput required: "+question)
}

func (c CLI) MarkDone(ctx context.Context, cfg config.Config, number int, prURL string) error {
	if err := c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.DoneLabel}, []string{cfg.GitHub.RunningLabel, cfg.GitHub.NeedsInputLabel}); err != nil {
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
			return fmt.Errorf("close Issue #%d: %w: %s", number, err, strings.TrimSpace(string(out)))
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
	marker := fmt.Sprintf("<!-- codex-issue-loop:failed:%d -->", number)
	return c.ensureComment(ctx, cfg.GitHub.Repo, number, marker, marker+"\nAutomation stopped: "+reason)
}

func (c CLI) MarkRunning(ctx context.Context, cfg config.Config, number int) error {
	return c.editLabels(ctx, cfg.GitHub.Repo, number, []string{cfg.GitHub.RunningLabel}, []string{cfg.GitHub.NeedsInputLabel})
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
		return fmt.Errorf("update Issue #%d labels: %w: %s", number, err, strings.TrimSpace(string(out)))
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
	out, err := exec.CommandContext(ctx, path, "issue", "comment", fmt.Sprint(number), "--repo", repo, "--body", body).CombinedOutput()
	if err != nil {
		return fmt.Errorf("comment on Issue #%d: %w: %s", number, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func SelectReady(issues []Issue, snapshotIssues map[string]string) (Issue, bool) {
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	for _, issue := range issues {
		status := snapshotIssues[fmt.Sprint(issue.Number)]
		if status == "running" || status == "claimed" || status == "needs_input" || status == "completed" || status == "blocked" {
			continue
		}
		return issue, true
	}
	return Issue{}, false
}

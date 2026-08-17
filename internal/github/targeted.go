package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type TargetedRESTClient interface {
	GetREST(context.Context, config.Config, int) (Issue, error)
	InspectPullRequestREST(context.Context, config.Config, int, int, string) (RemoteState, error)
}

type restIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
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
}

func (c CLI) GetREST(ctx context.Context, cfg config.Config, number int) (Issue, error) {
	var item restIssue
	if err := c.apiJSON(ctx, "/repos/"+cfg.GitHub.Repo+"/issues/"+fmt.Sprint(number), &item); err != nil {
		return Issue{}, fmt.Errorf("get GitHub Issue #%d with REST: %w", number, err)
	}
	return normalizeRESTIssue(item), nil
}

func normalizeRESTIssue(item restIssue) Issue {
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
	return NormalizeIssue(Issue{
		Number: item.Number, Title: item.Title, Body: item.Body, URL: item.HTMLURL,
		CreatedAt: item.CreatedAt, State: item.State, Labels: labels,
		Assignees: assignees, Milestone: milestone,
	})
}

func (c CLI) InspectPullRequestREST(ctx context.Context, cfg config.Config, issueNumber, prNumber int, knownSHA string) (RemoteState, error) {
	issue, err := c.GetREST(ctx, cfg, issueNumber)
	if err != nil {
		return RemoteState{}, err
	}
	var comments []struct {
		Body string `json:"body"`
	}
	if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", cfg.GitHub.Repo, issueNumber), &comments); err != nil {
		return RemoteState{}, fmt.Errorf("get GitHub Issue #%d comments with REST: %w", issueNumber, err)
	}
	for _, comment := range comments {
		issue.Comments = append(issue.Comments, comment.Body)
	}
	var raw struct {
		Number         int        `json:"number"`
		HTMLURL        string     `json:"html_url"`
		State          string     `json:"state"`
		Draft          bool       `json:"draft"`
		MergedAt       *time.Time `json:"merged_at"`
		MergeableState string     `json:"mergeable_state"`
		Head           struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/pulls/%d", cfg.GitHub.Repo, prNumber), &raw); err != nil {
		return RemoteState{}, fmt.Errorf("get Pull Request #%d with REST: %w", prNumber, err)
	}
	sha := raw.Head.SHA
	if sha == "" {
		sha = knownSHA
	}
	checksStatus := "pending"
	if sha != "" {
		var checks struct {
			CheckRuns []checkRollup `json:"check_runs"`
		}
		if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", cfg.GitHub.Repo, sha), &checks); err != nil {
			return RemoteState{}, fmt.Errorf("get check runs for %s with REST: %w", sha, err)
		}
		var statuses struct {
			State    string            `json:"state"`
			Statuses []json.RawMessage `json:"statuses"`
		}
		if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/commits/%s/status", cfg.GitHub.Repo, sha), &statuses); err != nil {
			return RemoteState{}, fmt.Errorf("get commit status for %s with REST: %w", sha, err)
		}
		checksStatus = pullRequestChecksStatus(raw.MergeableState, checks.CheckRuns)
		if len(statuses.Statuses) > 0 {
			if statuses.State == "failure" || statuses.State == "error" {
				checksStatus = "failure"
			} else if statuses.State == "pending" && checksStatus == "success" {
				checksStatus = "pending"
			}
		}
	}
	return RemoteState{Issue: issue, PullRequests: []PullRequest{{
		Number: raw.Number, URL: raw.HTMLURL, State: raw.State, IsDraft: raw.Draft,
		MergedAt: raw.MergedAt, HeadRefName: raw.Head.Ref, HeadSHA: sha,
		MergeStateStatus: raw.MergeableState, ChecksStatus: checksStatus,
	}}}, nil
}

func (c CLI) apiJSON(ctx context.Context, endpoint string, target any) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "api", "--method", "GET", "-H", "Accept: application/vnd.github+json", endpoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh api: %w: %s", err, c.safe(out))
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("decode gh api response: %w", err)
	}
	return nil
}

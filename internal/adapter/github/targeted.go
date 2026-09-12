package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

// TargetedRESTClient returns Issue comments with the same contract as Client.Get.
type TargetedRESTClient interface {
	GetREST(context.Context, config.Config, int) (Issue, error)
	InspectPullRequestREST(context.Context, config.Config, int, int, string) (RemoteState, error)
}

type restIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	State       string    `json:"state"`
	StateReason string    `json:"state_reason"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	Milestone *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	User struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (c CLI) GetREST(ctx context.Context, cfg config.Config, number int) (Issue, error) {
	var item restIssue
	if err := c.apiJSON(ctx, "/repos/"+cfg.GitHub.Repo+"/issues/"+fmt.Sprint(number), &item); err != nil {
		return Issue{}, fmt.Errorf("get GitHub Issue #%d with REST: %w", number, err)
	}
	comments, err := c.issueComments(ctx, cfg, number)
	if err != nil {
		return Issue{}, err
	}
	issue := normalizeRESTIssue(item)
	issue.Comments = comments
	return NormalizeIssue(issue), nil
}

func (c CLI) issueComments(ctx context.Context, cfg config.Config, number int) ([]string, error) {
	var comments []string
	for page := 1; ; page++ {
		var batch []struct {
			Body string `json:"body"`
		}
		endpoint := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", cfg.GitHub.Repo, number, page)
		if err := c.apiJSON(ctx, endpoint, &batch); err != nil {
			return nil, fmt.Errorf("get GitHub Issue #%d comments with REST: %w", number, err)
		}
		for _, comment := range batch {
			comments = append(comments, comment.Body)
		}
		if len(batch) < 100 {
			return comments, nil
		}
	}
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
		CreatedAt: item.CreatedAt, State: item.State, StateReason: item.StateReason, Labels: labels,
		Assignees: assignees, Milestone: milestone, AuthorLogin: item.User.Login, AuthorType: item.User.Type,
	})
}

func (c CLI) InspectPullRequestREST(ctx context.Context, cfg config.Config, issueNumber, prNumber int, knownSHA string) (RemoteState, error) {
	issue, err := c.GetREST(ctx, cfg, issueNumber)
	if err != nil {
		return RemoteState{}, err
	}
	var raw struct {
		Number             int               `json:"number"`
		HTMLURL            string            `json:"html_url"`
		State              string            `json:"state"`
		Draft              bool              `json:"draft"`
		MergedAt           *time.Time        `json:"merged_at"`
		MergeableState     string            `json:"mergeable_state"`
		MergeCommitSHA     string            `json:"merge_commit_sha"`
		RequestedReviewers []json.RawMessage `json:"requested_reviewers"`
		RequestedTeams     []json.RawMessage `json:"requested_teams"`
		Base               struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := c.apiJSON(ctx, fmt.Sprintf("/repos/%s/pulls/%d", cfg.GitHub.Repo, prNumber), &raw); err != nil {
		return RemoteState{}, fmt.Errorf("get Pull Request #%d with REST: %w", prNumber, err)
	}
	reviewDecision, err := c.pullRequestReviewDecisionREST(ctx, cfg, prNumber)
	if err != nil {
		return RemoteState{}, err
	}
	if reviewDecision != "CHANGES_REQUESTED" && (len(raw.RequestedReviewers) > 0 || len(raw.RequestedTeams) > 0) {
		reviewDecision = "REVIEW_REQUIRED"
	}
	mergeCommitSHA := ""
	// Before merge, REST exposes a synthetic test merge commit at merge_commit_sha.
	if raw.MergedAt != nil {
		mergeCommitSHA = raw.MergeCommitSHA
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
		ReviewDecision: reviewDecision, BaseRefName: raw.Base.Ref, HeadRepository: raw.Head.Repo.FullName,
		MergeSHA: mergeCommitSHA, MergeCommitSHA: mergeCommitSHA,
	}}}, nil
}

func (c CLI) pullRequestReviewDecisionREST(ctx context.Context, cfg config.Config, prNumber int) (string, error) {
	latest := make(map[int64]string)
	for page := 1; ; page++ {
		var reviews []struct {
			State string `json:"state"`
			User  struct {
				ID int64 `json:"id"`
			} `json:"user"`
		}
		endpoint := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=100&page=%d", cfg.GitHub.Repo, prNumber, page)
		if err := c.apiJSON(ctx, endpoint, &reviews); err != nil {
			return "", fmt.Errorf("get Pull Request #%d reviews with REST: %w", prNumber, err)
		}
		// Reviews are chronological; comments and pending reviews do not supersede a submitted decision.
		for _, review := range reviews {
			if review.User.ID == 0 {
				continue
			}
			switch review.State {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				latest[review.User.ID] = review.State
			}
		}
		if len(reviews) < 100 {
			break
		}
	}
	decision := ""
	for _, review := range latest {
		if review == "CHANGES_REQUESTED" {
			return "CHANGES_REQUESTED", nil
		}
		if review == "APPROVED" {
			decision = "APPROVED"
		}
	}
	return decision, nil
}

func (c CLI) apiJSON(ctx context.Context, endpoint string, target any) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "api", "--method", "GET", "-H", "Accept: application/vnd.github+json", endpoint).CombinedOutput()
	if err != nil {
		return c.commandError(ctx, path, "gh api", err, out)
	}
	if err := json.Unmarshal(out, target); err != nil {
		return fmt.Errorf("decode gh api response: %w", err)
	}
	return nil
}

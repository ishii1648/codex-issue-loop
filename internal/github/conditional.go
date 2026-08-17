package github

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

type ConditionalQueueResult struct {
	Issues        []Issue
	StatusCode    int
	ETag          string
	LastModified  string
	RateRemaining string
	RateReset     string
	NotModified   bool
	Page          int
	ItemCount     int
}

type ConditionalQueueClient interface {
	ListReadyConditional(context.Context, config.Config, string, string) (ConditionalQueueResult, error)
}

type PagedConditionalQueueClient interface {
	ListReadyConditionalPage(context.Context, config.Config, int, string, string) (ConditionalQueueResult, error)
}

// ListReadyConditional uses a stable concrete REST collection URL. Unlike the
// legacy gh issue list call, a warm 304 does not execute a GraphQL query.
func (c CLI) ListReadyConditional(ctx context.Context, cfg config.Config, etag, lastModified string) (ConditionalQueueResult, error) {
	return c.ListReadyConditionalPage(ctx, cfg, 1, etag, lastModified)
}

// ListReadyConditionalPage gives each stable REST page its own validator. The
// broker persists the page bodies and composes 304 and 200 responses, allowing
// queues up to the existing 1000-Issue limit without a GraphQL fallback.
func (c CLI) ListReadyConditionalPage(ctx context.Context, cfg config.Config, page int, etag, lastModified string) (ConditionalQueueResult, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	if page < 1 || page > 10 {
		return ConditionalQueueResult{}, fmt.Errorf("conditional GitHub Issue sweep page %d is outside 1..10", page)
	}
	query := url.Values{"state": {"open"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}, "sort": {"created"}, "direction": {"asc"}}
	if len(cfg.GitHub.ReadyLabels) > 0 {
		query.Set("labels", strings.Join(cfg.GitHub.ReadyLabels, ","))
	}
	endpoint := "/repos/" + cfg.GitHub.Repo + "/issues?" + query.Encode()
	args := []string{"api", "--include", "--method", "GET", "-H", "Accept: application/vnd.github+json"}
	if etag != "" {
		args = append(args, "-H", "If-None-Match: "+etag)
	}
	if lastModified != "" {
		args = append(args, "-H", "If-Modified-Since: "+lastModified)
	}
	args = append(args, endpoint)
	out, commandErr := exec.CommandContext(ctx, path, args...).CombinedOutput()
	result, body, parseErr := parseIncludedResponse(out)
	result.Page = page
	if parseErr != nil {
		if commandErr != nil {
			return ConditionalQueueResult{}, fmt.Errorf("conditional GitHub Issue sweep: %w: %s", commandErr, c.safe(out))
		}
		return ConditionalQueueResult{}, parseErr
	}
	if result.StatusCode == http.StatusNotModified {
		result.NotModified = true
		return result, nil
	}
	if commandErr != nil {
		return ConditionalQueueResult{}, fmt.Errorf("conditional GitHub Issue sweep: %w: status=%d", commandErr, result.StatusCode)
	}
	if result.StatusCode != http.StatusOK {
		return ConditionalQueueResult{}, fmt.Errorf("conditional GitHub Issue sweep returned HTTP %d", result.StatusCode)
	}
	var raw []struct {
		Number    int             `json:"number"`
		Title     string          `json:"title"`
		Body      string          `json:"body"`
		HTMLURL   string          `json:"html_url"`
		CreatedAt time.Time       `json:"created_at"`
		State     string          `json:"state"`
		Pull      json.RawMessage `json:"pull_request"`
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
	if err := json.Unmarshal(body, &raw); err != nil {
		return ConditionalQueueResult{}, fmt.Errorf("decode conditional GitHub Issue sweep: %w", err)
	}
	result.ItemCount = len(raw)
	for _, item := range raw {
		if len(item.Pull) != 0 && string(item.Pull) != "null" {
			continue
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
		issue := NormalizeIssue(Issue{
			Number: item.Number, Title: item.Title, Body: item.Body, URL: item.HTMLURL,
			CreatedAt: item.CreatedAt, State: item.State, Labels: labels,
			Assignees: assignees, Milestone: milestone,
		})
		if EligibleIssue(issue, cfg.GitHub) {
			result.Issues = append(result.Issues, issue)
		}
	}
	OrderIssues(result.Issues, cfg.Queue)
	return result, nil
}

func parseIncludedResponse(data []byte) (ConditionalQueueResult, []byte, error) {
	separator := []byte("\r\n\r\n")
	index := bytes.Index(data, separator)
	if index < 0 {
		separator = []byte("\n\n")
		index = bytes.Index(data, separator)
	}
	if index < 0 {
		return ConditionalQueueResult{}, nil, fmt.Errorf("GitHub REST response did not include headers")
	}
	headerBlock := data[:index]
	body := data[index+len(separator):]
	scanner := bufio.NewScanner(bytes.NewReader(headerBlock))
	result := ConditionalQueueResult{}
	if !scanner.Scan() {
		return result, nil, fmt.Errorf("GitHub REST response status is missing")
	}
	parts := strings.Fields(scanner.Text())
	if len(parts) < 2 {
		return result, nil, fmt.Errorf("invalid GitHub REST response status")
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return result, nil, fmt.Errorf("invalid GitHub REST status: %w", err)
	}
	result.StatusCode = status
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "etag":
			result.ETag = strings.TrimSpace(value)
		case "last-modified":
			result.LastModified = strings.TrimSpace(value)
		case "x-ratelimit-remaining":
			result.RateRemaining = strings.TrimSpace(value)
		case "x-ratelimit-reset":
			result.RateReset = strings.TrimSpace(value)
		}
	}
	return result, body, scanner.Err()
}

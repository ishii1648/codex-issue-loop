package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

// GetIssueMetadata reads the full Issue body and labels without comments or
// prompt-oriented truncation. The producer may write the body back, so using
// the worker-safe truncated representation here could lose user content.
func (c CLI) GetIssueMetadata(ctx context.Context, cfg config.Config, issueNumber int) (Issue, error) {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	out, err := exec.CommandContext(ctx, path, "issue", "view", fmt.Sprint(issueNumber), "--repo", cfg.GitHub.Repo, "--json", "number,title,body,url,createdAt,state,labels").CombinedOutput()
	if err != nil {
		return Issue{}, fmt.Errorf("get GitHub Issue #%d metadata: %w: %s", issueNumber, err, c.safe(out))
	}
	var item rawIssue
	if err := json.Unmarshal(out, &item); err != nil {
		return Issue{}, fmt.Errorf("decode GitHub Issue #%d metadata: %w", issueNumber, err)
	}
	labels := make([]string, 0, len(item.Labels))
	for _, label := range item.Labels {
		labels = append(labels, label.Name)
	}
	return Issue{
		Number: item.Number, Title: item.Title, Body: item.Body, URL: item.URL,
		CreatedAt: item.CreatedAt, State: item.State, Labels: labels,
	}, nil
}

// SetIssueBody sends the body on stdin so Issue text and any accidentally
// included sensitive content never appears in the process argument list.
func (c CLI) SetIssueBody(ctx context.Context, cfg config.Config, issueNumber int, body string) error {
	path := c.Path
	if path == "" {
		path = "gh"
	}
	command := exec.CommandContext(ctx, path, "issue", "edit", fmt.Sprint(issueNumber), "--repo", cfg.GitHub.Repo, "--body-file", "-")
	command.Stdin = bytes.NewBufferString(body)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("update GitHub Issue #%d body: %w: %s", issueNumber, err, c.safe(stderr.Bytes()))
	}
	return nil
}

func (c CLI) AddIssueLabels(ctx context.Context, cfg config.Config, issueNumber int, labels []string) error {
	return c.editIssueLabels(ctx, cfg, issueNumber, "--add-label", labels)
}

func (c CLI) RemoveIssueLabels(ctx context.Context, cfg config.Config, issueNumber int, labels []string) error {
	return c.editIssueLabels(ctx, cfg, issueNumber, "--remove-label", labels)
}

func (c CLI) editIssueLabels(ctx context.Context, cfg config.Config, issueNumber int, operation string, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	path := c.Path
	if path == "" {
		path = "gh"
	}
	args := []string{"issue", "edit", fmt.Sprint(issueNumber), "--repo", cfg.GitHub.Repo}
	for _, label := range labels {
		args = append(args, operation, label)
	}
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s GitHub Issue #%d labels: %w: %s", operation, issueNumber, err, c.safe(out))
	}
	return nil
}

package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type Manager struct {
	GitPath string
	GHPath  string
	Secrets []string
}

func (m Manager) Publish(ctx context.Context, cfg config.Config, issue gh.Issue, worktreePath, branch, summary string) (worker.GitResult, error) {
	if worktreePath == "" || branch == "" {
		return worker.GitResult{}, fmt.Errorf("publish requires a worktree and branch")
	}
	git := m.GitPath
	if git == "" {
		git = "git"
	}
	status, err := m.run(ctx, git, "-C", worktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return worker.GitResult{}, fmt.Errorf("inspect publish changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		if _, err := m.run(ctx, git, "-C", worktreePath, "add", "--all"); err != nil {
			return worker.GitResult{}, fmt.Errorf("stage publish changes: %w", err)
		}
		if _, err := m.run(ctx, git, "-C", worktreePath, "diff", "--cached", "--check"); err != nil {
			return worker.GitResult{}, fmt.Errorf("validate staged changes: %w", err)
		}
		if _, err := m.run(ctx, git, "-C", worktreePath, "commit", "-m", commitTitle(issue)); err != nil {
			return worker.GitResult{}, fmt.Errorf("commit publish changes: %w", err)
		}
	}
	commit, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return worker.GitResult{}, fmt.Errorf("resolve publish commit: %w", err)
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "push", "--set-upstream", "origin", branch); err != nil {
		return worker.GitResult{}, fmt.Errorf("push publish branch: %w", err)
	}

	result := worker.GitResult{Branch: branch, Commit: strings.TrimSpace(commit)}
	if !cfg.Completion.CreateDraftPR {
		return result, nil
	}
	prURL, err := m.openPullRequest(ctx, cfg, issue, branch, summary)
	if err != nil {
		return worker.GitResult{}, err
	}
	result.PullRequestURL = prURL
	return result, nil
}

func (m Manager) openPullRequest(ctx context.Context, cfg config.Config, issue gh.Issue, branch, summary string) (string, error) {
	ghPath := m.GHPath
	if ghPath == "" {
		ghPath = "gh"
	}
	out, err := m.run(ctx, ghPath, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "open", "--head", branch, "--limit", "2", "--json", "url")
	if err != nil {
		return "", fmt.Errorf("inspect publish Pull Request: %w", err)
	}
	var existing []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &existing); err != nil {
		return "", fmt.Errorf("decode publish Pull Requests: %w", err)
	}
	if len(existing) > 1 {
		return "", fmt.Errorf("multiple open Pull Requests exist for branch %s", branch)
	}
	if len(existing) == 1 {
		return validatePullRequestURL(existing[0].URL)
	}

	body := fmt.Sprintf("Automated implementation for #%d.\n\n%s", issue.Number, strings.TrimSpace(summary))
	body = redact.StringWithSecrets(body, m.Secrets)
	if len(body) > 4096 {
		body = body[:4096]
	}
	args := []string{"pr", "create", "--repo", cfg.GitHub.Repo, "--base", cfg.Git.BaseBranch, "--head", branch, "--title", issue.Title, "--body", body}
	if cfg.Completion.CreateDraftPR {
		args = append(args, "--draft")
	}
	out, err = m.run(ctx, ghPath, args...)
	if err != nil {
		return "", fmt.Errorf("create publish Pull Request: %w", err)
	}
	return validatePullRequestURL(strings.TrimSpace(out))
}

func (m Manager) run(ctx context.Context, path string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(redact.StringWithSecrets(string(out), m.Secrets)))
	}
	return string(out), nil
}

func commitTitle(issue gh.Issue) string {
	title := strings.Join(strings.Fields(issue.Title), " ")
	if len(title) > 120 {
		title = title[:120]
	}
	return fmt.Sprintf("Implement #%d: %s", issue.Number, title)
}

func validatePullRequestURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || !strings.Contains(parsed.Path, "/pull/") {
		return "", fmt.Errorf("invalid Pull Request URL %q", value)
	}
	return parsed.String(), nil
}

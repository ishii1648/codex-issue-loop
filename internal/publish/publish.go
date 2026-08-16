package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"sort"
	"strings"

	"github.com/ishii1648/codex-issue-loop/internal/admission"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type Manager struct {
	GitPath string
	GHPath  string
	Secrets []string
}

func (m Manager) Publish(ctx context.Context, cfg config.Config, issue gh.Issue, worktreePath, branch, summary, baseSHA string, declared []string) (worker.GitResult, publication.Audit, error) {
	audit := publication.Audit{BaseSHA: baseSHA, DeclaredResources: append([]string(nil), declared...)}
	if worktreePath == "" || branch == "" {
		return worker.GitResult{}, audit, fmt.Errorf("publish requires a worktree and branch")
	}
	git := m.GitPath
	if git == "" {
		git = "git"
	}
	actualBranch, err := m.run(ctx, git, "-C", worktreePath, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(actualBranch) != branch {
		return worker.GitResult{}, audit, fmt.Errorf("publish worktree branch mismatch: saved=%s actual=%s", branch, strings.TrimSpace(actualBranch))
	}
	paths, err := m.changedPaths(ctx, git, worktreePath, baseSHA)
	if err != nil {
		return worker.GitResult{}, audit, err
	}
	audit.ChangedPaths = paths
	actual, err := admission.ResourcesForPaths(cfg.AdmissionSettings(), paths)
	if err != nil {
		return worker.GitResult{}, audit, fmt.Errorf("map changed paths to resources: %w", err)
	}
	audit.ActualResources = actual
	if !admission.Covers(declared, actual) {
		audit.Reason = publication.ReasonResourceClaimMismatch
		return worker.GitResult{}, audit, publication.ClaimMismatchError{Declared: declared, Actual: actual}
	}
	status, err := m.run(ctx, git, "-C", worktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return worker.GitResult{}, audit, fmt.Errorf("inspect publish changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		if _, err := m.run(ctx, git, "-C", worktreePath, "add", "--all"); err != nil {
			return worker.GitResult{}, audit, fmt.Errorf("stage publish changes: %w", err)
		}
		if _, err := m.run(ctx, git, "-C", worktreePath, "diff", "--cached", "--check"); err != nil {
			return worker.GitResult{}, audit, fmt.Errorf("validate staged changes: %w", err)
		}
		if _, err := m.run(ctx, git, "-c", "commit.gpgsign=false", "-C", worktreePath, "commit", "-m", commitTitle(issue)); err != nil {
			return worker.GitResult{}, audit, fmt.Errorf("commit publish changes: %w", err)
		}
	}
	commit, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return worker.GitResult{}, audit, fmt.Errorf("resolve publish commit: %w", err)
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "push", "--set-upstream", "origin", branch); err != nil {
		return worker.GitResult{}, audit, fmt.Errorf("push publish branch: %w", err)
	}

	result := worker.GitResult{Branch: branch, Commit: strings.TrimSpace(commit)}
	if !cfg.Completion.CreateDraftPR {
		return result, audit, nil
	}
	prURL, err := m.openPullRequest(ctx, cfg, issue, branch, summary)
	if err != nil {
		return worker.GitResult{}, audit, err
	}
	result.PullRequestURL = prURL
	return result, audit, nil
}

func (m Manager) changedPaths(ctx context.Context, git, worktreePath, baseSHA string) ([]string, error) {
	if strings.TrimSpace(baseSHA) == "" {
		return nil, fmt.Errorf("inspect publish changes: durable base SHA is missing")
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "--verify", baseSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("inspect publish base %s: %w", baseSHA, err)
	}
	tracked, err := m.run(ctx, git, "-C", worktreePath, "diff", "--name-only", "-z", "--no-renames", baseSHA, "--")
	if err != nil {
		return nil, fmt.Errorf("list tracked publish changes: %w", err)
	}
	untracked, err := m.run(ctx, git, "-C", worktreePath, "ls-files", "--others", "--exclude-standard", "-z", "--")
	if err != nil {
		return nil, fmt.Errorf("list untracked publish changes: %w", err)
	}
	set := map[string]bool{}
	for _, output := range []string{tracked, untracked} {
		for _, path := range strings.Split(output, "\x00") {
			if path != "" {
				set[path] = true
			}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
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
	return extractPullRequestURL(out)
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

func extractPullRequestURL(output string) (string, error) {
	var found string
	for _, field := range strings.Fields(output) {
		candidate, err := validatePullRequestURL(field)
		if err != nil {
			continue
		}
		if found != "" && found != candidate {
			return "", fmt.Errorf("multiple Pull Request URLs in command output")
		}
		found = candidate
	}
	if found == "" {
		return "", fmt.Errorf("Pull Request URL not found in command output %q", strings.TrimSpace(output))
	}
	return found, nil
}

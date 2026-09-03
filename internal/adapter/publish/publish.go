package publish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/domain/admission"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

type Manager struct {
	GitPath   string
	GHPath    string
	GofmtPath string
	Secrets   []string
}

type pullRequest struct {
	URL         string  `json:"url"`
	State       string  `json:"state"`
	MergedAt    *string `json:"mergedAt"`
	BaseRefName string  `json:"baseRefName"`
	BaseRefOID  string  `json:"baseRefOid"`
	HeadRefName string  `json:"headRefName"`
	HeadRefOID  string  `json:"headRefOid"`
	gh.PullRequestHeadRepository
}

func (m Manager) validateExistingPullRequest(ctx context.Context, cfg config.Config, git, worktreePath, branch, savedURL, fallbackBase string) (string, *pullRequest, error) {
	ghPath := m.GHPath
	if ghPath == "" {
		ghPath = "gh"
	}
	out, err := m.run(ctx, ghPath, "pr", "list", "--repo", cfg.GitHub.Repo, "--state", "all", "--head", branch, "--limit", "3", "--json", "url,state,mergedAt,baseRefName,baseRefOid,headRefName,headRefOid,headRepository,headRepositoryOwner")
	if err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "inspect existing Pull Request: " + err.Error()}
	}
	var matches []pullRequest
	if err := json.Unmarshal([]byte(out), &matches); err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "decode existing Pull Requests: " + err.Error()}
	}
	if len(matches) > 1 {
		return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("multiple Pull Requests exist for branch %s", branch)}
	}
	if len(matches) == 0 {
		if savedURL != "" {
			return "", nil, publication.PullRequestMismatchError{Detail: "saved Pull Request was not found for branch " + branch}
		}
		return fallbackBase, nil, nil
	}
	pr := &matches[0]
	validatedURL, err := validatePullRequestURL(pr.URL)
	if err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: err.Error()}
	}
	pr.URL = validatedURL
	if savedURL != "" {
		validatedSaved, savedErr := validatePullRequestURL(savedURL)
		if savedErr != nil || validatedSaved != pr.URL {
			return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("saved Pull Request URL does not match branch: saved=%s actual=%s", savedURL, pr.URL)}
		}
	}
	if strings.EqualFold(pr.State, "closed") && pr.MergedAt == nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "Pull Request is closed without merge"}
	}
	if !strings.EqualFold(pr.State, "open") {
		return "", nil, publication.PullRequestMismatchError{Detail: "Pull Request is not open"}
	}
	headRepository := pr.PullRequestHeadRepository.FullName()
	if pr.HeadRefName != branch || pr.BaseRefName != cfg.Git.BaseBranch || !strings.EqualFold(headRepository, cfg.GitHub.Repo) {
		return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("Pull Request refs do not match: repository=%s head=%s base=%s", headRepository, pr.HeadRefName, pr.BaseRefName)}
	}
	if pr.BaseRefOID == "" || pr.HeadRefOID == "" {
		return "", nil, publication.PullRequestMismatchError{Detail: "Pull Request is missing authoritative base or head SHA"}
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "fetch", "--no-tags", "origin", cfg.Git.BaseBranch); err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "fetch authoritative Pull Request base: " + err.Error()}
	}
	remoteBase, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "refs/remotes/origin/"+cfg.Git.BaseBranch)
	if err != nil || strings.TrimSpace(remoteBase) != pr.BaseRefOID {
		return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("Pull Request base SHA changed during validation: pr=%s remote=%s", pr.BaseRefOID, strings.TrimSpace(remoteBase))}
	}
	for name, sha := range map[string]string{"base": pr.BaseRefOID, "head": pr.HeadRefOID} {
		if _, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "--verify", sha+"^{commit}"); err != nil {
			return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("Pull Request %s SHA is unavailable: %s", name, sha)}
		}
	}
	localHead, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "resolve worktree HEAD: " + err.Error()}
	}
	localHead = strings.TrimSpace(localHead)
	if strings.TrimSpace(fallbackBase) == "" {
		return "", nil, publication.PullRequestMismatchError{Detail: "durable publication base SHA is missing"}
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "--verify", fallbackBase+"^{commit}"); err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: "durable publication base SHA is unavailable"}
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "merge-base", "--is-ancestor", fallbackBase, localHead); err != nil {
		return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("durable publication base is not an ancestor of worktree HEAD: base=%s head=%s", fallbackBase, localHead)}
	}
	if localHead != pr.HeadRefOID {
		// A previous publisher attempt may have committed locally and failed
		// before push. Only that forward-only relationship is retryable.
		if _, err := m.run(ctx, git, "-C", worktreePath, "merge-base", "--is-ancestor", pr.HeadRefOID, localHead); err != nil {
			return "", nil, publication.PullRequestMismatchError{Detail: fmt.Sprintf("worktree HEAD diverges from Pull Request head: local=%s pr=%s", localHead, pr.HeadRefOID)}
		}
	}
	return pr.BaseRefOID, pr, nil
}

func (m Manager) formatGo(ctx context.Context, cfg config.Config, worktreePath string, changedPaths []string) (publication.FormatterAudit, error) {
	audit := publication.FormatterAudit{Name: "gofmt", Enabled: true, Result: "succeeded"}
	if m.GofmtPath == "" {
		audit.Result, audit.FailureCode = "failed", "executable_unavailable"
		return audit, publication.FormatterError{Code: audit.FailureCode, Detail: "registered gofmt path is missing"}
	}
	root, err := canonicalWorktree(worktreePath)
	if err != nil {
		audit.Result, audit.FailureCode = "failed", "worktree_unsafe"
		return audit, publication.FormatterError{Code: audit.FailureCode, Detail: err.Error()}
	}
	files := make([]string, 0, len(changedPaths))
	before := map[string][sha256.Size]byte{}
	for _, path := range changedPaths {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		absolute, exists, pathErr := safeRegularFile(root, path)
		if pathErr != nil {
			audit.Result, audit.FailureCode = "failed", "path_unsafe"
			return audit, publication.FormatterError{Code: audit.FailureCode, Detail: pathErr.Error()}
		}
		if !exists { // deleted path (including the source side of a rename)
			continue
		}
		digest, hashErr := fileDigest(absolute)
		if hashErr != nil {
			audit.Result, audit.FailureCode = "failed", "file_read_failed"
			return audit, publication.FormatterError{Code: audit.FailureCode, Detail: hashErr.Error()}
		}
		files = append(files, absolute)
		before[absolute] = digest
	}
	sort.Strings(files)
	audit.FileCount = len(files)
	if len(files) == 0 {
		return audit, nil
	}
	formatCtx, cancel := context.WithTimeout(ctx, cfg.Formatters.Go.Timeout.Duration)
	defer cancel()
	args := append([]string{"-w"}, files...)
	if _, err := m.run(formatCtx, m.GofmtPath, args...); err != nil {
		code := "exit_failure"
		if ctx.Err() != nil {
			code = "canceled"
		} else if formatCtx.Err() == context.DeadlineExceeded {
			code = "timeout"
		}
		audit.Result, audit.FailureCode = "failed", code
		return audit, publication.FormatterError{Code: code, Detail: boundedDetail(err.Error())}
	}
	verifyArgs := append([]string{"-l"}, files...)
	unformatted, err := m.run(formatCtx, m.GofmtPath, verifyArgs...)
	if err != nil || strings.TrimSpace(unformatted) != "" {
		code := "verification_failed"
		if ctx.Err() != nil {
			code = "canceled"
		} else if formatCtx.Err() == context.DeadlineExceeded {
			code = "timeout"
		}
		audit.Result, audit.FailureCode = "failed", code
		detail := "gofmt -l reported unformatted files"
		if err != nil {
			detail = boundedDetail(err.Error())
		}
		return audit, publication.FormatterError{Code: code, Detail: detail}
	}
	for _, path := range files {
		after, err := fileDigest(path)
		if err != nil {
			audit.Result, audit.FailureCode = "failed", "file_read_failed"
			return audit, publication.FormatterError{Code: audit.FailureCode, Detail: err.Error()}
		}
		if after != before[path] {
			audit.Changed = true
		}
	}
	return audit, nil
}

func canonicalWorktree(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("worktree root is not a directory: %s", resolved)
	}
	return filepath.Clean(resolved), nil
}

func safeRegularFile(root, gitPath string) (string, bool, error) {
	if gitPath == "" || filepath.IsAbs(gitPath) || filepath.Clean(gitPath) != gitPath || gitPath == "." || strings.ContainsRune(gitPath, '\x00') {
		return "", false, fmt.Errorf("unsafe formatter path %q", gitPath)
	}
	absolute := filepath.Join(root, filepath.FromSlash(gitPath))
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("formatter path escapes worktree %q", gitPath)
	}
	info, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect formatter path %q: %w", gitPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("formatter path is not a regular file %q", gitPath)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Nlink != 1 {
		return "", false, fmt.Errorf("formatter path has external hard links %q", gitPath)
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || resolved != absolute {
		return "", false, fmt.Errorf("formatter path traverses a symlink %q", gitPath)
	}
	return absolute, true, nil
}

func fileDigest(path string) ([sha256.Size]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return [sha256.Size]byte{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func boundedDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		return value[len(value)-500:]
	}
	return value
}

func (m Manager) Publish(ctx context.Context, cfg config.Config, issue gh.Issue, worktreePath, branch, savedPRURL, summary, baseSHA string, resources publication.ResourceScope) (worker.GitResult, publication.Audit, error) {
	audit := publication.Audit{
		BaseSHA: baseSHA, DeclaredResources: append([]string(nil), resources.Declared...),
		Formatter: publication.FormatterAudit{Name: "gofmt", Enabled: cfg.Formatters.Go.Enabled, Result: "disabled"},
	}
	if len(resources.Effective) == 0 {
		return worker.GitResult{}, audit, fmt.Errorf("publish requires at least one effective resource")
	}
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
	var existingPR *pullRequest
	if savedPRURL != "" {
		effectiveBase, pr, validateErr := m.validateExistingPullRequest(ctx, cfg, git, worktreePath, branch, savedPRURL, baseSHA)
		if validateErr != nil {
			audit.Reason = publication.ReasonPullRequestMismatch
			return worker.GitResult{}, audit, validateErr
		}
		baseSHA, audit.BaseSHA, existingPR = effectiveBase, effectiveBase, pr
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
	if !admission.Covers(resources.Effective, actual) {
		audit.Reason = publication.ReasonResourceClaimMismatch
		return worker.GitResult{}, audit, publication.ClaimMismatchError{Declared: resources.Declared, Effective: resources.Effective, Actual: actual}
	}
	if savedPRURL == "" && cfg.Completion.CreateDraftPR {
		effectiveBase, pr, validateErr := m.validateExistingPullRequest(ctx, cfg, git, worktreePath, branch, "", baseSHA)
		if validateErr != nil {
			audit.Reason = publication.ReasonPullRequestMismatch
			return worker.GitResult{}, audit, validateErr
		}
		existingPR = pr
		if pr != nil && effectiveBase != baseSHA {
			baseSHA, audit.BaseSHA = effectiveBase, effectiveBase
			paths, err = m.changedPaths(ctx, git, worktreePath, baseSHA)
			if err != nil {
				return worker.GitResult{}, audit, err
			}
			audit.ChangedPaths = paths
			actual, err = admission.ResourcesForPaths(cfg.AdmissionSettings(), paths)
			if err != nil {
				return worker.GitResult{}, audit, fmt.Errorf("map authoritative Pull Request paths to resources: %w", err)
			}
			audit.ActualResources = actual
			if !admission.Covers(resources.Effective, actual) {
				audit.Reason = publication.ReasonResourceClaimMismatch
				return worker.GitResult{}, audit, publication.ClaimMismatchError{Declared: resources.Declared, Effective: resources.Effective, Actual: actual}
			}
		}
	}
	if cfg.Formatters.Go.Enabled {
		formatterAudit, formatErr := m.formatGo(ctx, cfg, worktreePath, paths)
		audit.Formatter = formatterAudit
		if formatErr != nil {
			audit.Reason = publication.ReasonFormatterFailed
			return worker.GitResult{}, audit, formatErr
		}
		// gofmt may have changed files, but cannot introduce a new path. Re-run
		// Git's whitespace validation before any staging mutation.
		if _, err := m.run(ctx, git, "-C", worktreePath, "diff", "--check"); err != nil {
			audit.Reason = publication.ReasonFormatterFailed
			audit.Formatter.Result = "failed"
			audit.Formatter.FailureCode = "git_diff_check_failed"
			return worker.GitResult{}, audit, publication.FormatterError{Code: "git_diff_check_failed", Detail: err.Error()}
		}
	}
	if err := ctx.Err(); err != nil {
		return worker.GitResult{}, audit, err
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
	prURL := ""
	if existingPR != nil {
		prURL = existingPR.URL
	} else {
		prURL, err = m.openPullRequest(ctx, cfg, issue, branch, summary)
	}
	if err != nil {
		return worker.GitResult{}, audit, err
	}
	result.PullRequestURL = prURL
	return result, audit, nil
}

func (m Manager) changedPaths(ctx context.Context, git, worktreePath, baseSHA string) ([]string, error) {
	if strings.TrimSpace(baseSHA) == "" {
		return nil, publication.DurableBaseMissingError{}
	}
	if _, err := m.run(ctx, git, "-C", worktreePath, "rev-parse", "--verify", baseSHA+"^{commit}"); err != nil {
		return nil, fmt.Errorf("inspect publish base %s: %w", baseSHA, err)
	}
	tracked, err := m.run(ctx, git, "-C", worktreePath, "diff", "--name-only", "-z", "--no-renames", baseSHA, "--")
	if err != nil {
		return nil, fmt.Errorf("list tracked publish changes: %w", err)
	}
	status, err := m.run(ctx, git, "-C", worktreePath, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--")
	if err != nil {
		return nil, fmt.Errorf("list worktree publish changes: %w", err)
	}
	worktreePaths, err := parsePorcelainV1Z(status)
	if err != nil {
		return nil, fmt.Errorf("parse worktree publish changes: %w", err)
	}
	set := map[string]bool{}
	for _, path := range strings.Split(tracked, "\x00") {
		if path != "" {
			set[path] = true
		}
	}
	for _, path := range worktreePaths {
		set[path] = true
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

func parsePorcelainV1Z(output string) ([]string, error) {
	records := strings.Split(output, "\x00")
	paths := make([]string, 0, len(records))
	for index := 0; index < len(records); index++ {
		record := records[index]
		if record == "" {
			continue
		}
		if len(record) < 4 || record[2] != ' ' {
			return nil, fmt.Errorf("malformed git status record")
		}
		paths = append(paths, record[3:])
		if strings.ContainsAny(record[:2], "RC") {
			index++ // -z emits the rename/copy source as a second NUL record.
			if index >= len(records) || records[index] == "" {
				return nil, fmt.Errorf("malformed git rename status record")
			}
		}
	}
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

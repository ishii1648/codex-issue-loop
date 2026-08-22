package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

const githubTestCapabilityContract = "<!-- agent-loop:capabilities\nversion: 1\nprofile: standard\nnetwork: none\nbrowser_cdp: false\ndownload: false\nexternal_time_gate: false\n-->"

func TestEligibleRequiresReadyAndRejectsStateLabels(t *testing.T) {
	cfg := config.Defaults().GitHub
	if !Eligible([]string{"codex-loop:ready"}, cfg) {
		t.Fatal("ready Issue was rejected")
	}
	if Eligible([]string{"codex-loop:ready", "blocked"}, cfg) {
		t.Fatal("excluded Issue was accepted")
	}
	if Eligible([]string{"codex-loop:ready", "codex-loop:running"}, cfg) {
		t.Fatal("running Issue was accepted")
	}
}

func TestFaultCLIInspectReturnsIssueAndPullRequests(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	script := `#!/bin/sh
case "$1 $2" in
  "issue view")
    printf '%s\n' '{"number":7,"title":"Test","body":"Body","url":"https://example.test/issues/7","state":"OPEN","labels":[{"name":"codex-loop:running"}],"assignees":[],"milestone":null,"comments":[{"body":"claim"}]}'
    ;;
  "pr list")
    case " $* " in
      *" --limit 2 "*) ;;
      *) exit 3 ;;
    esac
    printf '%s\n' '[{"number":11,"url":"https://example.test/pull/11","state":"MERGED","isDraft":false,"mergedAt":"2026-08-18T00:00:00Z","headRefName":"codex/issue-7-test","baseRefName":"main","headRefOid":"abc123","mergeCommit":{"oid":"merge123"},"headRepository":{"name":"repo"},"headRepositoryOwner":{"login":"owner"},"mergeStateStatus":"CLEAN","statusCheckRollup":[{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}]}]'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	remote, err := (CLI{Path: fake}).Inspect(context.Background(), cfg, 7, "codex/issue-7-test")
	if err != nil {
		t.Fatal(err)
	}
	if remote.Issue.State != "OPEN" || len(remote.PullRequests) != 1 || remote.PullRequests[0].Number != 11 || remote.PullRequests[0].HeadRefName != "codex/issue-7-test" || remote.PullRequests[0].BaseRefName != "main" || remote.PullRequests[0].ChecksStatus != "success" || remote.PullRequests[0].MergeCommitSHA != "merge123" || remote.PullRequests[0].HeadRepository != "owner/repo" {
		t.Fatalf("remote=%+v", remote)
	}
}

func TestPullRequestLifecycleCommands(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", logPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CloseIssue = false
	client := CLI{Path: fake}
	prURL := "https://github.example/owner/repo/pull/11"
	if err := client.ReadyPullRequest(context.Background(), cfg, prURL); err != nil {
		t.Fatal(err)
	}
	if err := client.UpdatePullRequest(context.Background(), cfg, prURL); err != nil {
		t.Fatal(err)
	}
	if err := client.MergePullRequest(context.Background(), cfg, prURL); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "pr ready "+prURL+" --repo owner/repo") ||
		!strings.Contains(text, "pr update-branch "+prURL+" --repo owner/repo") ||
		!strings.Contains(text, "pr merge "+prURL+" --repo owner/repo --squash") {
		t.Fatalf("unexpected calls:\n%s", text)
	}
}

func TestMarkConflictRetryRemovesOnlyBlockedExclusionAndWritesIdempotencyMarker(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue edit") exit 0 ;;
  "issue view") printf '%%s\n' '' ;;
  "issue comment") exit 0 ;;
  *) exit 2 ;;
esac
`, logPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	if err := (CLI{Path: fake}).MarkConflictRetry(context.Background(), cfg, 7, "retry_1"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "--add-label codex-loop:running") || !strings.Contains(text, "--remove-label blocked") || strings.Contains(text, "--remove-label do-not-automate") || !strings.Contains(text, "codex-issue-loop:conflict-retry:retry_1") {
		t.Fatalf("unexpected calls:\n%s", text)
	}
}

func TestMarkPublicationRecoveryNeverRemovesExclusionLabels(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue edit"|"issue comment") exit 0 ;;
  "issue view") printf '%%s\n' '' ;;
  *) exit 2 ;;
esac
`, logPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	if err := (CLI{Path: fake}).MarkPublicationRecovery(context.Background(), cfg, 7, "publication_recovery_1"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "--remove-label codex-loop:failed") || strings.Contains(text, "--remove-label blocked") || strings.Contains(text, "--remove-label do-not-automate") || !strings.Contains(text, "codex-issue-loop:publication-recovery:publication_recovery_1") {
		t.Fatalf("unexpected publication recovery calls:\n%s", text)
	}
}

func TestMarkAnsweredWorkspaceRecoveryUsesDedicatedIdempotentMarker(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	commentedPath := filepath.Join(dir, "commented")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue edit") exit 0 ;;
  "issue view")
    if test -f %q; then
      printf '%%s\n' '<!-- codex-issue-loop:answered-workspace-recovery:answered_workspace_recovery_1 -->'
    fi
    ;;
  "issue comment") touch %q ;;
  *) exit 2 ;;
esac
`, logPath, commentedPath, commentedPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	client := CLI{Path: fake}
	for range 2 {
		if err := client.MarkAnsweredWorkspaceRecovery(context.Background(), cfg, 7, "answered_workspace_recovery_1"); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "--add-label codex-loop:running") || !strings.Contains(text, "--remove-label blocked") ||
		strings.Contains(text, "--remove-label do-not-automate") ||
		!strings.Contains(text, "codex-issue-loop:answered-workspace-recovery:answered_workspace_recovery_1") ||
		strings.Count(text, "issue comment") != 1 {
		t.Fatalf("unexpected answered workspace recovery calls:\n%s", text)
	}
}

func TestMarkPullRequestChecksRecoveryOnlyTransitionsSupervisorLabels(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue edit"|"issue comment") exit 0 ;;
  "issue view") printf '%%s\n' '' ;;
  *) exit 2 ;;
esac
`, logPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	if err := (CLI{Path: fake}).MarkPullRequestChecksRecovery(context.Background(), cfg, 7, "checks_recovery_1"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if !strings.Contains(text, "--add-label codex-loop:running") || !strings.Contains(text, "--remove-label codex-loop:failed") ||
		strings.Contains(text, "--remove-label blocked") || strings.Contains(text, "--remove-label do-not-automate") ||
		!strings.Contains(text, "codex-issue-loop:checks-recovery:checks_recovery_1") {
		t.Fatalf("unexpected checks recovery calls:\n%s", text)
	}
}

func TestPullRequestChecksStatus(t *testing.T) {
	tests := []struct {
		name       string
		mergeState string
		checks     []checkRollup
		want       string
	}{
		{name: "no checks on clean Pull Request", mergeState: "CLEAN", want: "success"},
		{name: "no checks on dirty Pull Request", mergeState: "DIRTY", want: "pending"},
		{name: "no checks before mergeability is known", mergeState: "UNKNOWN", want: "pending"},
		{name: "successful check run", checks: []checkRollup{{Status: "COMPLETED", Conclusion: "SUCCESS"}}, want: "success"},
		{name: "pending check run", checks: []checkRollup{{Status: "IN_PROGRESS"}}, want: "pending"},
		{name: "failed check run", checks: []checkRollup{{Status: "COMPLETED", Conclusion: "FAILURE"}}, want: "failure"},
		{name: "successful status context", checks: []checkRollup{{State: "SUCCESS"}}, want: "success"},
		{name: "pending status context", checks: []checkRollup{{State: "PENDING"}}, want: "pending"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := pullRequestChecksStatus(test.mergeState, test.checks); got != test.want {
				t.Fatalf("status=%q, want %q", got, test.want)
			}
		})
	}
}

func TestFaultPartialLabelCommentSyncCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	failedOnce := filepath.Join(dir, "failed-once")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
  "issue edit") exit 0 ;;
  "issue view") printf '%%s\n' '' ;;
  "issue comment")
    if [ ! -f %q ]; then
      touch %q
      exit 1
    fi
    exit 0
    ;;
  *) exit 2 ;;
esac
`, logPath, failedOnce, failedOnce)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CloseIssue = false
	client := CLI{Path: fake}
	if err := client.MarkDone(context.Background(), cfg, 7, "https://example.test/pull/1"); err == nil {
		t.Fatal("injected comment failure was not returned")
	}
	if err := client.MarkDone(context.Background(), cfg, 7, "https://example.test/pull/1"); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "issue edit") != 2 || strings.Count(string(calls), "issue comment") != 2 {
		t.Fatalf("unexpected calls:\n%s", calls)
	}
	for _, label := range []string{cfg.GitHub.FailedLabel, "blocked"} {
		if !strings.Contains(string(calls), "--remove-label "+label) {
			t.Fatalf("completion did not remove %q label:\n%s", label, calls)
		}
	}
	if strings.Contains(string(calls), "--remove-label do-not-automate") {
		t.Fatalf("completion removed manual exclusion label:\n%s", calls)
	}
}

func TestFaultGitHubAdapterRejectsMalformedResponse(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' '{broken'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	if _, err := (CLI{Path: fake}).ListReady(context.Background(), cfg); err == nil {
		t.Fatal("malformed GitHub response was accepted")
	}
}

func TestCLIPrimaryGraphQLRateLimitUsesRESTRateLimitReset(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	logPath := filepath.Join(dir, "calls.log")
	reset := time.Date(2026, 8, 17, 3, 0, 19, 0, time.UTC)
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
if [ "$1 $2" = "api /rate_limit" ]; then
  printf '%%s\n' '{"resources":{"graphql":{"reset":%d,"remaining":0}}}'
  exit 0
fi
printf '%%s\n' 'GraphQL: API rate limit already exceeded for user ID 7684738.' >&2
exit 1
`, logPath, reset.Unix())
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	_, err := (CLI{Path: fake}).ListReady(context.Background(), cfg)
	limited, ok := AsRateLimit(err)
	if !ok {
		t.Fatalf("error was not classified as primary rate limit: %v", err)
	}
	if limited.Resource != "graphql" || !limited.ResetAt.Equal(reset) || limited.Source != "rest-rate-limit" {
		t.Fatalf("rate limit=%+v", limited)
	}
	calls, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(calls), "issue list") != 1 || strings.Count(string(calls), "api /rate_limit") != 1 {
		t.Fatalf("unexpected calls:\n%s", calls)
	}
}

func TestCLIPrimaryGraphQLRateLimitUsesShortRetryWhenRESTHasRemaining(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	nextReset := time.Now().UTC().Add(time.Hour)
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "api /rate_limit" ]; then
  printf '%%s\n' '{"resources":{"graphql":{"reset":%d,"remaining":4930}}}'
  exit 0
fi
printf '%%s\n' 'GraphQL: API rate limit already exceeded for user ID 7684738.' >&2
exit 1
`, nextReset.Unix())
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	before := time.Now().UTC()
	_, err := (CLI{Path: fake}).ListReady(context.Background(), cfg)
	after := time.Now().UTC()
	limited, ok := AsRateLimit(err)
	if !ok {
		t.Fatalf("error was not classified as primary rate limit: %v", err)
	}
	if limited.Source != "rest-rate-limit-recovered" || limited.ResetAt.Before(before.Add(4*time.Second)) || limited.ResetAt.After(after.Add(6*time.Second)) {
		t.Fatalf("recovered rate limit=%+v before=%s after=%s", limited, before, after)
	}
}

func TestPrimaryRateLimitPrefersResponseResetHeader(t *testing.T) {
	reset := time.Date(2026, 8, 17, 4, 0, 0, 0, time.UTC)
	resource, got, source, ok := primaryRateLimit([]byte(fmt.Sprintf("GraphQL: rate limit exceeded\nx-ratelimit-reset: %d", reset.Unix())))
	if !ok || resource != "graphql" || !got.Equal(reset) || source != "x-ratelimit-reset" {
		t.Fatalf("resource=%q reset=%s source=%q ok=%v", resource, got, source, ok)
	}
}

func TestListReadyDoesNotTruncateQueuesOverOneHundredIssues(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	responsePath := filepath.Join(dir, "issues.json")
	argsPath := filepath.Join(dir, "args.txt")
	items := make([]map[string]any, 120)
	for index := range items {
		items[index] = map[string]any{
			"number": index + 1, "title": fmt.Sprintf("Issue %d", index+1), "body": "", "url": "https://example.test/issues",
			"labels": []map[string]string{{"name": "codex-loop:ready"}}, "assignees": []any{}, "milestone": nil,
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(responsePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\ncat %q\n", argsPath, responsePath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	issues, err := (CLI{Path: fake}).ListReady(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 120 || issues[0].Number != 1 || issues[119].Number != 120 {
		t.Fatalf("unexpected queue: len=%d first=%d last=%d", len(issues), issues[0].Number, issues[len(issues)-1].Number)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--limit 1000") || !strings.Contains(string(args), "createdAt") || !strings.Contains(string(args), "--label codex-loop:ready") {
		t.Fatalf("large queue limit missing: %s", args)
	}
}

func TestListReadyPreservesORFilteringForMultipleReadyLabels(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	argsPath := filepath.Join(dir, "args.txt")
	if err := os.WriteFile(fake, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" > %q\nprintf '%%s\\n' '[]'\n", argsPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.ReadyLabels = []string{"ready:a", "ready:b"}
	if _, err := (CLI{Path: fake}).ListReady(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(args), "--label") {
		t.Fatalf("multiple ready labels were changed from OR to GitHub CLI AND filtering: %s", args)
	}
}

func TestOrderIssuesSupportsCreatedAtAndPriorityWithStableTieBreaks(t *testing.T) {
	base := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	fixture := []Issue{
		{Number: 40, CreatedAt: base.Add(2 * time.Hour)},
		{Number: 30, CreatedAt: base, Labels: []string{"priority:low"}},
		{Number: 20, CreatedAt: base.Add(time.Hour), Labels: []string{"PRIORITY:HIGH", "priority:low"}},
		{Number: 10, CreatedAt: base.Add(time.Hour), Labels: []string{"priority:high"}},
		{Number: 50},
	}

	created := append([]Issue(nil), fixture...)
	OrderIssues(created, config.Queue{Order: "created_at_asc"})
	assertIssueNumbers(t, created, []int{30, 10, 20, 40, 50})

	priority := append([]Issue(nil), fixture...)
	OrderIssues(priority, config.Queue{Order: "priority_then_created_at", PriorityLabels: []string{"priority:high", "priority:low"}})
	assertIssueNumbers(t, priority, []int{10, 20, 30, 40, 50})

	numbers := append([]Issue(nil), fixture...)
	OrderIssues(numbers, config.Queue{Order: "issue_number_asc"})
	assertIssueNumbers(t, numbers, []int{10, 20, 30, 40, 50})
}

func TestListReadyOrdersAfterCollectingPaginatedFixture(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	responsePath := filepath.Join(dir, "issues.json")
	items := make([]map[string]any, 120)
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	for index := range items {
		number := 120 - index
		labels := []map[string]string{{"name": "codex-loop:ready"}}
		if number == 120 {
			labels = append(labels, map[string]string{"name": "priority:high"})
		}
		items[index] = map[string]any{
			"number": number, "title": fmt.Sprintf("Issue %d", number), "body": "", "url": "https://example.test/issues",
			"createdAt": base.Add(time.Duration(number) * time.Minute), "labels": labels, "assignees": []any{}, "milestone": nil,
		}
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(responsePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\ncat %q\n", responsePath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Queue.Order = "priority_then_created_at"
	cfg.Queue.PriorityLabels = []string{"priority:high"}
	issues, err := (CLI{Path: fake}).ListReady(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 120 || issues[0].Number != 120 || issues[1].Number != 1 || issues[119].Number != 119 {
		t.Fatalf("pagination fixture order is unstable: len=%d first=%d second=%d last=%d", len(issues), issues[0].Number, issues[1].Number, issues[len(issues)-1].Number)
	}
}

func assertIssueNumbers(t *testing.T, issues []Issue, want []int) {
	t.Helper()
	if len(issues) != len(want) {
		t.Fatalf("len=%d want=%d", len(issues), len(want))
	}
	for index := range want {
		if issues[index].Number != want[index] {
			t.Fatalf("index %d: got=%d want=%d", index, issues[index].Number, want[index])
		}
	}
}

func TestIssueInputIsBoundedAndControlCharactersAreRemoved(t *testing.T) {
	comments := make([]string, 25)
	for index := range comments {
		comments[index] = fmt.Sprintf("comment-%02d\x00", index) + strings.Repeat("x", maxCommentBytes)
	}
	issue := NormalizeIssue(Issue{Title: "bad\x00title" + strings.Repeat("t", maxIssueTitleBytes), Body: strings.Repeat("b", maxIssueBodyBytes+10), Comments: comments})
	if strings.ContainsRune(issue.Title, '\x00') || len(issue.Title) > maxIssueTitleBytes+len("\n[TRUNCATED]") {
		t.Fatalf("unsafe title length=%d value=%q", len(issue.Title), issue.Title)
	}
	if len(issue.Body) > maxIssueBodyBytes+len("\n[TRUNCATED]") || len(issue.Comments) != maxIssueComments || !strings.HasPrefix(issue.Comments[0], "comment-05") {
		t.Fatalf("input limits not enforced: body=%d comments=%d first=%q", len(issue.Body), len(issue.Comments), issue.Comments[0][:10])
	}
}

func TestGitHubCommentsAndErrorsRedactSecrets(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	calls := filepath.Join(dir, "calls")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nif [ \"$1 $2\" = \"issue view\" ]; then exit 0; fi\nif [ \"$1 $2\" = \"issue comment\" ]; then exit 0; fi\nprintf '%%s\\n' 'custom-secret-value ghp_abcdefghijklmnopqrstuvwxyz123456' >&2\nexit 1\n", calls)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := CLI{Path: fake, Secrets: []string{"custom-secret-value"}}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	if err := client.MarkFailed(context.Background(), cfg, 7, "custom-secret-value ghp_abcdefghijklmnopqrstuvwxyz123456", false); err == nil || strings.Contains(err.Error(), "custom-secret-value") || strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("unsafe error: %v", err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "custom-secret-value") || strings.Contains(string(data), "ghp_") {
		t.Fatalf("secret sent to GitHub command: %s", data)
	}
}

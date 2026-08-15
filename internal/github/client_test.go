package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

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
    printf '%s\n' '[{"number":11,"url":"https://example.test/pull/11","state":"OPEN","isDraft":true,"mergedAt":null,"headRefName":"codex/issue-7-test"}]'
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
	if remote.Issue.State != "OPEN" || len(remote.PullRequests) != 1 || remote.PullRequests[0].Number != 11 || remote.PullRequests[0].HeadRefName != "codex/issue-7-test" {
		t.Fatalf("remote=%+v", remote)
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

func TestSelectReadyIsDeterministic(t *testing.T) {
	issues := []Issue{{Number: 9}, {Number: 2}, {Number: 5}}
	selected, ok := SelectReady(issues, map[string]string{"2": "completed"})
	if !ok || selected.Number != 5 {
		t.Fatalf("selected=%+v ok=%v", selected, ok)
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

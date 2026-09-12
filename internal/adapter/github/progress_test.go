package github

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestProgressMarkerRedactionAndUnknownPost(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh")
	comments := filepath.Join(dir, "comments")
	posts := filepath.Join(dir, "posts")
	failedRead := filepath.Join(dir, "failed-read")
	source := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
 "issue view") if [ -f %q ]; then exit 1; fi; cat %q ;;
 "issue comment") printf 'post\n' >> %q; printf '%%s\n' "$7" >> %q; exit 1 ;;
 *) exit 2 ;;
esac
`, failedRead, comments, posts, comments)
	if err := os.WriteFile(script, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(comments, nil, 0600); err != nil {
		t.Fatal(err)
	}
	client := CLI{Path: script, Secrets: []string{"test-private-value"}}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	for _, key := range []string{"first", "first", "second"} {
		_ = client.CommentProgress(context.Background(), cfg, 1, key, "理由：test-private-value "+"ghp_"+strings.Repeat("a", 32))
	}
	data, _ := os.ReadFile(comments)
	if strings.Contains(string(data), "test-private-value") || strings.Contains(string(data), "ghp_") || !strings.Contains(string(data), "codex-issue-loop:progress:") {
		t.Fatalf("unsafe or missing comments: %s", data)
	}
	counts, _ := os.ReadFile(posts)
	if strings.Count(string(counts), "post") != 2 {
		t.Fatalf("posts=%s", counts)
	}
	if err := os.WriteFile(failedRead, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.CommentProgress(context.Background(), cfg, 1, "third", "確認できた事実"); err == nil {
		t.Fatal("failed inspection was ignored")
	}
	counts, _ = os.ReadFile(posts)
	if strings.Count(string(counts), "post") != 2 {
		t.Fatal("posted without successful duplicate inspection")
	}
}

func TestLifecycleProgressFailureDoesNotFailOperations(t *testing.T) {
	for _, action := range []string{"claim", "done", "failed", "conflict-retry"} {
		t.Run(action, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			calls := filepath.Join(dir, "calls")
			source := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
case "$1 $2" in
 "issue edit"|"issue close"|"issue view") exit 0 ;;
 "issue comment") exit 1 ;;
 *) exit 2 ;;
esac
`, calls)
			if err := os.WriteFile(script, []byte(source), 0700); err != nil {
				t.Fatal(err)
			}
			client := CLI{Path: script}
			cfg := config.Defaults()
			cfg.GitHub.Repo = "owner/repo"
			cfg.Completion.CloseIssue = true
			var err error
			switch action {
			case "claim":
				err = client.Claim(context.Background(), cfg, Issue{Number: 1}, "run_1")
			case "done":
				err = client.MarkDone(context.Background(), cfg, 1, "")
			case "failed":
				err = client.MarkFailed(context.Background(), cfg, 1, "publish failed", false)
			case "conflict-retry":
				err = client.MarkConflictRetry(context.Background(), cfg, 1, "recovery_1")
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := os.ReadFile(calls)
			if strings.Count(string(data), "issue comment") != 1 {
				t.Fatal(string(data))
			}
			if action == "done" && (!strings.Contains(string(data), "issue close") || strings.Contains(string(data), "マージを確認")) {
				t.Fatal(string(data))
			}
			if action == "claim" && !strings.Contains(string(data), "このIssueの処理を開始しました") {
				t.Fatal(string(data))
			}
			if action == "failed" && (!strings.Contains(string(data), "PR公開で自動処理を停止") || !strings.Contains(string(data), "agent-loop issue plan")) {
				t.Fatal(string(data))
			}
		})
	}
}

func TestInputRequestOmitsEmptyOptionalFields(t *testing.T) {
	body := renderInputRequest("marker", "legacy", inputRequestMarkerPayload{RequestID: "req_1", Question: "質問"})
	for _, unwanted := range []string{"**理由：**", "**推奨：**", "選択肢：", "次のIssue"} {
		if strings.Contains(body, unwanted) {
			t.Fatal(body)
		}
	}
	if !strings.Contains(body, "回答は選択肢のIDから選んでください") || !strings.Contains(body, "/agent-loop answer req_1 {回答}") {
		t.Fatal(body)
	}
}

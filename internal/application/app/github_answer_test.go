package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func TestGitHubAnswerWithoutLocalLayout(t *testing.T) {
	dir := t.TempDir()
	unavailable := filepath.Join(dir, "not-directory")
	if err := os.WriteFile(unavailable, []byte("unavailable"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_HOME", unavailable)
	created := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(map[string]any{"version": 1, "request_id": "req_1", "issue_number": 1, "question": "Choose?", "options": nil, "allow_free_text": true, "created_at": created})
	marker := "<!-- codex-issue-loop:input-request:v1 request=req_1 payload=" + base64.RawURLEncoding.EncodeToString(payload) + " digest=" + state.BodyDigest(string(payload)) + " -->"
	body := marker + "\n<!-- codex-issue-loop:request:req_1 -->\n## Input required\n\nIssue: #1\nRequest: `req_1`\n\nChoose?\n\nFree-text allowed: `true`\nCreated: `2026-09-07T00:00:00Z`\n\nReply with exactly:\n```text\n/agent-loop answer req_1 <answer>\n```"
	comments, _ := json.Marshal([]any{[]any{map[string]any{"id": 1, "body": body, "created_at": created, "updated_at": created, "user": map[string]any{"id": 7, "login": "loop", "type": "User"}}}})
	commentsPath, postedPath := filepath.Join(dir, "comments"), filepath.Join(dir, "posted")
	if err := os.WriteFile(commentsPath, comments, 0600); err != nil {
		t.Fatal(err)
	}
	fake := fmt.Sprintf(`#!/bin/sh
case "$*" in
 *"--method POST"*) cat > %q; printf '%%s' '{"id":42}' ;;
 *"/user"*) printf '%%s' '{"id":7,"login":"loop","type":"User"}' ;;
 *"/comments?per_page=100"*) cat %q ;;
 *"/repos/owner/repo/issues/1"*) printf '%%s' '{"number":1,"state":"open"}' ;;
 *) exit 2 ;;
esac
`, postedPath, commentsPath)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	a := App{In: bytes.NewBufferString("ok\n"), Out: &out, Err: &errOut}
	args := []string{"answer", "--via", "github", "--repo", "owner/repo", "--issue", "1", "--request-id", "req_1", "--message-file", "-", "--json"}
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("code=%d: %s", code, errOut.String())
	}
	var result struct {
		Status    string `json:"status"`
		CommentID int64  `json:"comment_id"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Status != "submitted" || result.CommentID != 42 {
		t.Fatalf("%s", out.String())
	}
	data, err := os.ReadFile(postedPath)
	if err != nil || !bytes.Contains(data, []byte("/agent-loop answer req_1 ok")) {
		t.Fatalf("missing answer: %v", err)
	}
	if err := os.Remove(postedPath); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"githubx", "local"} {
		args[2] = invalid
		a.In = bytes.NewBufferString("ok")
		if code := a.Run(context.Background(), args); code == 0 {
			t.Fatal("invalid route accepted")
		}
	}
	args[2] = "github"
	args[9] = "--check"
	args = append(args[:10], "--json")
	if code := a.Run(context.Background(), args); code != 0 {
		t.Fatalf("check requires local layout: %s", errOut.String())
	}
	if _, err := os.Stat(postedPath); !os.IsNotExist(err) {
		t.Fatal("check posted answer")
	}
}

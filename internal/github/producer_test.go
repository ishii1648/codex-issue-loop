package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func TestProducerEditsKeepBodyOffProcessArguments(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	logPath := filepath.Join(dir, "calls")
	bodyPath := filepath.Join(dir, "body")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PRODUCER_CALL_LOG\"\nif [ \"$1 $2\" = 'issue edit' ] && printf '%s\\n' \"$*\" | grep -q -- '--body-file -'; then\n  cat > \"$PRODUCER_BODY_LOG\"\nfi\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRODUCER_CALL_LOG", logPath)
	t.Setenv("PRODUCER_BODY_LOG", bodyPath)
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	client := CLI{Path: bin}
	secretLikeBody := "Issue scope with token-value-that-must-not-be-an-argument"
	if err := client.SetIssueBody(context.Background(), cfg, 69, secretLikeBody); err != nil {
		t.Fatal(err)
	}
	if err := client.AddIssueLabels(context.Background(), cfg, 69, []string{"area:config", "codex-loop:ready"}); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), secretLikeBody) || !strings.Contains(string(calls), "--body-file -") || !strings.Contains(string(calls), "--add-label codex-loop:ready") {
		t.Fatalf("calls=%s", calls)
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil || string(body) != secretLikeBody {
		t.Fatalf("body=%q err=%v", body, err)
	}
}

func TestGetIssueMetadataPreservesFullBody(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "gh")
	responsePath := filepath.Join(dir, "response.json")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat \"$PRODUCER_RESPONSE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("界", 30000)
	response, err := json.Marshal(map[string]any{
		"number": 69, "title": "scope", "body": body, "state": "OPEN",
		"labels": []map[string]string{{"name": "area:config"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(responsePath, response, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRODUCER_RESPONSE", responsePath)
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	issue, err := (CLI{Path: bin}).GetIssueMetadata(context.Background(), cfg, 69)
	if err != nil || issue.Body != body || len(issue.Labels) != 1 {
		t.Fatalf("body bytes=%d want=%d labels=%v err=%v", len(issue.Body), len(body), issue.Labels, err)
	}
}

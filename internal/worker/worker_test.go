package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestResultValidation(t *testing.T) {
	valid := Result{Version: 1, Status: "completed", ExecutionProfile: "standard", Git: &GitResult{}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := Result{Version: 1, Status: "needs_input", ExecutionProfile: "extended"}
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected missing question error")
	}
}

func TestCodexRunParsesStructuredResultAndSession(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-codex")
	script := `#!/bin/sh
result=""
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then
    shift
    result="$1"
  fi
  shift
done
printf '%s\n' '{"type":"thread.started","thread_id":"session-123"}'
printf '%s\n' '{"version":1,"status":"completed","execution_profile":"standard","summary":"done","question":null,"tests":[],"git":{"branch":"codex/issue-1-test","commit":"abc","pull_request_url":"https://example.test/pr/1"},"retry":null}' > "$result"
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.RepoPath = dir
	cfg.Worker.Command = fake
	current := state.Issue{RunID: "run_1", Attempts: 1}
	result, err := (Codex{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1, Title: "Test"}, current, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "session-123" || result.Status != "completed" {
		t.Fatalf("result=%+v", result)
	}
}

func TestPromptDoesNotAskForProfileAndIncludesCompletion(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.Completion.CreateDraftPR = true
	prompt := BuildPrompt(cfg, gh.Issue{Number: 1, Title: "Test", Body: "Body"}, state.Issue{RunID: "run", Attempts: 1}, "")
	for _, expected := range []string{"Do not ask the user to choose the execution profile", "Create or update a draft pull request", "continue directly into implementation"} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("prompt missing %q", expected)
		}
	}
}

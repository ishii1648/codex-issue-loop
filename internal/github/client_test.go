package github

import (
	"context"
	"os"
	"path/filepath"
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

func TestCLIInspectReturnsIssueAndPullRequests(t *testing.T) {
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

func TestSelectReadyIsDeterministic(t *testing.T) {
	issues := []Issue{{Number: 9}, {Number: 2}, {Number: 5}}
	selected, ok := SelectReady(issues, map[string]string{"2": "completed"})
	if !ok || selected.Number != 5 {
		t.Fatalf("selected=%+v ok=%v", selected, ok)
	}
}

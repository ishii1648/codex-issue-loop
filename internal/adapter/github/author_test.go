package github

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestVerifyIssueAuthorTrustBoundary(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-gh")
	script := `#!/bin/sh
case "$*" in
  *"/repos/owner/repo/collaborators/writer/permission"*)
    printf '%s\n' '{"permission":"write","user":{"login":"writer"}}' ;;
  *"/repos/owner/repo/collaborators/reader/permission"*)
    printf '%s\n' '{"permission":"read","user":{"login":"reader"}}' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.TrustedIssueAuthors.AllowLogins = []string{"trusted-bot"}
	client := CLI{Path: fake}
	tests := []struct {
		name  string
		issue Issue
		want  bool
	}{
		{name: "owner", issue: Issue{AuthorLogin: "owner", AuthorType: "User"}, want: true},
		{name: "write collaborator", issue: Issue{AuthorLogin: "writer", AuthorType: "User"}, want: true},
		{name: "read collaborator", issue: Issue{AuthorLogin: "reader", AuthorType: "User"}},
		{name: "allowlisted bot", issue: Issue{AuthorLogin: "trusted-bot", AuthorType: "Bot"}, want: true},
		{name: "bot requires allowlist", issue: Issue{AuthorLogin: "other-bot", AuthorType: "Bot"}},
		{name: "missing author", issue: Issue{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := client.VerifyIssueAuthor(context.Background(), cfg, test.issue)
			if err != nil {
				t.Fatal(err)
			}
			if got.Trusted != test.want {
				t.Fatalf("verification=%+v want trusted=%v", got, test.want)
			}
		})
	}
}

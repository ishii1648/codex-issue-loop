package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestCanceledPullRequestCloseIsVerifiedAndIdempotent(t *testing.T) {
	for _, mode := range []string{"OPEN", "CLOSED", "MERGED", "FAIL_ONCE", "UNCONFIRMED"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CANCELED_PR_FIXTURE", root)
			t.Setenv("CANCELED_PR_MODE", mode)
			script := `#!/bin/sh
case "$1 $2" in
 "pr view")
  status="$CANCELED_PR_MODE"
  case "$status" in FAIL_ONCE|UNCONFIRMED) status=OPEN;; esac
  if [ -f "$CANCELED_PR_FIXTURE/closed" ]; then status=CLOSED; fi
  printf '{"number":42,"url":"https://github.com/owner/repo/pull/42","state":"%s"}\n' "$status";;
 "pr close")
  echo close >> "$CANCELED_PR_FIXTURE/calls"
  if [ "$CANCELED_PR_MODE" = FAIL_ONCE ] && [ ! -f "$CANCELED_PR_FIXTURE/tried" ]; then touch "$CANCELED_PR_FIXTURE/tried"; exit 1; fi
  if [ "$CANCELED_PR_MODE" != UNCONFIRMED ]; then touch "$CANCELED_PR_FIXTURE/closed"; fi;;
 *) exit 2;;
esac
`
			path := filepath.Join(root, "gh")
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			client := CLI{Path: path}
			cfg := config.Defaults()
			cfg.GitHub.Repo = "owner/repo"
			err := client.ClosePullRequest(context.Background(), cfg, "https://github.com/owner/repo/pull/42")
			if mode == "FAIL_ONCE" || mode == "UNCONFIRMED" {
				if err == nil {
					t.Fatal("unconfirmed closure accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "UNCONFIRMED" {
				return
			}
			if err := client.ClosePullRequest(context.Background(), cfg, "https://github.com/owner/repo/pull/42"); err != nil {
				t.Fatal(err)
			}
			if err := client.ClosePullRequest(context.Background(), cfg, "https://github.com/owner/repo/pull/42"); err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(filepath.Join(root, "calls"))
			want := 0
			if mode == "OPEN" {
				want = 1
			}
			if mode == "FAIL_ONCE" {
				want = 2
			}
			if strings.Count(string(calls), "close") != want {
				t.Fatalf("calls=%s", calls)
			}
		})
	}
}

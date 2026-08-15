package launchd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

func TestWritePlistUsesAbsoluteCommandsAndEscapesPaths(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, ReposRoot: filepath.Join(root, "repos"), LaunchAgents: filepath.Join(root, "launch")}
	entry := registry.Entry{RepoID: "repo-id", RepoPath: filepath.Join(root, "repo & work"), EnvironmentPath: "/custom/bin:/usr/bin"}
	if err := (Manager{Layout: l}).WritePlist(entry, "/absolute/agent-loop"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.PlistPath(entry.RepoID))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{"/absolute/agent-loop", "repo &amp; work", "/custom/bin:/usr/bin", "SuccessfulExit"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plist missing %q:\n%s", expected, text)
		}
	}
	for _, path := range []string{l.PlistPath(entry.RepoID), filepath.Join(l.RepoDir(entry.RepoID), "supervisor.log"), filepath.Join(l.RepoDir(entry.RepoID), "supervisor.err.log")} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe mode for %s: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestRestartWaitsForBootoutBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "loaded")
	if err := os.WriteFile(statePath, []byte("loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(root, "launchctl")
	script := `#!/bin/sh
case "$1" in
  print)
    test -f "$LAUNCHD_TEST_STATE"
    ;;
  bootout)
    (sleep 0.2; rm -f "$LAUNCHD_TEST_STATE") &
    ;;
  bootstrap)
    if test -f "$LAUNCHD_TEST_STATE"; then
      echo 'still loaded' >&2
      exit 1
    fi
    : > "$LAUNCHD_TEST_STATE"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAUNCHD_TEST_STATE", statePath)
	l := layout.Layout{Root: root, LaunchAgents: filepath.Join(root, "launch")}
	entry := registry.Entry{RepoID: "repo-id"}
	if err := (Manager{Layout: l, Launchctl: launchctl}).Restart(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("LaunchAgent was not loaded after restart: %v", err)
	}
}

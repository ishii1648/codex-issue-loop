package launchd

import (
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

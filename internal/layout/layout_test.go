package layout

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFaultLayoutUsesIsolatedRootsAndPrivateDirectories(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "app")
	skills := filepath.Join(root, "skills")
	launchAgents := filepath.Join(root, "launch-agents")
	t.Setenv("AGENT_LOOP_HOME", home)
	t.Setenv("AGENT_LOOP_SKILLS_DIR", skills)
	t.Setenv("AGENT_LOOP_LAUNCH_AGENTS_DIR", launchAgents)
	layout, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if layout.Root != home || layout.SkillsDir != skills || layout.LaunchAgents != launchAgents {
		t.Fatalf("layout=%+v", layout)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{layout.Root, layout.ReposRoot, layout.BinDir} {
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("path=%s mode=%v err=%v", path, info.Mode().Perm(), err)
		}
	}
	if got := layout.PlistPath("repo-id"); got != filepath.Join(launchAgents, "com.codex-issue-loop.repo-id.plist") {
		t.Fatalf("plist=%s", got)
	}
	if got := layout.NotificationTokenPath("repo-id"); got != filepath.Join(layout.RepoDir("repo-id"), "notification-token") {
		t.Fatalf("notification token=%s", got)
	}
}

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
)

func TestRegisterServiceWritesIndependentMonitorLaunchAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	state := filepath.Join(home, "monitor-state")
	if err := os.MkdirAll(filepath.Join(state, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "bin", "agent-loop-monitor"), []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, ".agent-loop-monitor.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nrepositories: [{name: owner/repo}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plist, err := registerService(config.Config{Path: configPath, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "com.codex-issue-loop.monitor") || !strings.Contains(text, "agent-loop-monitor</string><string>run") || strings.Contains(text, "agent-loop</string><string>run") {
		t.Fatalf("unexpected plist: %s", text)
	}
	if strings.Contains(text, "internal/application/supervisor") {
		t.Fatal("monitor LaunchAgent references supervisor")
	}
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSupportsMultipleIsolatedRepositories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.yaml")
	state := filepath.Join(t.TempDir(), "state")
	body := "version: 1\npoll_interval: 30s\nstate_dir: " + state + "\nrepositories:\n" +
		"  - name: ishii1648/codex-issue-loop\n    acceptance_timeout: 5m\n    processing_timeout: 1h\n" +
		"  - name: ishii1648/zeitreise\n    ready_labels: [queue:ready]\n    running_label: queue:running\n    terminal_labels: [queue:done]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 2 || cfg.Repositories[0].AcceptanceTimeout.Duration != 5*time.Minute || cfg.Repositories[1].ReadyLabels[0] != "queue:ready" {
		t.Fatalf("config = %+v", cfg)
	}
	if RepoID(cfg.Repositories[0].Name) == RepoID(cfg.Repositories[1].Name) {
		t.Fatal("repository state directories collide")
	}
}

func TestLoadRejectsUnknownFieldsAndRelativeState(t *testing.T) {
	for _, body := range []string{
		"version: 1\nstate_dir: relative\nrepositories: [{name: owner/repo}]\n",
		"version: 1\nunknown: true\nrepositories: [{name: owner/repo}]\n",
		"version: 1\nrepositories: [{name: owner/repo}, {name: OWNER/REPO}]\n",
	} {
		path := filepath.Join(t.TempDir(), "monitor.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid config accepted: %s", body)
		}
	}
}

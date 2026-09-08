package config

import (
	"os"
	"path/filepath"
	"strings"
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

func TestLoadRejectsInvalidLabels(t *testing.T) {
	for _, tt := range []struct {
		name   string
		labels string
		want   string
	}{
		{"empty exclude", `exclude_labels: [""]`, "labels must not be blank"},
		{"whitespace exclude", `exclude_labels: [blocked, " \t"]`, "labels must not be blank"},
		{"ready running", "ready_labels: [ready, shared]\n    running_label: shared", "overlap"},
		{"ready terminal", "ready_labels: [ready, shared]\n    terminal_labels: [done, shared]", "overlap"},
		{"running terminal", "running_label: shared\n    terminal_labels: [done, shared]", "overlap"},
		{"ready exclude", "ready_labels: [ready, shared]\n    exclude_labels: [blocked, shared]", "overlap"},
		{"running exclude", "running_label: shared\n    exclude_labels: [blocked, shared]", "overlap"},
		{"ready running case", "ready_labels: [READY]\n    running_label: ready", "overlap"},
		{"ready terminal case", "ready_labels: [READY]\n    terminal_labels: [ready]", "overlap"},
		{"running terminal case", "running_label: RUNNING\n    terminal_labels: [running]", "overlap"},
		{"ready exclude case", "ready_labels: [READY]\n    exclude_labels: [ready]", "overlap"},
		{"running exclude case", "running_label: RUNNING\n    exclude_labels: [running]", "overlap"},
		{"default ready", "running_label: CODEX-LOOP:READY", "overlap"},
		{"default running", "terminal_labels: [CODEX-LOOP:RUNNING]", "overlap"},
		{"default terminal", "ready_labels: [BLOCKED]", "overlap"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "monitor.yaml")
			body := "version: 1\nrepositories:\n  - name: owner/repo\n    " + tt.labels + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoadAllowsTerminalExcludeOverlap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "monitor.yaml")
	body := "version: 1\nrepositories:\n  - name: owner/repo\n    exclude_labels: [BLOCKED]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
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

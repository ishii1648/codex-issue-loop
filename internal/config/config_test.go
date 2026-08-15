package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadDefaultsAndPercentJitter(t *testing.T) {
	dir := writeConfig(t, `version: 2
github:
  repo: owner/repo
watch:
  reconcile_jitter: 25%
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Queue.PollInterval.Duration != 60*time.Second {
		t.Fatalf("poll interval = %s", cfg.Queue.PollInterval.Duration)
	}
	if cfg.Watch.ReconcileInterval.Duration != 60*time.Second {
		t.Fatalf("reconcile interval = %s", cfg.Watch.ReconcileInterval.Duration)
	}
	if cfg.Watch.ReconcileJitter != 0.25 {
		t.Fatalf("jitter = %f", cfg.Watch.ReconcileJitter)
	}
	if cfg.Worker.Profiles["extended"].MaxContinuations != 3 {
		t.Fatalf("extended profile not defaulted")
	}
	if cfg.Worker.TimeoutGrace.Duration != 30*time.Second {
		t.Fatalf("timeout grace = %s", cfg.Worker.TimeoutGrace.Duration)
	}
	if cfg.Logs.RotateBytes != 16*1024*1024 || cfg.Logs.Generations != 7 || cfg.Logs.WorkerRunMaxCount != 100 {
		t.Fatalf("unexpected log defaults: %+v", cfg.Logs)
	}
	if cfg.Worktrees.CompletedMaxAge.Duration != 7*24*time.Hour || cfg.Worktrees.FailedMaxAge.Duration != 30*24*time.Hour ||
		cfg.Worktrees.BlockedMaxAge.Duration != 0 || cfg.Worktrees.NeedsInputMaxAge.Duration != 0 {
		t.Fatalf("unexpected worktree retention defaults: %+v", cfg.Worktrees)
	}
}

func TestLoadRejectsNegativeWorktreeRetention(t *testing.T) {
	dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\nworktrees:\n  completed_max_age: -1h\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "worktree retention") {
		t.Fatalf("negative retention accepted: %v", err)
	}
}

func TestLoadNotificationOptInAndRejectsUnsafeSettings(t *testing.T) {
	dir := writeConfig(t, `version: 2
github:
  repo: owner/repo
notifications:
  enabled: true
  endpoint: http://127.0.0.1:8080
  topic: opaque-topic
`)
	cfg, err := Load(dir)
	if err != nil || !cfg.Notifications.Enabled || cfg.Notifications.MaxAttempts != 8 {
		t.Fatalf("cfg=%+v err=%v", cfg.Notifications, err)
	}
	for _, fragment := range []string{
		"endpoint: http://example.com\n  topic: opaque-topic",
		"endpoint: https://ntfy.sh\n  topic: short",
		"endpoint: https://ntfy.sh\n  topic: opaque-topic\n  retry_initial: 20s\n  retry_max: 10s",
	} {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\nnotifications:\n  enabled: true\n  "+fragment+"\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "notifications") {
			t.Fatalf("unsafe notification config accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsInvalidLogRetention(t *testing.T) {
	for _, fragment := range []string{
		"logs:\n  rotate_bytes: 100\n",
		"logs:\n  rotate_interval: 0s\n",
		"logs:\n  generations: 0\n",
		"logs:\n  worker_run_max_age: 0s\n",
		"logs:\n  worker_run_max_count: 0\n",
	} {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\n"+fragment)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "logs") {
			t.Fatalf("invalid log config accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsInvalidTimeoutGrace(t *testing.T) {
	for _, value := range []string{"0s", "3h"} {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\nworker:\n  timeout: 2h\n  timeout_grace: "+value+"\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "timeout") {
			t.Fatalf("invalid timeout grace %s accepted: %v", value, err)
		}
	}
}

func TestLoadAcceptsQueueOrderStrategies(t *testing.T) {
	for _, fragment := range []string{
		"order: issue_number_asc\n",
		"order: created_at_asc\n",
		"order: priority_then_created_at\n  priority_labels: [priority:critical, priority:high]\n",
	} {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\nqueue:\n  "+fragment)
		if _, err := Load(dir); err != nil {
			t.Fatalf("valid queue order rejected: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsInvalidQueueOrderConfiguration(t *testing.T) {
	for _, fragment := range []string{
		"order: random\n",
		"order: priority_then_created_at\n",
		"order: priority_then_created_at\n  priority_labels: ['']\n",
		"order: priority_then_created_at\n  priority_labels: [' priority:high']\n",
		"order: priority_then_created_at\n  priority_labels: [priority:high, PRIORITY:HIGH]\n",
	} {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\nqueue:\n  "+fragment)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "queue.") {
			t.Fatalf("invalid queue order accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsUnknownNestedKey(t *testing.T) {
	dir := writeConfig(t, `version: 2
github:
  repo: owner/repo
watch:
  reconcile_interval: 1m
  typo: true
`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "typo") {
		t.Fatalf("expected unknown key error, got %v", err)
	}
}

func TestLoadRejectsUnsafeSandbox(t *testing.T) {
	dir := writeConfig(t, `version: 2
github:
  repo: owner/repo
worker:
  sandbox: danger-full-access
`)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("expected sandbox error, got %v", err)
	}
}

func TestLoadRejectsUnsafePathsRefsAndSecretNames(t *testing.T) {
	tests := []string{
		"git:\n  worktree_root: ../outside\n",
		"git:\n  branch_prefix: ../escape\n",
		"security:\n  redact_env: [\"BAD=NAME\"]\n",
	}
	for _, fragment := range tests {
		dir := writeConfig(t, "version: 2\ngithub:\n  repo: owner/repo\n"+fragment)
		if _, err := Load(dir); err == nil {
			t.Fatalf("unsafe config accepted: %s", fragment)
		}
	}
}

func TestRedactionValuesReadsNamedEnvironmentOnly(t *testing.T) {
	t.Setenv("AGENT_LOOP_TEST_SECRET", "configured-value")
	cfg := Defaults()
	cfg.Security.RedactEnv = []string{"AGENT_LOOP_TEST_SECRET"}
	values := cfg.RedactionValues()
	if len(values) != 1 || values[0] != "configured-value" {
		t.Fatalf("values=%q", values)
	}
}

func TestLoadRejectsLegacyAndFutureSchemaWithActionableErrors(t *testing.T) {
	for _, test := range []struct {
		version int
		want    string
	}{
		{version: 1, want: "migration required"},
		{version: CurrentVersion + 1, want: "unsupported config version"},
	} {
		dir := writeConfig(t, fmt.Sprintf("version: %d\ngithub:\n  repo: owner/repo\n", test.version))
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("version %d: %v", test.version, err)
		}
	}
}

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

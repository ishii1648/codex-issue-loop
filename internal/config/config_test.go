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
	dir := writeConfig(t, `version: 4
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
	if cfg.Worker.Backend != "codex" || cfg.Worker.EffectiveCommand() != "codex" {
		t.Fatalf("legacy worker defaults changed: %+v", cfg.Worker)
	}
	if cfg.Worker.AppServer.Enabled || cfg.Worker.AppServer.GoalTokenBudget != 200000 || cfg.Worker.AppServer.GoalTimeBudget.Duration != 2*time.Hour {
		t.Fatalf("unexpected opt-in App Server defaults: %+v", cfg.Worker.AppServer)
	}
	if cfg.Completion.AutoMerge {
		t.Fatal("auto merge must be opt-in")
	}
	if cfg.ConflictRecovery.MaxAttemptsPerBase != 3 || cfg.ConflictRecovery.MaxBaseUpdates != 3 {
		t.Fatalf("unexpected conflict recovery defaults: %+v", cfg.ConflictRecovery)
	}
	if cfg.Formatters.Go.Enabled || cfg.Formatters.Go.Timeout.Duration != 30*time.Second {
		t.Fatalf("unexpected formatter defaults: %+v", cfg.Formatters)
	}
	if !cfg.Completion.CreateDraftPR || !cfg.Completion.CloseIssue {
		t.Fatalf("unexpected completion defaults: %+v", cfg.Completion)
	}
	if cfg.Logs.RotateBytes != 16*1024*1024 || cfg.Logs.Generations != 7 || cfg.Logs.WorkerRunMaxCount != 100 {
		t.Fatalf("unexpected log defaults: %+v", cfg.Logs)
	}
	if cfg.Worktrees.CompletedMaxAge.Duration != 7*24*time.Hour || cfg.Worktrees.FailedMaxAge.Duration != 30*24*time.Hour ||
		cfg.Worktrees.BlockedMaxAge.Duration != 0 || cfg.Worktrees.NeedsInputMaxAge.Duration != 0 {
		t.Fatalf("unexpected worktree retention defaults: %+v", cfg.Worktrees)
	}
}

func TestLoadBuiltInGoFormatterOptIn(t *testing.T) {
	dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nformatters:\n  go:\n    enabled: true\n    timeout: 45s\n")
	cfg, err := Load(dir)
	if err != nil || !cfg.Formatters.Go.Enabled || cfg.Formatters.Go.Timeout.Duration != 45*time.Second {
		t.Fatalf("formatters=%+v err=%v", cfg.Formatters, err)
	}
	dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nformatters:\n  go:\n    enabled: true\n    timeout: 0s\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "formatters.go.timeout") {
		t.Fatalf("invalid formatter timeout accepted: %v", err)
	}
	for _, unsafe := range []string{"command: ./hook.sh", "args: [-local, malicious]"} {
		dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nformatters:\n  go:\n    enabled: true\n    "+unsafe+"\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "field") {
			t.Fatalf("repository-provided formatter command accepted: %s: %v", unsafe, err)
		}
	}
}

func TestLoadAppServerGoalOptInAndRejectsUnsafeBudgets(t *testing.T) {
	dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  backend: codex\n  app_server:\n    enabled: true\n    goal_token_budget: 50000\n    goal_time_budget: 45m\n")
	cfg, err := Load(dir)
	if err != nil || !cfg.Worker.AppServer.Enabled || cfg.Worker.AppServer.GoalTokenBudget != 50000 || cfg.Worker.AppServer.GoalTimeBudget.Duration != 45*time.Minute {
		t.Fatalf("app_server=%+v err=%v", cfg.Worker.AppServer, err)
	}
	for _, fragment := range []string{
		"backend: claude-code\n  app_server:\n    enabled: true",
		"backend: codex\n  app_server:\n    enabled: true\n    goal_token_budget: 0",
		"backend: codex\n  app_server:\n    enabled: true\n    goal_time_budget: 0s",
	} {
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  "+fragment+"\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "app_server") {
			t.Fatalf("unsafe App Server config accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadLocalhostOnlyCommandNetworkIsClosedAndOptIn(t *testing.T) {
	dir := writeConfig(t, `version: 4
github:
  repo: owner/repo
worker:
  command_network:
    policy: localhost-only
    proxy: true
    allowed_hosts: [localhost, 127.0.0.1]
`)
	cfg, err := Load(dir)
	if err != nil || !cfg.Worker.CommandNetwork.LocalhostOnly() {
		t.Fatalf("command_network=%+v err=%v", cfg.Worker.CommandNetwork, err)
	}
	for _, fragment := range []string{
		"policy: localhost-only\n    proxy: false\n    allowed_hosts: [localhost, 127.0.0.1]",
		"policy: localhost-only\n    proxy: true\n    allowed_hosts: []",
		"policy: localhost-only\n    proxy: true\n    allowed_hosts: ['*']",
		"policy: localhost-only\n    proxy: true\n    allowed_hosts: [localhost, example.com]",
		"policy: disabled\n    proxy: true\n    allowed_hosts: []",
	} {
		dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  command_network:\n    "+fragment+"\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "command_network") {
			t.Fatalf("unsafe command network accepted: %s: %v", fragment, err)
		}
	}
	for _, fragment := range []string{
		"backend: claude-code",
		"sandbox: read-only",
		"app_server:\n    enabled: true",
	} {
		dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  "+fragment+"\n  command_network:\n    policy: localhost-only\n    proxy: true\n    allowed_hosts: [localhost, 127.0.0.1]\n")
		if _, err := Load(dir); err == nil {
			t.Fatalf("incompatible command network accepted: %s", fragment)
		}
	}
}

func TestLoadWorkerBackendsModelsAndVariants(t *testing.T) {
	tests := []struct{ backend, model, variant, command string }{
		{backend: "codex", model: "gpt-test", command: "codex"},
		{backend: "claude-code", model: "claude-sonnet-test", variant: "high", command: "claude"},
		{backend: "opencode", model: "opencode-go/kimi-k2.7-code", variant: "high", command: "opencode"},
	}
	for _, test := range tests {
		dir := writeConfig(t, fmt.Sprintf("version: 4\ngithub:\n  repo: owner/repo\nworker:\n  backend: %s\n  model: %s\n  variant: %s\n", test.backend, test.model, test.variant))
		cfg, err := Load(dir)
		if err != nil || cfg.Worker.EffectiveCommand() != test.command {
			t.Fatalf("backend=%s cfg=%+v err=%v", test.backend, cfg.Worker, err)
		}
	}
}

func TestLoadRejectsUnknownBackendAndInvalidOpenCodeModel(t *testing.T) {
	for _, fragment := range []string{"backend: shell", "backend: opencode\n  model: missing-provider"} {
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  "+fragment+"\n")
		if _, err := Load(dir); err == nil {
			t.Fatalf("invalid worker accepted: %s", fragment)
		}
	}
}

func TestLoadRejectsNegativeWorktreeRetention(t *testing.T) {
	dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworktrees:\n  completed_max_age: -1h\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "worktree retention") {
		t.Fatalf("negative retention accepted: %v", err)
	}
}

func TestLoadAutoMergePolicy(t *testing.T) {
	dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\ncompletion:\n  create_draft_pr: true\n  auto_merge: true\n  close_issue: true\n")
	cfg, err := Load(dir)
	if err != nil || !cfg.Completion.AutoMerge || !cfg.Completion.CloseIssue {
		t.Fatalf("completion=%+v err=%v", cfg.Completion, err)
	}
	dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\ncompletion:\n  create_draft_pr: false\n  auto_merge: true\n")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "auto_merge") {
		t.Fatalf("auto merge without Pull Request was accepted: %v", err)
	}
}

func TestLoadConflictRecoveryBudget(t *testing.T) {
	dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nconflict_recovery:\n  max_attempts_per_base: 2\n  max_base_updates: 4\n")
	cfg, err := Load(dir)
	if err != nil || cfg.ConflictRecovery.MaxAttemptsPerBase != 2 || cfg.ConflictRecovery.MaxBaseUpdates != 4 {
		t.Fatalf("conflict_recovery=%+v err=%v", cfg.ConflictRecovery, err)
	}
	for _, field := range []string{"max_attempts_per_base", "max_base_updates"} {
		dir = writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nconflict_recovery:\n  "+field+": 0\n")
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "conflict_recovery") {
			t.Fatalf("invalid %s accepted: %v", field, err)
		}
	}
}

func TestLoadRejectsRemovedNotificationsSection(t *testing.T) {
	dir := writeConfig(t, `version: 4
github:
  repo: owner/repo
notifications:
  enabled: false
`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "field notifications not found") {
		t.Fatalf("removed notifications section was accepted: %v", err)
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
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\n"+fragment)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "logs") {
			t.Fatalf("invalid log config accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsInvalidTimeoutGrace(t *testing.T) {
	for _, value := range []string{"0s", "3h"} {
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nworker:\n  timeout: 2h\n  timeout_grace: "+value+"\n")
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
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nqueue:\n  "+fragment)
		if _, err := Load(dir); err != nil {
			t.Fatalf("valid queue order rejected: %s: %v", fragment, err)
		}
	}
}

func TestLoadResourceDefinitionsEnableRepositoryConcurrency(t *testing.T) {
	dir := writeConfig(t, `version: 4
github:
  repo: owner/repo
queue:
  concurrency: 3
resources:
  metadata_version: 1
  definitions:
    - name: config
      paths: [internal/config/**, .agent-loop.yaml]
    - name: docs
      paths: [docs/**, README.md]
`)
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	settings := cfg.AdmissionSettings()
	if settings.Legacy || settings.Concurrency != 3 || len(settings.Definitions) != 2 {
		t.Fatalf("settings=%+v", settings)
	}
}

func TestSelfHostingCanaryAndConcurrencyOneFallbackConfigurations(t *testing.T) {
	repositoryRoot := filepath.Clean(filepath.Join("..", ".."))
	data, err := os.ReadFile(filepath.Join(repositoryRoot, FileName))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(repositoryRoot)
	if err != nil {
		t.Fatal(err)
	}
	wantResources := []string{"config", "scheduler", "github", "worker", "host", "operations", "release", "docs"}
	if cfg.Queue.Concurrency != 2 || cfg.Resources.MetadataVersion != 1 || len(cfg.Resources.Definitions) != len(wantResources) {
		t.Fatalf("self-hosting canary config=%+v", cfg)
	}
	for index, name := range wantResources {
		if cfg.Resources.Definitions[index].Name != name {
			t.Fatalf("resource %d=%q want=%q", index, cfg.Resources.Definitions[index].Name, name)
		}
	}

	fallback := strings.Replace(string(data), "  concurrency: 2\n", "  concurrency: 1\n", 1)
	if fallback == string(data) {
		t.Fatal("self-hosting concurrency setting was not found")
	}
	fallbackDir := writeConfig(t, fallback)
	fallbackConfig, err := Load(fallbackDir)
	if err != nil {
		t.Fatal(err)
	}
	if fallbackConfig.Queue.Concurrency != 1 || len(fallbackConfig.Resources.Definitions) != len(wantResources) {
		t.Fatalf("concurrency 1 fallback changed taxonomy: %+v", fallbackConfig)
	}
}

func TestLoadRejectsConcurrentQueueWithoutValidResourceDefinitions(t *testing.T) {
	fragments := []string{
		"queue:\n  concurrency: 2\n",
		"queue:\n  concurrency: 2\nresources:\n  definitions:\n    - name: repo\n      paths: [docs/**]\n",
		"resources:\n  metadata_version: 2\n  definitions:\n    - name: docs\n      paths: [docs/**]\n",
		"resources:\n  definitions:\n    - name: docs\n      paths: [../docs/**]\n",
	}
	for _, fragment := range fragments {
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\n"+fragment)
		if _, err := Load(dir); err == nil {
			t.Fatalf("invalid resource config accepted:\n%s", fragment)
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
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\nqueue:\n  "+fragment)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "queue.") {
			t.Fatalf("invalid queue order accepted: %s: %v", fragment, err)
		}
	}
}

func TestLoadRejectsUnknownNestedKey(t *testing.T) {
	dir := writeConfig(t, `version: 4
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
	dir := writeConfig(t, `version: 4
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
		dir := writeConfig(t, "version: 4\ngithub:\n  repo: owner/repo\n"+fragment)
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
		{version: 3, want: "migration required"},
		{version: CurrentVersion + 1, want: "unsupported config version"},
	} {
		dir := writeConfig(t, fmt.Sprintf("version: %d\ngithub:\n  repo: owner/repo\n", test.version))
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("version %d: %v", test.version, err)
		}
	}
}

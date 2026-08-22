package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/capability"
	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/userrules"
	"github.com/ishii1648/codex-issue-loop/internal/webhook"
)

func TestDoctorDetectsWorkerProfileLaunchMismatch(t *testing.T) {
	cfg := config.Defaults()
	profile := cfg.Worker.Profiles["extended"]
	profile.Capabilities.Network = "public"
	profile.Capabilities.BrowserCDP = true
	cfg.Worker.Profiles["extended"] = profile
	diagnostics := diagnoseWorkerProfileCapabilities(registry.Entry{RepoID: "repo-test"}, cfg)
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "WORKER_PROFILE_LAUNCH_MISMATCH" && !diagnostic.OK && strings.Contains(diagnostic.Detail, capability.CodeWorkerProfileDrift) {
			found = true
		}
	}
	if !found {
		t.Fatalf("profile/launch mismatch was not diagnosed: %+v", diagnostics)
	}
}

func TestDoctorDiagnosesGoFormatterCapability(t *testing.T) {
	cfg := config.Defaults()
	cfg.Formatters.Go.Enabled = true
	entry := registry.Entry{RepoID: "repo", RepoPath: "/repo", Commands: map[string]string{}}
	if item := diagnosticByCode(t, diagnoseFormatters(context.Background(), entry, cfg), "FORMATTER_GO_NOT_REGISTERED"); item.OK {
		t.Fatalf("missing gofmt passed: %+v", item)
	}
	formatter := filepath.Join(t.TempDir(), "gofmt")
	if err := os.WriteFile(formatter, []byte("#!/bin/sh\nprintf 'wrong\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry.Commands["gofmt"] = formatter
	if item := diagnosticByCode(t, diagnoseFormatters(context.Background(), entry, cfg), "FORMATTER_GO_CAPABILITY_MISSING"); item.OK {
		t.Fatalf("invalid gofmt capability passed: %+v", item)
	}
	if err := os.WriteFile(formatter, []byte("#!/bin/sh\nprintf 'package probe\\n\\nfunc f() {}\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if item := diagnosticByCode(t, diagnoseFormatters(context.Background(), entry, cfg), "FORMATTER_GO_AVAILABLE"); !item.OK {
		t.Fatalf("available gofmt failed: %+v", item)
	}
	cfg.Formatters.Go.Enabled = false
	if item := diagnosticByCode(t, diagnoseFormatters(context.Background(), entry, cfg), "FORMATTER_GO_DISABLED"); !item.OK {
		t.Fatalf("disabled gofmt failed: %+v", item)
	}
}

func TestDoctorDistinguishesBrokerRegistrationRuntimeAndFreshness(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{
		Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos"),
		LaunchAgents: filepath.Join(root, "launchagents"),
	}
	if err := os.MkdirAll(l.LaunchAgents, 0o700); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(root, "launchctl")
	script := `#!/bin/sh
case "$DOCTOR_BROKER_MODE" in
  unloaded) exit 1 ;;
  crash) printf 'state = waiting\nlast exit code = 78\n' ;;
  running) printf 'state = running\npid = 4321\n' ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	cfg.GitHub.RepositoryID = 1234
	cfg.Webhook.Mode = "webhook"
	cfg.Webhook.ListenerAddress = "127.0.0.1:8787"
	cfg.Webhook.PublicURLIdentifier = "fixture.example/webhook"
	cfg.Webhook.SecretSource.Env = "DOCTOR_WEBHOOK_SECRET"
	cfg.Webhook.InstallationIDs = []int64{99}
	t.Setenv("DOCTOR_WEBHOOK_SECRET", "fixture-secret")
	entry := registry.Entry{RepoID: "repo", RepoPath: root, Commands: map[string]string{"launchctl": launchctl}}
	if item := diagnosticByCode(t, diagnoseWebhook(context.Background(), l, entry, cfg), "WEBHOOK_BROKER_NOT_REGISTERED"); item.OK {
		t.Fatalf("missing plist passed: %+v", item)
	}
	if err := os.WriteFile(l.BrokerPlistPath(), []byte("plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCTOR_BROKER_MODE", "unloaded")
	if item := diagnosticByCode(t, diagnoseWebhook(context.Background(), l, entry, cfg), "WEBHOOK_BROKER_UNLOADED"); item.OK {
		t.Fatalf("unloaded broker passed: %+v", item)
	}
	if err := os.MkdirAll(l.BrokerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSONFixture(t, filepath.Join(l.BrokerDir(), "status.json"), webhook.Status{
		Version: 1, Mode: "webhook", ListenerAddress: cfg.Webhook.ListenerAddress, UpdatedAt: time.Now().Add(-10 * time.Minute),
	})
	t.Setenv("DOCTOR_BROKER_MODE", "crash")
	diagnostics := diagnoseWebhook(context.Background(), l, entry, cfg)
	if diagnosticByCode(t, diagnostics, "WEBHOOK_BROKER_CRASH_LOOP").OK || diagnosticByCode(t, diagnostics, "WEBHOOK_BROKER_STATUS_STALE").OK {
		t.Fatalf("crash/stale broker passed: %+v", diagnostics)
	}
	writeJSONFixture(t, filepath.Join(l.BrokerDir(), "status.json"), webhook.Status{
		Version: 1, Mode: "webhook", ListenerAddress: cfg.Webhook.ListenerAddress, UpdatedAt: time.Now().UTC(),
	})
	t.Setenv("DOCTOR_BROKER_MODE", "running")
	diagnostics = diagnoseWebhook(context.Background(), l, entry, cfg)
	if !diagnosticByCode(t, diagnostics, "WEBHOOK_BROKER_RUNNING").OK || !diagnosticByCode(t, diagnostics, "WEBHOOK_BROKER_STATUS_FRESH").OK {
		t.Fatalf("healthy broker failed: %+v", diagnostics)
	}
}

func diagnosticByCode(t *testing.T, diagnostics []diagnostic, code string) diagnostic {
	t.Helper()
	for _, item := range diagnostics {
		if item.Code == code {
			return item
		}
	}
	t.Fatalf("diagnostic %s not found: %+v", code, diagnostics)
	return diagnostic{}
}

func TestDoctorOutputHasStableSchemaCodesAndSafeRemediations(t *testing.T) {
	result := doctorResult{SchemaVersion: doctorSchemaVersion, GeneratedAt: time.Now().UTC(), Diagnostics: []diagnostic{
		failedDiagnostic("SUPERVISOR_BLOCKED", "repository", "repo_1", "blocked", "auth expired", command("inspect", "agent-loop status --json")),
	}}
	result.OK = diagnosticsOK(result.Diagnostics)
	var jsonOut bytes.Buffer
	if err := (App{Out: &jsonOut}).writeDoctorResult(true, result); err != nil {
		t.Fatal(err)
	}
	var decoded doctorResult
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SchemaVersion != 1 || decoded.OK || decoded.Diagnostics[0].Code != "SUPERVISOR_BLOCKED" || decoded.Diagnostics[0].Remediations[0].Automatic || decoded.Diagnostics[0].Remediations[0].Destructive {
		t.Fatalf("decoded=%+v", decoded)
	}
	var human bytes.Buffer
	if err := (App{Out: &human}).writeDoctorResult(false, result); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"doctor: FAILED", "[FAIL] SUPERVISOR_BLOCKED", "next: inspect", "agent-loop status --json"} {
		if !strings.Contains(human.String(), expected) {
			t.Fatalf("human output missing %q: %s", expected, human.String())
		}
	}
}

func TestDoctorReportsInitAsAdvisoryUserRuleRemediation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude"))
	t.Setenv("AGENT_LOOP_HOME", filepath.Join(root, "agent-loop"))
	diagnostics := diagnoseUserRules()
	for _, code := range []string{"USER_RULE_CODEX_MISSING", "USER_RULE_CLAUDE_MISSING"} {
		item := diagnosticByCode(t, diagnostics, code)
		if item.OK || len(item.Remediations) != 2 || !strings.Contains(item.Remediations[0].Command, "agent-loop init") {
			t.Fatalf("diagnostic=%+v", item)
		}
		for _, remediation := range item.Remediations {
			if remediation.Automatic || remediation.Destructive {
				t.Fatalf("unsafe remediation=%+v", remediation)
			}
		}
	}
	config, err := userrules.ConfigFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	plan, err := userrules.Plan(config, []userrules.Agent{userrules.AgentCodex, userrules.AgentClaude})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := userrules.Apply(plan); err != nil {
		t.Fatal(err)
	}
	diagnostics = diagnoseUserRules()
	for _, code := range []string{"USER_RULE_CODEX_CURRENT", "USER_RULE_CLAUDE_CURRENT"} {
		if item := diagnosticByCode(t, diagnostics, code); !item.OK {
			t.Fatalf("diagnostic=%+v", item)
		}
	}
}

func TestDiagnoseSchemasDistinguishesSupportedRequiredAndUnsupported(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, RegistryPath: filepath.Join(root, "registry.json"), ReposRoot: filepath.Join(root, "repos")}
	if err := os.MkdirAll(l.ReposRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		version int
		code    string
		ready   bool
	}{
		{name: "supported", version: 4, code: "SCHEMA_VERSION_SUPPORTED", ready: true},
		{name: "migration-required", version: 3, code: "SCHEMA_MIGRATION_REQUIRED"},
		{name: "unsupported", version: 5, code: "SCHEMA_VERSION_UNSUPPORTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(l.RegistryPath, []byte(fmt.Sprintf("{\"version\":%d,\"repos\":{}}\n", test.version)), 0o600); err != nil {
				t.Fatal(err)
			}
			diagnostics, ready := diagnoseSchemas(l)
			if ready != test.ready || len(diagnostics) != 1 || diagnostics[0].Code != test.code || diagnostics[0].OK != test.ready {
				t.Fatalf("diagnostics=%+v ready=%v", diagnostics, ready)
			}
		})
	}
}

func TestFaultDoctorDetectsCorruptStateWithoutModifyingIt(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo, Commands: map[string]string{"git": "/usr/bin/git"}}
	store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{broken state\n")
	if err := os.WriteFile(store.StatePath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostics := diagnoseDurableState(l, entry, cfg)
	item := diagnosticByCode(t, diagnostics, "STATE_CORRUPT")
	if item.OK || len(item.Remediations) != 2 || item.Remediations[0].Automatic || item.Remediations[0].Destructive {
		t.Fatalf("diagnostic=%+v", item)
	}
	after, err := os.ReadFile(store.StatePath())
	if err != nil || !bytes.Equal(after, corrupt) {
		t.Fatalf("doctor modified corrupt state: data=%q err=%v", after, err)
	}
}

func TestDoctorCorrelatesBlockedAndStoppedStateWithEventAndLog(t *testing.T) {
	for _, supervisorState := range []state.SupervisorState{state.SupervisorStateBlocked, state.SupervisorStateStopped} {
		t.Run(string(supervisorState), func(t *testing.T) {
			repo, l := testEnvironment(t)
			if err := l.Ensure(); err != nil {
				t.Fatal(err)
			}
			cfg := mustConfig(t, repo)
			entry := registry.Entry{RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo, Commands: map[string]string{"git": "/usr/bin/git"}}
			store := state.Store{Dir: l.RepoDir(entry.RepoID), RepoID: entry.RepoID, RepoPath: repo}
			if err := store.Initialize(); err != nil {
				t.Fatal(err)
			}
			_, err := store.Update("fixture_"+string(supervisorState), 0, "", nil, func(snapshot *state.Snapshot) error {
				snapshot.Supervisor.State = supervisorState
				snapshot.Supervisor.Message = "authentication expired"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.Dir, "launchd.stderr.log"), []byte("older\nlatest failure context\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code := "SUPERVISOR_" + strings.ToUpper(string(supervisorState))
			item := diagnosticByCode(t, diagnoseDurableState(l, entry, cfg), code)
			if item.OK || !strings.Contains(item.Detail, "fixture_"+string(supervisorState)) || !strings.Contains(item.Detail, "latest failure context") || len(item.Remediations) < 2 {
				t.Fatalf("diagnostic=%+v", item)
			}
			for _, fix := range item.Remediations {
				if fix.Automatic || fix.Destructive {
					t.Fatalf("unsafe remediation=%+v", fix)
				}
			}
		})
	}
}

func TestDoctorDiagnosesMissingRegisteredBinaryLabelsPlistAndState(t *testing.T) {
	repo, l := testEnvironment(t)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(filepath.Dir(repo), "bin")
	fakeGH := filepath.Join(binDir, "gh-doctor")
	script := `#!/bin/sh
case "$1 $2" in
  "repo view") printf '%s\n' '{"nameWithOwner":"owner/repo"}' ;;
  "label list") printf '%s\n' '[]' ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fakeGH, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, repo)
	entry := registry.Entry{
		RepoID: registry.RepoID(cfg.GitHub.Repo, repo), RepoPath: repo,
		Commands: map[string]string{"gh": fakeGH, "codex": filepath.Join(binDir, "moved-codex")},
	}
	diagnostics := diagnoseRepository(context.Background(), l, entry)
	for _, code := range []string{"REGISTERED_BINARY_MISSING", "LAUNCH_AGENT_MISSING", "GITHUB_LABELS_MISSING", "STATE_MISSING"} {
		if item := diagnosticByCode(t, diagnostics, code); item.OK || len(item.Remediations) == 0 {
			t.Fatalf("diagnostic=%+v", item)
		}
	}
}

func TestFaultDoctorHostAuthAndSleepFixturesHaveUniqueCodes(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ghScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'gh version 2.69.0'; exit 0; fi
if [ "$1 $2" = "auth status" ]; then echo 'authentication expired' >&2; exit 1; fi
if [ "$1 $2 $3" = "issue list --help" ]; then echo '--json --limit --label --assignee --milestone'; exit 0; fi
if [ "$1 $2 $3" = "issue edit --help" ]; then echo '--add-label --remove-label'; exit 0; fi
if [ "$1 $2 $3" = "issue comment --help" ]; then echo '--body'; exit 0; fi
exit 2
`
	codexScript := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.136.0'; exit 0; fi
if [ "$1 $2" = "login status" ]; then echo 'not logged in' >&2; exit 1; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
if [ "$1 $2 $3" = "exec resume --help" ]; then echo '--json --output-schema --output-last-message'; exit 0; fi
exit 2
`
	pmsetScript := "#!/bin/sh\nprintf '%s\\n' 'AC Power:' ' sleep 1'\n"
	for name, body := range map[string]string{"gh": ghScript, "codex": codexScript, "pmset": pmsetScript} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	diagnostics := diagnoseHost(context.Background(), layout.Layout{Root: root, BinDir: filepath.Join(root, "installed-bin"), SkillsDir: filepath.Join(root, "skills")})
	for _, code := range []string{"GITHUB_AUTH_INVALID", "CODEX_AUTH_INVALID", "MACOS_SLEEP_ENABLED"} {
		item := diagnosticByCode(t, diagnostics, code)
		if item.OK || len(item.Remediations) == 0 {
			t.Fatalf("diagnostic=%+v", item)
		}
	}
	seen := map[string]bool{}
	for _, item := range diagnostics {
		if seen[item.Code] {
			t.Fatalf("duplicate host diagnostic code %s", item.Code)
		}
		seen[item.Code] = true
	}
}

func TestDoctorDetectsInstalledBinaryAndSkillMismatch(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, BinDir: filepath.Join(root, "bin"), SkillsDir: filepath.Join(root, "skills")}
	for _, dir := range []string{l.BinDir, filepath.Join(l.SkillsDir, "agent-loop")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(root, "source")
	if err := os.WriteFile(source, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := installArtifacts(l, source, "v1.0.0", "abc"); err != nil {
		t.Fatal(err)
	}
	if item := diagnosticByCode(t, diagnoseInstallation(l), "INSTALL_VERSION_CONSISTENT"); !item.OK {
		t.Fatalf("diagnostic=%+v", item)
	}
	if err := os.WriteFile(filepath.Join(l.SkillsDir, "agent-loop", "SKILL.md"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if item := diagnosticByCode(t, diagnoseInstallation(l), "INSTALL_VERSION_MISMATCH"); item.OK {
		t.Fatalf("diagnostic=%+v", item)
	}
}

func TestLatestEventRejectsMalformedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	body := "{\"version\":1,\"event_id\":\"evt\",\"sequence\":1,\"timestamp\":\"2026-08-15T00:00:00Z\",\"repo_id\":\"repo\",\"type\":\"started\"}\n{broken\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	event, err := latestEvent(path)
	if err == nil || event.Type != "started" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
}

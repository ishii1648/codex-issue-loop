package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/registry"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

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
	for _, supervisorState := range []string{"blocked", "stopped"} {
		t.Run(supervisorState, func(t *testing.T) {
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
			_, err := store.Update("fixture_"+supervisorState, 0, "", nil, func(snapshot *state.Snapshot) error {
				snapshot.Supervisor.State = supervisorState
				snapshot.Supervisor.Message = "authentication expired"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.Dir, "supervisor.err.log"), []byte("older\nlatest failure context\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			code := "SUPERVISOR_" + strings.ToUpper(supervisorState)
			item := diagnosticByCode(t, diagnoseDurableState(l, entry, cfg), code)
			if item.OK || !strings.Contains(item.Detail, "fixture_"+supervisorState) || !strings.Contains(item.Detail, "latest failure context") || len(item.Remediations) < 2 {
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
	diagnostics := diagnoseHost(context.Background())
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

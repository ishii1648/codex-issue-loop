package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersionComparison(t *testing.T) {
	for _, test := range []struct {
		current, minimum string
		ok               bool
	}{{"0.136.0", "0.136.0", true}, {"0.137.0", "0.136.0", true}, {"1.0.0", "0.136.0", true}, {"0.135.9", "0.136.0", false}, {"bad", "0.136.0", false}} {
		if got := AtLeast(test.current, test.minimum); got != test.ok {
			t.Fatalf("AtLeast(%q, %q)=%v", test.current, test.minimum, got)
		}
	}
}

func TestCapabilityProbes(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	gh := filepath.Join(dir, "gh")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.136.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
if [ "$1 $2 $3 $4 $5" = "exec --cd . resume --help" ]; then echo '--json --output-schema --output-last-message'; exit 0; fi
exit 2
`)
	writeExecutable(t, gh, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'gh version 2.69.0'; exit 0; fi
case "$1 $2 $3" in
  'issue list --help') echo '--json --limit --label --assignee --milestone' ;;
  'issue edit --help') echo '--add-label --remove-label' ;;
  'issue comment --help') echo '--body' ;;
  *) exit 2 ;;
esac
`)
	if report := ProbeCodex(context.Background(), codex); !report.OK() || !report.Has("session_resume") {
		t.Fatalf("codex report=%+v", report)
	}
	if report := ProbeGH(context.Background(), gh); !report.OK() {
		t.Fatalf("gh report=%+v", report)
	}
}

func TestCodexProbeAllowsMissingResumeAsFallbackCapability(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.136.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
exit 2
`)
	report := ProbeCodex(context.Background(), codex)
	if !report.VersionOK || !report.Has("exec_structured") || report.Has("session_resume") || len(report.Missing) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestCodexProbeDetectsLocalhostNetworkProxyCapability(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.147.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd --ignore-user-config --strict-config --disable'; exit 0; fi
if [ "$1 $2 $3 $4 $5" = "exec --cd . resume --help" ]; then echo '--json --output-schema --output-last-message'; exit 0; fi
if [ "$1 $2" = "features list" ]; then echo 'network_proxy apps browser_use computer_use plugins remote_plugin skill_search tool_suggest'; exit 0; fi
exit 2
`)
	report := ProbeCodex(context.Background(), codex)
	if !report.OK() || !report.Has("localhost_network_proxy") {
		t.Fatalf("report=%+v", report)
	}
}

func TestCodexProbeRejectsResumeThatCannotAcceptPinnedWorkspace(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.147.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
if [ "$1 $2 $3" = "exec resume --help" ]; then echo '--json --output-schema --output-last-message'; exit 0; fi
exit 2
`)
	report := ProbeCodex(context.Background(), codex)
	if !report.VersionOK || !report.Has("exec_structured") || report.Has("session_resume") || len(report.Missing) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func TestCodexProbeDetectsGeneratedAppServerGoalContract(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1 $2 $3" = "app-server generate-json-schema --help" ]; then echo '--out --experimental'; exit 0; fi
if [ "$1 $2 $3" = "app-server generate-json-schema --experimental" ]; then
  schema_out=''
  while [ "$#" -gt 0 ]; do
    if [ "$1" = "--out" ]; then schema_out="$2"; break; fi
    shift
  done
  printf '%s' 'thread/start thread/resume thread/goal/set thread/goal/get thread/goal/clear turn/start turn/steer' > "$schema_out/ClientRequest.json"
  printf '%s' 'item/tool/requestUserInput item/commandExecution/requestApproval item/fileChange/requestApproval' > "$schema_out/ServerRequest.json"
  printf '%s' 'thread/tokenUsage/updated turn/completed' > "$schema_out/ServerNotification.json"
  exit 0
fi
exit 2
`)
	if !probeCodexAppServerGoal(context.Background(), codex) {
		t.Fatal("generated App Server Goal contract was not detected")
	}
}

func TestBuiltInBackendCapabilityProbes(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	opencode := filepath.Join(dir, "opencode")
	writeExecutable(t, claude, `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2.1.119'; exit 0; fi
if [ "$1" = "--help" ]; then echo '--output-format --json-schema --resume --model --effort --permission-mode --settings --disallowedTools --strict-mcp-config --mcp-config'; exit 0; fi
exit 2
`)
	writeExecutable(t, opencode, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode 1.1.1'; exit 0; fi
if [ "$1" = "--help" ]; then echo '--pure'; exit 0; fi
if [ "$1 $2" = "serve --help" ]; then echo '--hostname --port'; exit 0; fi
if [ "$1 $2" = "models --help" ]; then exit 0; fi
exit 2
`)
	for backend, path := range map[string]string{"claude-code": claude, "opencode": opencode} {
		if report := ProbeBackend(context.Background(), backend, path); !report.OK() || !report.Has("structured_output") || !report.Has("session_resume") {
			t.Fatalf("backend=%s report=%+v", backend, report)
		}
	}
}

func TestGofmtCapabilityProbe(t *testing.T) {
	if err := ProbeGofmt(context.Background(), filepath.Join(runtime.GOROOT(), "bin", "gofmt")); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(t.TempDir(), "gofmt")
	writeExecutable(t, invalid, "#!/bin/sh\nprintf 'not gofmt\\n'\n")
	if err := ProbeGofmt(context.Background(), invalid); err == nil {
		t.Fatal("invalid gofmt capability passed")
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(fmt.Errorf("write fake executable: %w", err))
	}
}

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
)

func TestBackendFactoryAndCapabilities(t *testing.T) {
	cfg := config.Defaults()
	for _, id := range []string{"codex", "claude-code", "opencode"} {
		cfg.Worker.Backend = id
		backend, err := NewBackend(cfg, FactoryOptions{})
		if err != nil || backend.ID() != id || !backend.Capabilities().StructuredOutput || !backend.Capabilities().NonInteractivePolicy {
			t.Fatalf("backend=%v err=%v capabilities=%+v", backend, err, backend.Capabilities())
		}
	}
}

func TestClaudeCodeInitialAndResumePassModelAndEffortWithoutPromptArgv(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	argsPath := filepath.Join(dir, "args")
	promptPath := filepath.Join(dir, "prompt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" > %q
cat > %q
printf '%%s\n' '{"type":"result","session_id":"claude-session","structured_output":{"version":1,"status":"completed","execution_profile":"standard","summary":"done","question":null,"tests":[],"git":null,"retry":null}}'
`, argsPath, promptPath)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := backendTestConfig(dir, "claude-code", fake, "claude-sonnet-test", "high")
	adapter := ClaudeCode{StateDir: dir, RuntimeVersion: "2.1.119"}
	current := state.Issue{RunID: "run_claude", Attempts: 1}
	var spawn ProcessStart
	recordSpawn := func(start ProcessStart) error { spawn = start; return nil }
	canonicalDir, err := config.CanonicalRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Run(context.Background(), cfg, gh.Issue{Number: 1, Title: "secret prompt marker"}, current, "", recordSpawn)
	if err != nil || result.SessionID != "claude-session" || result.Identity.Backend != "claude-code" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if spawn.ExpectedCWD != canonicalDir || spawn.ActualCWD != canonicalDir {
		t.Fatalf("Claude Code spawn=%+v", spawn)
	}
	args, _ := os.ReadFile(argsPath)
	if strings.Contains(string(args), "secret prompt marker") || !strings.Contains(string(args), "--model claude-sonnet-test") || !strings.Contains(string(args), "--effort high") || !strings.Contains(string(args), "--permission-mode dontAsk") {
		t.Fatalf("unsafe or incomplete argv: %s", args)
	}
	prompt, _ := os.ReadFile(promptPath)
	if !strings.Contains(string(prompt), "secret prompt marker") {
		t.Fatalf("prompt not sent on stdin: %s", prompt)
	}
	current.SessionID = result.SessionID
	if _, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 1}, current, "continue marker", recordSpawn); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--resume claude-session") || !strings.Contains(string(args), "--model claude-sonnet-test") {
		t.Fatalf("resume argv=%s", args)
	}
}

func TestClaudeCodeRejectsInvalidStructuredOutput(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"session_id\":\"bad\",\"result\":\"not-json\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := backendTestConfig(dir, "claude-code", fake, "sonnet", "")
	_, err := (ClaudeCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_bad", Attempts: 1}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "structured result") {
		t.Fatalf("invalid result accepted: %v", err)
	}
}

func TestOpenCodeServerAdapterInitialResumeAndOpenCodeGoModel(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	captured := filepath.Join(dir, "request.json")
	t.Setenv("AGENT_LOOP_OPENCODE_CAPTURE", captured)
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/kimi-k2.7-code", "high")
	adapter := OpenCode{StateDir: dir, RuntimeVersion: "1.14.0"}
	var spawn ProcessStart
	recordSpawn := func(start ProcessStart) error { spawn = start; return nil }
	canonicalDir, err := config.CanonicalRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Run(context.Background(), cfg, gh.Issue{Number: 73, Title: "adapter"}, state.Issue{RunID: "run_open", Attempts: 1}, "", recordSpawn)
	if err != nil || result.SessionID != "ses_fake" || result.Identity.Provider != "opencode-go" || result.Identity.ResolvedModel != "opencode-go/kimi-k2.7-code" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if spawn.ExpectedCWD != canonicalDir || spawn.ActualCWD != canonicalDir {
		t.Fatalf("OpenCode spawn=%+v", spawn)
	}
	assertOpenCodeRequest(t, captured, "opencode-go", "kimi-k2.7-code", "high")
	current := state.Issue{RunID: "run_resume", Attempts: 1, SessionID: "ses_saved"}
	if _, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 73}, current, "resume", recordSpawn); err != nil {
		t.Fatal(err)
	}
	assertOpenCodeRequest(t, captured, "opencode-go", "kimi-k2.7-code", "high")
}

func TestOpenCodeTimeoutAbortsSessionAndStopsServerGroup(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	aborted := filepath.Join(dir, "aborted")
	t.Setenv("AGENT_LOOP_OPENCODE_MODE", "timeout")
	t.Setenv("AGENT_LOOP_OPENCODE_ABORTED", aborted)
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	cfg.Worker.Timeout.Duration = time.Second
	cfg.Worker.TimeoutGrace.Duration = 100 * time.Millisecond
	pid := 0
	_, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_timeout", Attempts: 1}, "", func(start ProcessStart) error { pid = start.PID; return nil })
	var termination *TerminationError
	if !errors.As(err, &termination) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	if _, err := os.Stat(aborted); err != nil {
		t.Fatalf("session abort was not requested: %v", err)
	}
	if processAlive(pid) {
		t.Fatalf("opencode server process %d survived timeout", pid)
	}
}

func TestOpenCodeTimeoutRetriesSessionAbortBeforeStoppingServer(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	aborted := filepath.Join(dir, "aborted")
	t.Setenv("AGENT_LOOP_OPENCODE_MODE", "timeout")
	t.Setenv("AGENT_LOOP_OPENCODE_ABORTED", aborted)
	t.Setenv("AGENT_LOOP_OPENCODE_ABORT_FAIL_ONCE", "1")
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	cfg.Worker.Timeout.Duration = time.Second
	cfg.Worker.TimeoutGrace.Duration = 100 * time.Millisecond
	_, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_abort_retry", Attempts: 1}, "", nil)
	var termination *TerminationError
	if !errors.As(err, &termination) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(aborted); err != nil {
		t.Fatalf("session abort was not retried: %v", err)
	}
}

func TestOpenCodeProviderFailuresAreNormalized(t *testing.T) {
	for _, mode := range []string{"auth", "model"} {
		t.Run(mode, func(t *testing.T) {
			requireLoopbackListener(t)
			dir := t.TempDir()
			fake := openCodeHelperCommand(t, dir)
			t.Setenv("AGENT_LOOP_OPENCODE_MODE", mode)
			cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
			_, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_" + mode, Attempts: 1}, "", nil)
			if err == nil || !strings.Contains(err.Error(), "opencode message failed") {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
		})
	}
}

func requireLoopbackListener(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listeners are unavailable in this test environment: %v", err)
	}
	_ = listener.Close()
}

func backendTestConfig(dir, backend, command, model, variant string) config.Config {
	cfg := config.Defaults()
	cfg.GitHub.Repo, cfg.RepoPath = "owner/repo", dir
	cfg.Worker.Backend, cfg.Worker.Command, cfg.Worker.Model, cfg.Worker.Variant = backend, command, model, variant
	return cfg
}

func openCodeHelperCommand(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "opencode")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_LOOP_OPENCODE_HELPER", "1")
	t.Setenv("AGENT_LOOP_OPENCODE_HELPER_EXE", executable)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \"$AGENT_LOOP_OPENCODE_HELPER_EXE\" -test.run=TestOpenCodeHelperProcess -- \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenCodeHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_LOOP_OPENCODE_HELPER") != "1" {
		return
	}
	port := ""
	for index, arg := range os.Args {
		if arg == "--port" && index+1 < len(os.Args) {
			port = os.Args[index+1]
		}
	}
	if _, err := strconv.Atoi(port); err != nil {
		os.Exit(2)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"healthy":true}`) })
	mux.HandleFunc("/session", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"id":"ses_fake"}`) })
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/abort") {
			aborted := os.Getenv("AGENT_LOOP_OPENCODE_ABORTED")
			firstAttempt := aborted + ".attempt"
			if os.Getenv("AGENT_LOOP_OPENCODE_ABORT_FAIL_ONCE") == "1" {
				if _, err := os.Stat(firstAttempt); errors.Is(err, os.ErrNotExist) {
					_ = os.WriteFile(firstAttempt, []byte("attempted"), 0o600)
					http.Error(w, "retry abort", http.StatusServiceUnavailable)
					return
				}
			}
			_ = os.WriteFile(aborted, []byte("aborted"), 0o600)
			_, _ = io.WriteString(w, "true")
			return
		}
		data, _ := io.ReadAll(r.Body)
		if capture := os.Getenv("AGENT_LOOP_OPENCODE_CAPTURE"); capture != "" {
			_ = os.WriteFile(capture, data, 0o600)
		}
		if os.Getenv("AGENT_LOOP_OPENCODE_MODE") == "timeout" {
			<-r.Context().Done()
			return
		}
		if os.Getenv("AGENT_LOOP_OPENCODE_MODE") == "auth" {
			http.Error(w, `{"name":"ProviderAuthError"}`, http.StatusUnauthorized)
			return
		}
		if os.Getenv("AGENT_LOOP_OPENCODE_MODE") == "model" {
			http.Error(w, `{"name":"ModelNotFound"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"info":{"providerID":"opencode-go","modelID":"kimi-k2.7-code","structured":{"version":1,"status":"completed","execution_profile":"standard","summary":"done","question":null,"tests":[],"git":null,"retry":null}}}`)
	})
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		os.Exit(2)
	}
}

func assertOpenCodeRequest(t *testing.T, path, provider, model, variant string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Model   map[string]string `json:"model"`
		Variant string            `json:"variant"`
		Format  map[string]any    `json:"format"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if body.Model["providerID"] != provider || body.Model["modelID"] != model || body.Variant != variant || body.Format["type"] != "json_schema" {
		t.Fatalf("request=%s", data)
	}
}

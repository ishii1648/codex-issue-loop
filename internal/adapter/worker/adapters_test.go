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
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestClaudeCodeResumeWithoutResultDoesNotReusePreviousResult(t *testing.T) {
	for _, exitCode := range []int{0, 1} {
		t.Run(fmt.Sprintf("exit_%d", exitCode), func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "claude")
			script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"result","session_id":"claude-session","structured_output":{"version":1,"status":"needs_input","execution_profile":"standard","summary":"waiting","question":{"text":"Continue?","reason":"approval","recommended_option":"yes","options":[{"id":"yes","label":"Yes"}],"allow_free_text":true},"tests":[],"git":null,"retry":null}}'
printf '%s\n' '{"type":"system","session_id":"claude-session"}'
`
			if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := backendTestConfig(dir, "claude-code", fake, "sonnet", "")
			cfg.Logs.RotateBytes = 1
			adapter := ClaudeCode{StateDir: dir}
			current := state.Issue{RunID: "run_resume", Attempts: 1}
			result, err := adapter.Run(context.Background(), cfg, gh.Issue{Number: 1}, current, "", nil)
			if err != nil || result.Status != "needs_input" {
				t.Fatalf("initial result=%+v err=%v", result, err)
			}
			current.SessionID = result.SessionID
			runDir := filepath.Join(dir, "runs", current.RunID)
			previousLogs, err := filepath.Glob(filepath.Join(runDir, "claude-code*.jsonl"))
			if err != nil || len(previousLogs) != 1 {
				t.Fatalf("initial logs=%v err=%v", previousLogs, err)
			}
			previousResults, err := filepath.Glob(filepath.Join(runDir, "result-*.json"))
			if err != nil || len(previousResults) != 1 {
				t.Fatalf("initial result files=%v err=%v", previousResults, err)
			}
			if err := os.WriteFile(fake, []byte(fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nexit %d\n", exitCode)), 0o700); err != nil {
				t.Fatal(err)
			}
			result, err = adapter.Resume(context.Background(), cfg, gh.Issue{Number: 1}, current, "continue", nil)
			if err == nil || result.Status != "" || result.SessionID != current.SessionID {
				t.Fatalf("resume result=%+v err=%v", result, err)
			}
			if exitCode == 0 && !strings.Contains(err.Error(), "no schema-conforming result event") {
				t.Fatalf("expected parse error, got %v", err)
			}
			results, err := filepath.Glob(filepath.Join(runDir, "result-*.json"))
			if err != nil || len(results) != 1 || results[0] != previousResults[0] {
				t.Fatalf("result files changed: %v err=%v", results, err)
			}
			logs, err := filepath.Glob(filepath.Join(runDir, "claude-code*.jsonl"))
			if err != nil || len(logs) != 2 {
				t.Fatalf("expected separate execution logs, got %v err=%v", logs, err)
			}
			for _, log := range logs {
				if log == previousLogs[0] {
					continue
				}
				if _, _, err := parseClaudeResult(log); err == nil {
					t.Fatal("accepted previous execution result")
				}
			}
		})
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

func TestOpenCodeMessageCompletesAfterThirtySeconds(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	t.Setenv("AGENT_LOOP_OPENCODE_MODE", "delayed")
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	cfg.Worker.Timeout.Duration = time.Minute
	started := time.Now()
	result, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_delayed", Attempts: 1}, "", nil)
	if err != nil || result.Status != "completed" || result.SessionID != "ses_fake" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 35*time.Second {
		t.Fatalf("message response was not delayed for 35 seconds: %s", elapsed)
	}
}

func TestOpenCodeTimeoutAbortsSessionAndStopsServerGroup(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	canonicalDir, err := config.CanonicalRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	aborted := filepath.Join(canonicalDir, "aborted")
	t.Setenv("AGENT_LOOP_OPENCODE_MODE", "timeout")
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	cfg.Worker.Timeout.Duration = 3 * time.Second
	cfg.Worker.TimeoutGrace.Duration = 100 * time.Millisecond
	pid := 0
	result, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_timeout", Attempts: 1}, "", func(start ProcessStart) error { pid = start.PID; return nil })
	var termination *TerminationError
	if !errors.As(err, &termination) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	if result.SessionID != "ses_fake" {
		t.Fatalf("timeout occurred before the session became abortable: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(aborted); err != nil {
		t.Fatalf("session abort was not requested: %v termination=%+v logs=%s", err, termination, openCodeFailureLogs(dir, "run_timeout"))
	}
	if processAlive(pid) {
		t.Fatalf("opencode server process %d survived timeout", pid)
	}
}

func TestOpenCodeTimeoutRetriesSessionAbortBeforeStoppingServer(t *testing.T) {
	requireLoopbackListener(t)
	dir := t.TempDir()
	fake := openCodeHelperCommand(t, dir)
	canonicalDir, err := config.CanonicalRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	aborted := filepath.Join(canonicalDir, "aborted")
	t.Setenv("AGENT_LOOP_OPENCODE_MODE", "timeout")
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	cfg.Worker.Timeout.Duration = 3 * time.Second
	cfg.Worker.TimeoutGrace.Duration = 100 * time.Millisecond
	result, err := (OpenCode{StateDir: dir}).Run(context.Background(), cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_abort_retry", Attempts: 1}, "", nil)
	var termination *TerminationError
	if !errors.As(err, &termination) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if result.SessionID != "ses_fake" {
		t.Fatalf("timeout occurred before the session became abortable: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(aborted); err != nil {
		t.Fatalf("session abort was not retried: %v termination=%+v logs=%s", err, termination, openCodeFailureLogs(dir, "run_abort_retry"))
	}
}

func TestOpenCodeServerExitDuringStartup(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := backendTestConfig(dir, "opencode", fake, "opencode-go/test", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err := (OpenCode{StateDir: dir}).Run(ctx, cfg, gh.Issue{Number: 1}, state.Issue{RunID: "run_startup_exit", Attempts: 1}, "", nil)
	elapsed := time.Since(start)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(err.Error(), "start opencode server") {
		t.Fatalf("expected startup exit status 1, got %v", err)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("startup exit took %s", elapsed)
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

func TestOpenCodeListeningWriterRequiresConcreteLoopbackPortAnnouncement(t *testing.T) {
	writer := newOpenCodeListeningWriter()
	if _, err := writer.Write([]byte("startup\nopencode server listening on http://127.0.0.1:")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("43210\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case port := <-writer.port:
		if port != 43210 {
			t.Fatalf("port=%d", port)
		}
	default:
		t.Fatal("missing port announcement")
	}

	invalid := newOpenCodeListeningWriter()
	_, _ = invalid.Write([]byte("opencode server listening on http://127.0.0.1:0\n"))
	select {
	case port := <-invalid.port:
		t.Fatalf("invalid port announcement was accepted: %d", port)
	default:
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
	if parsed, err := strconv.Atoi(port); err != nil || parsed < 0 || parsed > 65535 {
		os.Exit(2)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "helper server failed to listen: %v\n", err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintf(os.Stdout, "opencode server listening on http://127.0.0.1:%d\n", listener.Addr().(*net.TCPAddr).Port)
	mux := http.NewServeMux()
	var abortFailOnce atomic.Bool
	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		username, password, ok := r.BasicAuth()
		if !ok || username != os.Getenv("OPENCODE_SERVER_USERNAME") || password != os.Getenv("OPENCODE_SERVER_PASSWORD") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		if requireAuth(w, r) {
			_, _ = io.WriteString(w, `{"healthy":true}`)
		}
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		var request struct {
			Title string `json:"title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		abortFailOnce.Store(strings.Contains(request.Title, "run_abort_retry"))
		_, _ = io.WriteString(w, `{"id":"ses_fake"}`)
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/abort") {
			directory := r.URL.Query().Get("directory")
			aborted := filepath.Join(directory, "aborted")
			_, _ = fmt.Fprintf(os.Stderr, "abort marker=%q\n", aborted)
			if directory == "" || !filepath.IsAbs(directory) {
				http.Error(w, "abort directory is missing or not absolute", http.StatusInternalServerError)
				return
			}
			firstAttempt := aborted + ".attempt"
			if abortFailOnce.Load() {
				if _, err := os.Stat(firstAttempt); errors.Is(err, os.ErrNotExist) {
					if err := os.WriteFile(firstAttempt, []byte("attempted"), 0o600); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					_, _ = io.WriteString(w, "false")
					return
				}
			}
			if err := os.WriteFile(aborted, []byte("aborted"), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
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
		if os.Getenv("AGENT_LOOP_OPENCODE_MODE") == "delayed" {
			time.Sleep(35 * time.Second)
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
	if err := http.Serve(listener, mux); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "helper server stopped: %v\n", err)
		os.Exit(2)
	}
}

func openCodeFailureLogs(dir, runID string) string {
	var combined strings.Builder
	for _, name := range []string{"opencode-server.log", "opencode-server.stderr.log"} {
		data, _ := os.ReadFile(filepath.Join(dir, "runs", runID, name))
		combined.WriteString(name + ":" + string(data) + "\n")
	}
	return combined.String()
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

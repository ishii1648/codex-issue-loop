package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

func TestCodexAppServerExtendedContract(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	capture := filepath.Join(dir, "protocol.jsonl")
	t.Setenv("AGENT_LOOP_APP_SERVER_CAPTURE", capture)
	cfg := backendTestConfig(dir, "codex", fake, "gpt-test", "")
	cfg.Worker.AppServer.Enabled = true
	cfg.Worker.AppServer.GoalTokenBudget = 4321
	cfg.Worker.AppServer.GoalTimeBudget = config.Duration{Duration: 30 * time.Minute}
	adapter := CodexAppServer{
		Exec: Codex{StateDir: dir, RuntimeVersion: "0.147.0"}, StateDir: dir, RuntimeVersion: "0.147.0",
	}
	current := state.Issue{RunID: "run_goal", SessionID: "thread_saved", ExecutionProfile: "extended"}
	var spawn ProcessStart
	canonicalDir, err := config.CanonicalRepoPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53, Title: "Goal adapter"}, current, "continue", func(start ProcessStart) error {
		spawn = start
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.SessionID != "thread_saved" || result.Identity.Provider != "app-server-goal" {
		t.Fatalf("result=%+v", result)
	}
	if spawn.ExpectedCWD != canonicalDir || spawn.ActualCWD != canonicalDir {
		t.Fatalf("App Server spawn=%+v", spawn)
	}
	if result.Goal == nil || result.Goal.Status != "complete" || result.Goal.TokensUsed != 34 || result.Goal.InputTokens != 21 || result.Goal.OutputTokens != 13 || result.Goal.TimeBudgetSeconds != 1800 {
		t.Fatalf("goal=%+v", result.Goal)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	protocol := string(data)
	for _, method := range []string{"initialize", "thread/resume", "thread/goal/get", "thread/goal/set", "turn/start", "thread/goal/clear"} {
		if !strings.Contains(protocol, `"method":"`+method+`"`) {
			t.Fatalf("protocol does not contain %s:\n%s", method, protocol)
		}
	}
	if !strings.Contains(protocol, `"decision":"decline"`) || !strings.Contains(protocol, `"tokenBudget":4321`) {
		t.Fatalf("approval or budget contract missing:\n%s", protocol)
	}
}

func TestCodexAppServerConvertsRequestUserInput(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	t.Setenv("AGENT_LOOP_APP_SERVER_MODE", "needs_input")
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_input", SessionID: "thread_saved", ExecutionProfile: "extended"}
	result, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53}, current, "continue", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "needs_input" || result.Question == nil || result.Question.Text != "Which behavior?" || len(result.Question.Options) != 2 || result.Goal == nil || result.Goal.Status != "blocked" {
		t.Fatalf("result=%+v", result)
	}
}

func TestCodexAppServerUsesSteerForRejoinedActiveTurn(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	capture := filepath.Join(dir, "protocol.jsonl")
	t.Setenv("AGENT_LOOP_APP_SERVER_CAPTURE", capture)
	t.Setenv("AGENT_LOOP_APP_SERVER_MODE", "active")
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_steer", SessionID: "thread_saved", ExecutionProfile: "extended"}
	if _, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53}, current, "steer", nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(capture)
	if !strings.Contains(string(data), `"method":"turn/steer"`) || strings.Contains(string(data), `"method":"turn/start"`) {
		t.Fatalf("unexpected protocol:\n%s", data)
	}
}

func TestCodexAppServerStartsThreadForFreshExtendedRetry(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	capture := filepath.Join(dir, "protocol.jsonl")
	t.Setenv("AGENT_LOOP_APP_SERVER_CAPTURE", capture)
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_fresh", ExecutionProfile: "extended"}
	result, err := adapter.Run(context.Background(), cfg, gh.Issue{Number: 53, Title: "Goal adapter"}, current, "retry", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != "thread_saved" || result.Goal == nil || result.Goal.ThreadID != "thread_saved" {
		t.Fatalf("fresh thread was not persisted: %+v", result)
	}
	data, _ := os.ReadFile(capture)
	if !strings.Contains(string(data), `"method":"thread/start"`) || strings.Contains(string(data), `"method":"thread/resume"`) {
		t.Fatalf("unexpected fresh protocol:\n%s", data)
	}
}

func TestCodexAppServerDisconnectAfterTurnStartDoesNotFallback(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	t.Setenv("AGENT_LOOP_APP_SERVER_MODE", "disconnect")
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_disconnect", SessionID: "thread_saved", ExecutionProfile: "extended"}
	result, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53}, current, "continue", nil)
	if err == nil || !strings.Contains(err.Error(), "app-server protocol") {
		t.Fatalf("transport disconnect was not returned for durable retry: result=%+v err=%v", result, err)
	}
	if result.SessionID != "thread_saved" || result.Goal == nil {
		t.Fatalf("durable resume state was lost: %+v", result)
	}
}

func TestCodexAppServerConnectionFailureFallsBackToExecResume(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	invocations := filepath.Join(dir, "invocations")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$1" >> %q
if [ "$1" = "app-server" ]; then exit 9; fi
result_path=''
previous=''
for argument in "$@"; do
  if [ "$previous" = "--output-last-message" ]; then result_path="$argument"; fi
  previous="$argument"
done
cat >/dev/null
printf '%%s\n' '{"version":1,"status":"completed","execution_profile":"extended","summary":"fallback","question":null,"tests":[],"git":null,"retry":null}' > "$result_path"
printf '%%s\n' '{"thread_id":"thread_saved"}'
`, invocations)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_fallback", SessionID: "thread_saved", ExecutionProfile: "extended"}
	result, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53}, current, "continue", nil)
	if err != nil || result.Status != "completed" || result.Summary != "fallback" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	data, _ := os.ReadFile(invocations)
	if string(data) != "app-server\nexec\n" {
		t.Fatalf("expected safe app-server to exec fallback, got %q", data)
	}
}

func TestCodexAppServerGoalTimeBudgetIsPersistedAsTerminal(t *testing.T) {
	dir := t.TempDir()
	fake := appServerHelperCommand(t, dir)
	t.Setenv("AGENT_LOOP_APP_SERVER_MODE", "timeout")
	cfg := backendTestConfig(dir, "codex", fake, "", "")
	cfg.Worker.AppServer.Enabled = true
	cfg.Worker.Timeout.Duration = time.Second
	cfg.Worker.TimeoutGrace.Duration = 100 * time.Millisecond
	cfg.Worker.AppServer.GoalTimeBudget = config.Duration{Duration: 150 * time.Millisecond}
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	current := state.Issue{RunID: "run_budget", SessionID: "thread_saved", ExecutionProfile: "extended"}
	result, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 53}, current, "continue", nil)
	var termination *TerminationError
	if !errors.As(err, &termination) || result.Goal == nil || result.Goal.Status != "budgetLimited" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCodexAppServerHelperProcess(t *testing.T) {
	if os.Getenv("AGENT_LOOP_APP_SERVER_HELPER") != "1" {
		return
	}
	runFakeAppServer()
	os.Exit(0)
}

func appServerHelperCommand(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "codex")
	content := fmt.Sprintf("#!/bin/sh\nAGENT_LOOP_APP_SERVER_HELPER=1 exec %q -test.run=TestCodexAppServerHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

func runFakeAppServer() {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	mode := os.Getenv("AGENT_LOOP_APP_SERVER_MODE")
	capturePath := os.Getenv("AGENT_LOOP_APP_SERVER_CAPTURE")
	goalStatus := "active"
	pendingTurnResponse := json.RawMessage(nil)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if capturePath != "" {
			file, _ := os.OpenFile(capturePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if file != nil {
				_, _ = file.Write(append(line, '\n'))
				_ = file.Close()
			}
		}
		var message struct {
			ID     json.RawMessage        `json:"id"`
			Method string                 `json:"method"`
			Params map[string]interface{} `json:"params"`
		}
		_ = json.Unmarshal(line, &message)
		if message.Method == "" {
			if string(message.ID) == "800" && pendingTurnResponse != nil {
				if mode == "disconnect" {
					return
				}
				_ = encoder.Encode(map[string]any{"id": pendingTurnResponse, "result": map[string]any{"turn": fakeTurn("turn_1", "inProgress")}})
				if mode == "timeout" {
					pendingTurnResponse = nil
					continue
				}
				emitFakeTurn(encoder, mode, "turn_1")
				pendingTurnResponse = nil
			}
			if string(message.ID) == "900" {
				emitFakeCompleted(encoder, "turn_1")
			}
			continue
		}
		switch message.Method {
		case "initialize":
			respondFake(encoder, message.ID, map[string]any{"userAgent": "fake", "platformFamily": "unix", "platformOs": "macos", "codexHome": "/tmp"})
		case "initialized":
		case "thread/start":
			respondFake(encoder, message.ID, map[string]any{"thread": map[string]any{"id": "thread_saved", "turns": []any{}}})
		case "thread/resume":
			turns := []any{}
			if mode == "active" {
				turns = append(turns, fakeTurn("turn_active", "inProgress"))
			}
			respondFake(encoder, message.ID, map[string]any{"thread": map[string]any{"id": "thread_saved", "turns": turns}})
		case "thread/goal/get":
			respondFake(encoder, message.ID, map[string]any{"goal": fakeGoal(goalStatus)})
		case "thread/goal/set":
			if value, ok := message.Params["status"].(string); ok {
				goalStatus = value
			}
			respondFake(encoder, message.ID, map[string]any{"goal": fakeGoal(goalStatus)})
		case "thread/goal/clear":
			respondFake(encoder, message.ID, map[string]any{})
		case "turn/start":
			pendingTurnResponse = append([]byte(nil), message.ID...)
			_ = encoder.Encode(map[string]any{"id": 800, "method": "item/commandExecution/requestApproval", "params": map[string]any{}})
		case "turn/steer":
			respondFake(encoder, message.ID, map[string]any{"turnId": "turn_active"})
			emitFakeTurn(encoder, mode, "turn_active")
		}
	}
}

func respondFake(encoder *json.Encoder, id json.RawMessage, result any) {
	_ = encoder.Encode(map[string]any{"id": id, "result": result})
}

func fakeGoal(status string) map[string]any {
	return map[string]any{
		"threadId": "thread_saved", "objective": "Complete GitHub Issue #53 according to the worker prompt and repository acceptance criteria", "status": status,
		"tokenBudget": 4321, "tokensUsed": 34, "timeUsedSeconds": 7, "createdAt": 1, "updatedAt": 2,
	}
}

func fakeTurn(id, status string) map[string]any {
	return map[string]any{"id": id, "status": status, "items": []any{}}
}

func emitFakeTurn(encoder *json.Encoder, mode, turnID string) {
	_ = encoder.Encode(map[string]any{"method": "thread/tokenUsage/updated", "params": map[string]any{
		"threadId": "thread_saved", "turnId": turnID, "tokenUsage": map[string]any{"total": map[string]any{
			"inputTokens": 21, "cachedInputTokens": 5, "outputTokens": 13, "reasoningOutputTokens": 3, "totalTokens": 34,
		}},
	}})
	if mode == "needs_input" {
		_ = encoder.Encode(map[string]any{"id": 900, "method": "item/tool/requestUserInput", "params": map[string]any{
			"threadId": "thread_saved", "turnId": turnID, "itemId": "item_input", "isBlocking": true,
			"questions": []any{
				map[string]any{"id": "behavior", "header": "Behavior", "question": "Which behavior?", "options": []any{
					map[string]any{"label": "Safe", "description": "Use safe behavior"},
					map[string]any{"label": "Fast", "description": "Use fast behavior"},
				}},
			},
		}})
		return
	}
	emitFakeCompleted(encoder, turnID)
}

func emitFakeCompleted(encoder *json.Encoder, turnID string) {
	final := `{"version":1,"status":"completed","execution_profile":"extended","summary":"done","question":null,"tests":[],"git":null,"retry":null}`
	_ = encoder.Encode(map[string]any{"method": "item/completed", "params": map[string]any{
		"threadId": "thread_saved", "turnId": turnID, "completedAtMs": 1,
		"item": map[string]any{"id": "message_1", "type": "agentMessage", "text": final},
	}})
	_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{
		"threadId": "thread_saved", "turn": fakeTurn(turnID, "completed"),
	}})
}

func TestCodexAppServerIntegration(t *testing.T) {
	if os.Getenv("AGENT_LOOP_CODEX_APP_SERVER_INTEGRATION") != "1" {
		t.Skip("set AGENT_LOOP_CODEX_APP_SERVER_INTEGRATION=1 to run against the installed Codex")
	}
	command := os.Getenv("AGENT_LOOP_CODEX_COMMAND")
	if command == "" {
		command = "codex"
	}
	dir := t.TempDir()
	cfg := backendTestConfig(dir, "codex", command, "", "")
	cfg.Worker.AppServer.Enabled = true
	cfg.Worker.AppServer.GoalTokenBudget = 2000
	cfg.Worker.AppServer.GoalTimeBudget = config.Duration{Duration: 2 * time.Minute}
	adapter := CodexAppServer{Exec: Codex{StateDir: dir}, StateDir: dir}
	// A real persisted thread id is intentionally required: the integration
	// test is opt-in and validates resume/Goal without creating queue state.
	threadID := os.Getenv("AGENT_LOOP_CODEX_THREAD_ID")
	if threadID == "" {
		t.Skip("set AGENT_LOOP_CODEX_THREAD_ID to a disposable persisted thread")
	}
	current := state.Issue{RunID: "run_integration", SessionID: threadID, ExecutionProfile: "extended"}
	_, err := adapter.Resume(context.Background(), cfg, gh.Issue{Number: 1, Title: "App Server integration probe"}, current,
		"Return the required worker-result JSON with status blocked and summary integration probe only; do not modify files.", nil)
	if err != nil {
		t.Fatal(err)
	}
}

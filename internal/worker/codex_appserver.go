package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

// CodexAppServer is an opt-in adapter layered over the existing codex exec
// adapter. Initial runs remain on exec so only a durable extended execution is
// assigned a thread Goal.
type CodexAppServer struct {
	Exec           Codex
	StateDir       string
	Secrets        []string
	RuntimeVersion string
}

func (c CodexAppServer) ID() string { return "codex" }

func (c CodexAppServer) Capabilities() Capabilities {
	capabilities := c.Exec.Capabilities()
	capabilities.ThreadGoal = true
	return capabilities
}

func (c CodexAppServer) Run(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, promptSuffix string, started Started) (Result, error) {
	if current.ExecutionProfile != "extended" {
		return c.Exec.Run(ctx, cfg, issue, current, promptSuffix, started)
	}
	result, err := c.execute(ctx, cfg, issue, current, BuildPrompt(cfg, issue, current, promptSuffix), true, started)
	var appErr *appServerError
	if errors.As(err, &appErr) && appErr.safeFallback {
		fallbackSuffix := "The optional Codex App Server Goal adapter was unavailable before a turn started. Continue safely with codex exec in the existing worktree and use durable state.\n\n" + promptSuffix
		return c.Exec.Run(ctx, cfg, issue, current, fallbackSuffix, started)
	}
	return result, err
}

func (c CodexAppServer) Resume(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started Started) (Result, error) {
	if current.ExecutionProfile != "extended" || current.SessionID == "" {
		return c.Exec.Resume(ctx, cfg, issue, current, prompt, started)
	}
	result, err := c.execute(ctx, cfg, issue, current, prompt, false, started)
	var appErr *appServerError
	if errors.As(err, &appErr) && appErr.safeFallback {
		fallbackPrompt := "The optional Codex App Server Goal adapter was unavailable before a turn started. Continue safely with codex exec resume; preserve the existing worktree and durable state.\n\n" + prompt
		return c.Exec.Resume(ctx, cfg, issue, current, fallbackPrompt, started)
	}
	return result, err
}

type appServerError struct {
	err          error
	safeFallback bool
}

func (e *appServerError) Error() string { return e.err.Error() }
func (e *appServerError) Unwrap() error { return e.err }

func (c CodexAppServer) execute(parent context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, freshThread bool, started Started) (Result, error) {
	identity := Identity{Backend: c.ID(), RuntimeVersion: c.RuntimeVersion, Provider: "app-server-goal", RequestedModel: cfg.Worker.Model, ResolvedModel: cfg.Worker.Model}
	workspace, err := config.CanonicalRepoPath(cfg.RepoPath)
	if err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, fmt.Errorf("resolve Codex App Server workspace: %w", err)
	}
	cfg.RepoPath = workspace
	runDir := filepath.Join(c.StateDir, "runs", current.RunID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, err
	}
	if err := os.Chmod(runDir, 0o700); err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, err
	}
	policy := retention.Policy{MaxBytes: cfg.Logs.RotateBytes, MaxAge: cfg.Logs.RotateInterval.Duration, Keep: cfg.Logs.Generations}
	stdoutLog, err := retention.OpenWriter(filepath.Join(runDir, "codex-app-server.jsonl"), policy)
	if err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, err
	}
	defer stdoutLog.Close()
	stderrLog, err := retention.OpenWriter(filepath.Join(runDir, "codex-app-server.stderr.log"), policy)
	if err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, err
	}
	defer stderrLog.Close()

	limit := cfg.Worker.Timeout.Duration
	if goalLimit := cfg.Worker.AppServer.GoalTimeBudget.Duration; goalLimit > 0 && goalLimit < limit {
		limit = goalLimit
	}
	ctx, cancel := context.WithTimeout(parent, limit)
	defer cancel()

	command := cfg.Worker.EffectiveCommand()
	cmd := exec.Command(command, "app-server", "--stdio", "--config", `approval_policy="never"`)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, &appServerError{err: fmt.Errorf("open codex app-server stdin: %w", err), safeFallback: true}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, &appServerError{err: fmt.Errorf("open codex app-server stdout: %w", err), safeFallback: true}
	}
	safeStdout := redact.NewLineWriterWithSecrets(stdoutLog, c.Secrets)
	safeStderr := redact.NewLineWriterWithSecrets(stderrLog, c.Secrets)
	cmd.Stderr = safeStderr
	if err := cmd.Start(); err != nil {
		return Result{SessionID: current.SessionID, Identity: identity}, &appServerError{err: fmt.Errorf("start codex app-server: %w", err), safeFallback: true}
	}
	if started != nil {
		if err := started(processStart(cmd, workspace)); err != nil {
			_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return Result{SessionID: current.SessionID, Identity: identity}, fmt.Errorf("record app-server process: %w", err)
		}
	}

	protocol := &appServerProtocol{
		reader: bufio.NewScanner(io.TeeReader(stdout, safeStdout)), writer: stdin,
		threadID: current.SessionID, objective: goalObjective(issue),
		freshThread:       freshThread,
		tokenBudget:       cfg.Worker.AppServer.GoalTokenBudget,
		timeBudgetSeconds: int64(cfg.Worker.AppServer.GoalTimeBudget.Duration / time.Second),
	}
	protocol.reader.Buffer(make([]byte, 64*1024), 4*1024*1024)
	type protocolOutcome struct {
		result Result
		err    error
	}
	finished := make(chan protocolOutcome, 1)
	go func() {
		result, runErr := protocol.run(cfg, prompt)
		finished <- protocolOutcome{result: result, err: runErr}
	}()

	var outcome protocolOutcome
	select {
	case outcome = <-finished:
	case <-ctx.Done():
		_ = stdin.Close()
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
		forced := waitAppServerProcess(cmd, cfg.Worker.TimeoutGrace.Duration)
		_ = safeStdout.Flush()
		_ = safeStderr.Flush()
		goal := protocol.goalSnapshot()
		if goal != nil && cfg.Worker.AppServer.GoalTimeBudget.Duration > 0 && cfg.Worker.AppServer.GoalTimeBudget.Duration <= cfg.Worker.Timeout.Duration && ctx.Err() == context.DeadlineExceeded {
			goal.Status = "budgetLimited"
			goal.TimeUsedSeconds = int64(limit / time.Second)
		}
		sessionID := protocol.sessionID()
		if sessionID == "" {
			sessionID = current.SessionID
		}
		return Result{SessionID: sessionID, Identity: identity, Goal: goal}, &TerminationError{
			Timeout: limit, GracePeriod: cfg.Worker.TimeoutGrace.Duration, Forced: forced, Cause: ctx.Err(),
		}
	}
	_ = stdin.Close()
	_ = waitAppServerProcess(cmd, cfg.Worker.TimeoutGrace.Duration)
	flushErr := safeStdout.Flush()
	if err := safeStderr.Flush(); flushErr == nil {
		flushErr = err
	}
	outcome.result.SessionID = protocol.sessionID()
	if outcome.result.SessionID == "" {
		outcome.result.SessionID = current.SessionID
	}
	outcome.result.Identity = identity
	if outcome.result.Goal == nil {
		outcome.result.Goal = protocol.goalSnapshot()
	}
	if outcome.err == nil && flushErr != nil {
		outcome.err = flushErr
	}
	if outcome.err != nil {
		return outcome.result, outcome.err
	}
	internalGoal := outcome.result.Goal
	safeResult, err := redact.Marshal(outcome.result, c.Secrets)
	if err != nil {
		return outcome.result, fmt.Errorf("sanitize App Server result: %w", err)
	}
	if err := json.Unmarshal(safeResult, &outcome.result); err != nil {
		return outcome.result, fmt.Errorf("decode sanitized App Server result: %w", err)
	}
	outcome.result.SessionID = protocol.sessionID()
	if outcome.result.SessionID == "" {
		outcome.result.SessionID = current.SessionID
	}
	outcome.result.Identity = identity
	outcome.result.Goal = internalGoal
	if err := outcome.result.Validate(); err != nil {
		return outcome.result, err
	}
	return outcome.result, nil
}

func waitAppServerProcess(cmd *exec.Cmd, grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return false
	case <-time.After(grace):
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return true
	}
}

func goalObjective(issue gh.Issue) string {
	// GitHub titles are untrusted prompt data. Keep the Goal objective stable
	// and point at the separately bounded worker prompt instead of copying it.
	return fmt.Sprintf("Complete GitHub Issue #%d according to the worker prompt and repository acceptance criteria", issue.Number)
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type appServerProtocol struct {
	reader  *bufio.Scanner
	writer  io.Writer
	writeMu sync.Mutex
	nextID  int64

	threadID          string
	freshThread       bool
	turnID            string
	objective         string
	tokenBudget       int64
	timeBudgetSeconds int64
	turnStarted       bool
	lastMessage       string
	request           *Question
	goal              *Goal
	tokenUsage        tokenUsage
	stateMu           sync.RWMutex
}

type tokenUsage struct {
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	TotalTokens       int64 `json:"totalTokens"`
}

func (p *appServerProtocol) run(cfg config.Config, prompt string) (Result, error) {
	if _, err := p.call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "codex-issue-loop", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		return p.failed(err)
	}
	if err := p.notify("initialized", map[string]any{}); err != nil {
		return p.failed(err)
	}
	activeTurn := ""
	if p.freshThread {
		startRaw, err := p.call("thread/start", map[string]any{
			"cwd": cfg.RepoPath, "approvalPolicy": "never", "sandbox": cfg.Worker.Sandbox,
			"model": nullableString(cfg.Worker.Model), "ephemeral": false,
		})
		if err != nil {
			return p.failed(err)
		}
		var started struct {
			Thread struct {
				ID string `json:"id"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(startRaw, &started); err != nil || started.Thread.ID == "" {
			return p.failed(fmt.Errorf("decode thread/start response: %w", err))
		}
		p.setThreadID(started.Thread.ID)
	} else {
		resumeRaw, err := p.call("thread/resume", map[string]any{
			"threadId": p.threadID, "cwd": cfg.RepoPath, "approvalPolicy": "never", "sandbox": cfg.Worker.Sandbox,
		})
		if err != nil {
			return p.failed(err)
		}
		activeTurn, err = parseResumedThread(resumeRaw, p.threadID)
		if err != nil {
			return p.failed(err)
		}
	}
	if _, err := p.call("thread/goal/get", map[string]any{"threadId": p.threadID}); err != nil {
		return p.failed(err)
	}
	budget := p.tokenBudget
	goalRaw, err := p.call("thread/goal/set", map[string]any{
		"threadId": p.threadID, "objective": p.objective, "status": "active", "tokenBudget": budget,
	})
	if err != nil {
		return p.failed(err)
	}
	p.updateGoal(goalRaw)

	input := []map[string]string{{"type": "text", "text": prompt}}
	p.turnStarted = true
	if activeTurn != "" {
		p.turnID = activeTurn
		if _, err := p.call("turn/steer", map[string]any{
			"threadId": p.threadID, "expectedTurnId": activeTurn, "input": input,
		}); err != nil {
			return p.failed(err)
		}
	} else {
		var outputSchema any
		if err := json.Unmarshal(schema, &outputSchema); err != nil {
			return Result{}, err
		}
		turnRaw, err := p.call("turn/start", map[string]any{
			"threadId": p.threadID, "input": input, "cwd": cfg.RepoPath,
			"approvalPolicy": "never",
			"model":          nullableString(cfg.Worker.Model), "outputSchema": outputSchema,
		})
		if err != nil {
			return p.failed(err)
		}
		var turnResponse struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if err := json.Unmarshal(turnRaw, &turnResponse); err != nil || turnResponse.Turn.ID == "" {
			return p.failed(fmt.Errorf("decode turn/start response: %w", err))
		}
		p.turnID = turnResponse.Turn.ID
	}

	if err := p.waitForTurn(); err != nil {
		return p.failed(err)
	}
	if p.request != nil {
		result := Result{Version: 1, Status: "needs_input", ExecutionProfile: "extended", Summary: "App Server requested user input", Question: p.request, Tests: []Test{}}
		return p.finish(result, "blocked")
	}
	if strings.TrimSpace(p.lastMessage) == "" {
		return p.failed(fmt.Errorf("turn completed without a final agent message"))
	}
	result, err := decodeResult([]byte(p.lastMessage))
	if err != nil {
		return p.failed(fmt.Errorf("decode App Server worker result: %w", err))
	}
	status := "paused"
	switch result.Status {
	case "completed":
		status = "complete"
	case "needs_input", "blocked":
		status = "blocked"
	}
	return p.finish(result, status)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func parseResumedThread(raw json.RawMessage, expectedThread string) (string, error) {
	var response struct {
		Thread struct {
			ID    string `json:"id"`
			Turns []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"turns"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return "", fmt.Errorf("decode thread/resume response: %w", err)
	}
	if response.Thread.ID != expectedThread {
		return "", fmt.Errorf("thread/resume returned thread %q, expected %q", response.Thread.ID, expectedThread)
	}
	for index := len(response.Thread.Turns) - 1; index >= 0; index-- {
		if response.Thread.Turns[index].Status == "inProgress" {
			return response.Thread.Turns[index].ID, nil
		}
	}
	return "", nil
}

func (p *appServerProtocol) finish(result Result, goalStatus string) (Result, error) {
	if terminal := p.goalTerminalStatus(); terminal == "usageLimited" || terminal == "budgetLimited" {
		goalStatus = terminal
	}
	goalRaw, err := p.call("thread/goal/set", map[string]any{"threadId": p.threadID, "status": goalStatus})
	if err != nil {
		return p.failed(err)
	}
	p.updateGoal(goalRaw)
	if getRaw, err := p.call("thread/goal/get", map[string]any{"threadId": p.threadID}); err == nil {
		p.updateGoal(getRaw)
	}
	result.Goal = p.goalSnapshot()
	if goalStatus == "complete" {
		_, _ = p.call("thread/goal/clear", map[string]any{"threadId": p.threadID})
	}
	return result, nil
}

func (p *appServerProtocol) failed(err error) (Result, error) {
	return Result{Goal: p.goalSnapshot()}, &appServerError{err: fmt.Errorf("codex app-server protocol: %w", err), safeFallback: !p.turnStarted}
}

func (p *appServerProtocol) call(method string, params any) (json.RawMessage, error) {
	p.nextID++
	id := p.nextID
	if err := p.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		message, err := p.read()
		if err != nil {
			return nil, err
		}
		if message.Method != "" {
			if err := p.handleServerMessage(message); err != nil {
				return nil, err
			}
			continue
		}
		var responseID int64
		if json.Unmarshal(message.ID, &responseID) != nil || responseID != id {
			continue
		}
		if message.Error != nil {
			return nil, fmt.Errorf("%s failed (%d): %s", method, message.Error.Code, message.Error.Message)
		}
		return message.Result, nil
	}
}

func (p *appServerProtocol) waitForTurn() error {
	for {
		message, err := p.read()
		if err != nil {
			return err
		}
		if message.Method == "turn/completed" {
			var completed struct {
				ThreadID string `json:"threadId"`
				Turn     struct {
					ID     string `json:"id"`
					Status string `json:"status"`
				} `json:"turn"`
			}
			if json.Unmarshal(message.Params, &completed) == nil && completed.ThreadID == p.threadID && completed.Turn.ID == p.turnID {
				if completed.Turn.Status == "failed" {
					return fmt.Errorf("turn %s failed", p.turnID)
				}
				return nil
			}
		}
		if message.Method != "" {
			if err := p.handleServerMessage(message); err != nil {
				return err
			}
		}
	}
}

func (p *appServerProtocol) handleServerMessage(message rpcMessage) error {
	switch message.Method {
	case "item/completed":
		var completed struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			Item     struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(message.Params, &completed) == nil && completed.ThreadID == p.threadID && completed.Item.Type == "agentMessage" {
			p.lastMessage = completed.Item.Text
		}
	case "thread/tokenUsage/updated":
		var usage struct {
			ThreadID   string `json:"threadId"`
			TokenUsage struct {
				Total tokenUsage `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(message.Params, &usage) == nil && usage.ThreadID == p.threadID {
			p.stateMu.Lock()
			p.tokenUsage = usage.TokenUsage.Total
			p.stateMu.Unlock()
		}
	case "thread/goal/updated":
		p.updateGoal(message.Params)
	case "item/tool/requestUserInput":
		p.request = questionFromAppServer(message.Params)
		answers := map[string]any{}
		var request struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		_ = json.Unmarshal(message.Params, &request)
		for _, question := range request.Questions {
			answers[question.ID] = map[string]any{"answers": []string{}}
		}
		return p.respond(message.ID, map[string]any{"answers": answers})
	default:
		if len(message.ID) > 0 && string(message.ID) != "null" {
			if strings.Contains(message.Method, "requestApproval") {
				if message.Method == "item/permissions/requestApproval" {
					return p.respond(message.ID, map[string]any{"permissions": map[string]any{}})
				}
				return p.respond(message.ID, map[string]any{"decision": "decline"})
			}
			return p.respondError(message.ID, -32601, "unsupported server request")
		}
	}
	return nil
}

func questionFromAppServer(raw json.RawMessage) *Question {
	var request struct {
		Questions []struct {
			ID       string `json:"id"`
			Question string `json:"question"`
			IsOther  bool   `json:"isOther"`
			IsSecret bool   `json:"isSecret"`
			Options  []struct {
				Label       string `json:"label"`
				Description string `json:"description"`
			} `json:"options"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(raw, &request)
	texts := make([]string, 0, len(request.Questions))
	for _, item := range request.Questions {
		text := item.Question
		if item.IsSecret {
			text += " (秘密値は回答に含めないでください)"
		}
		texts = append(texts, text)
	}
	question := &Question{
		Text: strings.Join(texts, "\n"), Reason: "Codex App Serverのtool/requestUserInputを永続requestへ変換しました。",
		Options: []state.Option{}, AllowFreeText: len(request.Questions) != 1,
	}
	if len(request.Questions) == 1 {
		item := request.Questions[0]
		question.AllowFreeText = item.IsOther || len(item.Options) == 0
		for index, option := range item.Options {
			if index == 3 {
				break
			}
			id := fmt.Sprintf("option_%d", index+1)
			question.Options = append(question.Options, state.Option{ID: id, Label: option.Label})
			if index == 0 {
				question.RecommendedOption = id
			}
		}
	}
	if question.Text == "" {
		question.Text = "Codexが続行に必要な入力を要求しました。"
	}
	return question
}

func (p *appServerProtocol) updateGoal(raw json.RawMessage) {
	var envelope struct {
		Goal struct {
			ThreadID        string `json:"threadId"`
			Objective       string `json:"objective"`
			Status          string `json:"status"`
			TokenBudget     *int64 `json:"tokenBudget"`
			TokensUsed      int64  `json:"tokensUsed"`
			TimeUsedSeconds int64  `json:"timeUsedSeconds"`
			UpdatedAt       int64  `json:"updatedAt"`
		} `json:"goal"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Goal.ThreadID == "" {
		return
	}
	goal := &Goal{
		ThreadID: envelope.Goal.ThreadID, Objective: envelope.Goal.Objective, Status: envelope.Goal.Status,
		TokenBudget: envelope.Goal.TokenBudget, TimeBudgetSeconds: p.timeBudgetSeconds,
		TokensUsed: envelope.Goal.TokensUsed, TimeUsedSeconds: envelope.Goal.TimeUsedSeconds,
		UpdatedAt: envelope.Goal.UpdatedAt,
	}
	p.stateMu.Lock()
	p.goal = goal
	p.stateMu.Unlock()
}

func (p *appServerProtocol) goalSnapshot() *Goal {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.goal == nil && p.threadID == "" {
		return nil
	}
	goal := Goal{ThreadID: p.threadID, Objective: p.objective, Status: "active", TimeBudgetSeconds: p.timeBudgetSeconds}
	if p.goal != nil {
		goal = *p.goal
	}
	if goal.TokenBudget == nil && p.tokenBudget > 0 {
		budget := p.tokenBudget
		goal.TokenBudget = &budget
	}
	if p.tokenUsage.TotalTokens > goal.TokensUsed {
		goal.TokensUsed = p.tokenUsage.TotalTokens
	}
	goal.InputTokens = p.tokenUsage.InputTokens
	goal.CachedInputTokens = p.tokenUsage.CachedInputTokens
	goal.OutputTokens = p.tokenUsage.OutputTokens
	return &goal
}

func (p *appServerProtocol) goalTerminalStatus() string {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	if p.goal == nil {
		return ""
	}
	return p.goal.Status
}

func (p *appServerProtocol) setThreadID(threadID string) {
	p.stateMu.Lock()
	p.threadID = threadID
	p.stateMu.Unlock()
}

func (p *appServerProtocol) sessionID() string {
	p.stateMu.RLock()
	defer p.stateMu.RUnlock()
	return p.threadID
}

func (p *appServerProtocol) notify(method string, params any) error {
	return p.write(map[string]any{"method": method, "params": params})
}

func (p *appServerProtocol) respond(id json.RawMessage, result any) error {
	return p.write(map[string]any{"id": id, "result": result})
}

func (p *appServerProtocol) respondError(id json.RawMessage, code int, message string) error {
	return p.write(map[string]any{"id": id, "error": rpcError{Code: code, Message: message}})
}

func (p *appServerProtocol) write(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = p.writer.Write(data)
	return err
}

func (p *appServerProtocol) read() (rpcMessage, error) {
	if !p.reader.Scan() {
		if err := p.reader.Err(); err != nil {
			return rpcMessage{}, err
		}
		return rpcMessage{}, io.EOF
	}
	var message rpcMessage
	if err := json.Unmarshal(p.reader.Bytes(), &message); err != nil {
		return rpcMessage{}, fmt.Errorf("decode app-server message: %w", err)
	}
	return message, nil
}

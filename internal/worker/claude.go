package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	"github.com/ishii1648/codex-issue-loop/internal/state"
)

type ClaudeCode struct {
	StateDir        string
	Secrets         []string
	ResumeSupported *bool
	RuntimeVersion  string
}

func (c ClaudeCode) ID() string { return "claude-code" }

func (c ClaudeCode) Capabilities() Capabilities {
	resume := c.ResumeSupported == nil || *c.ResumeSupported
	return Capabilities{StructuredOutput: true, ResumableSession: resume, ModelSelection: true, VariantSelection: true, NonInteractivePolicy: true, WorkspaceIsolation: true}
}

func (c ClaudeCode) Run(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, suffix string, started Started) (Result, error) {
	return c.execute(ctx, cfg, current.RunID, "", BuildPrompt(cfg, issue, current, suffix), started)
}

func (c ClaudeCode) Resume(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started Started) (Result, error) {
	if c.ResumeSupported != nil && !*c.ResumeSupported {
		return c.Run(ctx, cfg, issue, current, "The installed Claude Code CLI cannot resume the prior session. Continue safely in a new session from the existing worktree and durable state.\n\n"+prompt, started)
	}
	if current.SessionID == "" {
		return Result{}, fmt.Errorf("cannot resume Issue #%d without a session ID", issue.Number)
	}
	return c.execute(ctx, cfg, current.RunID, current.SessionID, prompt, started)
}

func (c ClaudeCode) execute(parent context.Context, cfg config.Config, runID, sessionID, prompt string, started Started) (Result, error) {
	workspace, err := config.CanonicalRepoPath(cfg.RepoPath)
	if err != nil {
		return Result{}, fmt.Errorf("resolve Claude Code workspace: %w", err)
	}
	runDir := filepath.Join(c.StateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return Result{}, err
	}
	stdoutPath := filepath.Join(runDir, "claude-code.jsonl")
	stderrPath := filepath.Join(runDir, "claude-code.stderr.log")
	policy := retention.Policy{MaxBytes: cfg.Logs.RotateBytes, MaxAge: cfg.Logs.RotateInterval.Duration, Keep: cfg.Logs.Generations}
	stdout, err := retention.OpenWriter(stdoutPath, policy)
	if err != nil {
		return Result{}, err
	}
	defer stdout.Close()
	stderr, err := retention.OpenWriter(stderrPath, policy)
	if err != nil {
		return Result{}, err
	}
	defer stderr.Close()

	settings, _ := json.Marshal(map[string]any{
		"sandbox":     map[string]any{"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false},
		"permissions": map[string]any{"deny": []string{"Bash(*git commit*)", "Bash(*git push*)", "Bash(*gh pr create*)", "Bash(*gh pr merge*)"}},
	})
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--json-schema", string(schema),
		"--permission-mode", "dontAsk", "--settings", string(settings), "--strict-mcp-config", "--mcp-config", "{}", "--disallowedTools", "mcp__*",
		"Bash(*git commit*)", "Bash(*git push*)", "Bash(*gh pr create*)", "Bash(*gh pr merge*)"}
	if cfg.Worker.Model != "" {
		args = append(args, "--model", cfg.Worker.Model)
	}
	if cfg.Worker.Variant != "" {
		args = append(args, "--effort", cfg.Worker.Variant)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	ctx, cancel := context.WithTimeout(parent, cfg.Worker.Timeout.Duration)
	defer cancel()
	cmd := exec.Command(cfg.Worker.EffectiveCommand(), args...)
	cmd.Dir = workspace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = strings.NewReader(prompt)
	safeOut := redact.NewLineWriterWithSecrets(stdout, c.Secrets)
	safeErr := redact.NewLineWriterWithSecrets(stderr, c.Secrets)
	cmd.Stdout, cmd.Stderr = safeOut, safeErr
	runErr := cmd.Start()
	if runErr == nil && started != nil {
		if err := started(processStart(cmd, workspace)); err != nil {
			_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return Result{}, fmt.Errorf("record worker process: %w", err)
		}
	}
	if runErr == nil {
		runErr = waitForProcess(ctx, cmd, cfg.Worker.Timeout.Duration, cfg.Worker.TimeoutGrace.Duration)
	}
	if err := safeOut.Flush(); err != nil && runErr == nil {
		runErr = err
	}
	if err := safeErr.Flush(); err != nil && runErr == nil {
		runErr = err
	}

	result, discovered, parseErr := parseClaudeResult(stdoutPath)
	if discovered == "" {
		discovered = sessionID
	}
	identity := Identity{Backend: c.ID(), RuntimeVersion: c.RuntimeVersion, Provider: "anthropic", RequestedModel: cfg.Worker.Model, ResolvedModel: cfg.Worker.Model, Variant: cfg.Worker.Variant}
	result.SessionID, result.Identity = discovered, identity
	if parseErr != nil {
		if runErr != nil {
			return result, fmt.Errorf("claude-code worker failed: %w", runErr)
		}
		return result, parseErr
	}
	if err := sanitizeResult(filepath.Join(runDir, fmt.Sprintf("result-%d.json", time.Now().UnixNano())), &result, c.Secrets); err != nil {
		return result, err
	}
	result.SessionID, result.Identity = discovered, identity
	if err := result.Validate(); err != nil {
		return result, err
	}
	if runErr != nil {
		return result, fmt.Errorf("claude-code worker exited unsuccessfully: %w", runErr)
	}
	return result, nil
}

func parseClaudeResult(path string) (Result, string, error) {
	var history bytes.Buffer
	if err := retention.WriteHistory(&history, path); err != nil {
		return Result{}, "", err
	}
	scanner := bufio.NewScanner(&history)
	scanner.Buffer(make([]byte, 4096), 2*1024*1024)
	var result Result
	sessionID := ""
	found := false
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if id, _ := event["session_id"].(string); id != "" {
			sessionID = id
		}
		for _, candidate := range []any{event["structured_output"], event["result"]} {
			data, ok := resultJSON(candidate)
			if !ok {
				continue
			}
			parsed, err := decodeResult(data)
			if err == nil && parsed.Version == 1 {
				result, found = parsed, true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Result{}, sessionID, err
	}
	if !found {
		return Result{}, sessionID, fmt.Errorf("decode claude-code structured result: no schema-conforming result event")
	}
	return result, sessionID, nil
}

func resultJSON(value any) ([]byte, bool) {
	switch typed := value.(type) {
	case map[string]any:
		data, err := json.Marshal(typed)
		return data, err == nil
	case string:
		value := strings.TrimSpace(typed)
		if strings.HasPrefix(value, "```") {
			value = strings.TrimPrefix(value, "```json")
			value = strings.TrimPrefix(value, "```")
			value = strings.TrimSuffix(value, "```")
			value = strings.TrimSpace(value)
		}
		return []byte(value), strings.HasPrefix(value, "{")
	default:
		return nil, false
	}
}

func sanitizeResult(path string, result *Result, secrets []string) error {
	data, err := redact.Marshal(result, secrets)
	if err != nil {
		return fmt.Errorf("sanitize worker result: %w", err)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode sanitized worker result: %w", err)
	}
	if err := fsutil.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write sanitized worker result: %w", err)
	}
	return nil
}

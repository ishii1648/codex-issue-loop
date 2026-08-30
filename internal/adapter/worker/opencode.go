package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/config"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

type OpenCode struct {
	StateDir        string
	Secrets         []string
	ResumeSupported *bool
	RuntimeVersion  string
}

func (o OpenCode) ID() string { return "opencode" }

func (o OpenCode) Capabilities() Capabilities {
	resume := o.ResumeSupported == nil || *o.ResumeSupported
	return Capabilities{StructuredOutput: true, ResumableSession: resume, ModelSelection: true, VariantSelection: true, NonInteractivePolicy: true, WorkspaceIsolation: true}
}

func (o OpenCode) Run(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, suffix string, started Started) (Result, error) {
	return o.execute(ctx, cfg, current.RunID, "", BuildPrompt(cfg, issue, current, suffix), started)
}

func (o OpenCode) Resume(ctx context.Context, cfg config.Config, issue gh.Issue, current state.Issue, prompt string, started Started) (Result, error) {
	if o.ResumeSupported != nil && !*o.ResumeSupported {
		return o.Run(ctx, cfg, issue, current, "The installed OpenCode runtime cannot resume the prior session. Continue safely in a new session from the existing worktree and durable state.\n\n"+prompt, started)
	}
	if current.SessionID == "" {
		return Result{}, fmt.Errorf("cannot resume Issue #%d without a session ID", issue.Number)
	}
	return o.execute(ctx, cfg, current.RunID, current.SessionID, prompt, started)
}

func (o OpenCode) execute(parent context.Context, cfg config.Config, runID, sessionID, prompt string, started Started) (Result, error) {
	workspace, err := config.CanonicalRepoPath(cfg.RepoPath)
	if err != nil {
		return Result{}, fmt.Errorf("resolve OpenCode workspace: %w", err)
	}
	cfg.RepoPath = workspace
	provider, model, ok := strings.Cut(cfg.Worker.Model, "/")
	if !ok || provider == "" || model == "" {
		return Result{}, fmt.Errorf("opencode worker requires worker.model in provider/model format")
	}
	identity := Identity{Backend: o.ID(), RuntimeVersion: o.RuntimeVersion, Provider: provider, RequestedModel: cfg.Worker.Model, ResolvedModel: cfg.Worker.Model, Variant: cfg.Worker.Variant}
	runDir := filepath.Join(o.StateDir, "runs", runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return Result{Identity: identity}, err
	}
	policy := retention.Policy{MaxBytes: cfg.Logs.RotateBytes, MaxAge: cfg.Logs.RotateInterval.Duration, Keep: cfg.Logs.Generations}
	stdout, err := retention.OpenWriter(filepath.Join(runDir, "opencode-server.log"), policy)
	if err != nil {
		return Result{Identity: identity}, err
	}
	defer stdout.Close()
	stderr, err := retention.OpenWriter(filepath.Join(runDir, "opencode-server.stderr.log"), policy)
	if err != nil {
		return Result{Identity: identity}, err
	}
	defer stderr.Close()
	safeOut := redact.NewLineWriterWithSecrets(stdout, o.Secrets)
	safeErr := redact.NewLineWriterWithSecrets(stderr, o.Secrets)

	port, err := reserveLoopbackPort()
	if err != nil {
		return Result{Identity: identity}, err
	}
	inlineConfig := map[string]any{}
	if existing := os.Getenv("OPENCODE_CONFIG_CONTENT"); existing != "" {
		if err := json.Unmarshal([]byte(existing), &inlineConfig); err != nil {
			return Result{Identity: identity}, fmt.Errorf("decode existing OPENCODE_CONFIG_CONTENT: %w", err)
		}
	}
	inlineConfig["permission"] = map[string]any{
		"*": "allow", "external_directory": "deny", "doom_loop": "deny", "question": "deny",
		"bash": map[string]string{"*": "allow", "*git commit*": "deny", "*git push*": "deny", "*gh pr create*": "deny", "*gh pr merge*": "deny"},
	}
	policyJSON, _ := json.Marshal(inlineConfig)
	serverUsername := "agent-loop"
	serverPassword, err := randomOpenCodeCredential()
	if err != nil {
		return Result{Identity: identity}, err
	}
	cmd := exec.Command(cfg.Worker.EffectiveCommand(), "--pure", "serve", "--hostname", "127.0.0.1", "--port", strconv.Itoa(port))
	cmd.Dir = workspace
	cmd.Env = replaceEnvironment(os.Environ(), "OPENCODE_CONFIG_CONTENT", string(policyJSON))
	cmd.Env = replaceEnvironment(cmd.Env, "OPENCODE_SERVER_USERNAME", serverUsername)
	cmd.Env = replaceEnvironment(cmd.Env, "OPENCODE_SERVER_PASSWORD", serverPassword)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = safeOut, safeErr
	if err := cmd.Start(); err != nil {
		return Result{Identity: identity}, err
	}
	if started != nil {
		if err := started(processStart(cmd, workspace)); err != nil {
			_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			return Result{Identity: identity}, fmt.Errorf("record worker process: %w", err)
		}
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- waitForProcess(serverCtx, cmd, cfg.Worker.Timeout.Duration, cfg.Worker.TimeoutGrace.Duration)
	}()
	stop := func() error { stopServer(); return <-waitDone }

	ctx, cancel := context.WithTimeout(parent, cfg.Worker.Timeout.Duration)
	defer cancel()
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	client, transport := newOpenCodeClient(30*time.Second, serverUsername, serverPassword)
	defer transport.CloseIdleConnections()
	startupCtx, startupCancel := context.WithTimeout(ctx, 10*time.Second)
	err = waitForOpenCode(startupCtx, client, baseURL)
	startupCancel()
	if err != nil {
		stopErr := stop()
		_ = safeOut.Flush()
		_ = safeErr.Flush()
		if ctxErr := ctx.Err(); ctxErr != nil {
			termination := &TerminationError{Timeout: cfg.Worker.Timeout.Duration, GracePeriod: cfg.Worker.TimeoutGrace.Duration, Cause: ctxErr}
			var stopped *TerminationError
			if errors.As(stopErr, &stopped) {
				termination.Forced = stopped.Forced
			}
			return Result{SessionID: sessionID, Identity: identity}, termination
		}
		return Result{SessionID: sessionID, Identity: identity}, fmt.Errorf("start opencode server: %w", err)
	}
	if sessionID == "" {
		var created struct {
			ID string `json:"id"`
		}
		if err := openCodeJSON(ctx, client, http.MethodPost, baseURL+"/session?directory="+url.QueryEscape(cfg.RepoPath), map[string]string{"title": "agent-loop " + runID}, &created); err != nil {
			_ = stop()
			return Result{Identity: identity}, fmt.Errorf("create opencode session: %w", err)
		}
		sessionID = created.ID
		if sessionID == "" {
			_ = stop()
			return Result{Identity: identity}, fmt.Errorf("create opencode session: response has no session id")
		}
	}
	var schemaValue any
	if err := json.Unmarshal(schema, &schemaValue); err != nil {
		_ = stop()
		return Result{SessionID: sessionID, Identity: identity}, err
	}
	body := map[string]any{
		"model":   map[string]string{"providerID": provider, "modelID": model},
		"variant": cfg.Worker.Variant,
		"format":  map[string]any{"type": "json_schema", "schema": schemaValue, "retryCount": 2},
		"parts":   []map[string]string{{"type": "text", "text": prompt}},
	}
	var response struct {
		Info struct {
			Structured json.RawMessage `json:"structured"`
			ProviderID string          `json:"providerID"`
			ModelID    string          `json:"modelID"`
			Error      any             `json:"error"`
		} `json:"info"`
	}
	messageErr := openCodeJSON(ctx, client, http.MethodPost, baseURL+"/session/"+url.PathEscape(sessionID)+"/message?directory="+url.QueryEscape(cfg.RepoPath), body, &response)
	var abortErr error
	if ctx.Err() != nil {
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 2*time.Second)
		abortErr = abortOpenCodeSession(abortCtx, baseURL, sessionID, cfg.RepoPath, serverUsername, serverPassword)
		abortCancel()
	}
	stopErr := stop()
	_ = safeOut.Flush()
	_ = safeErr.Flush()
	if response.Info.ProviderID != "" {
		identity.Provider = response.Info.ProviderID
	}
	if response.Info.ModelID != "" {
		identity.ResolvedModel = identity.Provider + "/" + response.Info.ModelID
	}
	partial := Result{SessionID: sessionID, Identity: identity}
	if messageErr != nil {
		if ctx.Err() != nil {
			termination := &TerminationError{Timeout: cfg.Worker.Timeout.Duration, GracePeriod: cfg.Worker.TimeoutGrace.Duration, CleanupError: abortErr, Cause: ctx.Err()}
			var stopped *TerminationError
			if errors.As(stopErr, &stopped) {
				termination.Forced = stopped.Forced
			}
			return partial, termination
		}
		return partial, fmt.Errorf("opencode message failed: %s", redact.StringWithSecrets(messageErr.Error(), o.Secrets))
	}
	if response.Info.Error != nil {
		return partial, fmt.Errorf("opencode provider returned an error")
	}
	result, decodeErr := decodeResult(response.Info.Structured)
	if len(response.Info.Structured) == 0 || string(response.Info.Structured) == "null" || decodeErr != nil {
		return partial, fmt.Errorf("decode opencode structured result: response has no valid structured output")
	}
	result.SessionID, result.Identity = sessionID, identity
	if err := sanitizeResult(filepath.Join(runDir, fmt.Sprintf("result-%d.json", time.Now().UnixNano())), &result, o.Secrets); err != nil {
		return result, err
	}
	result.SessionID, result.Identity = sessionID, identity
	if err := result.Validate(); err != nil {
		return result, err
	}
	return result, nil
}

func abortOpenCodeSession(ctx context.Context, baseURL, sessionID, repoPath, username, password string) error {
	client, transport := newOpenCodeClient(0, username, password)
	defer transport.CloseIdleConnections()
	endpoint := baseURL + "/session/" + url.PathEscape(sessionID) + "/abort?directory=" + url.QueryEscape(repoPath)
	var lastErr error
	for {
		if err := openCodeJSON(ctx, client, http.MethodPost, endpoint, nil, nil); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(lastErr, ctx.Err())
		case <-timer.C:
		}
	}
}

func reserveLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve opencode loopback port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

func randomOpenCodeCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

type openCodeAuthTransport struct {
	base               http.RoundTripper
	username, password string
}

func (t openCodeAuthTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	copy.SetBasicAuth(t.username, t.password)
	return t.base.RoundTrip(copy)
}

func newOpenCodeClient(timeout time.Duration, username, password string) (*http.Client, *http.Transport) {
	transport := &http.Transport{Proxy: nil}
	return &http.Client{Timeout: timeout, Transport: openCodeAuthTransport{base: transport, username: username, password: password}}, transport
}

func waitForOpenCode(ctx context.Context, client *http.Client, baseURL string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/health", nil)
		if response, err := client.Do(req); err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func openCodeJSON(ctx context.Context, client *http.Client, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, truncateProviderDetail(string(data)))
	}
	if output != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return err
		}
	}
	return nil
}

func truncateProviderDetail(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 500 {
		value = value[:500] + "..."
	}
	return value
}

func replaceEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

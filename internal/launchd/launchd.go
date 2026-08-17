package launchd

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

type Manager struct {
	Layout    layout.Layout
	Launchctl string
}

type Status struct {
	Loaded         bool   `json:"loaded"`
	Running        bool   `json:"running"`
	State          string `json:"state,omitempty"`
	PID            int    `json:"pid,omitempty"`
	LastExitStatus *int   `json:"last_exit_status,omitempty"`
	Raw            string `json:"raw,omitempty"`
}

func (m Manager) WritePlist(entry registry.Entry, binary string) error {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(entry.RepoPath) {
		return fmt.Errorf("launchd paths must be absolute")
	}
	if err := os.MkdirAll(m.Layout.LaunchAgents, 0o700); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	pathEnv := "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	if entry.EnvironmentPath != "" {
		pathEnv = entry.EnvironmentPath
	}
	stateDir := m.Layout.RepoDir(entry.RepoID)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory for launchd logs: %w", err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return fmt.Errorf("secure state directory for launchd logs: %w", err)
	}
	values := map[string]string{
		"label": m.Layout.Label(entry.RepoID), "binary": binary, "repo": entry.RepoPath,
		"stdout": filepath.Join(stateDir, "launchd.stdout.log"), "stderr": filepath.Join(stateDir, "launchd.stderr.log"),
		"home": home, "path": pathEnv,
	}
	for _, logPath := range []string{values["stdout"], values["stderr"]} {
		if info, statErr := os.Lstat(logPath); statErr == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("supervisor log path is not a regular file: %s", logPath)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("inspect supervisor log %s: %w", logPath, statErr)
		}
		file, openErr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			return fmt.Errorf("create private supervisor log %s: %w", logPath, openErr)
		}
		if chmodErr := file.Chmod(0o600); chmodErr != nil {
			_ = file.Close()
			return fmt.Errorf("secure supervisor log %s: %w", logPath, chmodErr)
		}
		if closeErr := file.Close(); closeErr != nil {
			return closeErr
		}
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>{{label}}</string>
  <key>ProgramArguments</key>
  <array><string>{{binary}}</string><string>run</string><string>--repo</string><string>{{repo}}</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>WorkingDirectory</key><string>{{repo}}</string>
  <key>StandardOutPath</key><string>{{stdout}}</string>
  <key>StandardErrorPath</key><string>{{stderr}}</string>
  <key>EnvironmentVariables</key>
  <dict><key>HOME</key><string>{{home}}</string><key>PATH</key><string>{{path}}</string></dict>
</dict>
</plist>
`
	for key, value := range values {
		plist = strings.ReplaceAll(plist, "{{"+key+"}}", escape(value))
	}
	return fsutil.WriteFile(m.Layout.PlistPath(entry.RepoID), []byte(plist), 0o600)
}

func (m Manager) WriteBrokerPlist(binary, pathEnv string) error {
	if !filepath.IsAbs(binary) {
		return fmt.Errorf("broker binary path must be absolute")
	}
	if pathEnv == "" {
		pathEnv = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	if err := os.MkdirAll(m.Layout.LaunchAgents, 0o700); err != nil {
		return err
	}
	brokerDir := m.Layout.BrokerDir()
	if err := os.MkdirAll(brokerDir, 0o700); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	stdout := filepath.Join(brokerDir, "launchd.stdout.log")
	stderr := filepath.Join(brokerDir, "launchd.stderr.log")
	for _, path := range []string{stdout, stderr} {
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			return openErr
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	values := map[string]string{
		"label": m.Layout.BrokerLabel(), "binary": binary, "root": m.Layout.Root,
		"stdout": stdout, "stderr": stderr, "home": home, "path": pathEnv,
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>{{label}}</string>
  <key>ProgramArguments</key><array><string>{{binary}}</string><string>broker</string></array>
  <key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>{{stdout}}</string><key>StandardErrorPath</key><string>{{stderr}}</string>
  <key>EnvironmentVariables</key><dict><key>HOME</key><string>{{home}}</string><key>PATH</key><string>{{path}}</string><key>AGENT_LOOP_HOME</key><string>{{root}}</string></dict>
</dict></plist>
`
	for key, value := range values {
		plist = strings.ReplaceAll(plist, "{{"+key+"}}", escape(value))
	}
	return fsutil.WriteFile(m.Layout.BrokerPlistPath(), []byte(plist), 0o600)
}

func escape(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func (m Manager) Start(ctx context.Context, entry registry.Entry) error {
	status, err := m.Status(ctx, entry)
	if err != nil {
		return err
	}
	if status.Loaded {
		return nil
	}
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, path, "bootstrap", target, m.Layout.PlistPath(entry.RepoID)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, _ = m.Status(ctx, entry)
		if status.Loaded {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("LaunchAgent did not become loaded")
}

func (m Manager) Stop(ctx context.Context, entry registry.Entry) error {
	status, err := m.Status(ctx, entry)
	if err != nil {
		return err
	}
	if !status.Loaded {
		return nil
	}
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return err
	}
	service := target + "/" + m.Layout.Label(entry.RepoID)
	out, err := exec.CommandContext(ctx, path, "bootout", service).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, _ = m.Status(ctx, entry)
		if !status.Loaded {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("LaunchAgent did not become unloaded")
}

func (m Manager) Restart(ctx context.Context, entry registry.Entry) error {
	if err := m.Stop(ctx, entry); err != nil {
		return err
	}
	return m.Start(ctx, entry)
}

func (m Manager) Status(ctx context.Context, entry registry.Entry) (Status, error) {
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return Status{}, err
	}
	service := target + "/" + m.Layout.Label(entry.RepoID)
	out, err := exec.CommandContext(ctx, path, "print", service).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Status{Loaded: false}, nil
		}
		return Status{}, fmt.Errorf("launchctl print: %w", err)
	}
	return parseServiceStatus(out), nil
}

func (m Manager) StartBroker(ctx context.Context) error {
	status, err := m.BrokerStatus(ctx)
	if err != nil || status.Loaded {
		return err
	}
	if err := m.bootstrap(ctx, m.Layout.BrokerPlistPath(), m.Layout.BrokerLabel()); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, _ = m.BrokerStatus(ctx)
		if status.Loaded {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("webhook broker LaunchAgent did not become loaded")
}

func (m Manager) StopBroker(ctx context.Context) error {
	status, err := m.BrokerStatus(ctx)
	if err != nil || !status.Loaded {
		return err
	}
	if err := m.bootout(ctx, m.Layout.BrokerLabel()); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, _ = m.BrokerStatus(ctx)
		if !status.Loaded {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("webhook broker LaunchAgent did not stop")
}

func (m Manager) RestartBroker(ctx context.Context) error {
	if err := m.StopBroker(ctx); err != nil {
		return err
	}
	return m.StartBroker(ctx)
}

func (m Manager) BrokerStatus(ctx context.Context) (Status, error) {
	return m.serviceStatus(ctx, m.Layout.BrokerLabel())
}

func (m Manager) bootstrap(ctx context.Context, plist, label string) error {
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, path, "bootstrap", target, plist).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap %s: %w: %s", label, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m Manager) bootout(ctx context.Context, label string) error {
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return err
	}
	out, err := exec.CommandContext(ctx, path, "bootout", target+"/"+label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout %s: %w: %s", label, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m Manager) serviceStatus(ctx context.Context, label string) (Status, error) {
	path := m.Launchctl
	if path == "" {
		path = "launchctl"
	}
	target, err := guiTarget()
	if err != nil {
		return Status{}, err
	}
	out, err := exec.CommandContext(ctx, path, "print", target+"/"+label).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return Status{Loaded: false}, nil
		}
		return Status{}, err
	}
	return parseServiceStatus(out), nil
}

func parseServiceStatus(out []byte) Status {
	raw := strings.TrimSpace(string(out))
	status := Status{Loaded: true, Raw: raw}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "state":
			status.State = strings.TrimSpace(value)
		case "pid":
			status.PID, _ = strconv.Atoi(strings.TrimSpace(value))
		case "last exit code":
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err == nil {
				status.LastExitStatus = &parsed
			}
		}
	}
	status.Running = status.PID > 0 || strings.EqualFold(status.State, "running")
	return status
}

func guiTarget() (string, error) {
	uid := os.Getuid()
	if uid >= 0 {
		return "gui/" + strconv.Itoa(uid), nil
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return "gui/" + current.Uid, nil
}

package app

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
)

const serviceLabel = "com.codex-issue-loop.monitor"

type serviceStatus struct {
	Label   string `json:"label"`
	Loaded  bool   `json:"loaded"`
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
}

func installBinary(stateDir string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(stateDir, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	destination := filepath.Join(binDir, "agent-loop-monitor")
	if err := writePrivateFile(destination, data, 0o755); err != nil {
		return "", err
	}
	return destination, nil
}

func registerService(cfg config.Config) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	binary := filepath.Join(cfg.StateDir, "bin", "agent-loop-monitor")
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("agent-loop-monitor is not installed at %s", binary)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return "", err
	}
	stdout := filepath.Join(cfg.StateDir, "launchd.stdout.log")
	stderr := filepath.Join(cfg.StateDir, "launchd.stderr.log")
	for _, path := range []string{stdout, stderr} {
		file, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			return "", openErr
		}
		if closeErr := file.Close(); closeErr != nil {
			return "", closeErr
		}
	}
	values := map[string]string{"label": serviceLabel, "binary": binary, "config": cfg.Path, "stdout": stdout, "stderr": stderr, "home": home}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>{{label}}</string>
<key>ProgramArguments</key><array><string>{{binary}}</string><string>run</string><string>--config</string><string>{{config}}</string><string>--json</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>30</integer><key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>{{stdout}}</string><key>StandardErrorPath</key><string>{{stderr}}</string>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>{{home}}</string><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
</dict></plist>
`
	for key, value := range values {
		var escaped bytes.Buffer
		_ = xml.EscapeText(&escaped, []byte(value))
		plist = strings.ReplaceAll(plist, "{{"+key+"}}", escaped.String())
	}
	path := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, writePrivateFile(path, []byte(plist), 0o600)
}

func controlService(ctx context.Context, action string) (serviceStatus, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceStatus{}, err
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
	target := "gui/" + strconv.Itoa(os.Getuid())
	service := target + "/" + serviceLabel
	switch action {
	case "start":
		output, runErr := exec.CommandContext(ctx, "launchctl", "bootstrap", target, plist).CombinedOutput()
		if runErr != nil && !strings.Contains(string(output), "service already loaded") {
			return serviceStatus{}, fmt.Errorf("launchctl bootstrap: %w: %s", runErr, strings.TrimSpace(string(output)))
		}
	case "stop":
		output, runErr := exec.CommandContext(ctx, "launchctl", "bootout", service).CombinedOutput()
		if runErr != nil && !strings.Contains(string(output), "Could not find service") {
			return serviceStatus{}, fmt.Errorf("launchctl bootout: %w: %s", runErr, strings.TrimSpace(string(output)))
		}
	case "restart":
		_, _ = exec.CommandContext(ctx, "launchctl", "bootout", service).CombinedOutput()
		output, runErr := exec.CommandContext(ctx, "launchctl", "bootstrap", target, plist).CombinedOutput()
		if runErr != nil {
			return serviceStatus{}, fmt.Errorf("launchctl bootstrap: %w: %s", runErr, strings.TrimSpace(string(output)))
		}
	case "status":
	default:
		return serviceStatus{}, fmt.Errorf("unknown service action %q", action)
	}
	return inspectService(ctx, service)
}

func inspectService(ctx context.Context, service string) (serviceStatus, error) {
	output, err := exec.CommandContext(ctx, "launchctl", "print", service).CombinedOutput()
	if err != nil {
		return serviceStatus{Label: serviceLabel}, nil
	}
	status := serviceStatus{Label: serviceLabel, Loaded: true}
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "pid = ") {
			status.PID, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
			status.Running = status.PID > 0
		}
	}
	return status, nil
}

func writePrivateFile(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".monitor-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

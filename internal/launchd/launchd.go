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

	"github.com/ishii1648/codex-issue-loop/internal/config"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/layout"
	"github.com/ishii1648/codex-issue-loop/internal/registry"
)

type Manager struct {
	Layout    layout.Layout
	Launchctl string
}

type Status struct {
	Loaded bool   `json:"loaded"`
	Raw    string `json:"raw,omitempty"`
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
	exitTimeout := 60
	if _, statErr := os.Stat(filepath.Join(entry.RepoPath, config.FileName)); statErr == nil {
		cfg, loadErr := config.Load(entry.RepoPath)
		if loadErr != nil {
			return fmt.Errorf("load worker grace period for launchd: %w", loadErr)
		}
		exitTimeout = int((cfg.Worker.TimeoutGrace.Duration + time.Second - 1) / time.Second)
		// launchd must never expire its service exit window before the worker
		// process-group grace period used by an explicit forced stop.
		exitTimeout += 5
	}
	values["exit_timeout"] = strconv.Itoa(exitTimeout)
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
  <key>ExitTimeOut</key><integer>{{exit_timeout}}</integer>
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
	return Status{Loaded: true, Raw: strings.TrimSpace(string(out))}, nil
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

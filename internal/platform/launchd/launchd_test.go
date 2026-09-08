package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/layout"
	"github.com/ishii1648/codex-issue-loop/internal/platform/registry"
)

func TestWriteDeliveryPlistUsesHostLevelIntervalAndDefaultConfig(t *testing.T) {
	root := t.TempDir()
	l := layout.Layout{Root: root, LaunchAgents: filepath.Join(root, "launch")}
	if err := (Manager{Layout: l}).WriteDeliveryPlist("/absolute/agent-loop", "/usr/bin:/bin", 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.DeliveryPlistPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"com.codex-issue-loop.delivery", "<string>delivery</string>", "<string>reconcile</string>", "<key>RunAtLoad</key>", "<key>StartInterval</key><integer>900</integer>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "--config") {
		t.Fatal("LaunchAgent must use the default host-level config path")
	}
	if info, err := os.Stat(l.DeliveryPlistPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("plist mode=%v err=%v", info, err)
	}
}

func TestWritePlistUsesAbsoluteCommandsAndEscapesPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "managed & root")
	l := layout.Layout{Root: root, ReposRoot: filepath.Join(root, "repos"), LaunchAgents: filepath.Join(root, "launch")}
	entry := registry.Entry{RepoID: "repo-id", RepoPath: filepath.Join(root, "repo & work"), EnvironmentPath: "/custom/bin:/usr/bin"}
	if err := (Manager{Layout: l}).WritePlist(entry, "/absolute/agent-loop"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(l.PlistPath(entry.RepoID))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range []string{"/absolute/agent-loop", "repo &amp; work", "/custom/bin:/usr/bin", "SuccessfulExit", "<key>AGENT_LOOP_HOME</key><string>" + strings.ReplaceAll(l.Root, "&", "&amp;") + "</string>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("plist missing %q:\n%s", expected, text)
		}
	}
	for _, path := range []string{l.PlistPath(entry.RepoID), filepath.Join(l.RepoDir(entry.RepoID), "launchd.stdout.log"), filepath.Join(l.RepoDir(entry.RepoID), "launchd.stderr.log")} {
		info, statErr := os.Stat(path)
		if statErr != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unsafe mode for %s: info=%v err=%v", path, info, statErr)
		}
	}
}

func TestRestartWaitsForBootoutBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "loaded")
	if err := os.WriteFile(statePath, []byte("loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launchctl := filepath.Join(root, "launchctl")
	script := `#!/bin/sh
case "$1" in
  print)
    test -f "$LAUNCHD_TEST_STATE"
    ;;
  bootout)
    (sleep 0.2; rm -f "$LAUNCHD_TEST_STATE") &
    ;;
  bootstrap)
    if test -f "$LAUNCHD_TEST_STATE"; then
      echo 'still loaded' >&2
      exit 1
    fi
    : > "$LAUNCHD_TEST_STATE"
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LAUNCHD_TEST_STATE", statePath)
	l := layout.Layout{Root: root, LaunchAgents: filepath.Join(root, "launch")}
	entry := registry.Entry{RepoID: "repo-id"}
	if err := (Manager{Layout: l, Launchctl: launchctl}).Restart(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("LaunchAgent was not loaded after restart: %v", err)
	}
}

func TestLaunchctlPrintDeadline(t *testing.T) {
	entry := registry.Entry{RepoID: "repo-id"}
	for _, tc := range []struct {
		name    string
		initial string
		run     func(Manager, context.Context) error
	}{
		{"Status", "", func(m Manager, ctx context.Context) error { _, err := m.Status(ctx, entry); return err }},
		{"BrokerStatus", "", func(m Manager, ctx context.Context) error { _, err := m.BrokerStatus(ctx); return err }},
		{"DeliveryStatus", "", func(m Manager, ctx context.Context) error { _, err := m.DeliveryStatus(ctx); return err }},
		{"RequestStop", "", func(m Manager, ctx context.Context) error { _, err := m.RequestStop(ctx, entry); return err }},
		{"WaitStopped", "", func(m Manager, ctx context.Context) error { return m.WaitStopped(ctx, entry, 5*time.Second) }},
		{"Start", "113", func(m Manager, ctx context.Context) error { return m.Start(ctx, entry) }},
		{"StartBroker", "113", Manager.StartBroker},
		{"StopBroker", "0", Manager.StopBroker},
		{"StartDelivery", "113", Manager.StartDelivery},
		{"StopDelivery", "0", Manager.StopDelivery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			launchctl := filepath.Join(root, "launchctl")
			script := `#!/bin/sh
case "$1" in
  print)
    if test -n "$LAUNCHD_TEST_INITIAL" && ! test -f "$LAUNCHD_TEST_STATE"; then
      exit "$LAUNCHD_TEST_INITIAL"
    fi
    : > "$LAUNCHD_TEST_STARTED"
    exec sleep 30
    ;;
  bootstrap|bootout)
    : > "$LAUNCHD_TEST_STATE"
    ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(launchctl, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			started := filepath.Join(root, "started")
			t.Setenv("LAUNCHD_TEST_STARTED", started)
			t.Setenv("LAUNCHD_TEST_STATE", filepath.Join(root, "state"))
			t.Setenv("LAUNCHD_TEST_INITIAL", tc.initial)
			m := Manager{Layout: layout.Layout{Root: root, LaunchAgents: filepath.Join(root, "launch")}, Launchctl: launchctl}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := tc.run(m, ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("error = %v, want context.DeadlineExceeded", err)
			}
			if _, err := os.Stat(started); err != nil {
				t.Fatalf("blocking launchctl print did not start: %v", err)
			}
		})
	}
}

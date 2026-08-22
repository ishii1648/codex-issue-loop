package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestE2ESupervisorCleanupOnSuccessFailureSignalAndTimeout(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "e2e-supervisor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
		code int
		term bool
	}{
		{name: "success", args: []string{"--", "sh", "-c", "exit 0"}, code: 0},
		{name: "failure", args: []string{"--", "sh", "-c", "exit 7"}, code: 7},
		{name: "sigterm", args: []string{"--", "sh", "-c", "sleep 30"}, code: 143, term: true},
		{name: "timeout", args: []string{"--timeout", "1", "--", "sh", "-c", "sleep 30"}, code: 124},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			fake := filepath.Join(dir, "agent-loop")
			calls := filepath.Join(dir, "calls.log")
			body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\n", calls)
			if err := os.WriteFile(fake, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--repo", filepath.Join(dir, "repo")}, test.args...)
			command := exec.Command(script, args...)
			command.Env = append(os.Environ(), "AGENT_LOOP_BIN="+fake)
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			finished := false
			t.Cleanup(func() {
				if finished {
					return
				}
				_ = command.Process.Signal(syscall.SIGTERM)
				_ = command.Wait()
			})
			if test.term {
				waitForE2ECall(t, calls, "start --repo")
				if err := command.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			err := command.Wait()
			finished = true
			code := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					code = exit.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			if code != test.code {
				t.Fatalf("exit code=%d want=%d err=%v", code, test.code, err)
			}
			data, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			if strings.Count(text, "stop --repo") != 1 || strings.Count(text, "unregister --repo") != 1 {
				t.Fatalf("cleanup calls:\n%s", text)
			}
		})
	}
}

func waitForE2ECall(t *testing.T, path, pattern string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), pattern) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", pattern, path)
}

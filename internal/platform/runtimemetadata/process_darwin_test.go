package runtimemetadata

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestInspectProcessLifecycle(t *testing.T) {
	self, err := InspectProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if self.PID != os.Getpid() || self.UID != uint32(os.Geteuid()) || self.BootSessionID == "" || self.StartedAt.Seconds <= 0 {
		t.Fatal("invalid process identity")
	}
	command := exec.Command("/bin/sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer command.Process.Kill()
	child, err := InspectProcess(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if child.PID != command.Process.Pid || child.BootSessionID != self.BootSessionID {
		t.Fatal("incorrect child identity")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, err := InspectProcess(child.PID)
		if errors.Is(err, ErrNotRunning) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			_ = command.Wait()
			t.Fatalf("exited or zombie process was accepted: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	_ = command.Wait()
	if _, err := InspectProcess(child.PID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped process: %v", err)
	}
}

package drain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
)

func TestReadyRequiresMaintenanceAndNoWorkerIdentity(t *testing.T) {
	snapshot := state.Snapshot{Supervisor: state.Supervisor{State: state.SupervisorStateMaintenance}, Issues: map[string]*state.Issue{}}
	if !Ready(snapshot) {
		t.Fatal("idle maintenance snapshot was not ready")
	}
	snapshot.ActiveExecution = &state.ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 1}
	if Ready(snapshot) {
		t.Fatal("snapshot with an active execution was ready")
	}
	snapshot.ActiveExecution = nil
	snapshot.Issues["1"] = &state.Issue{WorkerPID: 123, WorkerPGID: 123}
	if Ready(snapshot) {
		t.Fatal("snapshot with a retained worker identity was ready")
	}
	snapshot.Issues["1"].WorkerPID, snapshot.Issues["1"].WorkerPGID = 0, 0
	snapshot.Supervisor.State = state.SupervisorStateDraining
	if Ready(snapshot) {
		t.Fatal("draining snapshot crossed the durable maintenance checkpoint")
	}
}

func TestRequestedFailsClosedForUnreadableBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fence")
	if Requested(path) {
		t.Fatal("absent fence requested maintenance")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if !Requested(path) {
		t.Fatal("invalid fence reopened admission")
	}
}

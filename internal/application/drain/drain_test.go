package drain

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
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

func TestRecoverableUnstartedConflictLaunchRequiresPriorSupervisorGeneration(t *testing.T) {
	started := time.Date(2026, 9, 5, 17, 34, 45, 0, time.UTC)
	snapshot := state.Snapshot{
		Supervisor: state.Supervisor{State: state.SupervisorStateDraining, StartedAt: started},
		ActiveExecution: &state.ActiveExecution{
			IssueNumber: 277, RunID: "conflict_174e861a0a076558", Generation: 15, StartedAt: started.Add(-14 * time.Minute),
		},
		Issues: map[string]*state.Issue{"277": {
			Number: 277, Status: issuedomain.StatusLaunching, LaunchSource: issuedomain.StatusResolvingConflict,
			RunID: "conflict_174e861a0a076558", Generation: 15,
		}},
	}
	if !RecoverableUnstartedConflictLaunch(snapshot) {
		t.Fatal("prior-supervisor conflict launch was not recoverable during drain")
	}
	snapshot.ActiveExecution.StartedAt = started.Add(time.Second)
	if RecoverableUnstartedConflictLaunch(snapshot) {
		t.Fatal("current-supervisor launch was recoverable during drain")
	}
	snapshot.ActiveExecution.StartedAt = started.Add(-time.Minute)
	snapshot.Issues["277"].WorkerPID, snapshot.Issues["277"].WorkerPGID = 123, 123
	if RecoverableUnstartedConflictLaunch(snapshot) {
		t.Fatal("recorded worker process was recoverable during drain")
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

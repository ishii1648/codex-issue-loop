package supervisor

import (
	"context"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/github"
	"github.com/ishii1648/codex-issue-loop/internal/state"
	"github.com/ishii1648/codex-issue-loop/internal/worker"
)

type fakeProcessGroups struct {
	alive      map[int]bool
	owned      map[int]bool
	ignoreTerm map[int]bool
	signals    map[int][]syscall.Signal
}

func (f *fakeProcessGroups) Alive(pid int) bool         { return f.alive[pid] }
func (f *fakeProcessGroups) GroupAlive(pgid int) bool   { return f.alive[pgid] }
func (f *fakeProcessGroups) OwnsGroup(_, pgid int) bool { return f.owned[pgid] }
func (f *fakeProcessGroups) SignalGroup(pgid int, signal syscall.Signal) error {
	f.signals[pgid] = append(f.signals[pgid], signal)
	if signal == syscall.SIGKILL || !f.ignoreTerm[pgid] {
		f.alive[pgid] = false
	}
	return nil
}

func TestStopWorkersTerminatesAndRecordsEveryIssueIndependently(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	for number, run := range map[int]string{2: "run_2", 1: "run_1"} {
		_, _, err := loop.Store.ReserveLease(state.LeaseReservation{
			IssueNumber: number, RunID: run, Slot: number - 1,
			ResolvedResources: []string{"resource-" + strconv.Itoa(number)}, ReservedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = loop.Store.Update("running", number, run, nil, func(snapshot *state.Snapshot) error {
			item := snapshot.Issues[strconv.Itoa(number)]
			item.Status, item.WorkerPID, item.WorkerPGID = "running", 100+number, 100+number
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	groups := &fakeProcessGroups{
		alive: map[int]bool{101: true, 102: true}, owned: map[int]bool{101: true, 102: true},
		ignoreTerm: map[int]bool{102: true}, signals: map[int][]syscall.Signal{},
	}
	report, err := StopWorkers(context.Background(), loop.Store, 20*time.Millisecond, "test stop", groups)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Workers) != 2 || report.Workers[0].IssueNumber != 1 || report.Workers[0].Forced || !report.Workers[1].Forced {
		t.Fatalf("report=%+v", report)
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"1", "2"} {
		issue := snapshot.Issues[key]
		if issue.Status != "retry_wait" || issue.WorkerPID != 0 || issue.WorkerPGID != 0 || issue.Lease == nil || issue.LastError != "test stop" {
			t.Fatalf("Issue %s=%+v", key, issue)
		}
	}
}

func TestStopWorkersRejectsUnownedProcessGroupWithoutMutatingIssue(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{Number: 1, RunID: "run_1", Status: "running", WorkerPID: 101, WorkerPGID: 101}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	groups := &fakeProcessGroups{alive: map[int]bool{101: true}, owned: map[int]bool{}, signals: map[int][]syscall.Signal{}}
	if _, err := StopWorkers(context.Background(), loop.Store, time.Second, "test stop", groups); err == nil {
		t.Fatal("unowned process group was stopped")
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Issues["1"].WorkerPID != 101 || len(groups.signals) != 0 {
		t.Fatalf("issue=%+v signals=%v", snapshot.Issues["1"], groups.signals)
	}
}

func TestFaultRealProcessStopRestartLeavesNoOrphanAndRetainsLeases(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.GitHub = numberedFakeGitHub{fakeGitHub: github}
	github.issue = gh.Issue{State: "OPEN", Labels: []string{loop.Config.GitHub.RunningLabel}}
	type child struct {
		cmd  *exec.Cmd
		done chan error
	}
	children := make([]child, 0, 2)
	for number := 1; number <= 2; number++ {
		readyReader, readyWriter, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=TestSupervisorOrphanProcessHelper", "--")
		command.Env = append(os.Environ(), "AGENT_LOOP_ORPHAN_HELPER=1")
		command.ExtraFiles = []*os.File{readyWriter}
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := command.Start(); err != nil {
			readyReader.Close()
			readyWriter.Close()
			t.Fatal(err)
		}
		pid := command.Process.Pid
		t.Cleanup(func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		})
		readyWriter.Close()
		if err := readyReader.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			_ = command.Process.Kill()
			t.Fatal(err)
		}
		ready := make([]byte, 1)
		if _, err := readyReader.Read(ready); err != nil || ready[0] != 1 {
			readyReader.Close()
			_ = command.Process.Kill()
			t.Fatalf("helper readiness=%v err=%v", ready, err)
		}
		readyReader.Close()
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		children = append(children, child{cmd: command, done: done})

		runID := "run_" + strconv.Itoa(number)
		_, _, err = loop.Store.ReserveLease(state.LeaseReservation{
			IssueNumber: number, RunID: runID, Slot: number - 1,
			ResolvedResources: []string{"resource-" + strconv.Itoa(number)}, ReservedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = loop.Store.Update("worker_process_started", number, runID, nil, func(snapshot *state.Snapshot) error {
			item := snapshot.Issues[strconv.Itoa(number)]
			item.Status = "running"
			item.WorkerPID = command.Process.Pid
			item.WorkerPGID = command.Process.Pid
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	report, err := StopWorkers(context.Background(), loop.Store, 2*time.Second, "supervisor restart", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Workers) != 2 {
		t.Fatalf("report=%+v", report)
	}
	for _, process := range children {
		select {
		case err := <-process.done:
			if err != nil {
				t.Fatalf("helper %d: %v", process.cmd.Process.Pid, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("helper %d was not reaped", process.cmd.Process.Pid)
		}
		if (OSProcessGroupController{}).GroupAlive(process.cmd.Process.Pid) {
			t.Fatalf("orphan process group %d remains alive", process.cmd.Process.Pid)
		}
	}
	snapshot, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Loop{
		Config: loop.Config, Store: loop.Store, GitHub: loop.GitHub, Worktrees: loop.Worktrees,
		Processes: OSProcessGroupController{}, Clock: loop.Clock, Logger: loop.Logger,
	}
	if err := restarted.reconcileStartup(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	loaded, err := loop.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"1", "2"} {
		item := loaded.Issues[key]
		if item.Status != "retry_wait" || item.WorkerPID != 0 || item.WorkerPGID != 0 || item.Lease == nil {
			t.Fatalf("Issue %s after restart=%+v", key, item)
		}
	}
}

func TestSupervisorOrphanProcessHelper(t *testing.T) {
	if os.Getenv("AGENT_LOOP_ORPHAN_HELPER") != "1" {
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	ready := os.NewFile(3, "ready")
	if ready == nil {
		os.Exit(2)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		os.Exit(2)
	}
	_ = ready.Close()
	<-signals
}

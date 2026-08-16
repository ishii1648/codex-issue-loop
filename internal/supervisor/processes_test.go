package supervisor

import (
	"context"
	"strconv"
	"syscall"
	"testing"
	"time"

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

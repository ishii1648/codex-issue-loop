package supervisor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	gh "github.com/ishii1648/codex-issue-loop/internal/adapter/github"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestProjectionWithoutPendingEffectDoesNotChangeLifecycle(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	before, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{Number: 1, RunID: "run_1", Status: issuedomain.StatusFailed}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.issue.State, github.issue.StateReason = "CLOSED", "NOT_PLANNED"
	github.issue.Labels = []string{loop.Config.GitHub.RunningLabel, "bug", "do-not-automate"}
	if err := loop.syncGitHub(context.Background(), *before.Issues["1"]); err != nil {
		t.Fatal(err)
	}
	after, err := loop.Store.Load()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("projection changed canonical state: %v", err)
	}
	if err := gh.ValidateIssueProjection(loop.Config, github.issue, issuedomain.StatusFailed, after.NeedsHuman(1, loop.Config.Completion.AutoMerge)); err != nil {
		t.Fatal(err)
	}
	if !containsString(github.issue.Labels, "bug") || containsString(github.issue.Labels, "do-not-automate") {
		t.Fatal(github.issue.Labels)
	}
}

func TestProjectionRejectsConcurrentCanonicalChangeAndConvergesNextTime(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{Number: 1, RunID: "run_1", Status: issuedomain.StatusFailed}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	github.projectionHook = func(number int, status issuedomain.Status) error {
		_, err := loop.Store.Update("concurrent_transition", number, "run_1", nil, func(s *state.Snapshot) error {
			transition, err := issuedomain.ReconcileObservation(s.Issues["1"].Status, issuedomain.StatusBlocked)
			if err != nil {
				return err
			}
			return state.ApplyIssueTransition(s.Issues["1"], transition)
		})
		return err
	}
	if err := loop.reconcileIssueProjection(context.Background(), 1); !errors.Is(err, errReconciliationStateChanged) {
		t.Fatalf("stale projection accepted: %v", err)
	}
	github.projectionHook = nil
	if err := loop.reconcileIssueProjection(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := gh.ValidateIssueProjection(loop.Config, github.issue, issuedomain.StatusBlocked, true); err != nil {
		t.Fatal(err)
	}
}

func TestManagedSweepIncludesCompletedCanceledAndQuarantine(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	loop.Clock = fixedClock{value: time.Now().UTC()}
	before, err := loop.Store.Update("fixtures", 0, "", nil, func(s *state.Snapshot) error {
		s.Issues["1"] = &state.Issue{Number: 1, Status: issuedomain.StatusCompleted}
		s.Issues["2"] = &state.Issue{Number: 2, Status: issuedomain.StatusCanceled, Cancellation: &state.Cancellation{Source: "operator", PreviousStatus: issuedomain.StatusBlocked, ExecutionReleaseResult: "not_present", CanceledAt: loop.now()}}
		s.QuarantinedIssues["3"] = &state.QuarantineRecord{IssueNumber: 3, ReasonCode: "test", Reason: "ambiguous", QuarantinedAt: loop.now()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]issuedomain.Status{}
	github.projectionHook = func(number int, status issuedomain.Status) error { seen[number] = status; return nil }
	s := &scheduler{loop: loop, active: map[int]activeJob{}, events: make(chan schedulerEvent, 1), issueRetry: map[int]time.Time{}, issueFails: map[int]int{}}
	defer s.cancelAndDrain()
	for range 3 {
		s.lifecycleMu.Lock()
		dispatched, err := s.dispatchManagedReconciliation(context.Background(), before)
		s.lifecycleMu.Unlock()
		if err != nil || !dispatched {
			t.Fatalf("dispatch=%v err=%v", dispatched, err)
		}
		select {
		case event := <-s.events:
			if err := s.handleEvent(event); err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("projection did not finish")
		}
	}
	want := map[int]issuedomain.Status{1: issuedomain.StatusCompleted, 2: issuedomain.StatusCanceled, 3: issuedomain.StatusBlocked}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen=%v", seen)
	}
	after, err := loop.Store.Load()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("sweep changed lifecycle: %v", err)
	}
	if dispatched, err := s.dispatchManagedReconciliation(context.Background(), after); dispatched || err != nil {
		t.Fatalf("per-Issue interval ignored: %v %v", dispatched, err)
	}
}

func TestRunningProjectionKeepsExistingJobAndExecutionOwner(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	_, identity, err := loop.Store.StartExecution(state.ExecutionStart{IssueNumber: 1, RunID: "run_1", StartedAt: loop.now()})
	if err != nil {
		t.Fatal(err)
	}
	before, err := loop.Store.Update("running_fixture", 1, "run_1", nil, func(s *state.Snapshot) error {
		s.Issues["1"].Status = issuedomain.StatusRunning
		s.Issues["1"].WorkerPID, s.Issues["1"].WorkerPGID = 123, 123
		setSupervisorTestWorkspace(s, s.Issues["1"])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &scheduler{loop: loop, active: map[int]activeJob{1: {runID: "run_1", slot: 0}}}
	github.projectionHook = func(number int, status issuedomain.Status) error {
		if s.lifecycleMu.TryLock() {
			s.lifecycleMu.Unlock()
			t.Error("projection bypassed lifecycle gate")
		}
		if number != 1 || status != issuedomain.StatusRunning || len(s.active) != 1 || s.active[1].slot != 0 {
			t.Errorf("worker job changed: %+v", s.active)
		}
		return nil
	}
	s.lifecycleMu.Lock()
	dispatched, err := s.dispatchManagedReconciliation(context.Background(), before)
	s.lifecycleMu.Unlock()
	if err != nil || dispatched {
		t.Fatalf("projection dispatched worker job: %v %v", dispatched, err)
	}
	after, err := loop.Store.Load()
	if err != nil || !reflect.DeepEqual(before, after) || !state.OwnsActiveExecution(&after, 1, identity) || len(s.active) != 1 {
		t.Fatalf("execution changed: %+v %v", after.ActiveExecution, err)
	}
}

package supervisor

import (
	"context"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/platform/failure"
)

type fixedClock struct{ value time.Time }

func (f fixedClock) Now() time.Time { return f.value }

type fixedRandom float64

func (f fixedRandom) Float64() float64 { return float64(f) }

func TestRetryDelayUsesExponentialCapAndJitter(t *testing.T) {
	tests := []struct {
		attempt int
		random  float64
		want    time.Duration
	}{
		{attempt: 1, random: 0, want: 4 * time.Second},
		{attempt: 1, random: 0.5, want: 5 * time.Second},
		{attempt: 2, random: 1, want: 12 * time.Second},
		{attempt: 7, random: 0.5, want: 5 * time.Minute},
		{attempt: 20, random: 1, want: 5 * time.Minute},
	}
	for _, test := range tests {
		loop := Loop{Random: fixedRandom(test.random)}
		if got := loop.retryDelay(test.attempt); got != test.want {
			t.Fatalf("attempt=%d random=%v delay=%s want=%s", test.attempt, test.random, got, test.want)
		}
	}
}

func TestPollDelayUsesIndependentJitter(t *testing.T) {
	loop := Loop{Random: fixedRandom(1)}
	if got := loop.pollDelay(time.Minute); got != 72*time.Second {
		t.Fatalf("delay=%s", got)
	}
}

func TestScheduleRetryPersistsClassificationReasonAndTime(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	loop.Random = fixedRandom(0.5)
	_, err := loop.Store.Update("running", 1, "run_1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{Number: 1, Status: issuedomain.StatusRunning, RunID: "run_1", Attempts: 1}
		setSupervisorTestWorkspace(snapshot, snapshot.Issues["1"])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	issue, _ := loop.issueState(1)
	if err := loop.scheduleRetry(context.Background(), issue, "worker timeout"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	got := snapshot.Issues["1"]
	wantRetry := now.Add(5 * time.Second)
	if got.Status != issuedomain.StatusRetryWait || got.FailureKind != string(failure.Transient) || got.LastError != "worker timeout" || got.RetryAfter == nil || !got.RetryAfter.Equal(wantRetry) {
		t.Fatalf("issue=%+v want retry=%s", got, wantRetry)
	}
	events, err := os.ReadFile(loop.Store.EventsPath())
	if err != nil || !strings.Contains(string(events), `"failure_kind":"transient"`) || !strings.Contains(string(events), `"reason":"worker timeout"`) {
		t.Fatalf("events=%s err=%v", events, err)
	}
}

func TestInvalidRetryDecisionIsIssueScoped(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	err := loop.scheduleRetry(context.Background(), state.Issue{
		Number: 1, Status: issuedomain.StatusCompleted, RunID: "run_1", Attempts: 1,
	}, "unexpected lifecycle state")
	if got := failure.KindOf(err); got != failure.Issue {
		t.Fatalf("kind=%s err=%v", got, err)
	}
}

func TestSupervisorRetryAndSuccessfulCycleResetPersistentCounter(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	loop.Clock = fixedClock{value: now}
	cause := failure.Wrap(failure.Transient, "poll GitHub Issue queue", context.DeadlineExceeded)
	if err := loop.recordSupervisorRetry(cause, failure.Transient, 2, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	if snapshot.Supervisor.State != "retry_wait" || snapshot.Supervisor.ConsecutiveFailures != 2 || snapshot.Supervisor.FailureKind != "transient" || snapshot.Supervisor.RetryAfter == nil || !snapshot.Supervisor.RetryAfter.Equal(now.Add(10*time.Second)) {
		t.Fatalf("supervisor=%+v", snapshot.Supervisor)
	}
	if err := loop.resetSupervisorFailures(2); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = loop.Store.Load()
	if snapshot.Supervisor.ConsecutiveFailures != 0 || snapshot.Supervisor.FailureKind != "" || snapshot.Supervisor.RetryAfter != nil {
		t.Fatalf("supervisor was not reset: %+v", snapshot.Supervisor)
	}
}

func TestRunOnceClassifiesGitHubPollingFailureAsTransient(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.listErr = context.DeadlineExceeded
	_, err := loop.RunOnce(context.Background())
	if failure.KindOf(err) != failure.Transient {
		t.Fatalf("kind=%s err=%v", failure.KindOf(err), err)
	}
}

func TestRetryLimitBecomesIssueFailure(t *testing.T) {
	loop, _ := testLoop(t, worker.Result{})
	_, err := loop.Store.Update("running", 1, "run_1", nil, func(snapshot *state.Snapshot) error {
		snapshot.Issues["1"] = &state.Issue{
			Number: 1, Status: issuedomain.StatusRunning, RunID: "run_1", Attempts: loop.Config.Queue.MaxAttempts,
		}
		setSupervisorTestWorkspace(snapshot, snapshot.Issues["1"])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	issue, _ := loop.issueState(1)
	if err := loop.scheduleRetry(context.Background(), issue, "permanent test failure"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := loop.Store.Load()
	got := snapshot.Issues["1"]
	if got.Status != issuedomain.StatusFailed || got.FailureKind != string(failure.Issue) || got.RetryAfter != nil {
		t.Fatalf("issue=%+v", got)
	}
}

func TestGitHubSynchronizationFailureIsTransient(t *testing.T) {
	loop, github := testLoop(t, worker.Result{})
	github.doneErr = context.DeadlineExceeded
	err := loop.syncGitHub(context.Background(), state.Issue{Number: 1, GitHubSync: issuedomain.GitHubSyncDone})
	if failure.KindOf(err) != failure.Transient {
		t.Fatalf("kind=%s err=%v", failure.KindOf(err), err)
	}
}

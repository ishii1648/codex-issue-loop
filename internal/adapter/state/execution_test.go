package state

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestStartExecutionRejectsQuarantinedIssueWithoutChangingIsolationRecord(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	record := &QuarantineRecord{
		IssueNumber: 1, RunID: "old_run", Generation: 3, RejectedStatus: issuedomain.StatusRunning,
		ReasonCode: "fixture", Reason: "ambiguous prior execution", QuarantinedAt: now,
	}
	before, err := store.Update("fixture_quarantine", 1, record.RunID, nil, func(snapshot *Snapshot) error {
		snapshot.QuarantinedIssues["1"] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := store.StartExecution(ExecutionStart{IssueNumber: 1, RunID: "new_run", StartedAt: now.Add(time.Minute)})
	var quarantined IssueQuarantinedError
	if !errors.As(err, &quarantined) || identity != (ExecutionIdentity{}) {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.StateRevision != before.StateRevision || after.ActiveExecution != nil || after.Issues["1"] != nil ||
		!reflect.DeepEqual(after.QuarantinedIssues["1"], record) {
		t.Fatalf("quarantine changed: before=%+v after=%+v", before.QuarantinedIssues["1"], after.QuarantinedIssues["1"])
	}
}

func newStore(t *testing.T) Store {
	t.Helper()
	store := Store{Dir: t.TempDir(), RepoID: "repo-deadbeef", RepoPath: "/tmp/repo"}
	if err := store.Initialize(); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSingleExecutionStartCaptureResumeAndTransfer(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	snapshot, first, err := store.StartExecution(ExecutionStart{IssueNumber: 1, Title: "first", RunID: "run_1", BaseSHA: "base", StartedAt: now})
	if err != nil || first.Generation != 1 || !OwnsActiveExecution(&snapshot, 1, first) {
		t.Fatalf("start snapshot=%+v identity=%+v err=%v", snapshot.ActiveExecution, first, err)
	}
	if _, _, err := store.StartExecution(ExecutionStart{IssueNumber: 2, Title: "second", RunID: "run_2", StartedAt: now}); err == nil {
		t.Fatal("second Issue acquired the repository execution")
	}
	checkpointID := NewID("checkpoint")
	snapshot, err = store.Update("checkpoint", 1, first.RunID, nil, func(snapshot *Snapshot) error {
		item := snapshot.Issues["1"]
		item.Worktree, item.Branch = "/tmp/issue-1", "codex/issue-1"
		item.Workspace = &WorkerWorkspace{Path: item.Worktree, Branch: item.Branch, RepoID: snapshot.RepoID, Repository: "owner/repo", GitCommonDir: "/tmp/repo/.git", MainCheckout: "/tmp/repo", CapturedAt: now}
		item.Status = issuedomain.StatusNeedsInput
		return CaptureContinuation(snapshot, 1, first, checkpointID, now.Add(time.Minute))
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ActiveExecution != nil || snapshot.Issues["1"].Continuation == nil {
		t.Fatalf("capture active=%+v continuation=%+v", snapshot.ActiveExecution, snapshot.Issues["1"].Continuation)
	}
	snapshot, err = store.Update("resume", 1, first.RunID, nil, func(snapshot *Snapshot) error {
		identity, resumeErr := ResumeContinuation(snapshot, 1, checkpointID, now.Add(2*time.Minute))
		if resumeErr != nil {
			return resumeErr
		}
		snapshot.Issues["1"].Status = issuedomain.StatusRunning
		snapshot.Issues["1"].WorkerPID, snapshot.Issues["1"].WorkerPGID = 123, 123
		first = identity
		return nil
	})
	if err != nil || first.Generation != 2 || !OwnsActiveExecution(&snapshot, 1, first) {
		t.Fatalf("resume active=%+v identity=%+v err=%v", snapshot.ActiveExecution, first, err)
	}
	snapshot, err = store.Update("transfer", 1, first.RunID, nil, func(snapshot *Snapshot) error {
		identity, transferErr := TransferExecution(snapshot, 1, first, "run_retry", now.Add(3*time.Minute))
		first = identity
		return transferErr
	})
	if err != nil || first.Generation != 3 || first.RunID != "run_retry" || !OwnsActiveExecution(&snapshot, 1, first) {
		t.Fatalf("transfer active=%+v identity=%+v err=%v", snapshot.ActiveExecution, first, err)
	}
}

func TestFaultConcurrentExecutionStartCreatesOneOwner(t *testing.T) {
	store := newStore(t)
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	var wait sync.WaitGroup
	wins := make(chan int, 2)
	for number := 1; number <= 2; number++ {
		wait.Add(1)
		go func(number int) {
			defer wait.Done()
			if _, _, err := store.StartExecution(ExecutionStart{IssueNumber: number, RunID: "run_" + string(rune('0'+number)), StartedAt: now}); err == nil {
				wins <- number
			}
		}(number)
	}
	wait.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("successful starts=%d want 1", len(wins))
	}
	snapshot, err := store.Load()
	if err != nil || snapshot.ActiveExecution == nil {
		t.Fatalf("active=%+v err=%v", snapshot.ActiveExecution, err)
	}
}

func TestPendingEffectIsRootScopedAndFenced(t *testing.T) {
	snapshot := validSnapshotForInvariantTest()
	snapshot.Issues["1"].RunID = "run_1"
	now := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	if err := SetEffect(&snapshot, 1, "run_1", issuedomain.EffectMarkDone, now); err != nil {
		t.Fatal(err)
	}
	effect := PendingEffect(&snapshot, 1)
	if effect == nil || effect.Kind != issuedomain.EffectMarkDone {
		t.Fatalf("effect=%+v", effect)
	}
	if err := ClearEffect(&snapshot, 1, "effect_stale"); err == nil {
		t.Fatal("stale effect identity was accepted")
	}
	if err := ClearEffect(&snapshot, 1, effect.ID); err != nil || PendingEffect(&snapshot, 1) != nil {
		t.Fatalf("clear err=%v effect=%+v", err, PendingEffect(&snapshot, 1))
	}
}

func TestCaptureNewQuestionAfterResolvedSuspension(t *testing.T) {
	now := time.Now().UTC()
	identity := ExecutionIdentity{RunID: "run_1", Generation: 6}
	item := &Issue{Number: 1, RunID: identity.RunID, Generation: identity.Generation,
		Status:       issuedomain.StatusRunning,
		Continuation: &ContinuationCheckpoint{ID: "checkpoint_old"},
		Suspension:   &Suspension{Status: issuedomain.SuspensionResolved, CheckpointID: "checkpoint_old"},
	}
	snapshot := Snapshot{Issues: map[string]*Issue{"1": item}, ActiveExecution: &ActiveExecution{IssueNumber: 1, RunID: identity.RunID, Generation: identity.Generation}}
	if err := CaptureContinuation(&snapshot, 1, identity, "checkpoint_new", now); err != nil {
		t.Fatal(err)
	}
	if item.Suspension != nil || item.Continuation.ID != "checkpoint_new" || snapshot.ActiveExecution != nil {
		t.Fatalf("new boundary retains old suspension: %+v", item)
	}
}

func TestAnsweredQuestionSurvivesNextCheckpoint(t *testing.T) {
	request := &Request{ID: "req_first", IssueNumber: 1, RunID: "run_1", CheckpointID: "checkpoint_first", Status: issuedomain.RequestStatusAnswered, Question: "Choose?", Answer: "yes"}
	item := &Issue{Number: 1, RunID: "run_1", Continuation: &ContinuationCheckpoint{ID: "checkpoint_second"}, Answers: []AnswerRecord{{RequestID: request.ID, Question: request.Question, Answer: request.Answer}}}
	snapshot := Snapshot{Issues: map[string]*Issue{"1": item}}
	if err := validateRequestAggregate(snapshot, request.ID, request); err != nil {
		t.Fatal(err)
	}
	item.Answers[0].Answer = "different"
	if err := validateRequestAggregate(snapshot, request.ID, request); err == nil {
		t.Fatal("mismatched historical answer accepted")
	}
}

func TestResolutionReleasePreservesPublicationHeadBinding(t *testing.T) {
	for _, stage := range []issuedomain.ContinuationStage{issuedomain.ContinuationStagePublish, issuedomain.ContinuationStageChecks} {
		t.Run(string(stage), func(t *testing.T) {
			now := time.Now().UTC()
			c := &ContinuationCheckpoint{ID: "checkpoint_saved", RunID: "run_saved", Generation: 1, BaseSHA: "base", HeadSHA: "local-head", WorktreeSHA256: "saved-digest", Stage: stage, Summary: "verified repair", ResultSHA256: "saved-result"}
			item := &Issue{Number: 1, RunID: "run_saved", Generation: 1, Status: issuedomain.StatusFailed, HeadSHA: "remote-head", Continuation: c}
			snapshot := Snapshot{Issues: map[string]*Issue{"1": item}}
			if _, err := ResumeContinuation(&snapshot, 1, c.ID, now); err != nil {
				t.Fatal(err)
			}
			transition, err := issuedomain.ResolveSuspension(item.Status, issuedomain.ResolutionRetryStage, stage)
			if err != nil {
				t.Fatal(err)
			}
			if err := ApplyIssueTransition(item, transition); err != nil {
				t.Fatal(err)
			}
			finalizeLifecycleBoundaries(&snapshot, now)
			expectedHead := "remote-head"
			if stage == issuedomain.ContinuationStagePublish {
				expectedHead = "local-head"
			}
			if snapshot.ActiveExecution != nil || item.Continuation.HeadSHA != expectedHead || item.Continuation.WorktreeSHA256 != "saved-digest" || item.Continuation.ResultSHA256 != "saved-result" || item.Continuation.Generation != 2 || item.HeadSHA != "remote-head" {
				t.Fatalf("continuation binding changed on execution release: %+v", item.Continuation)
			}
		})
	}
}

func TestReconcileStoppedAssignmentRecoversConflictLaunch(t *testing.T) {
	store, now := productionUnstartedConflictLaunchFixture(t)
	observedAt := now.Add(time.Minute)
	if err := store.ReconcileStoppedAssignment(observedAt); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["277"]
	if snapshot.ActiveExecution != nil || item.Status != issuedomain.StatusResolvingConflict ||
		item.LaunchSource != issuedomain.StatusUnset || item.Generation != 15 ||
		item.Continuation == nil || item.Continuation.Generation != 14 || !item.UpdatedAt.Equal(observedAt) {
		t.Fatalf("active=%+v issue=%+v", snapshot.ActiveExecution, item)
	}
	if snapshot.Supervisor.State != SupervisorStateStopped || snapshot.Supervisor.PID != 0 ||
		snapshot.Supervisor.Message != "retained stopped assignment retry" {
		t.Fatalf("supervisor=%+v", snapshot.Supervisor)
	}
}

func TestReconcileStoppedAssignmentRejectsUnsafeRecovery(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "worker process identity", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["277"].WorkerPID = 8123
			snapshot.Issues["277"].WorkerPGID = 8123
		}},
		{name: "missing conflict evidence", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation = nil }},
		{name: "non-launching execution", mutate: func(snapshot *Snapshot) {
			snapshot.Issues["277"].Status = issuedomain.StatusClaimed
			snapshot.Issues["277"].LaunchSource = issuedomain.StatusUnset
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, now := productionUnstartedConflictLaunchFixture(t)
			before, err := store.Update("fixture_unsafe_recovery", 277, "", nil, func(snapshot *Snapshot) error {
				test.mutate(snapshot)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.ReconcileStoppedAssignment(now.Add(time.Minute)); err == nil {
				t.Fatal("unsafe recovery was accepted")
			}
			after, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected recovery changed state: before=%+v after=%+v", before, after)
			}
		})
	}
}

package state

import (
	"sync"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

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

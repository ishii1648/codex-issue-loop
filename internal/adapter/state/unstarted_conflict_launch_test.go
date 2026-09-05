package state

import (
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func productionUnstartedConflictLaunchFixture(t *testing.T) (Store, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 5, 17, 20, 20, 136638000, time.UTC)
	store := Store{Dir: t.TempDir(), RepoID: "codex-issue-loop-4cc1c9a4", RepoPath: "/repo"}
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.StartExecution(ExecutionStart{
		IssueNumber: 277, Title: "queue-level monitor", RunID: "conflict_174e861a0a076558",
		BaseSHA: "35bcdbd67421986aa7a6a6ab87a81c1352c3e107", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	workspace := &WorkerWorkspace{
		Path: "/state/worktrees/issue-277", Branch: "codex/issue-277-p1-queue-level-deadline-event-replay",
		RepoID: store.RepoID, Repository: "ishii1648/codex-issue-loop", RepositoryID: 1334901081,
		GitCommonDir: "/repo/.git", MainCheckout: "/repo", CapturedAt: now.Add(-2 * time.Hour),
	}
	_, err := store.Update("fixture_unstarted_conflict_launch", 277, "conflict_174e861a0a076558", nil, func(snapshot *Snapshot) error {
		item := snapshot.Issues["277"]
		item.Status = issuedomain.StatusLaunching
		item.Generation = 15
		item.LaunchSource = issuedomain.StatusResolvingConflict
		item.Branch, item.Worktree, item.Workspace = workspace.Branch, workspace.Path, workspace
		item.PullRequestURL = "https://github.com/ishii1648/codex-issue-loop/pull/281"
		item.PullRequestNumber = 281
		item.HeadSHA = "684d57bf587e162ed396ed6928f4850ffe488d89"
		item.Continuation = &ContinuationCheckpoint{
			ID: "checkpoint_b5676a437bbfb3dc", CreatedAt: now.Add(-9 * time.Second), RunID: item.RunID, Generation: 14,
			BaseSHA: snapshot.ActiveExecution.BaseSHA, Workspace: cloneWorkspace(workspace), HeadSHA: item.HeadSHA,
			PullRequestURL: item.PullRequestURL, PullRequestNumber: item.PullRequestNumber, Stage: issuedomain.ContinuationStageChecks,
		}
		item.ConflictRecovery = &ConflictRecovery{
			PullRequestURL: item.PullRequestURL, PreviousBaseSHA: snapshot.ActiveExecution.BaseSHA,
			TargetBaseSHA: "812eb89c45087e4ef38105414c9f8a5329d51a70", OriginalHeadSHA: item.HeadSHA,
			ConflictFiles: []string{"monitor/internal/model/replay.go"}, Attempts: 2,
			Verification: []ConflictVerification{{Command: "go test ./...", Result: "成功"}},
		}
		item.UpdatedAt = now
		snapshot.ActiveExecution.Generation = item.Generation
		snapshot.Supervisor.State = SupervisorStateMaintenance
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, now
}

func TestRecoverUnstartedConflictLaunchRestoresProductionLaunchSource(t *testing.T) {
	store, now := productionUnstartedConflictLaunchFixture(t)
	snapshot, recovered, err := store.RecoverUnstartedConflictLaunch(now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["277"]
	if !recovered || item.Status != issuedomain.StatusResolvingConflict || item.LaunchSource != issuedomain.StatusUnset || snapshot.ActiveExecution != nil {
		t.Fatalf("recovered=%v active=%+v issue=%+v", recovered, snapshot.ActiveExecution, item)
	}
	if item.Continuation == nil || item.Continuation.Generation != 14 {
		t.Fatalf("continuation was not preserved: %+v", item.Continuation)
	}
}

func TestRecoverUnstartedConflictLaunchRejectsIncompleteEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{name: "continuation run mismatch", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation.RunID = "conflict_stale" }},
		{name: "continuation generation mismatch", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation.Generation = 15 }},
		{name: "continuation generation gap", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation.Generation = 13 }},
		{name: "missing checkpoint", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation = nil }},
		{name: "workspace mismatch", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].Continuation.Workspace.Branch = "codex/stale" }},
		{name: "conflict base mismatch", mutate: func(snapshot *Snapshot) { snapshot.Issues["277"].ConflictRecovery.PreviousBaseSHA = "stale" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, now := productionUnstartedConflictLaunchFixture(t)
			if _, err := store.Update("fixture_mismatch", 277, "conflict_174e861a0a076558", nil, func(snapshot *Snapshot) error {
				test.mutate(snapshot)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, recovered, err := store.RecoverUnstartedConflictLaunch(now.Add(time.Minute))
			if err == nil || recovered || !strings.Contains(err.Error(), "inconsistent") {
				t.Fatalf("recovered=%v err=%v", recovered, err)
			}
			unchanged, loadErr := store.Load()
			if loadErr != nil || unchanged.ActiveExecution == nil || unchanged.Issues["277"].Status != issuedomain.StatusLaunching {
				t.Fatalf("mismatched evidence changed ownership: active=%+v issue=%+v err=%v", unchanged.ActiveExecution, unchanged.Issues["277"], loadErr)
			}
		})
	}
}

func TestRecoverUnstartedConflictLaunchRejectsActiveIdentityMismatch(t *testing.T) {
	store, _ := productionUnstartedConflictLaunchFixture(t)
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Snapshot){
		func(snapshot *Snapshot) { snapshot.ActiveExecution.RunID = "conflict_stale" },
		func(snapshot *Snapshot) { snapshot.ActiveExecution.Generation-- },
	} {
		candidate := snapshot
		active := *snapshot.ActiveExecution
		candidate.ActiveExecution = &active
		mutate(&candidate)
		if err := validateUnstartedConflictLaunch(&candidate, candidate.Issues["277"]); err == nil {
			t.Fatal("mismatched active execution was accepted")
		}
		if snapshot.ActiveExecution == nil {
			t.Fatal("identity validation released ownership")
		}
	}
}

func TestRecoverUnstartedConflictLaunchDoesNotReleaseRecordedWorker(t *testing.T) {
	store, now := productionUnstartedConflictLaunchFixture(t)
	if _, err := store.Update("fixture_worker_started", 277, "conflict_174e861a0a076558", nil, func(snapshot *Snapshot) error {
		snapshot.Issues["277"].WorkerPID = 8123
		snapshot.Issues["277"].WorkerPGID = 8123
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, recovered, err := store.RecoverUnstartedConflictLaunch(now.Add(time.Minute))
	if err != nil || recovered || snapshot.ActiveExecution == nil || snapshot.Issues["277"].WorkerPID != 8123 {
		t.Fatalf("recovered=%v active=%+v issue=%+v err=%v", recovered, snapshot.ActiveExecution, snapshot.Issues["277"], err)
	}
}

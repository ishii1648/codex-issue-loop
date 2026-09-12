package state

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
)

func conflictApprovalSnapshot(t *testing.T, store Store) Snapshot {
	t.Helper()
	now := time.Now().UTC()
	workspace := &WorkerWorkspace{Path: "/worktree", Branch: "codex/issue-287", RepoID: store.RepoID, Repository: "owner/repo", GitCommonDir: "/repo/.git", MainCheckout: "/repo", CapturedAt: now}
	snapshot := store.emptySnapshot()
	snapshot.Issues["287"] = &Issue{
		Number: 287, RunID: "run_287", Generation: 2, Status: issuedomain.StatusBlocked, Branch: workspace.Branch, Worktree: workspace.Path, Workspace: workspace,
		HeadSHA: "head", PullRequestNumber: 287, PullRequestURL: "https://example.test/pull/287",
		Continuation:     &ContinuationCheckpoint{ID: "checkpoint_saved", CreatedAt: now, RunID: "run_287", Generation: 2, Stage: issuedomain.ContinuationStageResume, BaseSHA: "base", HeadSHA: "head", WorktreeSHA256: strings.Repeat("a", 64), Workspace: workspace, PullRequestNumber: 287, PullRequestURL: "https://example.test/pull/287"},
		Suspension:       &Suspension{ID: "suspension_saved", Status: issuedomain.SuspensionActive, ReasonCode: "issue", Reason: "outside recorded scope", Recoverability: issuedomain.RecoverabilityOperator, AllowedActions: []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage}, CheckpointID: "checkpoint_saved", SuspendedAt: now},
		ConflictRecovery: &ConflictRecovery{OriginalHeadSHA: "head", TargetBaseSHA: "target", PullRequestURL: "https://example.test/pull/287", AllowedPaths: []string{"state.go"}, Attempts: 3, History: []ConflictAttempt{{Number: 1, BaseSHA: "target", Status: issuedomain.ConflictAttemptStatusBlocked, StartedAt: now, FinishedAt: now}}},
	}
	snapshot.PendingRequests["req_saved"] = &Request{ID: "req_saved", IssueNumber: 287, RunID: "run_287", Question: "追加してよいか", Status: issuedomain.RequestStatusAnswered, Answer: "diagnosis.go と diagnosis_test.go を承認", CreatedAt: now, AnsweredAt: &now}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestConflictPathApprovalCommitRejectsStaleEvidence(t *testing.T) {
	for _, name := range []string{"run", "generation", "suspension", "checkpoint", "head", "digest", "pr", "workspace", "worker", "execution", "pending", "path", "empty"} {
		t.Run(name, func(t *testing.T) {
			snapshot := conflictApprovalSnapshot(t, newStore(t))
			item := snapshot.Issues["287"]
			observed := OperatorResolutionObservation{HeadSHA: "head", WorktreeSHA256: strings.Repeat("a", 64), AllowedPaths: []string{"diagnosis.go"}}
			switch name {
			case "run":
				item.Continuation.RunID = "other"
			case "generation":
				item.Continuation.Generation--
			case "suspension":
				item.Suspension.Status = issuedomain.SuspensionQuarantined
			case "checkpoint":
				item.Suspension.CheckpointID = "checkpoint_other"
			case "head":
				observed.HeadSHA = "other"
			case "digest":
				observed.WorktreeSHA256 = strings.Repeat("b", 64)
			case "pr":
				item.Continuation.PullRequestNumber++
			case "workspace":
				item.Continuation.Workspace = &WorkerWorkspace{}
			case "worker":
				item.WorkerPGID = 123
			case "execution":
				snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 288, RunID: "run_other", Generation: 1}
			case "pending":
				snapshot.PendingRequests["req_new"] = &Request{ID: "req_new", IssueNumber: 287, Status: issuedomain.RequestStatusPending}
			case "path":
				observed.AllowedPaths = []string{"diagnosis.go", "../outside"}
			case "empty":
				observed.AllowedPaths = nil
			}
			before, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := ResolveOperatorSuspension(&snapshot, 287, issuedomain.ResolutionApproveConflictPaths, observed, time.Now().UTC()); err == nil {
				t.Fatal("accepted inconsistent evidence")
			}
			after, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("rejected commit mutated state")
			}
		})
	}
}

func TestFaultConflictPathApprovalRecoversOnceAtEveryTransactionPoint(t *testing.T) {
	for _, crashPoint := range []string{"prepared", "event_appended", "snapshot_written"} {
		t.Run(crashPoint, func(t *testing.T) {
			store := newStore(t)
			saved := conflictApprovalSnapshot(t, store)
			base, err := store.Update("fixture", 287, "run_287", nil, func(snapshot *Snapshot) error {
				snapshot.Issues = saved.Issues
				snapshot.PendingRequests = saved.PendingRequests
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			next := base
			now := time.Now().UTC()
			paths := []string{"diagnosis.go", "diagnosis_test.go"}
			observed := OperatorResolutionObservation{HeadSHA: "head", WorktreeSHA256: strings.Repeat("a", 64), AllowedPaths: paths}
			if err := ResolveOperatorSuspension(&next, 287, issuedomain.ResolutionApproveConflictPaths, observed, now); err != nil {
				t.Fatal(err)
			}
			next.StateRevision++
			next.Supervisor.UpdatedAt = now
			if err := next.Validate(); err != nil {
				t.Fatal(err)
			}
			event := Event{Version: CurrentVersion, EventID: "evt_approval", Sequence: next.StateRevision, Timestamp: now, RepoID: store.RepoID, IssueNumber: 287, RunID: "run_287", Type: "issue_conflict_paths_approved"}
			if err := fsutil.WriteJSON(store.TransactionPath(), transaction{Version: CurrentVersion, Snapshot: next, Event: event}, 0600); err != nil {
				t.Fatal(err)
			}
			if crashPoint != "prepared" {
				if err := store.appendEventUnlocked(event); err != nil {
					t.Fatal(err)
				}
			}
			if crashPoint == "snapshot_written" {
				if err := fsutil.WriteJSON(store.StatePath(), next, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				loaded, err := store.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(loaded.Issues["287"], next.Issues["287"]) || !reflect.DeepEqual(loaded.PendingRequests, next.PendingRequests) || loaded.StateRevision != next.StateRevision || loaded.ActiveExecution != nil {
					t.Fatal("recovery changed approval evidence")
				}
			}
			if _, err := os.Stat(store.TransactionPath()); !os.IsNotExist(err) {
				t.Fatalf("transaction retained: %v", err)
			}
			events, _, partial, err := store.readEventsUnlocked()
			if err != nil || partial || len(events) != 2 || events[1].Type != "issue_conflict_paths_approved" {
				t.Fatalf("events=%+v err=%v", events, err)
			}
		})
	}
}

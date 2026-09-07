package state

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func TestCancelQuarantinedIssueRetainsArtifactsAndOtherExecution(t *testing.T) {
	now := time.Now().UTC()
	saved := &Issue{Number: 1, RunID: "run_1", Generation: 2, Status: issuedomain.StatusBlocked,
		Branch: "codex/issue-1", Worktree: "/saved/worktree", SessionID: "session_1", PullRequestURL: "https://github.com/owner/repo/pull/2"}
	active := &ActiveExecution{IssueNumber: 3, RunID: "run_3", Generation: 1}
	snapshot := Snapshot{Issues: map[string]*Issue{}, QuarantinedIssues: map[string]*QuarantineRecord{
		"1": {IssueNumber: 1, RunID: "run_1", Generation: 2, LastValid: saved},
	}, ActiveExecution: active}
	if err := CancelQuarantinedIssue(&snapshot, 1, now); err != nil {
		t.Fatal(err)
	}
	item := snapshot.Issues["1"]
	if item == nil || item.Status != issuedomain.StatusCanceled || item.RunID != saved.RunID || item.Generation != saved.Generation ||
		item.Branch != saved.Branch || item.Worktree != saved.Worktree || item.SessionID != saved.SessionID || item.PullRequestURL != saved.PullRequestURL ||
		item.Cancellation == nil || item.Cancellation.Source != "operator_quarantine_resolution" || snapshot.QuarantinedIssues["1"] != nil ||
		snapshot.ActiveExecution != active || saved.Status != issuedomain.StatusBlocked {
		t.Fatalf("quarantine cancellation lost evidence or execution ownership: %+v", snapshot)
	}
}

func TestCancelQuarantinedIssueRejectsRetainedExecutionWithoutMutation(t *testing.T) {
	record := &QuarantineRecord{IssueNumber: 1, RunID: "run_1", Generation: 2}
	active := &ActiveExecution{IssueNumber: 1, RunID: "run_1", Generation: 2}
	snapshot := Snapshot{Issues: map[string]*Issue{}, QuarantinedIssues: map[string]*QuarantineRecord{"1": record}, ActiveExecution: active}
	if err := CancelQuarantinedIssue(&snapshot, 1, time.Now().UTC()); err == nil {
		t.Fatal("canceled quarantine retaining execution authority")
	}
	if len(snapshot.Issues) != 0 || !reflect.DeepEqual(snapshot.QuarantinedIssues, map[string]*QuarantineRecord{"1": record}) || snapshot.ActiveExecution != active {
		t.Fatal("rejected cancellation changed state")
	}
}

func TestOperatorAdoptionCommit(t *testing.T) {
	now := time.Now().UTC()
	for _, action := range []issuedomain.ResolutionAction{issuedomain.ResolutionAdoptHead, issuedomain.ResolutionAdoptWorktree, issuedomain.ResolutionAdoptPR, issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage} {
		t.Run(string(action), func(t *testing.T) {
			for _, scenario := range []string{"valid", "invalid-evidence", "occupied-slot", "worker-identity", "resolved-suspension"} {
				invalid := scenario != "valid"
				item := &Issue{Number: 1, RunID: "run_1", Generation: 1, Status: issuedomain.StatusBlocked,
					Suspension:   &Suspension{Status: issuedomain.SuspensionActive, Origin: "worker", CheckpointID: "checkpoint_1", AllowedActions: []issuedomain.ResolutionAction{action, issuedomain.ResolutionResume}},
					Continuation: &ContinuationCheckpoint{ID: "checkpoint_1", RunID: "run_1", Generation: 1, Stage: issuedomain.ContinuationStageResume, WorktreeSHA256: strings.Repeat("a", 64), Session: &WorkerSession{ID: "session_1"}, Workspace: &WorkerWorkspace{}}, Workspace: &WorkerWorkspace{}}
				observed := OperatorResolutionObservation{HeadSHA: "adopted", WorktreeSHA256: strings.Repeat("b", 64), AllowedPaths: []string{"b", "a"}, PullRequestURL: "https://github.com/o/r/pull/2", PullRequestNumber: 2}
				if action == issuedomain.ResolutionAdoptWorktree {
					item.Continuation.WorktreeSHA256 = ""
					item.Suspension.Status = issuedomain.SuspensionQuarantined
					item.Suspension.Recoverability = issuedomain.RecoverabilityAmbiguous
					item.Suspension.MissingEvidence = []string{"worktree_sha256"}
					item.Suspension.AllowedActions = []issuedomain.ResolutionAction{issuedomain.ResolutionCancel}
					item.ConflictRecovery = &ConflictRecovery{AllowedPaths: []string{"a"}}
				}
				if action == issuedomain.ResolutionRetryStage {
					item.Continuation.Stage = issuedomain.ContinuationStageChecks
				}
				if scenario == "invalid-evidence" {
					switch action {
					case issuedomain.ResolutionAdoptHead:
						item.Continuation.HeadSHA = "already-recorded"
					case issuedomain.ResolutionAdoptWorktree:
						item.Continuation.Stage = issuedomain.ContinuationStagePublish
					case issuedomain.ResolutionAdoptPR:
						observed.PullRequestNumber = 0
					case issuedomain.ResolutionCancel:
						item.Status = issuedomain.StatusRunning
					case issuedomain.ResolutionRetryStage:
						observed.HeadSHA = ""
					}
				}
				snapshot := Snapshot{Issues: map[string]*Issue{"1": item}, PendingEffects: map[string]*EffectIntent{}}

				switch scenario {
				case "occupied-slot":
					snapshot.ActiveExecution = &ActiveExecution{IssueNumber: 1, RunID: item.RunID, Generation: item.Generation}
				case "worker-identity":
					item.WorkerPID = 123
				case "resolved-suspension":
					item.Suspension.Status = issuedomain.SuspensionResolved
				}
				before, marshalErr := json.Marshal(snapshot)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err := ResolveOperatorSuspension(&snapshot, 1, action, observed, now)
				if invalid {
					after, marshalErr := json.Marshal(snapshot)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					if err == nil || string(before) != string(after) {
						t.Fatalf("invalid %s mutated state or succeeded: %v", action, err)
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				switch action {
				case issuedomain.ResolutionAdoptHead:
					if item.Continuation.HeadSHA != "adopted" || item.Suspension.Status != issuedomain.SuspensionActive || item.Generation != 1 {
						t.Fatalf("unexpected head adoption: %+v", item)
					}
				case issuedomain.ResolutionAdoptWorktree:
					if item.Continuation.Stage != issuedomain.ContinuationStageConflict || item.Continuation.WorktreeSHA256 != observed.WorktreeSHA256 || item.Suspension.Status != issuedomain.SuspensionActive || item.Suspension.Recoverability != issuedomain.RecoverabilityOperator || len(item.Suspension.MissingEvidence) != 0 || !reflect.DeepEqual(item.ConflictRecovery.AllowedPaths, []string{"a", "b"}) || !reflect.DeepEqual(item.Suspension.AllowedActions, []issuedomain.ResolutionAction{issuedomain.ResolutionCancel, issuedomain.ResolutionRetryStage}) {
						t.Fatalf("unexpected worktree adoption: %+v", item)
					}
				case issuedomain.ResolutionAdoptPR:
					if item.Status != issuedomain.StatusCompleted || !item.PullRequestMerged || item.HeadSHA != observed.HeadSHA || item.PullRequestNumber != 2 || item.PullRequestURL != observed.PullRequestURL || item.Continuation != nil || item.Suspension != nil || PendingEffect(&snapshot, 1).Kind != issuedomain.EffectMarkDone {
						t.Fatalf("unexpected PR adoption: %+v", item)
					}
				case issuedomain.ResolutionCancel:
					if item.Status != issuedomain.StatusCanceled || item.Cancellation == nil || item.Cancellation.PreviousStatus != issuedomain.StatusBlocked || item.Suspension.Resolution != action {
						t.Fatalf("unexpected cancellation: %+v", item)
					}
				case issuedomain.ResolutionRetryStage:
					if item.Status != issuedomain.StatusAwaitingChecks || item.HeadSHA != observed.HeadSHA || item.PullRequestNumber != 2 || item.Generation != 2 || snapshot.ActiveExecution == nil || item.Suspension.Resolution != action || PendingEffect(&snapshot, 1).Kind != issuedomain.EffectApplyResolution {
						t.Fatalf("unexpected checks retry: %+v", item)
					}
				}
			}
		})
	}
}

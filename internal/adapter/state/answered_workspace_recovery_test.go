package state

import (
	"encoding/json"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/worktree"
)

func TestAnsweredWorkspaceRecoveryExactZeitreise449Fixture(t *testing.T) {
	snapshot, events := answeredWorkspace449Fixture(t)
	issue := snapshot.Issues["449"]
	request := snapshot.PendingRequests["req_6058cb295f5cb9ff"]
	evidence, err := answeredWorkspaceRecoveryFromEvents(events, *issue, *request)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.OriginalOwner.Generation != 1 || evidence.ResumeOwner.Generation != 2 ||
		evidence.RequestID != request.ID || evidence.ResourceParkID != issue.ResourcePark.ID || !evidence.RejectedLaunch.Valid {
		t.Fatalf("evidence=%+v", evidence)
	}

	store := Store{Dir: t.TempDir(), RepoID: snapshot.RepoID, RepoPath: snapshot.RepoPath}
	eventData, err := os.ReadFile("testdata/zeitreise-449-v0622-answered-missing-workspace-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), eventData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AnsweredWorkspaceRecoveryEvidence(*issue, *request); err != nil {
		t.Fatalf("store evidence rejected: %v", err)
	}
}

func TestAnsweredWorkspaceRecoveryAcceptsOneExactVerifiedProvenanceEvent(t *testing.T) {
	snapshot, events := answeredWorkspace449Fixture(t)
	issue := snapshot.Issues["449"]
	request := snapshot.PendingRequests["req_6058cb295f5cb9ff"]
	events = appendVerifiedAnsweredWorkspaceEvent(t, events, issue)
	if err := validateAnsweredWorkspaceRecoveryState(*issue, *request); err != nil {
		t.Fatal(err)
	}
	evidence, err := answeredWorkspaceRecoveryFromEvents(events, *issue, *request)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.VerifiedLaunch == nil || !evidence.VerifiedLaunch.Valid || evidence.VerifiedLaunch.Branch != issue.Branch {
		t.Fatalf("verified evidence=%+v", evidence)
	}
}

func TestAnsweredWorkspaceRecoveryRejectsMalformedVerifiedProvenanceEvent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Event, *Issue) []Event
	}{
		{name: "missing", mutate: func(events []Event, _ *Issue) []Event { return events[:len(events)-1] }},
		{name: "duplicate", mutate: func(events []Event, _ *Issue) []Event { return append(events, events[len(events)-1]) }},
		{name: "wrong_order", mutate: func(events []Event, _ *Issue) []Event {
			events[len(events)-1], events[len(events)-2] = events[len(events)-2], events[len(events)-1]
			return events
		}},
		{name: "different_run", mutate: func(events []Event, _ *Issue) []Event {
			events[len(events)-1].RunID = "run_other"
			return events
		}},
		{name: "sequence_gap", mutate: func(events []Event, _ *Issue) []Event {
			events[len(events)-1].Sequence++
			return events
		}},
		{name: "different_status", mutate: mutateVerifiedEventPayload(func(payload map[string]any) { payload["previous_status"] = "failed" })},
		{name: "different_head", mutate: mutateVerifiedEventPayload(func(payload map[string]any) { payload["head_sha"] = "1111111111111111111111111111111111111111" })},
		{name: "different_fingerprint", mutate: mutateVerifiedEventPayload(func(payload map[string]any) { payload["worktree_sha256"] = "different" })},
		{name: "validator_failed", mutate: mutateVerifiedEventPayload(func(payload map[string]any) {
			payload["validator"].(map[string]any)["checks"].(map[string]any)["repository_identity"] = false
		})},
		{name: "different_repository", mutate: mutateVerifiedEventPayload(func(payload map[string]any) {
			payload["actual_workspace"].(map[string]any)["repository"] = "other/repository"
		})},
		{name: "different_branch", mutate: mutateVerifiedEventPayload(func(payload map[string]any) {
			payload["validator"].(map[string]any)["branch"] = "codex/other"
		})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot, events := answeredWorkspace449Fixture(t)
			issue := snapshot.Issues["449"]
			request := snapshot.PendingRequests["req_6058cb295f5cb9ff"]
			events = appendVerifiedAnsweredWorkspaceEvent(t, events, issue)
			events = test.mutate(events, issue)
			if _, err := answeredWorkspaceRecoveryFromEvents(events, *issue, *request); err == nil {
				t.Fatal("malformed verified provenance event was accepted")
			}
		})
	}
}

func TestAnsweredWorkspaceRecoveryRejectsMalformedChainsWithoutMutation(t *testing.T) {
	snapshot, baseline := answeredWorkspace449Fixture(t)
	tests := []struct {
		name   string
		mutate func([]Event, *Issue, *Request) []Event
	}{
		{name: "short", mutate: func(events []Event, _ *Issue, _ *Request) []Event { return events[:len(events)-1] }},
		{name: "duplicate", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			return append(append([]Event(nil), events[:7]...), append([]Event{events[6]}, events[7:]...)...)
		}},
		{name: "superseded", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			extra := events[len(events)-1]
			extra.EventID = "evt_superseding"
			extra.Sequence++
			extra.Timestamp = extra.Timestamp.Add(time.Second)
			extra.Type = "retry_scheduled"
			return append(events, extra)
		}},
		{name: "reordered", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			events[6], events[7] = events[7], events[6]
			return events
		}},
		{name: "cross_run", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			events[8].RunID = "run_other"
			return events
		}},
		{name: "wrong_request", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			var payload map[string]any
			_ = json.Unmarshal(events[7].Payload, &payload)
			payload["request_id"] = "req_other"
			events[7].Payload, _ = json.Marshal(payload)
			return events
		}},
		{name: "wrong_park", mutate: func(events []Event, _ *Issue, request *Request) []Event {
			request.ResourceParkID = "park_other"
			return events
		}},
		{name: "wrong_session", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Session.ID = "session_other"
			return events
		}},
		{name: "wrong_branch", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Branch = "codex/issue-449-other"
			return events
		}},
		{name: "missing_repository_identity", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			var payload map[string]any
			_ = json.Unmarshal(events[9].Payload, &payload)
			payload["validation"].(map[string]any)["git_common_dir"] = ""
			events[9].Payload, _ = json.Marshal(payload)
			return events
		}},
		{name: "validator_error", mutate: func(events []Event, _ *Issue, _ *Request) []Event {
			var payload map[string]any
			_ = json.Unmarshal(events[9].Payload, &payload)
			validation := payload["validation"].(map[string]any)
			validation["checks"].(map[string]any)["repository_identity"] = false
			events[9].Payload, _ = json.Marshal(payload)
			return events
		}},
		{name: "workspace_present", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Workspace = &WorkerWorkspace{Path: issue.Worktree}
			return events
		}},
		{name: "active_worker", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.WorkerPID, issue.WorkerPGID = 99, 99
			return events
		}},
		{name: "base_mismatch", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Lease.BaseSHA = "1111111111111111111111111111111111111111"
			return events
		}},
		{name: "lease_resources_mismatch", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Lease.ResolvedResources = []string{"other"}
			return events
		}},
		{name: "generation_mismatch", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Lease.Owner.Generation = 3
			issue.LeaseGeneration = 3
			return events
		}},
		{name: "duplicate_answer", mutate: func(events []Event, issue *Issue, _ *Request) []Event {
			issue.Answers = append(issue.Answers, issue.Answers[0])
			return events
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			issueBytes, _ := json.Marshal(snapshot.Issues["449"])
			requestBytes, _ := json.Marshal(snapshot.PendingRequests["req_6058cb295f5cb9ff"])
			var issue Issue
			var request Request
			_ = json.Unmarshal(issueBytes, &issue)
			_ = json.Unmarshal(requestBytes, &request)
			events := append([]Event(nil), baseline...)
			events = test.mutate(events, &issue, &request)
			if err := validateAnsweredWorkspaceRecoveryState(issue, request); err == nil {
				if _, err := answeredWorkspaceRecoveryFromEvents(events, issue, request); err == nil {
					t.Fatal("malformed answered workspace recovery chain was accepted")
				}
			}
		})
	}
}

func answeredWorkspace449Fixture(t *testing.T) (Snapshot, []Event) {
	t.Helper()
	stateData, err := os.ReadFile("testdata/zeitreise-449-v0622-answered-missing-workspace-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(stateData, &snapshot); err != nil {
		t.Fatal(err)
	}
	eventData, err := os.ReadFile("testdata/zeitreise-449-v0622-answered-missing-workspace-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	for _, line := range fixtureEventLines(eventData) {
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return snapshot, events
}

func appendVerifiedAnsweredWorkspaceEvent(t *testing.T, events []Event, issue *Issue) []Event {
	t.Helper()
	var rejected struct {
		Validation worktree.LaunchValidation `json:"validation"`
	}
	if err := json.Unmarshal(events[9].Payload, &rejected); err != nil {
		t.Fatal(err)
	}
	now := events[len(events)-1].Timestamp.Add(time.Second)
	workspace := WorkerWorkspace{
		Path: issue.Worktree, Branch: issue.Branch, RepoID: events[0].RepoID, Repository: "owner/zeitreise",
		GitCommonDir: rejected.Validation.CommonDir, MainCheckout: rejected.Validation.MainCheckout, CapturedAt: now,
	}
	checks := map[string]bool{
		"managed_root": true, "no_symlink_components": true, "canonical_path": true, "not_main_checkout": true,
		"git_top_level": true, "repository_identity": true, "saved_branch": true,
	}
	validator := worktree.LaunchValidation{
		Valid: true, ExpectedCWD: issue.Worktree, CanonicalCWD: issue.Worktree, TopLevel: issue.Worktree,
		Branch: issue.Branch, CommonDir: workspace.GitCommonDir, MainCheckout: workspace.MainCheckout, Checks: checks,
	}
	recovery := &WorkspaceProvenanceRecovery{
		ID: "workspace_recovery_verified449", Status: issuedomain.WorkspaceProvenanceRecoveryStatusVerified, ConfirmedAt: now, OperatorConfirmed: true,
		OldProvenanceMissing: true, PreviousStatus: issuedomain.StatusBlocked, RunID: issue.RunID,
		HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorktreeSHA256: "verified-content-digest",
		ExpectedWorkspace: workspace, ActualWorkspace: workspace, ValidatorChecks: checks,
	}
	issue.Workspace, issue.WorkspaceRecovery = &workspace, recovery
	payload, err := json.Marshal(map[string]any{
		"recovery_id": recovery.ID, "operator_confirmation": map[string]bool{"confirm_verified_workspace": true},
		"mutation_scope":  []string{"issues[].workspace", "issues[].workspace_provenance_recovery", "events.jsonl"},
		"previous_status": issue.Status, "old_provenance_missing": true, "head_sha": recovery.HeadSHA,
		"worktree_sha256": recovery.WorktreeSHA256, "pull_request_url": issue.PullRequestURL,
		"expected_workspace": workspace, "actual_workspace": workspace, "validator": validator,
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(events, Event{
		Version: events[len(events)-1].Version, EventID: "evt_449_verified_workspace", Sequence: events[len(events)-1].Sequence + 1,
		Timestamp: now, RepoID: events[0].RepoID, IssueNumber: issue.Number, RunID: issue.RunID,
		Type: "workspace_provenance_recovered", Payload: payload,
	})
}

func mutateVerifiedEventPayload(mutate func(map[string]any)) func([]Event, *Issue) []Event {
	return func(events []Event, _ *Issue) []Event {
		index := len(events) - 1
		var payload map[string]any
		_ = json.Unmarshal(events[index].Payload, &payload)
		mutate(payload)
		events[index].Payload, _ = json.Marshal(payload)
		return events
	}
}

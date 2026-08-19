package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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

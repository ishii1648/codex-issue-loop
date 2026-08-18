package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func interruptedWorkspaceResumeFixture(t *testing.T) (Store, Issue, []byte) {
	t.Helper()
	data, err := os.ReadFile("testdata/zeitreise-442-v0614-full-27-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile("testdata/zeitreise-442-v0614-full-27-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var issue Issue
	if err := json.Unmarshal(stateData, &issue); err != nil {
		t.Fatal(err)
	}
	return store, issue, data
}

func TestInterruptedWorkspaceResumeEvidenceFromZeitreise442Full27EventFixture(t *testing.T) {
	store, issue, _ := interruptedWorkspaceResumeFixture(t)
	evidence, err := store.InterruptedWorkspaceResumeEvidence(issue)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ResumeID != issue.EnvironmentResume.ID || evidence.PreviousReason != issue.EnvironmentResume.PreviousReason ||
		evidence.BaseSHA != issue.EnvironmentResume.BaseSHA || evidence.CurrentBaseSHA != issue.EnvironmentResume.CurrentBaseSHA ||
		evidence.LeaseOwner != issue.Lease.Owner || evidence.LeaseSlot != issue.Lease.Slot || !evidence.LegacyLeaseRecovered {
		t.Fatalf("evidence=%+v issue=%+v", evidence, issue)
	}
}

func TestInterruptedWorkspaceResumeEvidenceRetainsSyntheticShortFixtureCompatibility(t *testing.T) {
	data, err := os.ReadFile("testdata/zeitreise-442-v0614-missing-workspace-resume-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile("testdata/zeitreise-442-v0614-missing-workspace-resume-state.json")
	if err != nil {
		t.Fatal(err)
	}
	var issue Issue
	if err := json.Unmarshal(stateData, &issue); err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(store.EventsPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err != nil {
		t.Fatalf("v0.6.16 synthetic compatibility fixture was rejected: %v", err)
	}
}

func TestInterruptedWorkspaceResumeCandidateFailsClosedForOtherSupervisorBlocks(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "workspace mismatch", mutate: func(issue *Issue) {
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/zeitreise-442: saved workspace provenance does not match the launch target"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "symlink", mutate: func(issue *Issue) {
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/zeitreise-442: worker worktree path must not contain a symbolic link"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "manual supervisor block", mutate: func(issue *Issue) { issue.BlockedCause.Kind = "manual" }},
		{name: "security supervisor block", mutate: func(issue *Issue) { issue.BlockedCause.Kind = "security" }},
		{name: "publication recovery", mutate: func(issue *Issue) { issue.PublicationRecovery = &PublicationRecovery{ID: "publication_442"} }},
		{name: "active worker", mutate: func(issue *Issue) { issue.WorkerPID = 442 }},
		{name: "active worker process group", mutate: func(issue *Issue) { issue.WorkerPGID = 442 }},
		{name: "workspace already saved", mutate: func(issue *Issue) { issue.Workspace = &WorkerWorkspace{Path: issue.Worktree} }},
		{name: "lease generation changed", mutate: func(issue *Issue) { issue.LeaseGeneration++ }},
		{name: "resume reason missing", mutate: func(issue *Issue) { issue.EnvironmentResume.PreviousReason = "" }},
		{name: "session missing", mutate: func(issue *Issue) { issue.SessionID = "" }},
		{name: "resource park on running resume", mutate: func(issue *Issue) { issue.ResourcePark = &ResourceLeasePark{ID: "unexpected"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, issue, _ := interruptedWorkspaceResumeFixture(t)
			test.mutate(&issue)
			if MayHaveInterruptedWorkspaceResumeEvidence(&issue) {
				t.Fatalf("unexpected recovery candidate: %+v", issue)
			}
		})
	}
}

func TestInterruptedWorkspaceResumeEvidenceRejectsTamperedOrReorderedHistory(t *testing.T) {
	t.Run("changed previous reason", func(t *testing.T) {
		store, issue, _ := interruptedWorkspaceResumeFixture(t)
		issue.EnvironmentResume.PreviousReason = "different environment reason"
		if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
			t.Fatal("changed EnvironmentResume.PreviousReason was accepted")
		}
	})
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{name: "changed base", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("2222222222222222222222222222222222222222"), []byte("ffffffffffffffffffffffffffffffffffffffff"), 1)
		}},
		{name: "different rejection", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("saved workspace provenance is missing"), []byte("saved workspace provenance was modified"), 1)
		}},
		{name: "missing authority event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 0)
		}},
		{name: "missing process event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 7)
		}},
		{name: "missing retry event", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 9)
		}},
		{name: "missing reconciliation", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 17)
		}},
		{name: "missing resume sync", mutate: func(data []byte) []byte {
			return removeFixtureEvent(data, 22)
		}},
		{name: "full history compressed to synthetic type sequence", mutate: func(data []byte) []byte {
			return selectFixtureEvents(t, data, 0, 2, 13, 14, 15, 20, 21, 22, 23, 24, 25, 26)
		}},
		{name: "duplicate authority event", mutate: func(data []byte) []byte {
			return duplicateFixtureEvent(data, 13)
		}},
		{name: "reordered process and preflight", mutate: func(data []byte) []byte {
			return swapFixtureEvents(data, 11, 12)
		}},
		{name: "unknown event", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"type":"worker_preflight_completed"`), []byte(`"type":"unknown_preflight"`), 1)
		}},
		{name: "cross run reconciliation", mutate: func(data []byte) []byte {
			return replaceFixtureEventRun(t, data, 18, "run_other")
		}},
		{name: "request owner unexpectedly present", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"current_base_sha":"2222222222222222222222222222222222222222"`), []byte(`"current_base_sha":"2222222222222222222222222222222222222222","lease_owner":{"run_id":"run_adaf3142bd207b24","generation":2}`), 1)
		}},
		{name: "legacy recovery marker missing", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":false`), 1)
		}},
		{name: "malformed legacy recovery marker", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":"true"`), 1)
		}},
		{name: "resume ID missing from second sync", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"resume_id":"resume_0733cc3d177d05f3","state":"environment_resume"`), []byte(`"state":"environment_resume"`), 1)
		}},
		{name: "unexpected request marker", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"resume_id":"resume_0733cc3d177d05f3"}}`), []byte(`"resume_id":"resume_0733cc3d177d05f3","unexpected_marker":true}}`), 1)
		}},
		{name: "superseded after blocked sync", mutate: func(data []byte) []byte {
			return append(data, []byte(`{"version":4,"event_id":"event_sanitized_3792","sequence":3792,"timestamp":"2026-08-17T12:33:06Z","repo_id":"repo_zeitreise","issue_number":442,"run_id":"run_adaf3142bd207b24","type":"worker_completed","payload":{}}`+"\n")...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, data := interruptedWorkspaceResumeFixture(t)
			if err := os.WriteFile(store.EventsPath(), test.mutate(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
				t.Fatal("tampered interrupted resume history was accepted")
			}
		})
	}
}

func fixtureEventLines(data []byte) [][]byte {
	return bytes.Split(bytes.TrimSpace(data), []byte("\n"))
}

func joinFixtureEventLines(lines [][]byte) []byte {
	return append(bytes.Join(lines, []byte("\n")), '\n')
}

func removeFixtureEvent(data []byte, index int) []byte {
	lines := fixtureEventLines(data)
	return joinFixtureEventLines(append(lines[:index:index], lines[index+1:]...))
}

func duplicateFixtureEvent(data []byte, index int) []byte {
	lines := fixtureEventLines(data)
	result := append([][]byte(nil), lines[:index+1]...)
	result = append(result, append([]byte(nil), lines[index]...))
	result = append(result, lines[index+1:]...)
	return joinFixtureEventLines(result)
}

func selectFixtureEvents(t *testing.T, data []byte, indices ...int) []byte {
	t.Helper()
	lines := fixtureEventLines(data)
	selected := make([][]byte, 0, len(indices))
	for offset, index := range indices {
		var event Event
		if err := json.Unmarshal(lines[index], &event); err != nil {
			t.Fatal(err)
		}
		event.Sequence = uint64(3765 + offset)
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		selected = append(selected, encoded)
	}
	return joinFixtureEventLines(selected)
}

func swapFixtureEvents(data []byte, left, right int) []byte {
	lines := fixtureEventLines(data)
	lines[left], lines[right] = lines[right], lines[left]
	return joinFixtureEventLines(lines)
}

func replaceFixtureEventRun(t *testing.T, data []byte, index int, runID string) []byte {
	t.Helper()
	lines := fixtureEventLines(data)
	var event Event
	if err := json.Unmarshal(lines[index], &event); err != nil {
		t.Fatal(err)
	}
	event.RunID = runID
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	lines[index] = encoded
	return joinFixtureEventLines(lines)
}

func TestInterruptedWorkspaceResumeEvidenceRejectsCurrentStateMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "status requested alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = "requested" }},
		{name: "status github synced alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = "github_synced" }},
		{name: "lease generation", mutate: func(issue *Issue) { issue.Lease.Owner.Generation++; issue.LeaseGeneration++ }},
		{name: "lease slot", mutate: func(issue *Issue) { issue.Lease.Slot = 1 }},
		{name: "lease reservation time", mutate: func(issue *Issue) { issue.Lease.ReservedAt = issue.Lease.ReservedAt.Add(-1) }},
		{name: "base SHA", mutate: func(issue *Issue) {
			issue.Lease.BaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
			issue.EnvironmentResume.BaseSHA = issue.Lease.BaseSHA
		}},
		{name: "current base SHA", mutate: func(issue *Issue) {
			issue.EnvironmentResume.CurrentBaseSHA = "ffffffffffffffffffffffffffffffffffffffff"
		}},
		{name: "resume ID", mutate: func(issue *Issue) { issue.EnvironmentResume.ID = "resume_other" }},
		{name: "worktree", mutate: func(issue *Issue) {
			issue.Worktree = "/tmp/other"
			issue.BlockedCause.Reason = "worker workspace validation failed for /tmp/other: saved workspace provenance is missing"
			issue.LastError = issue.BlockedCause.Reason
		}},
		{name: "branch", mutate: func(issue *Issue) { issue.Branch = "codex/other" }},
		{name: "session", mutate: func(issue *Issue) { issue.SessionID = "session_other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, issue, _ := interruptedWorkspaceResumeFixture(t)
			test.mutate(&issue)
			if _, err := store.InterruptedWorkspaceResumeEvidence(issue); err == nil {
				t.Fatal("mismatched current state was accepted")
			}
		})
	}
}

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
	data, err := os.ReadFile("testdata/zeitreise-442-v0614-missing-workspace-resume-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
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
	return store, issue, data
}

func TestInterruptedWorkspaceResumeEvidenceFromV0614Fixture(t *testing.T) {
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
			return bytes.Replace(data, []byte("abcdefabcdefabcdefabcdefabcdefabcdefabcd"), []byte("ffffffffffffffffffffffffffffffffffffffff"), 1)
		}},
		{name: "different rejection", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte("saved workspace provenance is missing"), []byte("saved workspace provenance was modified"), 1)
		}},
		{name: "missing worker start", mutate: func(data []byte) []byte {
			lines := bytes.Split(data, []byte("\n"))
			return bytes.Join(append(lines[:9], lines[10:]...), []byte("\n"))
		}},
		{name: "duplicate sync", mutate: func(data []byte) []byte {
			lines := bytes.Split(data, []byte("\n"))
			return bytes.Join(append(lines[:9], append([][]byte{lines[8]}, lines[9:]...)...), []byte("\n"))
		}},
		{name: "reordered sync", mutate: func(data []byte) []byte {
			lines := bytes.Split(data, []byte("\n"))
			lines[7], lines[8] = lines[8], lines[7]
			return bytes.Join(lines, []byte("\n"))
		}},
		{name: "cross run after request", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"sequence":3773,"timestamp":"2026-08-17T12:33:02Z","repo_id":"repo_zeitreise","issue_number":442,"run_id":"run_adaf3142bd207b24"`), []byte(`"sequence":3773,"timestamp":"2026-08-17T12:33:02Z","repo_id":"repo_zeitreise","issue_number":442,"run_id":"run_other"`), 1)
		}},
		{name: "request owner unexpectedly present", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"current_base_sha":"abcdefabcdefabcdefabcdefabcdefabcdefabcd"`), []byte(`"current_base_sha":"abcdefabcdefabcdefabcdefabcdefabcdefabcd","lease_owner":{"run_id":"run_adaf3142bd207b24","generation":8}`), 1)
		}},
		{name: "legacy recovery marker missing", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":false`), 1)
		}},
		{name: "malformed legacy recovery marker", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`"legacy_lease_recovered":true`), []byte(`"legacy_lease_recovered":"true"`), 1)
		}},
		{name: "resume ID missing from second sync", mutate: func(data []byte) []byte {
			return bytes.Replace(data, []byte(`,"resume_id":"resume_0733cc3d177d05f3"}`), []byte(`}`), 1)
		}},
		{name: "superseded after blocked sync", mutate: func(data []byte) []byte {
			return append(data, []byte(`{"version":4,"event_id":"event_3777","sequence":3777,"timestamp":"2026-08-17T12:33:06Z","repo_id":"repo_zeitreise","issue_number":442,"run_id":"run_adaf3142bd207b24","type":"worker_completed","payload":{}}`+"\n")...)
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

func TestInterruptedWorkspaceResumeEvidenceRejectsCurrentStateMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Issue)
	}{
		{name: "status requested alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = "requested" }},
		{name: "status github synced alone", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = "github_synced" }},
		{name: "lease generation", mutate: func(issue *Issue) { issue.Lease.Owner.Generation++; issue.LeaseGeneration++ }},
		{name: "lease slot", mutate: func(issue *Issue) { issue.Lease.Slot = 1 }},
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

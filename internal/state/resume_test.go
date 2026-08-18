package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	originalOwner := LeaseOwner{RunID: "run_zeitreise_442", Generation: 7}
	resumeOwner := LeaseOwner{RunID: "run_zeitreise_442", Generation: 8}
	baseSHA := "0123456789012345678901234567890123456789"
	resumedAt := time.Date(2026, 8, 17, 12, 33, 0, 0, time.UTC)
	lease := ResourceLease{
		Owner: resumeOwner, Slot: 0, DeclaredResources: []string{RepositoryResource},
		ResolvedResources: []string{RepositoryResource}, BaseSHA: baseSHA, ReservedAt: resumedAt,
	}
	reason := "worker workspace validation failed for /tmp/zeitreise-442: saved workspace provenance is missing"
	issue := Issue{
		Number: 442, Status: "blocked", RunID: resumeOwner.RunID, LeaseGeneration: resumeOwner.Generation, Lease: &lease,
		ResourcePark: &ResourceLeasePark{
			ID: "park_zeitreise_442", Kind: ResourceParkKindEnvironmentBlock, Status: "resumed",
			OriginalLease: ResourceLease{
				Owner: originalOwner, Slot: 0, DeclaredResources: []string{RepositoryResource},
				ResolvedResources: []string{RepositoryResource}, BaseSHA: baseSHA, ReservedAt: resumedAt.Add(-time.Hour),
			},
			ParkedAt: resumedAt.Add(-time.Minute), ResumedAt: resumedAt, ResumeOwner: &resumeOwner,
		},
		Branch: "codex/issue-442-legacy-block", Worktree: "/tmp/zeitreise-442", SessionID: "session_442",
		FailureKind: "issue", LastError: reason,
		BlockedCause: &BlockedCause{
			Origin: "supervisor", Kind: "worker_workspace", Resumable: false, Reason: reason,
			BlockedAt: resumedAt.Add(3 * time.Second),
		},
		EnvironmentResume: &EnvironmentResume{
			ID: "resume_0733cc3d177d05f3", Status: "github_synced", ConfirmedAt: resumedAt,
			PreviousReason: "localhost listen denied", BaseSHA: baseSHA,
			CurrentBaseSHA: "abcdefabcdefabcdefabcdefabcdefabcdefabcd",
		},
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
		evidence.LeaseOwner != issue.Lease.Owner || evidence.LeaseSlot != issue.Lease.Slot {
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
		{name: "resume running", mutate: func(issue *Issue) { issue.EnvironmentResume.Status = "running" }},
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
			return bytes.Join(append(lines[:2], lines[3:]...), []byte("\n"))
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

package state

import (
	"encoding/json"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLegacyWorkerBlockFromDurableRecordFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/zeitreise-442-worker-block-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	issue := Issue{
		Number: 442, Status: issuedomain.StatusBlocked, RunID: "run_zeitreise_442", FailureKind: "issue",
		LastError: legacyManualExclusionError,
	}

	cause, err := store.LegacyWorkerBlockProvenance(issue)
	if err != nil {
		t.Fatal(err)
	}
	wantTime := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	if cause.Origin != "worker" || cause.Kind != "environment" || !cause.Resumable || cause.Reason != "localhost listen denied" || !cause.BlockedAt.Equal(wantTime) {
		t.Fatalf("cause=%+v", cause)
	}
}

func TestTypedLegacyWorkerBlockRecoveryFromMissingLeaseFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/zeitreise-442-missing-lease-events.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: t.TempDir(), RepoID: "repo_zeitreise"}
	if err := os.WriteFile(filepath.Join(store.Dir, "events.jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	blockedAt := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	issue := Issue{
		Number: 442, Status: issuedomain.StatusBlocked, RunID: "run_zeitreise_442", LeaseGeneration: 7,
		Worktree: "/tmp/zeitreise-442", Branch: "codex/issue-442-legacy-block", FailureKind: "issue",
		LastError: "worker blocked: localhost listen denied",
		BlockedCause: &BlockedCause{
			Origin: "worker", Kind: "environment", Resumable: true,
			Reason: "localhost listen denied", BlockedAt: blockedAt,
		},
	}

	recovery, err := store.LegacyWorkerBlockRecoveryEvidence(issue)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.BaseSHA != "0123456789012345678901234567890123456789" || recovery.Cause != *issue.BlockedCause {
		t.Fatalf("recovery=%+v", recovery)
	}
}

func TestLegacyWorkerBlockFromEventsRequiresExactSameRunChain(t *testing.T) {
	blockedAt := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	legacyError := "issue: worker blocked: localhost listen denied"
	event := func(sequence uint64, eventType, runID string, payload any) Event {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return Event{Version: CurrentVersion, Sequence: sequence, Timestamp: blockedAt, RepoID: "repo", IssueNumber: 12, RunID: runID, Type: eventType, Payload: encoded}
	}
	blocked := event(1, "issue_blocked", "run_12", map[string]string{"error": legacyError, "failure_kind": "issue"})
	synced := event(2, "github_state_synced", "run_12", map[string]string{"state": "blocked"})
	legacyOverwrite := event(3, "startup_reconciled", "run_12", map[string]string{
		"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually",
	})
	issue := Issue{Number: 12, Status: issuedomain.StatusBlocked, RunID: "run_12", FailureKind: "issue", LastError: legacyManualExclusionError}

	cause, err := legacyWorkerBlockFromEvents([]Event{blocked, synced, legacyOverwrite}, issue)
	if err != nil {
		t.Fatal(err)
	}
	if cause.Origin != "worker" || cause.Kind != "environment" || !cause.Resumable || cause.Reason != "localhost listen denied" || !cause.BlockedAt.Equal(blockedAt) {
		t.Fatalf("cause=%+v", cause)
	}

	tests := []struct {
		name   string
		events []Event
		issue  Issue
	}{
		{name: "missing sync", events: []Event{blocked}, issue: issue},
		{name: "different run", events: []Event{blocked, event(2, "github_state_synced", "run_other", map[string]string{"state": "blocked"})}, issue: issue},
		{name: "reordered", events: []Event{synced, blocked}, issue: issue},
		{name: "non-contiguous sync", events: []Event{blocked, event(3, "github_state_synced", "run_12", map[string]string{"state": "blocked"})}, issue: issue},
		{name: "wrong sync state", events: []Event{blocked, event(2, "github_state_synced", "run_12", map[string]string{"state": "failed"})}, issue: issue},
		{name: "worker block substring", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": "operation failed after worker blocked: localhost listen denied", "failure_kind": "issue"}), synced}, issue: issue},
		{name: "missing failure kind", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": "worker blocked: localhost listen denied"}), synced}, issue: issue},
		{name: "wrong failure kind", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": "worker blocked: localhost listen denied", "failure_kind": "supervisor"}), synced}, issue: issue},
		{name: "empty reason", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": "worker blocked: ", "failure_kind": "issue"}), synced}, issue: issue},
		{name: "security provenance", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": legacyError, "failure_kind": "issue", "blocked_origin": "supervisor", "blocked_kind": "security"}), synced}, issue: issue},
		{name: "manual provenance", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": legacyError, "failure_kind": "issue", "blocked_origin": "operator", "blocked_kind": "manual"}), synced}, issue: issue},
		{name: "partial typed provenance", events: []Event{event(1, "issue_blocked", "run_12", map[string]string{"error": legacyError, "failure_kind": "issue", "blocked_origin": "worker"}), synced}, issue: issue},
		{name: "tampered timestamp", events: []Event{func() Event { value := blocked; value.Timestamp = time.Time{}; return value }(), synced}, issue: issue},
		{name: "superseded event", events: []Event{blocked, synced, event(3, "manual_block", "run_12", nil)}, issue: issue},
		{name: "different current run", events: []Event{blocked, synced}, issue: func() Issue { value := issue; value.RunID = "run_other"; return value }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := legacyWorkerBlockFromEvents(test.events, test.issue); err == nil {
				t.Fatal("unsafe legacy provenance was accepted")
			}
		})
	}
}

func TestMayHaveLegacyWorkerBlockProvenanceUsesExactMarkerAllowlist(t *testing.T) {
	base := Issue{Number: 12, Status: issuedomain.StatusBlocked, RunID: "run_12", FailureKind: "issue"}
	for _, marker := range []string{"worker blocked: reason", "issue: worker blocked: reason", legacyManualExclusionError} {
		issue := base
		issue.LastError = marker
		if !MayHaveLegacyWorkerBlockProvenance(&issue) {
			t.Fatalf("valid legacy candidate %q was rejected", marker)
		}
	}
	for _, marker := range []string{"prefix worker blocked: reason", "worker blocked", "worker blocked:   ", "issue: worker blocked"} {
		issue := base
		issue.LastError = marker
		if MayHaveLegacyWorkerBlockProvenance(&issue) {
			t.Fatalf("invalid legacy candidate %q was accepted", marker)
		}
	}
}

func TestLegacyWorkerBlockProvenanceRejectsAmbiguousHistory(t *testing.T) {
	store := newStore(t)
	legacyError := "issue: worker blocked: CDP unavailable"
	writeBlock := func() {
		_, err := store.Update("issue_blocked", 4, "run_4", map[string]string{"error": legacyError, "failure_kind": "issue"}, func(snapshot *Snapshot) error {
			snapshot.Issues["4"] = &Issue{Number: 4, Status: issuedomain.StatusBlocked, RunID: "run_4", FailureKind: "issue", LastError: legacyError}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update("github_state_synced", 4, "run_4", map[string]string{"state": "blocked"}, func(*Snapshot) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	writeBlock()
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cause, err := store.LegacyWorkerBlockProvenance(*snapshot.Issues["4"]); err != nil || cause.Reason != "CDP unavailable" {
		t.Fatalf("cause=%+v err=%v", cause, err)
	}
	writeBlock()
	snapshot, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LegacyWorkerBlockProvenance(*snapshot.Issues["4"]); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous history err=%v", err)
	}
}

func TestTypedLegacyWorkerBlockRequiresExactDurableCause(t *testing.T) {
	blockedAt := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	event := func(sequence uint64, eventType string, payload any) Event {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return Event{Version: CurrentVersion, Sequence: sequence, Timestamp: blockedAt, RepoID: "repo", IssueNumber: 12, RunID: "run_12", Type: eventType, Payload: encoded}
	}
	events := []Event{
		event(1, "issue_blocked", map[string]string{"error": "worker blocked: localhost listen denied", "failure_kind": "issue"}),
		event(2, "github_state_synced", map[string]string{"state": "blocked"}),
		event(3, "startup_reconciled", map[string]string{"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually"}),
		event(4, "startup_reconciled", map[string]string{"previous_status": "blocked", "status": "blocked", "reason": legacyNormalizedReason}),
	}
	want := BlockedCause{Origin: "worker", Kind: "environment", Resumable: true, Reason: "localhost listen denied", BlockedAt: blockedAt}
	issue := Issue{
		Number: 12, Status: issuedomain.StatusBlocked, RunID: "run_12", FailureKind: "issue",
		LastError: "worker blocked: localhost listen denied", BlockedCause: &want,
	}

	if cause, err := legacyWorkerBlockFromEvents(events, issue); err != nil || !reflect.DeepEqual(cause, &want) {
		t.Fatalf("cause=%+v err=%v", cause, err)
	}
	reorderedNormalization := []Event{
		events[0], events[1],
		event(3, "startup_reconciled", map[string]string{"previous_status": "blocked", "status": "blocked", "reason": legacyNormalizedReason}),
		event(4, "startup_reconciled", map[string]string{"previous_status": "blocked", "status": "blocked", "reason": "GitHub exclusion label was applied manually"}),
	}
	if _, err := legacyWorkerBlockFromEvents(reorderedNormalization, issue); err == nil {
		t.Fatal("typed normalization before the legacy reconciliation was accepted")
	}
	mutations := []func(*BlockedCause){
		func(cause *BlockedCause) { cause.Origin = "supervisor" },
		func(cause *BlockedCause) { cause.Kind = "security" },
		func(cause *BlockedCause) { cause.Resumable = false },
		func(cause *BlockedCause) { cause.Reason = "different" },
		func(cause *BlockedCause) { cause.BlockedAt = cause.BlockedAt.Add(time.Second) },
	}
	for index, mutate := range mutations {
		copy := want
		mutate(&copy)
		changed := issue
		changed.BlockedCause = &copy
		if _, err := legacyWorkerBlockFromEvents(events, changed); err == nil {
			t.Fatalf("mutation %d was accepted: %+v", index, copy)
		}
	}
}

func TestLegacyWorkerBlockRecoveryRequiresSameRunLeaseWorktreeAndBranch(t *testing.T) {
	blockedAt := time.Date(2026, 8, 17, 12, 30, 0, 0, time.UTC)
	reservedAt := blockedAt.Add(-time.Minute)
	event := func(sequence uint64, eventType string, payload any) Event {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return Event{Version: CurrentVersion, Sequence: sequence, Timestamp: reservedAt.Add(time.Duration(sequence) * time.Second), RepoID: "repo", IssueNumber: 12, RunID: "run_12", Type: eventType, Payload: encoded}
	}
	lease := event(1, "lease_reserved", map[string]any{
		"owner": LeaseOwner{RunID: "run_12", Generation: 3}, "resolved_resources": []string{RepositoryResource},
		"base_sha": "0123456789012345678901234567890123456789", "reserved_at": reservedAt,
	})
	started := event(2, "worker_started", map[string]string{"worktree": "/tmp/worktree", "branch": "codex/issue-12"})
	blocked := event(3, "issue_blocked", map[string]string{"error": "worker blocked: localhost listen denied", "failure_kind": "issue"})
	blocked.Timestamp = blockedAt
	events := []Event{lease, started, blocked, event(4, "github_state_synced", map[string]string{"state": "blocked"})}
	issue := Issue{
		Number: 12, Status: issuedomain.StatusBlocked, RunID: "run_12", LeaseGeneration: 3, Worktree: "/tmp/worktree", Branch: "codex/issue-12",
		FailureKind: "issue", LastError: "worker blocked: localhost listen denied",
	}
	block, err := legacyWorkerBlockEvidenceFromEvents(events, issue)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := legacyWorkerLeaseFromEvents(events, issue, block.blockedSequence)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.baseSHA != "0123456789012345678901234567890123456789" || !recovery.reservedAt.Equal(reservedAt) {
		t.Fatalf("recovery=%+v", recovery)
	}

	tests := []struct {
		name   string
		events []Event
		issue  Issue
	}{
		{name: "missing lease", events: events[1:], issue: issue},
		{name: "missing worker start", events: []Event{lease, blocked}, issue: issue},
		{name: "different worktree", events: []Event{lease, event(2, "worker_started", map[string]string{"worktree": "/tmp/other", "branch": issue.Branch}), blocked}, issue: issue},
		{name: "different branch", events: []Event{lease, event(2, "worker_started", map[string]string{"worktree": issue.Worktree, "branch": "codex/other"}), blocked}, issue: issue},
		{name: "different generation", events: events, issue: func() Issue { copy := issue; copy.LeaseGeneration = 4; return copy }()},
		{name: "duplicate lease", events: []Event{lease, event(2, "lease_reserved", map[string]any{"owner": LeaseOwner{RunID: "run_12", Generation: 3}, "resolved_resources": []string{RepositoryResource}, "base_sha": "0123456789012345678901234567890123456789", "reserved_at": reservedAt}), event(3, "worker_started", map[string]string{"worktree": issue.Worktree, "branch": issue.Branch}), func() Event { value := blocked; value.Sequence = 4; return value }()}, issue: issue},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := legacyWorkerLeaseFromEvents(test.events, test.issue, test.events[len(test.events)-1].Sequence); err == nil {
				t.Fatal("unsafe recovery evidence was accepted")
			}
		})
	}
}

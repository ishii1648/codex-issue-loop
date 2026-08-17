package state

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		Number: 442, Status: "blocked", RunID: "run_zeitreise_442", FailureKind: "issue",
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
	issue := Issue{Number: 12, Status: "blocked", RunID: "run_12", FailureKind: "issue", LastError: legacyManualExclusionError}

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
	base := Issue{Number: 12, Status: "blocked", RunID: "run_12", FailureKind: "issue"}
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
			snapshot.Issues["4"] = &Issue{Number: 4, Status: "blocked", RunID: "run_4", FailureKind: "issue", LastError: legacyError}
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

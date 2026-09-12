package migration

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func legacyCancelFixture(t *testing.T) []byte {
	t.Helper()
	return []byte(`{"version":5,"semantic_contract_version":4,"issue_lifecycle_api_version":"2.1","repo_id":"repo-anonymous","repo_path":"/missing/repository","state_revision":0,"supervisor":{"state":"stopped","updated_at":"2026-09-01T00:00:00Z"},"issues":{"17":{"number":17,"status":"blocked","worktree":"/missing/worktree","suspension":{"id":"suspension_legacy","origin":"operator","status":"resolved","reason_code":"environment","recoverability":"operator","reason":"canceled by operator","allowed_actions":["cancel"],"suspended_at":"2026-08-30T00:00:00Z","resolved_at":"2026-09-01T00:00:00Z","resolution":"cancel"}}},"pending_requests":{},"pending_effects":{},"quarantined_issues":{},"intake_verifications":{}}`)
}

func TestDecodeV6LegacyCancel(t *testing.T) {
	for _, lifecycle := range []string{"2.0", "2.1"} {
		for _, status := range []string{"blocked", "failed"} {
			for _, pr := range []string{"", ",\"pull_request_number\":42,\"pull_request_url\":\"https://github.com/example/repo/pull/42\""} {
				input := strings.ReplaceAll(string(legacyCancelFixture(t)), "2.1", lifecycle)
				input = strings.Replace(input, `"status":"blocked"`, `"status":"`+status+`"`+pr, 1)
				before := []byte(input)
				result, err := decodeV6Snapshot(before)
				if err != nil {
					t.Fatal(err)
				}
				item := result.Issues["17"]
				if result.Version != 6 || result.SemanticContractVersion != 0 || result.IssueLifecycleAPIVersion != "" || item.Status != issuedomain.StatusCanceled || item.Cancellation == nil {
					t.Fatalf("unexpected migration: %+v", result)
				}
				if item.Cancellation.Source != "legacy_cancel_migration" || item.Cancellation.PreviousStatus.String() != status || item.Cancellation.ExecutionReleaseResult != "not_present" || !item.Cancellation.CanceledAt.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
					t.Fatalf("cancellation=%+v", item.Cancellation)
				}
				if !bytes.Equal(before, []byte(input)) {
					t.Fatal("modified input")
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				again, err := decodeV6Snapshot(encoded)
				if err != nil {
					t.Fatal(err)
				}
				repeated, _ := json.Marshal(again)
				if !bytes.Equal(encoded, repeated) {
					t.Fatal("repeated migration changed cancellation")
				}
			}
		}
	}
}

func TestDecodeV6RefusesUnsafeLegacyCancellation(t *testing.T) {
	cases := map[string]func(*state.Snapshot){
		"missing timestamp": func(s *state.Snapshot) { s.Issues["17"].Suspension.ResolvedAt = time.Time{} },
		"contradictory timestamp": func(s *state.Snapshot) {
			s.Issues["17"].Suspension.ResolvedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
		},
		"existing cancellation": func(s *state.Snapshot) { s.Issues["17"].Cancellation = &state.Cancellation{} },
		"worker":                func(s *state.Snapshot) { s.Issues["17"].WorkerPID = 123; s.Issues["17"].WorkerPGID = 123 },
		"active execution":      func(s *state.Snapshot) { s.ActiveExecution = &state.ActiveExecution{IssueNumber: 17} },
		"pending request": func(s *state.Snapshot) {
			s.PendingRequests["req_1"] = &state.Request{IssueNumber: 17, Status: issuedomain.RequestStatusPending}
		},
		"unfinished effect": func(s *state.Snapshot) { s.PendingEffects["17"] = &state.EffectIntent{IssueNumber: 17} },
		"unknown lifecycle": func(s *state.Snapshot) { s.IssueLifecycleAPIVersion = "3.0" },
		"unknown version":   func(s *state.Snapshot) { s.Version = 7 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var snapshot state.Snapshot
			if err := json.Unmarshal(legacyCancelFixture(t), &snapshot); err != nil {
				t.Fatal(err)
			}
			mutate(&snapshot)
			input, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeV6Snapshot(input); err == nil {
				t.Fatal("unsafe input accepted")
			}
		})
	}
}

func TestDecodeV6PreservesOtherResolution(t *testing.T) {
	input := bytes.ReplaceAll(legacyCancelFixture(t), []byte(`"cancel"`), []byte(`"resume"`))
	result, err := decodeV6Snapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Issues["17"].Status != issuedomain.StatusBlocked || result.Issues["17"].Cancellation != nil {
		t.Fatal("converted another resolution")
	}
}

func TestDecodeV6KeepsExistingLegacyCancellation(t *testing.T) {
	migrated, err := decodeV6Snapshot(legacyCancelFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	migrated.Version, migrated.SemanticContractVersion, migrated.IssueLifecycleAPIVersion = 5, 4, "2.1"
	migrated.Issues["17"].Cancellation.Source = "operator_resolution"
	input, err := json.Marshal(migrated)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeV6Snapshot(input)
	if err != nil {
		t.Fatal(err)
	}
	if *result.Issues["17"].Cancellation != *migrated.Issues["17"].Cancellation {
		t.Fatal("existing cancellation changed")
	}
}

func TestDecodeV6RejectsCurrentResolvedCancelWithoutCompletion(t *testing.T) {
	var snapshot state.Snapshot
	if err := json.Unmarshal(legacyCancelFixture(t), &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.Version, snapshot.SemanticContractVersion, snapshot.IssueLifecycleAPIVersion = 6, 0, ""
	input, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeV6Snapshot(input); err == nil {
		t.Fatal("current contract accepted unresolved cancellation")
	}
}

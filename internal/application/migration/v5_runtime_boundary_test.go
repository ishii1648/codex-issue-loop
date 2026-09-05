package migration

import (
	"encoding/json"
	"testing"
	"time"
)

func TestV5MigrationRemovesScenarioSpecificRuntimeAxes(t *testing.T) {
	for _, fixture := range []struct {
		status string
		sync   string
	}{
		{status: "environment_resume_pending", sync: "environment_resume"},
		{status: "publication_recovery_pending", sync: "publication_recovery"},
		{status: "pull_request_checks_recovery_pending", sync: "pull_request_checks_recovery"},
	} {
		issue := map[string]json.RawMessage{
			"status": mustRaw(fixture.status), "run_id": mustRaw("run_1"),
			"github_sync": mustRaw(fixture.sync), "last_error": mustRaw("legacy boundary"),
		}
		if err := migrateV5Issue("1", issue, time.Unix(1, 0).UTC()); err != nil {
			t.Fatal(err)
		}
		if got := rawString(issue["status"]); got != "blocked" {
			t.Fatalf("status %q migrated to %q", fixture.status, got)
		}
		if _, exists := issue["github_sync"]; exists {
			t.Fatalf("sync %q survived migration", fixture.sync)
		}
		for _, field := range legacyRecoveryFields {
			if _, exists := issue[field]; exists {
				t.Fatalf("legacy field %q survived migration", field)
			}
		}
	}
}

func TestV5MigrationCarriesAnsweredEvidenceIntoGenericCheckpoint(t *testing.T) {
	issue := map[string]json.RawMessage{
		"status": mustRaw("blocked"), "run_id": mustRaw("run_1"), "last_error": mustRaw("workspace validation failed"),
		"resource_park": mustMarshal(map[string]any{
			"id": "park_1", "status": "parked", "original_lease": map[string]any{
				"owner": map[string]any{"run_id": "run_1", "generation": 1}, "slot": 0,
				"declared_resources": []string{"repo"}, "resolved_resources": []string{"repo"}, "reserved_at": time.Unix(1, 0).UTC(),
			}, "parked_at": time.Unix(2, 0).UTC(),
		}),
		"answered_workspace_recovery": mustMarshal(map[string]any{"request_id": "req_1", "resource_park_id": "park_1"}),
	}
	if err := migrateV5Issue("1", issue, time.Unix(3, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Kind      string `json:"kind"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(issue["continuation_checkpoint"], &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Kind != "needs_input" || checkpoint.RequestID != "req_1" {
		t.Fatalf("checkpoint=%+v", checkpoint)
	}
}

func TestV5MigrationCarriesFailureProvenanceIntoGenericEvidence(t *testing.T) {
	failedAt := time.Unix(4, 0).UTC()
	issue := map[string]json.RawMessage{
		"status": mustRaw("failed"), "run_id": mustRaw("run_1"), "last_error": mustRaw("checks exhausted"),
		"lease": mustMarshal(map[string]any{
			"owner": map[string]any{"run_id": "run_1", "generation": 1}, "slot": 0,
			"declared_resources": []string{"repo"}, "resolved_resources": []string{"repo"}, "reserved_at": time.Unix(1, 0).UTC(),
		}),
		"pull_request_checks_failure": mustMarshal(map[string]any{
			"origin": "pull_request_lifecycle", "phase": "required_checks", "code": "checks_retry_exhausted",
			"checks_status": "failure", "failed_at": failedAt,
		}),
		"blocked_cause": mustMarshal(map[string]any{"origin": "worker", "kind": "checks", "reason": "checks exhausted"}),
	}
	if err := migrateV5Issue("1", issue, time.Unix(5, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		Evidence struct {
			Origin, Phase, Code, Status string
			ObservedAt                  time.Time `json:"observed_at"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(issue["continuation_checkpoint"], &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Evidence.Origin != "pull_request_lifecycle" || checkpoint.Evidence.Phase != "required_checks" ||
		checkpoint.Evidence.Code != "checks_retry_exhausted" || checkpoint.Evidence.Status != "failure" || !checkpoint.Evidence.ObservedAt.Equal(failedAt) {
		t.Fatalf("checkpoint evidence=%+v", checkpoint.Evidence)
	}
	var suspension struct {
		Origin string `json:"origin"`
	}
	if err := json.Unmarshal(issue["suspension"], &suspension); err != nil || suspension.Origin != "worker" {
		t.Fatalf("suspension=%+v err=%v", suspension, err)
	}
}

func TestV5SemanticMigrationNormalizesOnlyGenericCheckpointStages(t *testing.T) {
	for legacy, want := range map[string]string{
		"resume_pending":                       "resume",
		"environment_resume_pending":           "resume",
		"publication_recovery_pending":         "publish",
		"pull_request_checks_recovery_pending": "checks",
		"awaiting_checks":                      "checks",
		"awaiting_merge":                       "checks",
		"resolving_conflict":                   "conflict",
	} {
		object := map[string]json.RawMessage{"issues": mustMarshal(map[string]any{
			"1": map[string]any{
				"number": 1, "status": "blocked", "last_error": "retained",
				"continuation_checkpoint": map[string]any{
					"id": "checkpoint_1", "status": "parked", "stage": legacy,
					"original_execution_lease": map[string]any{"owner": map[string]any{"run_id": "run_1", "generation": 1}, "reserved_at": time.Unix(1, 0).UTC()},
					"evidence":                 map[string]any{"origin": "worker", "code": "retained"},
				},
				"suspension": map[string]any{"id": "suspension_1", "status": "active", "reason": "retained"},
			},
		})}
		if err := normalizeV5SemanticStateObject(object, time.Unix(2, 0).UTC()); err != nil {
			t.Fatalf("stage %q: %v", legacy, err)
		}
		var issues map[string]struct {
			Continuation struct {
				Stage    string                     `json:"stage"`
				Evidence map[string]json.RawMessage `json:"evidence"`
			} `json:"continuation"`
			Suspension map[string]json.RawMessage `json:"suspension"`
		}
		if err := json.Unmarshal(object["issues"], &issues); err != nil {
			t.Fatal(err)
		}
		if got := issues["1"].Continuation.Stage; got != want {
			t.Fatalf("stage %q normalized to %q; want %q", legacy, got, want)
		}
		if string(issues["1"].Continuation.Evidence["code"]) != `"retained"` || string(issues["1"].Suspension["reason"]) != `"retained"` {
			t.Fatalf("semantic migration changed retained evidence: %+v", issues["1"])
		}
	}
	object := map[string]json.RawMessage{"issues": mustMarshal(map[string]any{
		"1": map[string]any{"continuation_checkpoint": map[string]any{"stage": "unknown"}},
	})}
	if err := normalizeV5SemanticStateObject(object, time.Unix(2, 0).UTC()); err == nil {
		t.Fatal("unknown continuation stage was normalized")
	}
	object = map[string]json.RawMessage{"issues": mustMarshal(map[string]any{
		"1": map[string]any{"continuation_checkpoint": map[string]any{"stage": "resume", "kind": "unknown"}},
	})}
	if err := normalizeV5SemanticStateObject(object, time.Unix(2, 0).UTC()); err == nil {
		t.Fatal("unknown continuation kind was normalized")
	}
}

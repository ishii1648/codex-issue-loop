package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/platform/schema"
)

var legacyRecoveryFields = []string{
	"blocked_cause",
	"environment_resume",
	"answered_workspace_recovery",
	"workspace_provenance_recovery",
	"publication_failure",
	"publication_recovery",
	"pull_request_checks_failure",
	"pull_request_checks_recovery",
	"merged_pull_request_adoption",
}

// DecodePreviousSnapshot is the only in-memory boundary that accepts a v4
// snapshot. Callers receive a validated v5 aggregate; runtime state decoding
// never interprets legacy fields directly.
func DecodePreviousSnapshot(data []byte, migratedAt time.Time) (state.Snapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return state.Snapshot{}, fmt.Errorf("decode v4 snapshot: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return state.Snapshot{}, fmt.Errorf("decode v4 snapshot trailing data")
	}
	var version int
	if err := json.Unmarshal(object["version"], &version); err != nil || version != schemaversion.Previous {
		return state.Snapshot{}, fmt.Errorf("migration decoder requires schema v%d, got %d", schemaversion.Previous, version)
	}
	if migratedAt.IsZero() {
		return state.Snapshot{}, fmt.Errorf("migration timestamp is required")
	}
	if err := normalizeMigratedSessions(object); err != nil {
		return state.Snapshot{}, err
	}
	if err := migrateV5StateObject(object, migratedAt.UTC()); err != nil {
		return state.Snapshot{}, err
	}
	object["version"] = json.RawMessage(fmt.Sprint(schemaversion.Current))
	object["semantic_contract_version"] = json.RawMessage(fmt.Sprint(statecontract.CurrentVersion))
	delete(object, "notifications")
	encoded, err := json.Marshal(object)
	if err != nil {
		return state.Snapshot{}, err
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return state.Snapshot{}, fmt.Errorf("decode migrated v5 snapshot: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return state.Snapshot{}, fmt.Errorf("validate migrated v5 snapshot: %w", err)
	}
	return snapshot, nil
}

func migrateV5StateObject(object map[string]json.RawMessage, migratedAt time.Time) error {
	var issues map[string]json.RawMessage
	if err := json.Unmarshal(object["issues"], &issues); err != nil {
		return fmt.Errorf("decode migrated state Issues: %w", err)
	}
	for key, raw := range issues {
		var issue map[string]json.RawMessage
		if err := json.Unmarshal(raw, &issue); err != nil {
			return fmt.Errorf("decode migrated Issue %s: %w", key, err)
		}
		if err := migrateV5Issue(key, issue, migratedAt); err != nil {
			return err
		}
		encoded, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		issues[key] = encoded
	}
	encoded, err := json.Marshal(issues)
	if err != nil {
		return err
	}
	object["issues"] = encoded
	return nil
}

// normalizeV5SemanticStateObject performs the only in-schema semantic v2 to
// v3 rewrite. Scenario-specific v5 fields remain rejected by Snapshot decoding;
// only generic checkpoint wire values created by the v4 migration are changed.
func normalizeV5SemanticStateObject(object map[string]json.RawMessage) error {
	var issues map[string]json.RawMessage
	if err := json.Unmarshal(object["issues"], &issues); err != nil {
		return fmt.Errorf("decode semantic migration Issues: %w", err)
	}
	for key, raw := range issues {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("decode semantic migration Issue %s: %w", key, err)
		}
		checkpoint := item["continuation_checkpoint"]
		if len(checkpoint) == 0 || string(checkpoint) == "null" {
			continue
		}
		var value map[string]json.RawMessage
		if err := json.Unmarshal(checkpoint, &value); err != nil {
			return fmt.Errorf("decode semantic migration Issue %s checkpoint: %w", key, err)
		}
		stage, err := normalizeContinuationStage(rawString(value["stage"]))
		if err != nil {
			return fmt.Errorf("Issue %s: %w", key, err)
		}
		value["stage"] = mustRaw(stage)
		switch kind := rawString(value["kind"]); kind {
		case "", "needs_input":
		case "environment_block":
			delete(value, "kind")
		default:
			return fmt.Errorf("Issue %s: unknown continuation checkpoint kind %q", key, kind)
		}
		item["continuation_checkpoint"] = mustMarshal(value)
		issues[key] = mustMarshal(item)
	}
	object["issues"] = mustMarshal(issues)
	return nil
}

func normalizeContinuationStage(stage string) (string, error) {
	switch stage {
	case "resume", "resume_pending", "environment_resume_pending":
		return "resume", nil
	case "publish", "publication_recovery_pending":
		return "publish", nil
	case "checks", "pull_request_checks_recovery_pending", "awaiting_checks", "awaiting_merge":
		return "checks", nil
	case "conflict", "resolving_conflict":
		return "conflict", nil
	default:
		return "", fmt.Errorf("unknown continuation checkpoint stage %q", stage)
	}
}

func migrateV5Issue(key string, issue map[string]json.RawMessage, migratedAt time.Time) error {
	status := rawString(issue["status"])
	originalStatus := status
	runID := rawString(issue["run_id"])
	lease := issue["lease"]
	checkpoint := issue["resource_park"]
	delete(issue, "lease")
	delete(issue, "resource_park")

	legacyScenario := legacyScenarioRecoveryStatus(status)
	if legacyScenario {
		status = "blocked"
		issue["status"] = mustRaw(status)
	}
	switch rawString(issue["github_sync"]) {
	case "environment_resume", "publication_recovery", "pull_request_checks_recovery", "answered_workspace_recovery":
		delete(issue, "github_sync")
	}
	terminal := status == "blocked" || status == "failed" || status == "completed"
	missingLease := legacyExecutionStatus(status) && (len(lease) == 0 || string(lease) == "null")
	missingWorkspace := legacyWorkspaceStatus(status) && legacyCrossedExecutionBoundary(issue) &&
		(len(issue["workspace"]) == 0 || string(issue["workspace"]) == "null")
	quarantineActive := !terminal && (missingLease || missingWorkspace)
	if quarantineActive {
		status, terminal = "blocked", true
		issue["status"] = mustRaw(status)
		delete(issue, "worker_pid")
		delete(issue, "worker_pgid")
	}
	if len(checkpoint) > 0 && string(checkpoint) != "null" {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(checkpoint, &value); err != nil {
			return fmt.Errorf("decode migrated Issue %s checkpoint: %w", key, err)
		}
		if original := value["original_lease"]; len(original) > 0 {
			value["original_execution_lease"] = original
			delete(value, "original_lease")
		}
		if terminal && len(lease) > 0 && string(lease) != "null" {
			value["original_execution_lease"] = lease
		}
		value["run_id"] = mustRaw(runID)
		copyRaw(value, "workspace", issue)
		copyRaw(value, "session", issue)
		copyRaw(value, "head_sha", issue)
		copyRaw(value, "pull_request_url", issue)
		copyRaw(value, "pull_request_number", issue)
		if rawString(value["stage"]) == "" {
			value["stage"] = mustRaw(legacyCheckpointStage(issue, originalStatus))
		}
		if terminal {
			value["status"] = mustRaw("parked")
			delete(value, "resume_owner")
			delete(value, "resumed_at")
			delete(value, "kind")
		}
		checkpoint = mustMarshal(value)
	} else if terminal && len(lease) > 0 && string(lease) != "null" {
		checkpoint = mustMarshal(map[string]any{
			"id": "checkpoint_migrated_" + key, "status": "parked",
			"original_execution_lease": json.RawMessage(lease), "parked_at": migratedAt,
			"run_id": runID, "stage": legacyCheckpointStage(issue, originalStatus),
		})
		var value map[string]json.RawMessage
		_ = json.Unmarshal(checkpoint, &value)
		copyRaw(value, "workspace", issue)
		copyRaw(value, "session", issue)
		copyRaw(value, "head_sha", issue)
		copyRaw(value, "pull_request_url", issue)
		copyRaw(value, "pull_request_number", issue)
		checkpoint = mustMarshal(value)
	}
	if len(checkpoint) > 0 && string(checkpoint) != "null" {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(checkpoint, &value); err != nil {
			return fmt.Errorf("decode migrated Issue %s continuation metadata: %w", key, err)
		}
		if raw := issue["answered_workspace_recovery"]; len(raw) > 0 && string(raw) != "null" {
			var answered struct {
				RequestID      string `json:"request_id"`
				ResourceParkID string `json:"resource_park_id"`
			}
			if err := json.Unmarshal(raw, &answered); err != nil {
				return fmt.Errorf("decode migrated Issue %s answered checkpoint: %w", key, err)
			}
			if answered.RequestID != "" {
				value["kind"] = mustRaw("needs_input")
				value["request_id"] = mustRaw(answered.RequestID)
			}
			if rawString(value["id"]) == "" && answered.ResourceParkID != "" {
				value["id"] = mustRaw(answered.ResourceParkID)
			}
		}
		for _, field := range []string{"publication_failure", "pull_request_checks_failure"} {
			raw := issue[field]
			if len(raw) == 0 || string(raw) == "null" {
				continue
			}
			var failure struct {
				Origin   string    `json:"origin"`
				Phase    string    `json:"phase"`
				Code     string    `json:"code"`
				Status   string    `json:"checks_status"`
				FailedAt time.Time `json:"failed_at"`
			}
			if err := json.Unmarshal(raw, &failure); err != nil {
				return fmt.Errorf("decode migrated Issue %s failure evidence: %w", key, err)
			}
			if failure.Status == "" {
				failure.Status = "failure"
			}
			value["evidence"] = mustMarshal(map[string]any{
				"origin": failure.Origin, "phase": failure.Phase, "code": failure.Code,
				"status": failure.Status, "observed_at": failure.FailedAt,
			})
			break
		}
		checkpoint = mustMarshal(value)
	}

	if terminal {
		delete(issue, "execution_lease")
		if status != "completed" && len(checkpoint) > 0 && string(checkpoint) != "null" {
			issue["continuation_checkpoint"] = checkpoint
		}
	} else if len(lease) > 0 && string(lease) != "null" {
		issue["execution_lease"] = lease
	}

	if status == "blocked" || status == "failed" {
		reason, reasonCode, recoverability, missing, actions := legacySuspension(issue, quarantineActive)
		suspensionStatus := "active"
		if recoverability == "ambiguous" {
			suspensionStatus = "quarantined"
		}
		checkpointID := ""
		if len(checkpoint) > 0 && string(checkpoint) != "null" {
			var value struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(checkpoint, &value)
			checkpointID = value.ID
		}
		issue["suspension"] = mustMarshal(map[string]any{
			"id": "suspension_migrated_" + key, "origin": legacySuspensionOrigin(issue), "status": suspensionStatus, "reason_code": reasonCode,
			"recoverability": recoverability, "reason": reason, "missing_evidence": missing,
			"allowed_actions": actions, "checkpoint_id": checkpointID, "suspended_at": migratedAt,
		})
	}
	for _, field := range legacyRecoveryFields {
		delete(issue, field)
	}
	return nil
}

func legacySuspensionOrigin(issue map[string]json.RawMessage) string {
	if raw := issue["blocked_cause"]; len(raw) > 0 && string(raw) != "null" {
		var cause struct {
			Origin string `json:"origin"`
		}
		if json.Unmarshal(raw, &cause) == nil && cause.Origin != "" {
			return cause.Origin
		}
	}
	return "migration"
}

func legacyCheckpointStage(issue map[string]json.RawMessage, originalStatus string) string {
	if raw := issue["publication_failure"]; len(raw) > 0 && string(raw) != "null" {
		return "publish"
	}
	if raw := issue["pull_request_checks_failure"]; len(raw) > 0 && string(raw) != "null" {
		return "checks"
	}
	if rawString(issue["pull_request_url"]) != "" {
		return "checks"
	}
	if originalStatus == "resolving_conflict" {
		return "conflict"
	}
	return "resume"
}

func legacySuspension(issue map[string]json.RawMessage, forceQuarantine bool) (string, string, string, []string, []string) {
	reason := rawString(issue["last_error"])
	reasonCode := "terminal"
	recoverability := "operator"
	missing := []string{}
	actions := []string{"cancel"}
	if len(issue["workspace"]) == 0 || string(issue["workspace"]) == "null" {
		missing = append(missing, "workspace")
		recoverability = "ambiguous"
	}
	if forceQuarantine {
		recoverability = "ambiguous"
		missing = append(missing, "execution_authority")
	}
	if raw := issue["blocked_cause"]; len(raw) > 0 && string(raw) != "null" {
		var cause struct {
			Kind      string `json:"kind"`
			Reason    string `json:"reason"`
			Resumable bool   `json:"resumable"`
		}
		_ = json.Unmarshal(raw, &cause)
		if cause.Kind != "" {
			reasonCode = cause.Kind
		}
		if cause.Reason != "" {
			reason = cause.Reason
		}
		if cause.Resumable {
			actions = append(actions, "resume")
		}
	}
	lowerReason := strings.ToLower(reason)
	if strings.Contains(lowerReason, "no such file or directory") {
		missing = append(missing, "workspace")
		recoverability = "none"
		actions = []string{"cancel"}
	} else if strings.Contains(lowerReason, "workspace provenance is missing") || len(issue["workspace_provenance_recovery"]) > 0 {
		actions = append(actions, "resume")
	}
	if recoverability == "ambiguous" {
		actions = []string{"cancel"}
	}
	if rawString(issue["pull_request_url"]) != "" {
		actions = append(actions, "adopt-pr", "retry-stage")
	}
	if len(issue["publication_failure"]) > 0 || len(issue["pull_request_checks_failure"]) > 0 {
		actions = append(actions, "retry-stage")
	}
	if reason == "" {
		reason = "migrated terminal lifecycle boundary"
	}
	sort.Strings(actions)
	actions = uniqueStrings(actions)
	sort.Strings(missing)
	missing = uniqueStrings(missing)
	return reason, reasonCode, recoverability, missing, actions
}

func legacyExecutionStatus(status string) bool {
	switch status {
	case "claiming", "claimed", "running", "resume_pending", "environment_resume_pending", "resolving_conflict":
		return true
	default:
		return false
	}
}

func legacyScenarioRecoveryStatus(status string) bool {
	switch status {
	case "environment_resume_pending", "publication_recovery_pending", "pull_request_checks_recovery_pending":
		return true
	default:
		return false
	}
}

func legacyWorkspaceStatus(status string) bool {
	switch status {
	case "claimed", "running", "answer_claim_waiting", "resume_pending", "environment_resume_pending",
		"publication_recovery_pending", "pull_request_checks_recovery_pending", "retry_wait", "needs_input",
		"awaiting_checks", "awaiting_merge", "resolving_conflict":
		return true
	default:
		return false
	}
}

func legacyCrossedExecutionBoundary(issue map[string]json.RawMessage) bool {
	for _, field := range []string{"workspace", "worktree", "branch", "session_id", "session", "publication_audit",
		"publication_failure", "publication_recovery", "pull_request_checks_failure", "pull_request_checks_recovery",
		"conflict_recovery", "environment_resume"} {
		if raw := issue[field]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
			return true
		}
	}
	for _, field := range []string{"attempts", "continuations"} {
		var count int
		_ = json.Unmarshal(issue[field], &count)
		if count > 0 {
			return true
		}
	}
	return false
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func copyRaw(destination map[string]json.RawMessage, key string, source map[string]json.RawMessage) {
	if raw := source[key]; len(raw) > 0 && string(raw) != "null" {
		destination[key] = raw
	}
}

func mustRaw(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func mustMarshal(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func uniqueStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

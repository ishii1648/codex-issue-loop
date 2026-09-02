package migration

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
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
			"id": "suspension_migrated_" + key, "status": suspensionStatus, "reason_code": reasonCode,
			"recoverability": recoverability, "reason": reason, "missing_evidence": missing,
			"allowed_actions": actions, "checkpoint_id": checkpointID, "suspended_at": migratedAt,
		})
	}
	for _, field := range legacyRecoveryFields {
		delete(issue, field)
	}
	return nil
}

func legacyCheckpointStage(issue map[string]json.RawMessage, originalStatus string) string {
	if raw := issue["publication_failure"]; len(raw) > 0 && string(raw) != "null" {
		return "publication_recovery_pending"
	}
	if raw := issue["pull_request_checks_failure"]; len(raw) > 0 && string(raw) != "null" {
		return "pull_request_checks_recovery_pending"
	}
	if rawString(issue["pull_request_url"]) != "" {
		return "awaiting_checks"
	}
	if originalStatus != "" && originalStatus != "blocked" && originalStatus != "failed" && originalStatus != "completed" {
		return originalStatus
	}
	return "resume_pending"
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

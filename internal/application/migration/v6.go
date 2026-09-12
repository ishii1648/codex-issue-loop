package migration

import (
	"encoding/json"
	"fmt"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
)

func decodeV6Snapshot(data []byte) (state.Snapshot, error) {
	var envelope struct {
		Version   int    `json:"version"`
		Semantic  int    `json:"semantic_contract_version"`
		Lifecycle string `json:"issue_lifecycle_api_version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return state.Snapshot{}, err
	}
	if envelope.Version != state.CurrentVersion && (envelope.Version != 5 || envelope.Semantic != 4 || (envelope.Lifecycle != "2.0" && envelope.Lifecycle != "2.1")) {
		return state.Snapshot{}, fmt.Errorf("unsupported snapshot contract (%d,%d,%s); supported migration is (5,4,2.0/2.1) to v%d", envelope.Version, envelope.Semantic, envelope.Lifecycle, state.CurrentVersion)
	}
	if envelope.Version != state.CurrentVersion {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return state.Snapshot{}, err
		}
		fields["version"] = json.RawMessage(fmt.Sprint(state.CurrentVersion))
		delete(fields, "semantic_contract_version")
		delete(fields, "issue_lifecycle_api_version")
		currentData, err := json.Marshal(fields)
		if err != nil {
			return state.Snapshot{}, err
		}
		var current state.Snapshot
		if err := json.Unmarshal(currentData, &current); err != nil {
			return state.Snapshot{}, fmt.Errorf("legacy fields cannot be represented in the current contract: %w", err)
		}
	}
	var snapshot state.Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return state.Snapshot{}, err
	}
	if envelope.Version != state.CurrentVersion {
		if snapshot.ActiveExecution != nil {
			return state.Snapshot{}, fmt.Errorf("snapshot migration requires no active execution")
		}
		for _, item := range snapshot.Issues {
			if item == nil {
				return state.Snapshot{}, fmt.Errorf("snapshot migration contains a null Issue")
			}
			if item.WorkerPID != 0 || item.WorkerPGID != 0 || item.Status.RequiresActiveExecution() {
				return state.Snapshot{}, fmt.Errorf("Issue #%d retains worker identity or executable state", item.Number)
			}
		}
		for _, request := range snapshot.PendingRequests {
			if request != nil && request.Status == issuedomain.RequestStatusPending {
				return state.Snapshot{}, fmt.Errorf("snapshot migration requires no unanswered requests")
			}
		}
		if len(snapshot.PendingEffects) != 0 {
			return state.Snapshot{}, fmt.Errorf("snapshot migration requires no unfinished effects")
		}
		for _, item := range snapshot.Issues {
			if err := state.MigrateLegacyCancellation(&snapshot, item.Number); err != nil {
				return state.Snapshot{}, err
			}
		}
		snapshot.Version = state.CurrentVersion
		snapshot.SemanticContractVersion = 0
		snapshot.IssueLifecycleAPIVersion = ""
	}
	if err := snapshot.Validate(); err != nil {
		return state.Snapshot{}, fmt.Errorf("validate migrated snapshot: %w", err)
	}
	return snapshot, nil
}

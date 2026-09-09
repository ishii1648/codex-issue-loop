package statecontract

import (
	"encoding/json"
	"fmt"
)

type SchemaVersionError struct {
	Kind    string
	Version int
}

func (e SchemaVersionError) Error() string {
	return fmt.Sprintf("unsupported %s version %d; this binary supports snapshot version %d; an explicit migration is required", e.Kind, e.Version, CurrentVersion)
}

// LegacySource identifies the supported migration envelope without upgrading it.
func (snapshot Snapshot) LegacySource() bool {
	return snapshot.Version == 5 && snapshot.SemanticContractVersion == 4 &&
		(snapshot.IssueLifecycleAPIVersion == "2.0" || snapshot.IssueLifecycleAPIVersion == "2.1")
}

func (snapshot Snapshot) ValidateVersion() error {
	if snapshot.Version != CurrentVersion || snapshot.SemanticContractVersion != 0 || snapshot.IssueLifecycleAPIVersion != "" {
		return SchemaVersionError{Kind: "snapshot", Version: snapshot.Version}
	}
	return nil
}

// ValidateLegacyV5 is reserved for the existing v4-to-v5 migration and backup
// inspection. It does not authorize a runtime load or commit of a v5 snapshot.
func (snapshot Snapshot) ValidateLegacyV5() error {
	if !snapshot.LegacySource() {
		return SchemaVersionError{Kind: "legacy snapshot", Version: snapshot.Version}
	}
	return snapshot.validateAggregate()
}

// ValidateEnvelope checks compatibility before any payload interpretation or repair.
func ValidateEnvelope(data []byte) error {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Version != CurrentVersion {
		return SchemaVersionError{Kind: "snapshot", Version: envelope.Version}
	}
	return nil
}

package snapshot

import "encoding/json"

// CheckVersion checks the envelope before interpreting version-specific fields.
func CheckVersion(data []byte) error {
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Version != CurrentVersion {
		return SchemaVersionError{Kind: "state", Version: envelope.Version}
	}
	return nil
}

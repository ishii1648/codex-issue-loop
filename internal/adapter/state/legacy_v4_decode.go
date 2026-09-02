package state

import "encoding/json"

// UnmarshalJSON accepts v4 lease names only so the versioned migrator and its
// byte-locked fixtures can decode the previous schema. MarshalJSON always uses
// the canonical v5 field names declared on Issue.
func (i *Issue) UnmarshalJSON(data []byte) error {
	type issueAlias Issue
	decoded := struct {
		*issueAlias
		LegacyLease *ExecutionLease         `json:"lease"`
		LegacyPark  *ContinuationCheckpoint `json:"resource_park"`
	}{issueAlias: (*issueAlias)(i)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if i.Lease == nil {
		i.Lease = decoded.LegacyLease
	}
	if i.ResourcePark == nil {
		i.ResourcePark = decoded.LegacyPark
	}
	return nil
}

func (c *ContinuationCheckpoint) UnmarshalJSON(data []byte) error {
	type checkpointAlias ContinuationCheckpoint
	decoded := struct {
		*checkpointAlias
		LegacyOriginal *ExecutionLease `json:"original_lease"`
	}{checkpointAlias: (*checkpointAlias)(c)}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if c.OriginalLease.Owner == (LeaseOwner{}) && decoded.LegacyOriginal != nil {
		c.OriginalLease = *decoded.LegacyOriginal
	}
	return nil
}

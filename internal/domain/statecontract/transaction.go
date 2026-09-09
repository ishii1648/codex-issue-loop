package statecontract

import (
	"encoding/json"
	"fmt"
)

func ValidateEventSequence(snapshot Snapshot, events []Event) error {
	last := uint64(0)
	for index, event := range events {
		if index == 0 && event.Type == "event_log_checkpoint" {
			if event.Sequence == 0 {
				return fmt.Errorf("event log checkpoint sequence must be positive")
			}
			last = event.Sequence
			continue
		}
		expected := last + 1
		if event.Sequence != expected {
			return fmt.Errorf("event sequence at index %d is %d, expected %d", index, event.Sequence, expected)
		}
		last = event.Sequence
	}
	if snapshot.StateRevision != last {
		return fmt.Errorf("state revision %d does not match last event sequence %d", snapshot.StateRevision, last)
	}
	return nil
}

type Transaction struct {
	Version  int      `json:"version"`
	Snapshot Snapshot `json:"snapshot"`
	Event    Event    `json:"event"`
}

func (txn Transaction) Validate(repoID string) error {
	if txn.Version != CurrentVersion || txn.Snapshot.Version != CurrentVersion || txn.Event.Version != CurrentVersion {
		version := txn.Version
		if version == CurrentVersion && txn.Snapshot.Version != CurrentVersion {
			version = txn.Snapshot.Version
		}
		if version == CurrentVersion && txn.Event.Version != CurrentVersion {
			version = txn.Event.Version
		}
		return SchemaVersionError{Kind: "transaction", Version: version}
	}
	if txn.Snapshot.RepoID != repoID || txn.Event.RepoID != repoID {
		return fmt.Errorf("transaction repository does not match state store")
	}
	if txn.Event.Sequence == 0 {
		return fmt.Errorf("transaction event sequence must be positive")
	}
	if txn.Snapshot.StateRevision != txn.Event.Sequence {
		return fmt.Errorf("transaction snapshot revision %d does not match event sequence %d", txn.Snapshot.StateRevision, txn.Event.Sequence)
	}
	if err := txn.Snapshot.Validate(); err != nil {
		return fmt.Errorf("prepared transaction snapshot: %w", err)
	}
	return nil
}

func (txn *Transaction) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Version  int             `json:"version"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Version != CurrentVersion {
		return SchemaVersionError{Kind: "transaction", Version: envelope.Version}
	}
	if err := ValidateEnvelope(envelope.Snapshot); err != nil {
		return err
	}
	type decodedTransaction Transaction
	var decoded decodedTransaction
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*txn = Transaction(decoded)
	return nil
}

func (event Event) Validate(repoID string) error {
	if event.Version != CurrentVersion {
		return SchemaVersionError{Kind: "event", Version: event.Version}
	}
	if event.RepoID != repoID {
		return fmt.Errorf("invalid event metadata at sequence %d", event.Sequence)
	}
	return nil
}

func (snapshot Snapshot) ValidateRepositoryIdentity(repoID string) error {
	if snapshot.RepoID != repoID {
		return fmt.Errorf("state repo_id %q does not match %q", snapshot.RepoID, repoID)
	}
	return nil
}

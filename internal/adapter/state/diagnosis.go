package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ReadDiagnosticSnapshot never repairs durable files. It uses the existing
// writer lock without creating it; an unavailable lock leaves state unconfirmed.
func (s Store) ReadDiagnosticSnapshot() (Snapshot, []Event, error) {
	lock, err := os.Open(s.lockPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, stateErr := os.Lstat(s.StatePath()); errors.Is(stateErr, os.ErrNotExist) {
				return Snapshot{}, nil, fmt.Errorf("snapshot absent; commit is unconfirmed: %w", stateErr)
			}
		}
		return Snapshot{}, nil, fmt.Errorf("STATE_UNCONFIRMED: cannot open existing state lock: %w", err)
	}
	defer unlock(lock)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return Snapshot{}, nil, fmt.Errorf("STATE_UNCONFIRMED: writer active; retry diagnosis: %w", err)
	}
	for _, path := range []string{s.TransactionPath(), s.quarantineRecoveryTransactionPath()} {
		if _, err := os.Lstat(path); err == nil {
			return Snapshot{}, nil, fmt.Errorf("STATE_UNCONFIRMED: transaction present at %s; snapshot/events commit is unconfirmed; no repair or rollback performed", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, nil, err
		}
	}
	_, _, partial, eventErr := s.readEventsUnlocked()
	if eventErr != nil {
		return Snapshot{}, nil, eventErr
	}
	if partial {
		return Snapshot{}, nil, errors.New("STATE_UNCONFIRMED: partial event tail; no truncation performed")
	}
	snapshot, events, err := s.ReadRecoveryInputs()
	if err != nil {
		return Snapshot{}, nil, err
	}
	if snapshot.Version != CurrentVersion {
		return Snapshot{}, nil, SchemaVersionError{Kind: "state", Version: snapshot.Version}
	}
	if snapshot.RepoPath != s.RepoPath {
		return Snapshot{}, nil, errors.New("snapshot repository path differs")
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, nil, err
	}
	for _, event := range events {
		if event.Version != CurrentVersion {
			return Snapshot{}, nil, SchemaVersionError{Kind: "event", Version: event.Version}
		}
	}
	if err := validateEventSequence(snapshot, events); err != nil {
		return Snapshot{}, nil, err
	}
	if snapshot.Recovery != nil {
		return snapshot, events, fmt.Errorf("STATE_RECOVERY_REQUIRED: observed recovery marker: %s; %s; original state is unconfirmed", snapshot.Recovery.Status, snapshot.Recovery.Reason)
	}
	return snapshot, events, nil
}

package state

import (
	"errors"
	"os"
	"syscall"
)

type StatusReadError struct{ Code string }

func (e StatusReadError) Error() string { return e.Code }

// ReadStatusSnapshot holds the existing writer lock only while reading a
// committed snapshot. It never repairs state or inspects event history.
func (s Store) ReadStatusSnapshot() (Snapshot, error) {
	lock, err := os.Open(s.lockPath())
	if err != nil {
		return Snapshot{}, StatusReadError{"state_unavailable"}
	}
	defer unlock(lock)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return Snapshot{}, StatusReadError{"state_busy"}
	}
	for _, path := range []string{s.TransactionPath(), s.quarantineRecoveryTransactionPath()} {
		if _, err := os.Lstat(path); err == nil {
			return Snapshot{}, StatusReadError{"state_unconfirmed"}
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, StatusReadError{"state_unavailable"}
		}
	}
	snapshot, err := s.ReadCanonicalSnapshot()
	if err != nil {
		var version SchemaVersionError
		if errors.As(err, &version) {
			return Snapshot{}, StatusReadError{"unsupported_snapshot_version"}
		}
		return Snapshot{}, StatusReadError{"invalid_state"}
	}
	if snapshot.RepoPath != s.RepoPath {
		return Snapshot{}, StatusReadError{"invalid_state"}
	}
	if snapshot.Recovery != nil {
		return Snapshot{}, StatusReadError{"state_recovery_required"}
	}
	if err := ValidateSemanticContract(snapshot); err != nil {
		return Snapshot{}, StatusReadError{"invalid_state"}
	}
	return snapshot, nil
}

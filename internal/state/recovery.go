package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
)

type transaction struct {
	Version  int      `json:"version"`
	Snapshot Snapshot `json:"snapshot"`
	Event    Event    `json:"event"`
}

func (s Store) recoverUnlocked() (Snapshot, error) {
	snapshot, snapshotExists, err := s.loadSnapshotUnlocked()
	if err != nil {
		return s.quarantineUnlocked(err)
	}
	events, validBytes, partial, err := s.readEventsUnlocked()
	if err != nil {
		return s.quarantineUnlocked(err)
	}

	txn, txnExists, err := s.loadTransactionUnlocked()
	if err != nil {
		return s.quarantineUnlocked(err)
	}
	if txnExists {
		if err := s.validateTransaction(txn); err != nil {
			return s.quarantineUnlocked(err)
		}
		if partial {
			if err := os.Truncate(s.EventsPath(), validBytes); err != nil {
				return Snapshot{}, fmt.Errorf("truncate partial event log: %w", err)
			}
		}
		last := uint64(0)
		if len(events) > 0 {
			last = events[len(events)-1].Sequence
		}
		switch {
		case last == txn.Event.Sequence:
			if !sameEvent(events[len(events)-1], txn.Event) {
				return s.quarantineUnlocked(fmt.Errorf("transaction event conflicts with event sequence %d", last))
			}
		case last+1 == txn.Event.Sequence:
			if err := s.appendEventUnlocked(txn.Event); err != nil {
				return Snapshot{}, err
			}
			events = append(events, txn.Event)
		default:
			return s.quarantineUnlocked(fmt.Errorf("transaction event sequence %d does not follow event log sequence %d", txn.Event.Sequence, last))
		}

		if snapshotExists {
			switch {
			case snapshot.StateRevision == txn.Snapshot.StateRevision:
				if !reflect.DeepEqual(snapshot, txn.Snapshot) {
					return s.quarantineUnlocked(fmt.Errorf("transaction snapshot conflicts at revision %d", snapshot.StateRevision))
				}
			case snapshot.StateRevision < txn.Snapshot.StateRevision:
				if err := fsutil.WriteJSON(s.StatePath(), txn.Snapshot, 0o600); err != nil {
					return Snapshot{}, fmt.Errorf("complete transaction snapshot: %w", err)
				}
				snapshot = txn.Snapshot
			default:
				return s.quarantineUnlocked(fmt.Errorf("state revision %d is ahead of prepared transaction revision %d", snapshot.StateRevision, txn.Snapshot.StateRevision))
			}
		} else {
			if err := fsutil.WriteJSON(s.StatePath(), txn.Snapshot, 0o600); err != nil {
				return Snapshot{}, fmt.Errorf("restore transaction snapshot: %w", err)
			}
			snapshot, snapshotExists = txn.Snapshot, true
		}
		if err := s.removeTransactionUnlocked(); err != nil {
			return Snapshot{}, err
		}
		events, validBytes, partial, err = s.readEventsUnlocked()
		if err != nil {
			return Snapshot{}, err
		}
	}

	if !snapshotExists {
		if len(events) > 0 || partial {
			return s.quarantineUnlocked(errors.New("state snapshot is missing while event log is not empty"))
		}
		return s.emptySnapshot(), nil
	}

	if partial {
		last := uint64(0)
		if len(events) > 0 {
			last = events[len(events)-1].Sequence
		}
		if snapshot.StateRevision != last {
			return s.quarantineUnlocked(fmt.Errorf("partial event tail with state revision %d and last complete event %d", snapshot.StateRevision, last))
		}
		if err := os.Truncate(s.EventsPath(), validBytes); err != nil {
			return Snapshot{}, fmt.Errorf("truncate partial event log: %w", err)
		}
		var repairErr error
		snapshot, repairErr = s.recordRepairUnlocked(snapshot, "event_log_tail_truncated", map[string]any{"truncated_at": validBytes})
		if repairErr != nil {
			return Snapshot{}, repairErr
		}
		events, _, _, err = s.readEventsUnlocked()
		if err != nil {
			return Snapshot{}, err
		}
	}

	if err := s.validateConsistency(snapshot, events); err != nil {
		return s.quarantineUnlocked(err)
	}
	return snapshot, nil
}

func (s Store) emptySnapshot() Snapshot {
	now := time.Now().UTC()
	return Snapshot{
		Version: 1, RepoID: s.RepoID, RepoPath: s.RepoPath,
		Supervisor: Supervisor{State: "stopped", UpdatedAt: now},
		Issues:     map[string]*Issue{}, PendingRequests: map[string]*Request{},
	}
}

func (s Store) loadSnapshotUnlocked() (Snapshot, bool, error) {
	data, err := os.ReadFile(s.StatePath())
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("read state: %w", err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, false, fmt.Errorf("decode state: %w", err)
	}
	if snapshot.Version != 1 {
		return Snapshot{}, false, fmt.Errorf("unsupported state version %d", snapshot.Version)
	}
	if snapshot.RepoID != s.RepoID {
		return Snapshot{}, false, fmt.Errorf("state repo_id %q does not match %q", snapshot.RepoID, s.RepoID)
	}
	if snapshot.Issues == nil {
		snapshot.Issues = map[string]*Issue{}
	}
	if snapshot.PendingRequests == nil {
		snapshot.PendingRequests = map[string]*Request{}
	}
	return snapshot, true, nil
}

func (s Store) readEventsUnlocked() ([]Event, int64, bool, error) {
	f, err := os.Open(s.EventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("open event log: %w", err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	events := []Event{}
	var validBytes int64
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			if errors.Is(readErr, io.EOF) {
				return events, validBytes, true, nil
			}
			return nil, validBytes, false, fmt.Errorf("read event log: %w", readErr)
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var event Event
			if err := json.Unmarshal(bytes.TrimSpace(line), &event); err != nil {
				return nil, validBytes, false, fmt.Errorf("decode event at byte %d: %w", validBytes, err)
			}
			if event.Version != 1 || event.RepoID != s.RepoID {
				return nil, validBytes, false, fmt.Errorf("invalid event metadata at sequence %d", event.Sequence)
			}
			events = append(events, event)
		}
		validBytes += int64(len(line))
		if errors.Is(readErr, io.EOF) {
			return events, validBytes, false, nil
		}
		if readErr != nil {
			return nil, validBytes, false, fmt.Errorf("read event log: %w", readErr)
		}
	}
}

func (s Store) loadTransactionUnlocked() (transaction, bool, error) {
	data, err := os.ReadFile(s.TransactionPath())
	if errors.Is(err, os.ErrNotExist) {
		return transaction{}, false, nil
	}
	if err != nil {
		return transaction{}, false, fmt.Errorf("read state transaction: %w", err)
	}
	var txn transaction
	if err := json.Unmarshal(data, &txn); err != nil {
		return transaction{}, false, fmt.Errorf("decode state transaction: %w", err)
	}
	return txn, true, nil
}

func (s Store) validateTransaction(txn transaction) error {
	if txn.Version != 1 || txn.Snapshot.Version != 1 || txn.Event.Version != 1 {
		return errors.New("unsupported transaction version")
	}
	if txn.Snapshot.RepoID != s.RepoID || txn.Event.RepoID != s.RepoID {
		return errors.New("transaction repository does not match state store")
	}
	if txn.Event.Sequence == 0 {
		return errors.New("transaction event sequence must be positive")
	}
	if txn.Snapshot.StateRevision != txn.Event.Sequence {
		return fmt.Errorf("transaction snapshot revision %d does not match event sequence %d", txn.Snapshot.StateRevision, txn.Event.Sequence)
	}
	return nil
}

func (s Store) validateConsistency(snapshot Snapshot, events []Event) error {
	last := uint64(0)
	for index, event := range events {
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

func sameEvent(left, right Event) bool {
	return left.Version == right.Version && left.EventID == right.EventID && left.Sequence == right.Sequence &&
		left.RepoID == right.RepoID && left.IssueNumber == right.IssueNumber && left.RunID == right.RunID &&
		left.Type == right.Type && bytes.Equal(left.Payload, right.Payload)
}

func (s Store) appendEventUnlocked(event Event) error {
	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	events, err := os.OpenFile(s.EventsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open event log: %w", err)
	}
	if _, err := events.Write(append(line, '\n')); err != nil {
		events.Close()
		return fmt.Errorf("append event: %w", err)
	}
	if err := events.Sync(); err != nil {
		events.Close()
		return fmt.Errorf("sync event log: %w", err)
	}
	if err := events.Close(); err != nil {
		return err
	}
	return nil
}

func (s Store) removeTransactionUnlocked() error {
	if err := os.Remove(s.TransactionPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed state transaction: %w", err)
	}
	return syncDirectory(s.Dir)
}

func (s Store) recordRepairUnlocked(snapshot Snapshot, eventType string, payload any) (Snapshot, error) {
	snapshot.StateRevision++
	now := time.Now().UTC()
	snapshot.Supervisor.UpdatedAt = now
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Snapshot{}, err
	}
	event := Event{
		Version: 1, EventID: NewID("evt"), Sequence: snapshot.StateRevision,
		Timestamp: now, RepoID: s.RepoID, Type: eventType, Payload: payloadJSON,
	}
	txn := transaction{Version: 1, Snapshot: snapshot, Event: event}
	if err := fsutil.WriteJSON(s.TransactionPath(), txn, 0o600); err != nil {
		return Snapshot{}, err
	}
	if err := s.appendEventUnlocked(event); err != nil {
		return Snapshot{}, err
	}
	if err := fsutil.WriteJSON(s.StatePath(), snapshot, 0o600); err != nil {
		return Snapshot{}, err
	}
	if err := s.removeTransactionUnlocked(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func (s Store) quarantineUnlocked(cause error) (Snapshot, error) {
	backupDir := filepath.Join(s.Dir, "recovery", time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+NewID("backup"))
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("create recovery backup: %w (original error: %v)", err, cause)
	}
	for _, path := range []string{s.StatePath(), s.EventsPath(), s.TransactionPath()} {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return Snapshot{}, fmt.Errorf("inspect recovery input %s: %w", path, err)
		}
		if err := os.Rename(path, filepath.Join(backupDir, filepath.Base(path))); err != nil {
			return Snapshot{}, fmt.Errorf("quarantine %s: %w", path, err)
		}
	}
	if err := syncDirectory(s.Dir); err != nil {
		return Snapshot{}, err
	}

	now := time.Now().UTC()
	snapshot := s.emptySnapshot()
	snapshot.StateRevision = 1
	snapshot.Supervisor = Supervisor{
		State: "blocked", UpdatedAt: now,
		Message: fmt.Sprintf("durable state recovery blocked: %v (backup: %s)", cause, backupDir),
	}
	snapshot.Recovery = &Recovery{Status: "blocked", Reason: cause.Error(), BackupDir: backupDir, DetectedAt: now}
	payload, _ := json.Marshal(map[string]string{"reason": cause.Error(), "backup_dir": backupDir})
	event := Event{
		Version: 1, EventID: NewID("evt"), Sequence: 1, Timestamp: now,
		RepoID: s.RepoID, Type: "recovery_blocked", Payload: payload,
	}
	if err := s.appendEventUnlocked(event); err != nil {
		return Snapshot{}, err
	}
	if err := fsutil.WriteJSON(s.StatePath(), snapshot, 0o600); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

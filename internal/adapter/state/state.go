package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"github.com/ishii1648/codex-issue-loop/internal/platform/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"
	"time"
)

const CurrentVersion = statecontract.CurrentVersion

type SchemaVersionError = statecontract.SchemaVersionError

type Store struct {
	Dir            string
	RepoID         string
	RepoPath       string
	Secrets        []string
	EventRetention retention.Policy
}

func (s Store) StatePath() string  { return filepath.Join(s.Dir, "state.json") }
func (s Store) EventsPath() string { return filepath.Join(s.Dir, "events.jsonl") }

func (s Store) TransactionPath() string { return filepath.Join(s.Dir, "state.txn.json") }
func (s Store) lockPath() string        { return filepath.Join(s.Dir, "state.lock") }

func (s Store) Ensure() error {
	if err := s.ensureDir(); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	_, err := s.Load()
	return err
}

func (s Store) Load() (Snapshot, error) {
	if err := s.ensureDir(); err != nil {
		return Snapshot{}, err
	}
	// Loading may complete a prepared transaction or repair a partial log tail.
	lock, err := s.lock(true)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock(lock)
	return s.recoverUnlocked()
}

func (s Store) Update(eventType string, issueNumber int, runID string, payload any, mutate func(*Snapshot) error) (Snapshot, error) {
	if err := s.ensureDir(); err != nil {
		return Snapshot{}, err
	}
	lock, err := s.lock(true)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock(lock)
	snapshot, err := s.recoverUnlocked()
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Recovery != nil && snapshot.Recovery.Status == RecoveryStateBlocked {
		return Snapshot{}, fmt.Errorf("durable state is recovery-blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)
	}
	var lastValid *Issue
	normalizeSnapshot(&snapshot)
	if issueNumber > 0 {
		lastValid = cloneIssue(snapshot.Issues[strconv.Itoa(issueNumber)])
	}
	if err := mutate(&snapshot); err != nil {
		if issueNumber > 0 {
			return Snapshot{}, IssueMutationError{IssueNumber: issueNumber, Err: err}
		}
		return Snapshot{}, err
	}
	normalizeSnapshot(&snapshot)
	now := time.Now().UTC()
	finalizeLifecycleBoundaries(&snapshot, now)
	if validationErr := snapshot.Validate(); validationErr != nil {
		if issueNumber < 1 || !isolateInvalidIssue(&snapshot, issueNumber, lastValid, validationErr, now) {
			return Snapshot{}, fmt.Errorf("validate snapshot before event %q: %w", eventType, validationErr)
		}
		if err := snapshot.Validate(); err != nil {
			return Snapshot{}, fmt.Errorf("validate repository after quarantining Issue #%d: %w", issueNumber, err)
		}
		payload = map[string]any{"rejected_event": eventType, "reason_code": "issue_invariant_violation", "reason": validationErr.Error()}
		eventType = "issue_quarantined"
	}
	snapshot.StateRevision++
	snapshot.Supervisor.UpdatedAt = now
	snapshotJSON, err := redact.Marshal(snapshot, s.Secrets)
	if err != nil {
		return Snapshot{}, fmt.Errorf("sanitize state snapshot: %w", err)
	}
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode sanitized state snapshot: %w", err)
	}
	payloadJSON, err := redact.Marshal(payload, s.Secrets)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal event payload: %w", err)
	}
	event := Event{
		Version: CurrentVersion, EventID: NewID("evt"), Sequence: snapshot.StateRevision,
		Timestamp: now, RepoID: s.RepoID, IssueNumber: issueNumber,
		RunID: runID, Type: eventType, Payload: payloadJSON,
	}
	txn := transaction{Version: CurrentVersion, Snapshot: snapshot, Event: event}
	if err := txn.Validate(s.RepoID); err != nil {
		return Snapshot{}, err
	}
	if err := fsutil.WriteJSON(s.TransactionPath(), txn, 0o600); err != nil {
		return Snapshot{}, fmt.Errorf("prepare state transaction: %w", err)
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
	if err := s.rotateEventsUnlocked(snapshot); err != nil {
		log.Printf("rotate event log: %s", redact.StringWithSecrets(err.Error(), s.Secrets))
	}
	return snapshot, nil
}

type IssueMutationError struct {
	IssueNumber int
	Err         error
}

func (e IssueMutationError) Error() string { return e.Err.Error() }
func (e IssueMutationError) Unwrap() error { return e.Err }
func (e IssueMutationError) IssueScope() int {
	return e.IssueNumber
}

func isolateInvalidIssue(snapshot *Snapshot, issueNumber int, lastValid *Issue, cause error, now time.Time) bool {
	if snapshot == nil || issueNumber < 1 || cause == nil {
		return false
	}
	key := strconv.Itoa(issueNumber)
	rejected := snapshot.Issues[key]
	record := &QuarantineRecord{
		IssueNumber: issueNumber, ReasonCode: "issue_invariant_violation", Reason: cause.Error(),
		QuarantinedAt: now.UTC(), LastValid: cloneIssue(lastValid),
	}
	if rejected != nil {
		record.RunID, record.Generation, record.RejectedStatus = rejected.RunID, rejected.Generation, rejected.Status
	} else if lastValid != nil {
		record.RunID, record.Generation, record.RejectedStatus = lastValid.RunID, lastValid.Generation, lastValid.Status
	}
	delete(snapshot.Issues, key)
	for id, request := range snapshot.PendingRequests {
		if request == nil || request.IssueNumber != issueNumber {
			continue
		}
		copy := *request
		copy.Options = append([]Option(nil), request.Options...)
		record.Requests = append(record.Requests, &copy)
		delete(snapshot.PendingRequests, id)
	}
	delete(snapshot.PendingEffects, key)
	sort.Slice(record.Requests, func(i, j int) bool { return record.Requests[i].ID < record.Requests[j].ID })
	if snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issueNumber {
		snapshot.ActiveExecution = nil
	}
	snapshot.QuarantinedIssues[key] = record
	return true
}

func cloneIssue(issue *Issue) *Issue {
	if issue == nil {
		return nil
	}
	data, err := json.Marshal(issue)
	if err != nil {
		return nil
	}
	var cloned Issue
	if err := json.Unmarshal(data, &cloned); err != nil {
		return nil
	}
	return &cloned
}

func (s Store) rotateEventsUnlocked(snapshot Snapshot) error {
	policy := s.EventRetention
	if policy.MaxBytes <= 0 || policy.MaxAge <= 0 || policy.Keep <= 0 || snapshot.StateRevision == 0 {
		return nil
	}
	info, err := os.Stat(s.EventsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() <= policy.MaxBytes && time.Since(info.ModTime()) <= policy.MaxAge {
		return nil
	}
	payload, err := json.Marshal(map[string]uint64{"archived_through": snapshot.StateRevision})
	if err != nil {
		return err
	}
	checkpoint := Event{
		Version: CurrentVersion, EventID: NewID("evt"), Sequence: snapshot.StateRevision,
		Timestamp: time.Now().UTC(), RepoID: s.RepoID, Type: "event_log_checkpoint", Payload: payload,
	}
	line, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return retention.ArchiveAndReplace(s.EventsPath(), append(line, '\n'), policy)
}

func (s Store) Initialize() error {
	if err := s.ensureDir(); err != nil {
		return err
	}
	lock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	snapshot, err := s.recoverUnlocked()
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.StatePath()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	return fsutil.WriteJSON(s.StatePath(), snapshot, 0o600)
}

func (s Store) AcquireSupervisorLock() (*os.File, error) {
	path := filepath.Join(s.Dir, "supervisor.lock")
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("another supervisor holds %s: %w", path, errors.Join(err, f.Close()))
	}
	return f, nil
}

// InspectExclusive runs inspect while holding the repository state mutation
// lock. The callback must not call Store methods. It may perform only a bounded
// external action whose safety depends on preventing a concurrent admission.
func (s Store) InspectExclusive(inspect func(Snapshot) error) error {
	if inspect == nil {
		return errors.New("exclusive state inspection callback is required")
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	lock, err := s.lock(true)
	if err != nil {
		return err
	}
	defer unlock(lock)
	snapshot, err := s.recoverUnlocked()
	if err != nil {
		return err
	}
	return inspect(snapshot)
}

func (s Store) ensureDir() error {
	const managedModeMask = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("managed state directory is not a directory: %s", s.Dir)
	}
	if info.Mode()&managedModeMask != 0o700 {
		if err := os.Chmod(s.Dir, 0o700); err != nil {
			return err
		}
	}
	for _, path := range []string{s.StatePath(), s.EventsPath(), s.TransactionPath(), s.lockPath(), filepath.Join(s.Dir, "supervisor.lock")} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("managed state path is not a regular file: %s", path)
		}
		if info.Mode()&managedModeMask == 0o600 {
			continue
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func ReleaseSupervisorLock(f *os.File) {
	if f == nil {
		return
	}
	unlock(f)
}

func (s Store) lock(exclusive bool) (*os.File, error) {
	f, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		return nil, fmt.Errorf("lock state: %w", errors.Join(err, f.Close()))
	}
	return f, nil
}

func unlock(f *os.File) {
	// Closing the descriptor also releases the lock if explicit unlock fails.
	defer io.Closer(f).Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		return
	}
}

func NewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func ValidID(value, prefix string) bool { return statecontract.ValidID(value, prefix) }

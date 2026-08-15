package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
)

type Supervisor struct {
	State     string    `json:"state"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Message   string    `json:"message,omitempty"`
}

type AnswerRecord struct {
	RequestID  string    `json:"request_id"`
	Question   string    `json:"question"`
	Answer     string    `json:"answer"`
	AnsweredAt time.Time `json:"answered_at"`
}

type Issue struct {
	Number           int            `json:"number"`
	Title            string         `json:"title"`
	Status           string         `json:"status"`
	RunID            string         `json:"run_id,omitempty"`
	Branch           string         `json:"branch,omitempty"`
	Worktree         string         `json:"worktree,omitempty"`
	Attempts         int            `json:"attempts"`
	Continuations    int            `json:"continuations"`
	ExecutionProfile string         `json:"execution_profile,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	WorkerPID        int            `json:"worker_pid,omitempty"`
	PullRequestURL   string         `json:"pull_request_url,omitempty"`
	GitHubSync       string         `json:"github_sync,omitempty"`
	LastError        string         `json:"last_error,omitempty"`
	RetryAfter       *time.Time     `json:"retry_after,omitempty"`
	Answers          []AnswerRecord `json:"answers,omitempty"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Request struct {
	ID            string     `json:"id"`
	IssueNumber   int        `json:"issue_number"`
	Question      string     `json:"question"`
	Reason        string     `json:"reason,omitempty"`
	Recommended   string     `json:"recommended_option,omitempty"`
	Options       []Option   `json:"options,omitempty"`
	AllowFreeText bool       `json:"allow_free_text"`
	Status        string     `json:"status"`
	Answer        string     `json:"answer,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	AnsweredAt    *time.Time `json:"answered_at,omitempty"`
}

type Recovery struct {
	Status     string    `json:"status"`
	Reason     string    `json:"reason"`
	BackupDir  string    `json:"backup_dir"`
	DetectedAt time.Time `json:"detected_at"`
}

type Snapshot struct {
	Version         int                 `json:"version"`
	RepoID          string              `json:"repo_id"`
	RepoPath        string              `json:"repo_path"`
	StateRevision   uint64              `json:"state_revision"`
	Supervisor      Supervisor          `json:"supervisor"`
	Issues          map[string]*Issue   `json:"issues"`
	PendingRequests map[string]*Request `json:"pending_requests"`
	Recovery        *Recovery           `json:"recovery,omitempty"`
}

type Event struct {
	Version     int             `json:"version"`
	EventID     string          `json:"event_id"`
	Sequence    uint64          `json:"sequence"`
	Timestamp   time.Time       `json:"timestamp"`
	RepoID      string          `json:"repo_id"`
	IssueNumber int             `json:"issue_number,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type Store struct {
	Dir      string
	RepoID   string
	RepoPath string
}

func (s Store) StatePath() string  { return filepath.Join(s.Dir, "state.json") }
func (s Store) EventsPath() string { return filepath.Join(s.Dir, "events.jsonl") }

func (s Store) TransactionPath() string { return filepath.Join(s.Dir, "state.txn.json") }
func (s Store) lockPath() string        { return filepath.Join(s.Dir, "state.lock") }

func (s Store) Ensure() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	_, err := s.Load()
	return err
}

func (s Store) Load() (Snapshot, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
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
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
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
	if snapshot.Recovery != nil && snapshot.Recovery.Status == "blocked" {
		return Snapshot{}, fmt.Errorf("durable state is recovery-blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)
	}
	if err := mutate(&snapshot); err != nil {
		return Snapshot{}, err
	}
	snapshot.StateRevision++
	now := time.Now().UTC()
	snapshot.Supervisor.UpdatedAt = now
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal event payload: %w", err)
	}
	event := Event{
		Version: 1, EventID: NewID("evt"), Sequence: snapshot.StateRevision,
		Timestamp: now, RepoID: s.RepoID, IssueNumber: issueNumber,
		RunID: runID, Type: eventType, Payload: payloadJSON,
	}
	txn := transaction{Version: 1, Snapshot: snapshot, Event: event}
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
	return snapshot, nil
}

func (s Store) Initialize() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
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
	return fsutil.WriteJSON(s.StatePath(), snapshot, 0o600)
}

func (s Store) AcquireSupervisorLock() (*os.File, error) {
	path := filepath.Join(s.Dir, "supervisor.lock")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another supervisor holds %s: %w", path, err)
	}
	return f, nil
}

func ReleaseSupervisorLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
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
		f.Close()
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return f, nil
}

func unlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func (s Snapshot) Attention(untilIdle bool) (string, bool) {
	requests := make([]string, 0)
	for id, request := range s.PendingRequests {
		if request.Status == "pending" {
			requests = append(requests, id)
		}
	}
	if len(requests) > 0 {
		sort.Strings(requests)
		return "needs_input", true
	}
	if s.Supervisor.State == "blocked" || s.Supervisor.State == "stopped" {
		return s.Supervisor.State, true
	}
	if untilIdle && s.Supervisor.State == "polling" {
		for _, issue := range s.Issues {
			if issue.GitHubSync != "" {
				return "", false
			}
			if issue.Status == "claiming" || issue.Status == "running" || issue.Status == "claimed" || issue.Status == "resume_pending" || issue.Status == "retry_wait" {
				return "", false
			}
		}
		return "idle", true
	}
	return "", false
}

func NewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

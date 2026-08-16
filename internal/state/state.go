package state

import (
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/ishii1648/codex-issue-loop/internal/redact"
	"github.com/ishii1648/codex-issue-loop/internal/retention"
	schemaversion "github.com/ishii1648/codex-issue-loop/internal/schema"
)

const CurrentVersion = schemaversion.Current

type SchemaVersionError struct {
	Kind    string
	Version int
}

func (e SchemaVersionError) Error() string {
	if e.Version == schemaversion.Previous {
		return fmt.Sprintf("%s schema migration required from version %d to %d; stop loops and run agent-loop migrate --apply", e.Kind, schemaversion.Previous, CurrentVersion)
	}
	return fmt.Sprintf("unsupported %s version %d; this binary supports version %d", e.Kind, e.Version, CurrentVersion)
}

type Supervisor struct {
	State               string     `json:"state"`
	PID                 int        `json:"pid,omitempty"`
	StartedAt           time.Time  `json:"started_at,omitempty"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Message             string     `json:"message,omitempty"`
	FailureKind         string     `json:"failure_kind,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
	RetryAfter          *time.Time `json:"retry_after,omitempty"`
}

type AnswerRecord struct {
	RequestID  string    `json:"request_id"`
	Question   string    `json:"question"`
	Answer     string    `json:"answer"`
	AnsweredAt time.Time `json:"answered_at"`
}

type WorkerSession struct {
	Backend string `json:"backend"`
	ID      string `json:"id"`
}

type WorkerIdentity struct {
	Backend        string `json:"backend"`
	RuntimeVersion string `json:"runtime_version,omitempty"`
	Provider       string `json:"provider,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	ResolvedModel  string `json:"resolved_model,omitempty"`
	Variant        string `json:"variant,omitempty"`
}

// WorkerGoal is a durable snapshot of the App Server thread-local Goal. The
// supervisor still owns Issue selection, queueing, leases, and process lifetime.
type WorkerGoal struct {
	ThreadID          string `json:"thread_id"`
	Objective         string `json:"objective"`
	Status            string `json:"status"`
	TokenBudget       *int64 `json:"token_budget,omitempty"`
	TimeBudgetSeconds int64  `json:"time_budget_seconds,omitempty"`
	TokensUsed        int64  `json:"tokens_used"`
	TimeUsedSeconds   int64  `json:"time_used_seconds"`
	InputTokens       int64  `json:"input_tokens,omitempty"`
	CachedInputTokens int64  `json:"cached_input_tokens,omitempty"`
	OutputTokens      int64  `json:"output_tokens,omitempty"`
	UpdatedAt         int64  `json:"updated_at,omitempty"`
}

// LeaseOwner fences lease mutations to one Issue run and one monotonically
// increasing generation. Run IDs alone are not sufficient because restored or
// retried state may reuse durable Issue records.
type LeaseOwner struct {
	RunID      string `json:"run_id"`
	Generation uint64 `json:"generation"`
}

// ResourceLease is the write-ahead reservation that must exist before a worker
// is started. A lease has no wall-clock expiry; ReservedAt is audit metadata.
type ResourceLease struct {
	Owner             LeaseOwner `json:"owner"`
	Slot              int        `json:"slot"`
	DeclaredResources []string   `json:"declared_resources"`
	ResolvedResources []string   `json:"resolved_resources"`
	ActualResources   []string   `json:"actual_resources,omitempty"`
	BaseSHA           string     `json:"base_sha,omitempty"`
	ReservedAt        time.Time  `json:"reserved_at"`
}

// ConflictAttempt is an append-only audit record for one autonomous conflict
// recovery worker invocation. A new base SHA starts a new per-base budget while
// preserving the earlier records.
type ConflictAttempt struct {
	Number        int       `json:"number"`
	BaseSHA       string    `json:"base_sha"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	ConflictFiles []string  `json:"conflict_files,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
}

type ConflictVerification struct {
	Command string `json:"command"`
	Result  string `json:"result"`
}

// ConflictRecovery contains everything required to resume an in-place merge
// after a supervisor restart. Prompt-only context is bounded by the preparer
// before it is persisted.
type ConflictRecovery struct {
	PullRequestURL  string                 `json:"pull_request_url"`
	RetryID         string                 `json:"retry_id,omitempty"`
	PreviousBaseSHA string                 `json:"previous_base_sha,omitempty"`
	TargetBaseSHA   string                 `json:"target_base_sha,omitempty"`
	OriginalHeadSHA string                 `json:"original_head_sha,omitempty"`
	ConflictFiles   []string               `json:"conflict_files,omitempty"`
	AllowedPaths    []string               `json:"allowed_paths,omitempty"`
	Attempts        int                    `json:"attempts"`
	BaseUpdates     int                    `json:"base_updates"`
	History         []ConflictAttempt      `json:"history,omitempty"`
	OriginalDiff    string                 `json:"original_diff,omitempty"`
	BaseCommits     string                 `json:"base_commits,omitempty"`
	ConflictContent string                 `json:"conflict_content,omitempty"`
	Verification    []ConflictVerification `json:"verification,omitempty"`
	LastReason      string                 `json:"last_reason,omitempty"`
	StartedAt       time.Time              `json:"started_at,omitempty"`
	UpdatedAt       time.Time              `json:"updated_at,omitempty"`
}

type Issue struct {
	Number            int               `json:"number"`
	Title             string            `json:"title"`
	Status            string            `json:"status"`
	RunID             string            `json:"run_id,omitempty"`
	LeaseGeneration   uint64            `json:"lease_generation,omitempty"`
	Lease             *ResourceLease    `json:"lease,omitempty"`
	DeclaredResources []string          `json:"declared_resources,omitempty"`
	ActualResources   []string          `json:"actual_resources,omitempty"`
	Branch            string            `json:"branch,omitempty"`
	Worktree          string            `json:"worktree,omitempty"`
	Attempts          int               `json:"attempts"`
	Continuations     int               `json:"continuations"`
	ExecutionProfile  string            `json:"execution_profile,omitempty"`
	SessionID         string            `json:"session_id,omitempty"`
	Session           *WorkerSession    `json:"session,omitempty"`
	WorkerIdentity    WorkerIdentity    `json:"worker_identity,omitempty"`
	Goal              *WorkerGoal       `json:"goal,omitempty"`
	WorkerPID         int               `json:"worker_pid,omitempty"`
	WorkerPGID        int               `json:"worker_pgid,omitempty"`
	PullRequestURL    string            `json:"pull_request_url,omitempty"`
	PullRequestMerged bool              `json:"pull_request_merged,omitempty"`
	GitHubSync        string            `json:"github_sync,omitempty"`
	FailureKind       string            `json:"failure_kind,omitempty"`
	LastError         string            `json:"last_error,omitempty"`
	RetryAfter        *time.Time        `json:"retry_after,omitempty"`
	Answers           []AnswerRecord    `json:"answers,omitempty"`
	ConflictRecovery  *ConflictRecovery `json:"conflict_recovery,omitempty"`
	UpdatedAt         time.Time         `json:"updated_at"`
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
	ResumeStatus  string     `json:"resume_status,omitempty"`
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

type Notification struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	IssueNumber int        `json:"issue_number,omitempty"`
	RequestID   string     `json:"request_id,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	NextAttempt time.Time  `json:"next_attempt,omitempty"`
	SentAt      *time.Time `json:"sent_at,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
}

type Snapshot struct {
	Version         int                      `json:"version"`
	RepoID          string                   `json:"repo_id"`
	RepoPath        string                   `json:"repo_path"`
	StateRevision   uint64                   `json:"state_revision"`
	Supervisor      Supervisor               `json:"supervisor"`
	Issues          map[string]*Issue        `json:"issues"`
	PendingRequests map[string]*Request      `json:"pending_requests"`
	Notifications   map[string]*Notification `json:"notifications,omitempty"`
	Recovery        *Recovery                `json:"recovery,omitempty"`
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
	Dir                  string
	RepoID               string
	RepoPath             string
	Secrets              []string
	EventRetention       retention.Policy
	NotificationsEnabled bool
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
	if snapshot.Recovery != nil && snapshot.Recovery.Status == "blocked" {
		return Snapshot{}, fmt.Errorf("durable state is recovery-blocked: %s (backup: %s)", snapshot.Recovery.Reason, snapshot.Recovery.BackupDir)
	}
	if err := s.rotateEventsUnlocked(snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("rotate event log: %w", err)
	}
	if err := mutate(&snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := validateResourceLeases(snapshot); err != nil {
		return Snapshot{}, err
	}
	snapshot.StateRevision++
	now := time.Now().UTC()
	if s.NotificationsEnabled {
		enqueueAttention(&snapshot, eventType, issueNumber, runID, now)
	}
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

func enqueueAttention(snapshot *Snapshot, eventType string, issueNumber int, runID string, now time.Time) {
	if snapshot.Notifications == nil {
		snapshot.Notifications = map[string]*Notification{}
	}
	add := func(id, kind, requestID string) {
		if id == "" || snapshot.Notifications[id] != nil {
			return
		}
		snapshot.Notifications[id] = &Notification{
			ID: id, Kind: kind, IssueNumber: issueNumber, RequestID: requestID, RunID: runID,
			Status: "pending", CreatedAt: now, NextAttempt: now,
		}
	}
	switch eventType {
	case "input_requested":
		for _, request := range snapshot.PendingRequests {
			if request != nil && request.IssueNumber == issueNumber && request.Status == "pending" {
				add("needs_input:"+request.ID, "needs_input", request.ID)
			}
		}
	case "issue_blocked":
		id := fmt.Sprintf("issue_blocked:%d:%s", issueNumber, runID)
		add(id, "issue_blocked", "")
	case "supervisor_blocked":
		digest := sha256.Sum256([]byte(snapshot.Supervisor.Message))
		add(fmt.Sprintf("supervisor_blocked:%x", digest[:8]), "supervisor_blocked", "")
	}
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
		f.Close()
		return nil, fmt.Errorf("another supervisor holds %s: %w", path, err)
	}
	return f, nil
}

func (s Store) ensureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return err
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
	for _, issue := range s.Issues {
		if issue != nil && issue.Status == "blocked" {
			return "blocked", true
		}
	}
	if s.Supervisor.State == "blocked" || s.Supervisor.State == "stopped" {
		return s.Supervisor.State, true
	}
	if untilIdle && s.Supervisor.State == "polling" {
		for _, issue := range s.Issues {
			if issue.GitHubSync != "" {
				return "", false
			}
			if issue.Status == "claiming" || issue.Status == "running" || issue.Status == "claimed" || issue.Status == "resume_pending" || issue.Status == "retry_wait" || issue.Status == "awaiting_checks" || issue.Status == "awaiting_merge" || issue.Status == "resolving_conflict" {
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

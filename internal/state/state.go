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
	"strings"
	"syscall"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/capability"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/fsutil"
	"github.com/ishii1648/codex-issue-loop/internal/publication"
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
	State               SupervisorState `json:"state"`
	PID                 int             `json:"pid,omitempty"`
	StartedAt           time.Time       `json:"started_at,omitempty"`
	UpdatedAt           time.Time       `json:"updated_at"`
	Message             string          `json:"message,omitempty"`
	FailureKind         string          `json:"failure_kind,omitempty"`
	ConsecutiveFailures int             `json:"consecutive_failures,omitempty"`
	RetryAfter          *time.Time      `json:"retry_after,omitempty"`
	RateLimit           *RateLimit      `json:"rate_limit,omitempty"`
}

type RateLimit struct {
	Resource             string    `json:"resource"`
	ObservedResetAt      time.Time `json:"observed_reset_at"`
	CooldownSource       string    `json:"cooldown_source"`
	SuppressedRetryCount uint64    `json:"suppressed_retry_count"`
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

// ResourceLeasePark is the durable continuation boundary for a resumable
// worker environment block. OriginalLease is an immutable copy of the released
// active lease; keeping it separate from Issue.Lease makes it visible to
// operators without participating in resource admission.
type ResourceLeasePark struct {
	ID            string                         `json:"id"`
	Kind          string                         `json:"kind,omitempty"`
	RequestID     string                         `json:"request_id,omitempty"`
	Status        issuedomain.ResourceParkStatus `json:"status"`
	OriginalLease ResourceLease                  `json:"original_lease"`
	ParkedAt      time.Time                      `json:"parked_at"`
	ResumedAt     time.Time                      `json:"resumed_at,omitempty"`
	ResumeOwner   *LeaseOwner                    `json:"resume_owner,omitempty"`
}

// ConflictAttempt is an append-only audit record for one autonomous conflict
// recovery worker invocation. A new base SHA starts a new per-base budget while
// preserving the earlier records.
type ConflictAttempt struct {
	Number        int                               `json:"number"`
	BaseSHA       string                            `json:"base_sha"`
	Status        issuedomain.ConflictAttemptStatus `json:"status"`
	Reason        string                            `json:"reason,omitempty"`
	ConflictFiles []string                          `json:"conflict_files,omitempty"`
	StartedAt     time.Time                         `json:"started_at"`
	FinishedAt    time.Time                         `json:"finished_at,omitempty"`
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

// BlockedCause records provenance needed by the operator resume command. Older
// or manually-created blocked records intentionally have no provenance and are
// therefore not resumable.
type BlockedCause struct {
	Origin    string    `json:"origin"`
	Kind      string    `json:"kind"`
	Resumable bool      `json:"resumable"`
	Reason    string    `json:"reason"`
	BlockedAt time.Time `json:"blocked_at"`
}

type EnvironmentResume struct {
	ID             string                              `json:"id"`
	Status         issuedomain.EnvironmentResumeStatus `json:"status"`
	ConfirmedAt    time.Time                           `json:"confirmed_at"`
	PreviousReason string                              `json:"previous_reason"`
	BaseSHA        string                              `json:"base_sha,omitempty"`
	CurrentBaseSHA string                              `json:"current_base_sha,omitempty"`
}

type PublicationRecoveryAttempt struct {
	Number     int                                          `json:"number"`
	Generation int                                          `json:"generation"`
	Status     issuedomain.PublicationRecoveryAttemptStatus `json:"status"`
	Reason     string                                       `json:"reason,omitempty"`
	StartedAt  time.Time                                    `json:"started_at"`
	FinishedAt time.Time                                    `json:"finished_at,omitempty"`
}

// PublicationRecovery records an operator-confirmed, publication-only retry.
// Worker attempt counters and history are intentionally separate and are never
// reset by this recovery path.
type PublicationRecovery struct {
	ID               string                                `json:"id"`
	Status           issuedomain.PublicationRecoveryStatus `json:"status"`
	Generation       int                                   `json:"generation"`
	Attempts         int                                   `json:"attempts"`
	MaxAttempts      int                                   `json:"max_attempts"`
	History          []PublicationRecoveryAttempt          `json:"history,omitempty"`
	ConfirmedAt      time.Time                             `json:"confirmed_at"`
	PreviousReason   string                                `json:"previous_reason"`
	ResultSHA256     string                                `json:"result_sha256"`
	Summary          string                                `json:"summary"`
	ExpectedHeadSHA  string                                `json:"expected_head_sha"`
	WorktreeSHA256   string                                `json:"worktree_sha256"`
	OriginalDirty    bool                                  `json:"original_dirty"`
	OriginalUnpushed bool                                  `json:"original_unpushed_commits"`
}

const (
	ChecksFailureOriginPullRequest  = "pull_request_lifecycle"
	ChecksFailurePhaseRequired      = "required_checks"
	ChecksFailureCodeRetryExhausted = "checks_retry_exhausted"
)

// PullRequestChecksFailure is immutable provenance for the Pull Request head
// that exhausted the normal worker retry budget. Recovery compares a later
// authoritative head against this record instead of the mutable observed head.
type PullRequestChecksFailure struct {
	Origin            string    `json:"origin"`
	Phase             string    `json:"phase"`
	Code              string    `json:"code"`
	Recoverable       bool      `json:"recoverable"`
	PullRequestURL    string    `json:"pull_request_url"`
	PullRequestNumber int       `json:"pull_request_number"`
	Branch            string    `json:"branch"`
	HeadSHA           string    `json:"head_sha"`
	ChecksStatus      string    `json:"checks_status"`
	RetryExhausted    bool      `json:"retry_exhausted"`
	FailedAt          time.Time `json:"failed_at"`
}

// PullRequestChecksRecovery records one operator-confirmed attempt to return
// an externally repaired head to the existing Pull Request lifecycle. Worker
// attempts, continuations, run history, and leases are deliberately separate.
type PullRequestChecksRecovery struct {
	ID             string                                      `json:"id"`
	Status         issuedomain.PullRequestChecksRecoveryStatus `json:"status"`
	Generation     int                                         `json:"generation"`
	ConfirmedAt    time.Time                                   `json:"confirmed_at"`
	PreviousReason string                                      `json:"previous_reason"`
	OldHeadSHA     string                                      `json:"old_head_sha"`
	NewHeadSHA     string                                      `json:"new_head_sha"`
	ChecksStatus   string                                      `json:"checks_status"`
}

// WorkerWorkspace is immutable provenance captured before the first worker
// spawn. Continuations must reproduce every field before a backend is invoked.
type WorkerWorkspace struct {
	Path         string    `json:"path"`
	Branch       string    `json:"branch"`
	RepoID       string    `json:"repo_id"`
	Repository   string    `json:"repository"`
	RepositoryID int64     `json:"repository_id,omitempty"`
	GitCommonDir string    `json:"git_common_dir"`
	MainCheckout string    `json:"main_checkout"`
	CapturedAt   time.Time `json:"captured_at"`
}

// AnsweredWorkspaceRecovery is the audit fence for the one legacy boundary
// where a needs-input continuation reacquired its lease before workspace
// provenance was introduced, then failed solely because Workspace was absent.
// It is deliberately separate from EnvironmentResume.
type AnsweredWorkspaceRecovery struct {
	ID                   string                                      `json:"id"`
	Status               issuedomain.AnsweredWorkspaceRecoveryStatus `json:"status"`
	ConfirmedAt          time.Time                                   `json:"confirmed_at"`
	OperatorConfirmed    bool                                        `json:"operator_confirmed"`
	OldProvenanceMissing bool                                        `json:"old_provenance_missing"`
	RequestID            string                                      `json:"request_id"`
	ResourceParkID       string                                      `json:"resource_park_id"`
	AnswerSHA256         string                                      `json:"answer_sha256"`
	HeadSHA              string                                      `json:"head_sha"`
	WorktreeSHA256       string                                      `json:"worktree_sha256"`
	ExpectedWorkspace    WorkerWorkspace                             `json:"expected_workspace"`
	ActualWorkspace      WorkerWorkspace                             `json:"actual_workspace"`
	ValidatorChecks      map[string]bool                             `json:"validator_checks"`
	OldOwner             LeaseOwner                                  `json:"old_owner"`
	NewOwner             LeaseOwner                                  `json:"new_owner"`
}

// WorkspaceProvenanceRecovery records an operator-confirmed, validation-only
// backfill of immutable workspace identity for a stopped legacy terminal
// record. It deliberately does not authorize a lifecycle transition, acquire
// a lease, or mutate GitHub state.
type WorkspaceProvenanceRecovery struct {
	ID                   string                                        `json:"id"`
	Status               issuedomain.WorkspaceProvenanceRecoveryStatus `json:"status"`
	ConfirmedAt          time.Time                                     `json:"confirmed_at"`
	OperatorConfirmed    bool                                          `json:"operator_confirmed"`
	OldProvenanceMissing bool                                          `json:"old_provenance_missing"`
	PreviousStatus       issuedomain.Status                            `json:"previous_status"`
	RunID                string                                        `json:"run_id"`
	HeadSHA              string                                        `json:"head_sha"`
	WorktreeSHA256       string                                        `json:"worktree_sha256"`
	ExpectedWorkspace    WorkerWorkspace                               `json:"expected_workspace"`
	ActualWorkspace      WorkerWorkspace                               `json:"actual_workspace"`
	ValidatorChecks      map[string]bool                               `json:"validator_checks"`
}

// Matches reports whether immutable saved provenance identifies the validated
// launch target. CapturedAt is audit metadata and is deliberately not part of
// the identity comparison.
func (w WorkerWorkspace) Matches(path, branch, repoID, repository string, repositoryID int64, gitCommonDir, mainCheckout string) bool {
	return w.Path == path && w.Branch == branch && w.RepoID == repoID &&
		w.Repository == repository && w.RepositoryID == repositoryID &&
		w.GitCommonDir == gitCommonDir && w.MainCheckout == mainCheckout
}

// MergedPullRequestAdoption records an operator-confirmed association between
// a terminal Issue and the single merged Pull Request for its saved branch.
// It exists only for publication that happened outside the supervisor after a
// worker stopped, and is never used to infer or adopt an open Pull Request.
type MergedPullRequestAdoption struct {
	ID                string                                      `json:"id"`
	Status            issuedomain.MergedPullRequestAdoptionStatus `json:"status"`
	Generation        int                                         `json:"generation"`
	ConfirmedAt       time.Time                                   `json:"confirmed_at"`
	AdoptedAt         time.Time                                   `json:"adopted_at"`
	PreviousStatus    issuedomain.Status                          `json:"previous_status"`
	PreviousReason    string                                      `json:"previous_reason"`
	PullRequestURL    string                                      `json:"pull_request_url"`
	PullRequestNumber int                                         `json:"pull_request_number"`
	Branch            string                                      `json:"branch"`
	HeadSHA           string                                      `json:"head_sha"`
	MergeSHA          string                                      `json:"merge_sha"`
	BaseBranch        string                                      `json:"base_branch"`
}

type Issue struct {
	Number                    int                          `json:"number"`
	Title                     string                       `json:"title"`
	Status                    issuedomain.Status           `json:"status"`
	RunID                     string                       `json:"run_id,omitempty"`
	LeaseGeneration           uint64                       `json:"lease_generation,omitempty"`
	Lease                     *ResourceLease               `json:"lease,omitempty"`
	ResourcePark              *ResourceLeasePark           `json:"resource_park,omitempty"`
	DeclaredResources         []string                     `json:"declared_resources,omitempty"`
	ActualResources           []string                     `json:"actual_resources,omitempty"`
	PublicationAudit          *publication.Audit           `json:"publication_audit,omitempty"`
	Branch                    string                       `json:"branch,omitempty"`
	Worktree                  string                       `json:"worktree,omitempty"`
	Workspace                 *WorkerWorkspace             `json:"workspace,omitempty"`
	Attempts                  int                          `json:"attempts"`
	Continuations             int                          `json:"continuations"`
	ExecutionProfile          string                       `json:"execution_profile,omitempty"`
	CapabilityRequirements    *capability.Requirements     `json:"capability_requirements,omitempty"`
	WorkerCapabilities        *capability.Provider         `json:"worker_capabilities,omitempty"`
	SessionID                 string                       `json:"session_id,omitempty"`
	Session                   *WorkerSession               `json:"session,omitempty"`
	WorkerIdentity            WorkerIdentity               `json:"worker_identity,omitempty"`
	WorkerPID                 int                          `json:"worker_pid,omitempty"`
	WorkerPGID                int                          `json:"worker_pgid,omitempty"`
	PullRequestURL            string                       `json:"pull_request_url,omitempty"`
	PullRequestNumber         int                          `json:"pull_request_number,omitempty"`
	HeadSHA                   string                       `json:"head_sha,omitempty"`
	PullRequestMerged         bool                         `json:"pull_request_merged,omitempty"`
	GitHubSync                issuedomain.GitHubSync       `json:"github_sync,omitempty"`
	FailureKind               string                       `json:"failure_kind,omitempty"`
	LastError                 string                       `json:"last_error,omitempty"`
	RetryAfter                *time.Time                   `json:"retry_after,omitempty"`
	Answers                   []AnswerRecord               `json:"answers,omitempty"`
	ConflictRecovery          *ConflictRecovery            `json:"conflict_recovery,omitempty"`
	BlockedCause              *BlockedCause                `json:"blocked_cause,omitempty"`
	EnvironmentResume         *EnvironmentResume           `json:"environment_resume,omitempty"`
	AnsweredWorkspaceRecovery *AnsweredWorkspaceRecovery   `json:"answered_workspace_recovery,omitempty"`
	WorkspaceRecovery         *WorkspaceProvenanceRecovery `json:"workspace_provenance_recovery,omitempty"`

	PublicationFailure *publication.FailureProvenance `json:"publication_failure,omitempty"`

	PublicationRecovery *PublicationRecovery `json:"publication_recovery,omitempty"`

	PullRequestChecksFailure  *PullRequestChecksFailure  `json:"pull_request_checks_failure,omitempty"`
	PullRequestChecksRecovery *PullRequestChecksRecovery `json:"pull_request_checks_recovery,omitempty"`
	MergedPullRequestAdoption *MergedPullRequestAdoption `json:"merged_pull_request_adoption,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Request struct {
	ID             string                    `json:"id"`
	IssueNumber    int                       `json:"issue_number"`
	Question       string                    `json:"question"`
	Reason         string                    `json:"reason,omitempty"`
	Recommended    string                    `json:"recommended_option,omitempty"`
	Options        []Option                  `json:"options,omitempty"`
	AllowFreeText  bool                      `json:"allow_free_text"`
	ResumeStatus   issuedomain.Status        `json:"resume_status,omitempty"`
	RunID          string                    `json:"run_id,omitempty"`
	ResourceParkID string                    `json:"resource_park_id,omitempty"`
	ReleasedOwner  *LeaseOwner               `json:"released_owner,omitempty"`
	Status         issuedomain.RequestStatus `json:"status"`
	Answer         string                    `json:"answer,omitempty"`
	CreatedAt      time.Time                 `json:"created_at"`
	AnsweredAt     *time.Time                `json:"answered_at,omitempty"`
}

type Recovery struct {
	Status     RecoveryState `json:"status"`
	Reason     string        `json:"reason"`
	BackupDir  string        `json:"backup_dir"`
	DetectedAt time.Time     `json:"detected_at"`
}

type Snapshot struct {
	Version                 int                 `json:"version"`
	SemanticContractVersion int                 `json:"semantic_contract_version"`
	RepoID                  string              `json:"repo_id"`
	RepoPath                string              `json:"repo_path"`
	StateRevision           uint64              `json:"state_revision"`
	Supervisor              Supervisor          `json:"supervisor"`
	Issues                  map[string]*Issue   `json:"issues"`
	PendingRequests         map[string]*Request `json:"pending_requests"`
	Recovery                *Recovery           `json:"recovery,omitempty"`
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
		if request.Status == issuedomain.RequestStatusPending {
			requests = append(requests, id)
		}
	}
	if len(requests) > 0 {
		sort.Strings(requests)
		return "needs_input", true
	}
	for _, issue := range s.Issues {
		if issue != nil && issue.Status == issuedomain.StatusBlocked {
			return "blocked", true
		}
	}
	for _, issue := range s.Issues {
		if issue != nil && issue.Status == issuedomain.StatusAnswerClaimWaiting {
			return "answer_claim_waiting", true
		}
	}
	for _, issue := range s.Issues {
		if RecoverablePullRequestChecksFailure(issue) && issue.Lease != nil {
			return "recoverable_checks_failure", true
		}
	}
	if s.Supervisor.State == SupervisorStateBlocked || s.Supervisor.State == SupervisorStateStopped {
		return string(s.Supervisor.State), true
	}
	if untilIdle && s.Supervisor.State == SupervisorStatePolling {
		for _, issue := range s.Issues {
			if issue.GitHubSync.Pending() {
				return "", false
			}
			if issue.Status.PreventsIdle() {
				return "", false
			}
		}
		return "idle", true
	}
	return "", false
}

func RecoverablePullRequestChecksFailure(issue *Issue) bool {
	if issue == nil {
		return false
	}
	value := issue.PullRequestChecksFailure
	return issue.Status == issuedomain.StatusFailed && issue.FailureKind == "issue" && !issue.PullRequestMerged && value != nil && value.Recoverable && value.RetryExhausted &&
		value.Origin == ChecksFailureOriginPullRequest && value.Phase == ChecksFailurePhaseRequired &&
		value.Code == ChecksFailureCodeRetryExhausted && value.ChecksStatus == "failure" &&
		value.PullRequestURL != "" && value.PullRequestURL == issue.PullRequestURL &&
		value.PullRequestNumber > 0 && value.PullRequestNumber == issue.PullRequestNumber &&
		value.Branch != "" && value.Branch == issue.Branch && value.HeadSHA != ""
}

func NewID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

func ValidID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

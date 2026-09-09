package statecontract

import (
	"encoding/json"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
	"time"
)

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

type AnswerProvenance struct {
	Source        string    `json:"source"`
	CommentID     int64     `json:"comment_id,omitempty"`
	Actor         string    `json:"actor,omitempty"`
	Permission    string    `json:"permission,omitempty"`
	RequestID     string    `json:"request_id"`
	IssueNumber   int       `json:"issue_number"`
	RunID         string    `json:"run_id,omitempty"`
	BodySHA256    string    `json:"body_sha256"`
	CommentedAt   time.Time `json:"commented_at,omitempty"`
	CommentEdited time.Time `json:"comment_edited_at,omitempty"`
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

type Cancellation struct {
	Source                 string             `json:"source"`
	GitHubStateReason      string             `json:"github_state_reason,omitempty"`
	PreviousStatus         issuedomain.Status `json:"previous_status"`
	ExecutionReleaseResult string             `json:"execution_release_result"`
	CanceledAt             time.Time          `json:"canceled_at"`
}

// ExecutionIdentity fences mutations to one Issue run and one monotonically
// increasing generation. Run IDs alone are not sufficient after a restart.
type ExecutionIdentity struct {
	RunID      string `json:"run_id"`
	Generation uint64 `json:"generation"`
}

// ActiveExecution is the repository's only execution authority. Waiting,
// suspended, and Pull Request lifecycle records never occupy it.
type ActiveExecution struct {
	IssueNumber int       `json:"issue_number"`
	RunID       string    `json:"run_id"`
	Generation  uint64    `json:"generation"`
	BaseSHA     string    `json:"base_sha,omitempty"`
	StartedAt   time.Time `json:"started_at"`
}

type EffectIntent struct {
	ID          string                 `json:"id"`
	IssueNumber int                    `json:"issue_number"`
	RunID       string                 `json:"run_id"`
	Kind        issuedomain.EffectKind `json:"kind"`
	CreatedAt   time.Time              `json:"created_at"`
}

// ContinuationCheckpoint is the durable boundary between released execution
// capacity and a later, explicitly validated continuation.
type ContinuationCheckpoint struct {
	ID                string                        `json:"id"`
	Kind              string                        `json:"kind,omitempty"`
	RequestID         string                        `json:"request_id,omitempty"`
	CreatedAt         time.Time                     `json:"created_at"`
	RunID             string                        `json:"run_id"`
	Generation        uint64                        `json:"generation"`
	BaseSHA           string                        `json:"base_sha,omitempty"`
	Workspace         *WorkerWorkspace              `json:"workspace,omitempty"`
	Session           *WorkerSession                `json:"session,omitempty"`
	HeadSHA           string                        `json:"head_sha,omitempty"`
	WorktreeSHA256    string                        `json:"worktree_sha256,omitempty"`
	PullRequestURL    string                        `json:"pull_request_url,omitempty"`
	PullRequestNumber int                           `json:"pull_request_number,omitempty"`
	Stage             issuedomain.ContinuationStage `json:"stage,omitempty"`
	ResultSHA256      string                        `json:"result_sha256,omitempty"`
	Summary           string                        `json:"summary,omitempty"`
	Evidence          *ContinuationEvidence         `json:"evidence,omitempty"`
}

// ContinuationEvidence records the immutable observation that caused a stage
// to stop without introducing a scenario-specific recovery aggregate.
type ContinuationEvidence struct {
	Origin     string    `json:"origin"`
	Phase      string    `json:"phase"`
	Code       string    `json:"code"`
	Status     string    `json:"status"`
	ObservedAt time.Time `json:"observed_at"`
}

type Suspension struct {
	ID              string                         `json:"id"`
	Origin          string                         `json:"origin,omitempty"`
	Status          issuedomain.SuspensionStatus   `json:"status"`
	ReasonCode      string                         `json:"reason_code"`
	Recoverability  issuedomain.Recoverability     `json:"recoverability"`
	Reason          string                         `json:"reason"`
	MissingEvidence []string                       `json:"missing_evidence,omitempty"`
	AllowedActions  []issuedomain.ResolutionAction `json:"allowed_actions"`
	CheckpointID    string                         `json:"checkpoint_id,omitempty"`
	SuspendedAt     time.Time                      `json:"suspended_at"`
	ResolvedAt      time.Time                      `json:"resolved_at,omitempty"`
	Resolution      issuedomain.ResolutionAction   `json:"resolution,omitempty"`
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

// CapturedAt is audit metadata and is deliberately not part of workspace
// identity comparison.
func (w WorkerWorkspace) Matches(path, branch, repoID, repository string, repositoryID int64, gitCommonDir, mainCheckout string) bool {
	return w.Path == path && w.Branch == branch && w.RepoID == repoID &&
		w.Repository == repository && w.RepositoryID == repositoryID &&
		w.GitCommonDir == gitCommonDir && w.MainCheckout == mainCheckout
}

type Issue struct {
	Number             int                             `json:"number"`
	Title              string                          `json:"title"`
	AuthorVerification *queuedomain.AuthorVerification `json:"author_verification,omitempty"`
	Status             issuedomain.Status              `json:"status"`
	RunID              string                          `json:"run_id,omitempty"`
	Generation         uint64                          `json:"generation,omitempty"`
	LaunchSource       issuedomain.Status              `json:"launch_source,omitempty"`
	Continuation       *ContinuationCheckpoint         `json:"continuation,omitempty"`
	Suspension         *Suspension                     `json:"suspension,omitempty"`
	PublicationAudit   *publication.Audit              `json:"publication_audit,omitempty"`
	Branch             string                          `json:"branch,omitempty"`
	Worktree           string                          `json:"worktree,omitempty"`
	Workspace          *WorkerWorkspace                `json:"workspace,omitempty"`
	Attempts           int                             `json:"attempts"`
	Continuations      int                             `json:"continuations"`
	ExecutionProfile   string                          `json:"execution_profile,omitempty"`
	SessionID          string                          `json:"session_id,omitempty"`
	Session            *WorkerSession                  `json:"session,omitempty"`
	WorkerIdentity     WorkerIdentity                  `json:"worker_identity,omitempty"`
	WorkerPID          int                             `json:"worker_pid,omitempty"`
	WorkerPGID         int                             `json:"worker_pgid,omitempty"`
	PullRequestURL     string                          `json:"pull_request_url,omitempty"`
	PullRequestNumber  int                             `json:"pull_request_number,omitempty"`
	HeadSHA            string                          `json:"head_sha,omitempty"`
	ReviewDecision     string                          `json:"review_decision,omitempty"`
	PullRequestMerged  bool                            `json:"pull_request_merged,omitempty"`
	GitHubStateReason  string                          `json:"github_state_reason,omitempty"`
	Cancellation       *Cancellation                   `json:"cancellation,omitempty"`
	FailureKind        string                          `json:"failure_kind,omitempty"`
	LastError          string                          `json:"last_error,omitempty"`
	RetryAfter         *time.Time                      `json:"retry_after,omitempty"`
	Answers            []AnswerRecord                  `json:"answers,omitempty"`
	ConflictRecovery   *ConflictRecovery               `json:"conflict_recovery,omitempty"`
	UpdatedAt          time.Time                       `json:"updated_at"`
}

type QuarantineRecord struct {
	IssueNumber    int                `json:"issue_number"`
	RunID          string             `json:"run_id,omitempty"`
	Generation     uint64             `json:"generation,omitempty"`
	RejectedStatus issuedomain.Status `json:"rejected_status,omitempty"`
	ReasonCode     string             `json:"reason_code"`
	Reason         string             `json:"reason"`
	QuarantinedAt  time.Time          `json:"quarantined_at"`
	LastValid      *Issue             `json:"last_valid,omitempty"`
	Requests       []*Request         `json:"requests,omitempty"`
}

type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type Request struct {
	ID                string                    `json:"id"`
	IssueNumber       int                       `json:"issue_number"`
	Question          string                    `json:"question"`
	Reason            string                    `json:"reason,omitempty"`
	Recommended       string                    `json:"recommended_option,omitempty"`
	Options           []Option                  `json:"options,omitempty"`
	AllowFreeText     bool                      `json:"allow_free_text"`
	ResumeStatus      issuedomain.Status        `json:"resume_status,omitempty"`
	RunID             string                    `json:"run_id,omitempty"`
	CheckpointID      string                    `json:"checkpoint_id,omitempty"`
	ReleasedExecution *ExecutionIdentity        `json:"released_execution,omitempty"`
	Status            issuedomain.RequestStatus `json:"status"`
	Answer            string                    `json:"answer,omitempty"`
	AnswerProvenance  *AnswerProvenance         `json:"answer_provenance,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	AnsweredAt        *time.Time                `json:"answered_at,omitempty"`
}

type Recovery struct {
	Status     RecoveryState `json:"status"`
	Reason     string        `json:"reason"`
	BackupDir  string        `json:"backup_dir"`
	DetectedAt time.Time     `json:"detected_at"`
}

type Snapshot struct {
	Version int `json:"version"`
	// Legacy envelope fields must be absent from every v6 runtime snapshot.
	SemanticContractVersion  int                                        `json:"semantic_contract_version,omitempty"`
	IssueLifecycleAPIVersion string                                     `json:"issue_lifecycle_api_version,omitempty"`
	RepoID                   string                                     `json:"repo_id"`
	RepoPath                 string                                     `json:"repo_path"`
	StateRevision            uint64                                     `json:"state_revision"`
	Supervisor               Supervisor                                 `json:"supervisor"`
	ActiveExecution          *ActiveExecution                           `json:"active_execution,omitempty"`
	Issues                   map[string]*Issue                          `json:"issues"`
	PendingEffects           map[string]*EffectIntent                   `json:"pending_effects"`
	QuarantinedIssues        map[string]*QuarantineRecord               `json:"quarantined_issues"`
	IntakeVerifications      map[string]*queuedomain.AuthorVerification `json:"intake_verifications"`
	PendingRequests          map[string]*Request                        `json:"pending_requests"`
	Recovery                 *Recovery                                  `json:"recovery,omitempty"`
}

// UnmarshalJSON rejects scenario-specific recovery state when it is already
// labeled as v5 or later. Only the v4 migration decoder may interpret those
// fields; silently discarding them here could resume without their evidence.
func (snapshot *Snapshot) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Version                 int                                   `json:"version"`
		SemanticContractVersion int                                   `json:"semantic_contract_version"`
		Issues                  map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Version == CurrentVersion {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, key := range []string{"semantic_contract_version", "issue_lifecycle_api_version"} {
			if _, present := fields[key]; present {
				return SchemaVersionError{Kind: "mixed snapshot header", Version: envelope.Version}
			}
		}
	}
	if envelope.Version == CurrentVersion || envelope.Version == 5 && envelope.SemanticContractVersion == 4 {
		for number, issue := range envelope.Issues {
			for _, field := range []string{
				"lease", "resource_park", "execution_lease", "lease_generation", "continuation_checkpoint", "blocked_cause", "environment_resume", "answered_workspace_recovery",
				"workspace_provenance_recovery", "publication_failure", "publication_recovery",
				"pull_request_checks_failure", "pull_request_checks_recovery", "merged_pull_request_adoption",
			} {
				if _, exists := issue[field]; exists {
					return fmt.Errorf("Issue %s uses removed v5 field %q; migrate the original v4 input instead", number, field)
				}
			}
			var status string
			if err := json.Unmarshal(issue["status"], &status); err == nil && (status == "environment_resume_pending" || status == "publication_recovery_pending" || status == "pull_request_checks_recovery_pending") {
				return fmt.Errorf("Issue %s uses removed v5 status %q", number, status)
			}
			if _, exists := issue["github_sync"]; exists {
				return fmt.Errorf("Issue %s uses removed v5 field %q; effects belong to the repository aggregate", number, "github_sync")
			}
		}
	}
	type snapshotAlias Snapshot
	var decoded snapshotAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*snapshot = Snapshot(decoded)
	if snapshot.Issues == nil {
		snapshot.Issues = map[string]*Issue{}
	}
	if snapshot.QuarantinedIssues == nil {
		snapshot.QuarantinedIssues = map[string]*QuarantineRecord{}
	}
	if snapshot.IntakeVerifications == nil {
		snapshot.IntakeVerifications = map[string]*queuedomain.AuthorVerification{}
	}
	if snapshot.PendingRequests == nil {
		snapshot.PendingRequests = map[string]*Request{}
	}
	if snapshot.PendingEffects == nil {
		snapshot.PendingEffects = map[string]*EffectIntent{}
	}
	return nil
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

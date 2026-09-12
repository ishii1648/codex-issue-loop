package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/publication"
	queuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/queue"
	"github.com/ishii1648/codex-issue-loop/internal/domain/statecontract"
	"sort"
	"strconv"
	"strings"
	"time"
)

const CurrentVersion = statecontract.CurrentVersion
const ContinuationKindNeedsInput = "needs_input"

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
	// Legacy markers identify migration inputs; v6 rejects their presence.
	Version                  int                                        `json:"version"`
	SemanticContractVersion  int                                        `json:"semantic_contract_version,omitempty"`
	IssueLifecycleAPIVersion string                                     `json:"issue_lifecycle_api_version,omitempty"`
	RepoID                   string                                     `json:"repo_id"`
	RepoPath                 string                                     `json:"repo_path"`
	StateRevision            uint64                                     `json:"state_revision"`
	Supervisor               Supervisor                                 `json:"supervisor"`
	LastExecutionReleasedAt  time.Time                                  `json:"last_execution_released_at,omitzero"`
	ActiveExecution          *ActiveExecution                           `json:"active_execution,omitempty"`
	Issues                   map[string]*Issue                          `json:"issues"`
	PendingEffects           map[string]*EffectIntent                   `json:"pending_effects"`
	QuarantinedIssues        map[string]*QuarantineRecord               `json:"quarantined_issues"`
	IntakeVerifications      map[string]*queuedomain.AuthorVerification `json:"intake_verifications"`
	PendingRequests          map[string]*Request                        `json:"pending_requests"`
	Recovery                 *Recovery                                  `json:"recovery,omitempty"`
}

// UnmarshalJSON rejects removed recovery fields in the current contract;
// silently discarding them could resume without their evidence.
func (snapshot *Snapshot) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Version                 int                                   `json:"version"`
		SemanticContractVersion int                                   `json:"semantic_contract_version,omitempty"`
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
		if _, exists := fields["semantic_contract_version"]; exists {
			return SemanticContractVersionError{Version: envelope.SemanticContractVersion, Current: 0}
		}
		if raw, exists := fields["issue_lifecycle_api_version"]; exists {
			var version string
			if err := json.Unmarshal(raw, &version); err != nil {
				return err
			}
			return LifecycleAPIVersionError{Version: version, Current: ""}
		}
	}
	if envelope.Version == CurrentVersion {
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

func (s Snapshot) Attention(untilIdle, autoMerge bool) (string, bool) {
	requests := make([]string, 0)
	for _, request := range s.Requests() {
		if request.Status == issuedomain.RequestStatusPending {
			requests = append(requests, request.ID)
		}
	}
	if len(requests) > 0 {
		sort.Strings(requests)
		return "needs_input", true
	}
	if len(s.HumanNeeds(autoMerge)) > 0 {
		return "needs_human", true
	}
	for _, issue := range s.Issues {
		if issue != nil && issue.Status == issuedomain.StatusBlocked {
			return "blocked", true
		}
	}
	if s.Supervisor.State == SupervisorStateBlocked || s.Supervisor.State == SupervisorStateStopped {
		return string(s.Supervisor.State), true
	}
	if untilIdle && s.Supervisor.State == SupervisorStatePolling {
		if len(s.PendingEffects) > 0 {
			return "", false
		}
		for _, issue := range s.Issues {
			if issue.Status.PreventsIdle() {
				return "", false
			}
		}
		return "idle", true
	}
	return "", false
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

// Requests includes quarantined requests so execution recovery cannot hide a conversation.
func (s Snapshot) Requests() []*Request {
	requests := make([]*Request, 0, len(s.PendingRequests))
	for _, request := range s.PendingRequests {
		if request != nil {
			requests = append(requests, request)
		}
	}
	for _, record := range s.QuarantinedIssues {
		if record != nil {
			for _, request := range record.Requests {
				if request != nil {
					requests = append(requests, request)
				}
			}
		}
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ID < requests[j].ID })
	return requests
}

func (s Snapshot) Request(id string) (*Request, error) {
	var found *Request
	for _, request := range s.Requests() {
		if request.ID != id {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous request ID %s", id)
		}
		found = request
	}
	if found == nil {
		return nil, fmt.Errorf("unknown request ID %s", id)
	}
	return found, nil
}

func ValidateAnswerObservation(snapshot Snapshot, request *Request, provenance *AnswerProvenance) error {
	if request.Status != issuedomain.RequestStatusPending && request.Status != issuedomain.RequestStatusAnswered {
		return ConflictError{Message: "request is canceled"}
	}
	if provenance == nil {
		return nil
	}
	if provenance.Source != "github_issue_comment" || provenance.CommentID <= 0 || provenance.Actor == "" ||
		provenance.RequestID != request.ID || provenance.IssueNumber != request.IssueNumber || provenance.RunID != request.RunID ||
		!ValidSHA256(provenance.BodySHA256) || provenance.CommentedAt.IsZero() || provenance.CommentedAt.Before(request.CreatedAt) ||
		provenance.CommentEdited.Before(provenance.CommentedAt) {
		return ConflictError{Message: "comment observation does not match request"}
	}
	if request.Status == issuedomain.RequestStatusAnswered {
		old := request.AnswerProvenance
		if old == nil || old.CommentID != provenance.CommentID || old.BodySHA256 != provenance.BodySHA256 ||
			old.Actor != provenance.Actor || !old.CommentEdited.Equal(provenance.CommentEdited) {
			return ConflictError{Message: "request was answered by a different observation"}
		}
		return nil
	}
	key := strconv.Itoa(request.IssueNumber)
	run := ""
	if item := snapshot.Issues[key]; item != nil {
		run = item.RunID
	} else if item := snapshot.QuarantinedIssues[key]; item != nil {
		run = item.RunID
	} else {
		return ConflictError{Message: "request has no Issue"}
	}
	if run != request.RunID {
		return ConflictError{Message: "request belongs to a stale run"}
	}
	return nil
}

func LegacyWorkerLaunchSource(snapshot *Snapshot, issue *Issue) (issuedomain.Status, string, bool) {
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != issue.Number || active.RunID != issue.RunID ||
		active.Generation != issue.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return issuedomain.StatusUnset, "active execution does not match the Issue run and generation", false
	}

	checkpoint := issue.Continuation
	answeredEvidence := len(issue.Answers) > 0
	for _, request := range snapshot.PendingRequests {
		if request != nil && request.IssueNumber == issue.Number && request.Status == issuedomain.RequestStatusAnswered {
			answeredEvidence = true
		}
	}
	if checkpoint == nil {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence has no continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind == "" && checkpoint.RequestID == "" {
		if answeredEvidence {
			return issuedomain.StatusUnset, "answered evidence is not bound to the continuation checkpoint", false
		}
		return issuedomain.StatusRetryWait, "", true
	}
	if checkpoint.Kind != ContinuationKindNeedsInput || checkpoint.RequestID == "" || checkpoint.RunID != issue.RunID ||
		checkpoint.Generation == 0 || checkpoint.Generation >= issue.Generation || checkpoint.Generation+1 != issue.Generation {
		return issuedomain.StatusUnset, "needs-input continuation identity does not match the Issue run and generation", false
	}
	request := snapshot.PendingRequests[checkpoint.RequestID]
	if request == nil || request.ID != checkpoint.RequestID || request.IssueNumber != issue.Number ||
		request.CheckpointID != checkpoint.ID || request.RunID != issue.RunID || request.ReleasedExecution == nil ||
		request.ReleasedExecution.RunID != checkpoint.RunID || request.ReleasedExecution.Generation != checkpoint.Generation ||
		request.Status != issuedomain.RequestStatusAnswered ||
		strings.TrimSpace(request.Answer) == "" || request.AnsweredAt == nil || request.AnsweredAt.IsZero() {
		return issuedomain.StatusUnset, "answered request does not match the continuation identity", false
	}
	answerCount := 0
	for _, answer := range issue.Answers {
		if answer.RequestID != checkpoint.RequestID {
			continue
		}
		answerCount++
		if answer.Question != request.Question || answer.Answer != request.Answer || answer.AnsweredAt.IsZero() || !answer.AnsweredAt.Equal(*request.AnsweredAt) {
			return issuedomain.StatusUnset, "recorded answer does not match the answered request", false
		}
	}
	if answerCount != 1 {
		return issuedomain.StatusUnset, "answered request does not have exactly one matching answer record", false
	}
	return issuedomain.StatusResumePending, "", true
}

func validateExecutionState(snapshot Snapshot) error {
	var executing *Issue
	for _, issue := range snapshot.Issues {
		if issue == nil || !issue.Status.RequiresActiveExecution() {
			continue
		}
		if executing != nil {
			return fmt.Errorf("Issues #%d and #%d both claim the single active execution", executing.Number, issue.Number)
		}
		executing = issue
	}
	if executing == nil {
		if snapshot.ActiveExecution != nil {
			return fmt.Errorf("active execution has no executing Issue")
		}
		return nil
	}
	active := snapshot.ActiveExecution
	if active == nil || active.IssueNumber != executing.Number || active.RunID != executing.RunID ||
		active.Generation != executing.Generation || active.Generation == 0 || active.StartedAt.IsZero() {
		return fmt.Errorf("Issue #%d does not match repository active execution", executing.Number)
	}
	if executing.Status == issuedomain.StatusLaunching && executing.WorkerPID == 0 && executing.Continuation != nil &&
		executing.Continuation.Kind == ContinuationKindNeedsInput &&
		(executing.LaunchSource == issuedomain.StatusResumePending || executing.LaunchSource == issuedomain.StatusRetryWait) {
		source, reason, ok := LegacyWorkerLaunchSource(&snapshot, executing)
		if !ok || source != executing.LaunchSource {
			if reason == "" {
				reason = fmt.Sprintf("evidence authorizes %s instead of %s", source, executing.LaunchSource)
			}
			return fmt.Errorf("Issue #%d launch source is inconsistent with answered continuation evidence: %s", executing.Number, reason)
		}
	}
	return nil
}

func ValidSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

type ConflictError struct{ Message string }

func (e ConflictError) Error() string { return e.Message }

type SemanticContractVersionError struct {
	Version int
	Current int
}

type LifecycleAPIVersionError struct {
	Version string
	Current string
}

func (e LifecycleAPIVersionError) Error() string {
	return fmt.Sprintf("snapshot Issue lifecycle API version %q does not match %q", e.Version, e.Current)
}

func (e SemanticContractVersionError) Error() string {
	return fmt.Sprintf("snapshot semantic contract version %d does not match %d", e.Version, e.Current)
}

// Validate is the aggregate fail-closed boundary for every durable snapshot.
// Callers must run it before committing a snapshot and after completing any
// recovery, migration, or fixture reconstruction.
func (snapshot Snapshot) Validate() error {
	if err := snapshot.ValidateVersion(); err != nil {
		return err
	}
	return snapshot.validateContent()
}

func (snapshot Snapshot) ValidateVersion() error {
	if snapshot.Version != CurrentVersion {
		return SchemaVersionError{Kind: "state", Version: snapshot.Version}
	}
	if snapshot.SemanticContractVersion != 0 {
		return SemanticContractVersionError{Version: snapshot.SemanticContractVersion, Current: 0}
	}
	if snapshot.IssueLifecycleAPIVersion != "" {
		return LifecycleAPIVersionError{Version: snapshot.IssueLifecycleAPIVersion, Current: ""}
	}
	return nil
}

// ValidateLegacy is only for the existing v4-to-v5 migration, never runtime loading or commit.
func (snapshot Snapshot) ValidateLegacy() error {
	if snapshot.Version != 5 {
		return SchemaVersionError{Kind: "legacy state", Version: snapshot.Version}
	}
	if snapshot.SemanticContractVersion != 4 {
		return SemanticContractVersionError{Version: snapshot.SemanticContractVersion, Current: 4}
	}
	if snapshot.IssueLifecycleAPIVersion != "2.0" && snapshot.IssueLifecycleAPIVersion != "2.1" {
		return LifecycleAPIVersionError{Version: snapshot.IssueLifecycleAPIVersion, Current: "2.1"}
	}
	return snapshot.validateContent()
}

func (snapshot Snapshot) validateContent() error {
	if err := snapshot.Supervisor.State.Validate(); err != nil {
		return err
	}
	if snapshot.Recovery != nil {
		if err := snapshot.Recovery.Status.Validate(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(snapshot.RepoID) == "" || strings.TrimSpace(snapshot.RepoPath) == "" {
		return fmt.Errorf("snapshot repository identity is incomplete")
	}
	if snapshot.Issues == nil || snapshot.PendingEffects == nil || snapshot.QuarantinedIssues == nil || snapshot.IntakeVerifications == nil || snapshot.PendingRequests == nil {
		return fmt.Errorf("snapshot aggregate maps must be initialized")
	}
	if err := validateExecutionState(snapshot); err != nil {
		return err
	}
	if err := validatePersistenceSemanticContract(snapshot); err != nil {
		return err
	}
	for key, issue := range snapshot.Issues {
		if issue == nil {
			return fmt.Errorf("Issue entry %q is null", key)
		}
		if issue.Number < 1 || key != strconv.Itoa(issue.Number) {
			return fmt.Errorf("Issue entry %q does not match Issue number %d", key, issue.Number)
		}
		if err := validateIssueAggregate(issue); err != nil {
			return fmt.Errorf("Issue #%d: %w", issue.Number, err)
		}
	}
	for key, record := range snapshot.QuarantinedIssues {
		if record == nil || record.IssueNumber < 1 || key != strconv.Itoa(record.IssueNumber) {
			return fmt.Errorf("quarantined Issue entry %q has invalid identity", key)
		}
		if snapshot.Issues[key] != nil {
			return fmt.Errorf("Issue #%d is both managed and quarantined", record.IssueNumber)
		}
		if record.ReasonCode == "" || strings.TrimSpace(record.Reason) == "" || record.QuarantinedAt.IsZero() {
			return fmt.Errorf("quarantined Issue #%d has incomplete evidence", record.IssueNumber)
		}
	}
	for key, effect := range snapshot.PendingEffects {
		if err := validateEffectIntent(snapshot, key, effect); err != nil {
			return err
		}
	}
	for key, verification := range snapshot.IntakeVerifications {
		if _, err := strconv.Atoi(key); err != nil || verification == nil || verification.Reason == "" || verification.VerifiedAt.IsZero() {
			return fmt.Errorf("Issue author verification entry %q is invalid", key)
		}
	}
	for id, request := range snapshot.PendingRequests {
		if request == nil {
			return fmt.Errorf("request entry %q is null", id)
		}
		if err := ValidateRequestAggregate(snapshot, id, request); err != nil {
			return err
		}
	}
	return nil
}

func validatePersistenceSemanticContract(snapshot Snapshot) error {
	violations := semanticViolations(snapshot)
	remaining := make([]SemanticViolation, 0, len(violations))
	for _, violation := range violations {
		issue := snapshot.Issues[strconv.Itoa(violation.IssueNumber)]
		if violation.Code == SemanticCodeWorkspaceProvenanceMissing && issue != nil &&
			(issue.Status == issuedomain.StatusBlocked || issue.Status == issuedomain.StatusFailed) &&
			issue.WorkerPID == 0 && issue.WorkerPGID == 0 {
			continue
		}
		remaining = append(remaining, violation)
	}
	if len(remaining) > 0 {
		return SemanticCompatibilityError{Violations: remaining}
	}
	return nil
}

func validateIssueAggregate(issue *Issue) error {
	if err := issue.Status.Validate(); err != nil {
		return err
	}
	if issue.Attempts < 0 || issue.Continuations < 0 {
		return fmt.Errorf("attempt and continuation counters must not be negative")
	}
	if issue.WorkerPID < 0 || issue.WorkerPGID < 0 || (issue.WorkerPID == 0) != (issue.WorkerPGID == 0) {
		return fmt.Errorf("worker PID and PGID must be present or absent together")
	}
	if issue.WorkerPID > 0 && (issue.RunID == "" || !issue.Status.RequiresActiveExecution()) {
		return fmt.Errorf("worker process is not owned by an executing lifecycle")
	}
	if issue.Status == issuedomain.StatusRunning && issue.WorkerPID == 0 {
		return fmt.Errorf("running worker process identity is missing")
	}
	if issue.Status == issuedomain.StatusLaunching {
		switch issue.LaunchSource {
		case issuedomain.StatusClaimed, issuedomain.StatusResumePending, issuedomain.StatusRetryWait, issuedomain.StatusResolvingConflict:
		default:
			return fmt.Errorf("launch source %q is invalid", issue.LaunchSource)
		}
	} else if issue.LaunchSource != issuedomain.StatusUnset {
		return fmt.Errorf("launch source is retained outside launching state")
	}
	if issue.Session != nil {
		if strings.TrimSpace(issue.Session.Backend) == "" || strings.TrimSpace(issue.Session.ID) == "" || issue.SessionID != issue.Session.ID {
			return fmt.Errorf("worker session identity is incomplete or inconsistent")
		}
	} else if issue.SessionID != "" {
		return fmt.Errorf("legacy session ID is missing its typed session")
	}
	if issue.PullRequestNumber < 0 {
		return fmt.Errorf("Pull Request number must not be negative")
	}
	if issue.PullRequestNumber > 0 && issue.PullRequestURL == "" {
		return fmt.Errorf("Pull Request number has no URL")
	}
	if issue.PullRequestMerged && (issue.PullRequestNumber == 0 || issue.PullRequestURL == "" || issue.HeadSHA == "") {
		return fmt.Errorf("merged Pull Request identity is incomplete")
	}
	if issue.Status == issuedomain.StatusCanceled {
		if issue.Cancellation == nil || issue.Cancellation.Source == "" || issue.Cancellation.PreviousStatus != issuedomain.StatusBlocked && issue.Cancellation.PreviousStatus != issuedomain.StatusFailed ||
			(issue.Cancellation.ExecutionReleaseResult != "not_present" && issue.Cancellation.ExecutionReleaseResult != "released") || issue.Cancellation.CanceledAt.IsZero() {
			return fmt.Errorf("canceled lifecycle has incomplete cancellation evidence")
		}
		if issue.Suspension != nil && (issue.Suspension.Status != issuedomain.SuspensionResolved || issue.Suspension.Resolution != issuedomain.ResolutionCancel) {
			return fmt.Errorf("canceled lifecycle retains an unresolved suspension")
		}
		if issue.Cancellation.Source == "github_not_planned" && (!strings.EqualFold(issue.GitHubStateReason, "NOT_PLANNED") || !strings.EqualFold(issue.Cancellation.GitHubStateReason, "NOT_PLANNED")) {
			return fmt.Errorf("GitHub cancellation is missing authoritative NOT_PLANNED state reason")
		}
	} else if issue.Cancellation != nil {
		return fmt.Errorf("non-canceled lifecycle contains cancellation evidence")
	}
	switch issue.ReviewDecision {
	case "", "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED":
	default:
		return fmt.Errorf("unsupported Pull Request review decision %q", issue.ReviewDecision)
	}
	if issue.RetryAfter != nil && issue.RetryAfter.IsZero() {
		return fmt.Errorf("retry deadline is zero")
	}
	if issue.Generation > 0 && issue.RunID == "" {
		return fmt.Errorf("execution generation has no run owner")
	}
	if issue.Workspace != nil && issue.Workspace.RepositoryID < 0 {
		return fmt.Errorf("workspace repository ID must not be negative")
	}
	if issue.ConflictRecovery != nil {
		for _, attempt := range issue.ConflictRecovery.History {
			if err := attempt.Status.Validate(); err != nil {
				return fmt.Errorf("conflict attempt %d: %w", attempt.Number, err)
			}
		}
	}
	if checkpoint := issue.Continuation; checkpoint != nil {
		if !ValidID(checkpoint.ID, "checkpoint_") && !ValidID(checkpoint.ID, "park_") {
			return fmt.Errorf("continuation checkpoint identity is invalid")
		}
		if checkpoint.RunID == "" || checkpoint.Generation == 0 || checkpoint.Generation > issue.Generation || checkpoint.CreatedAt.IsZero() {
			return fmt.Errorf("continuation checkpoint execution identity is incomplete")
		}
		if err := checkpoint.Stage.Validate(); err != nil {
			return fmt.Errorf("continuation checkpoint stage: %w", err)
		}
		if checkpoint.WorktreeSHA256 != "" && !ValidSHA256(checkpoint.WorktreeSHA256) {
			return fmt.Errorf("continuation checkpoint worktree digest is invalid")
		}
		if checkpoint.ResultSHA256 != "" && !ValidSHA256(checkpoint.ResultSHA256) {
			return fmt.Errorf("continuation checkpoint result digest is invalid")
		}
	}
	if err := validateSuspension(issue); err != nil {
		return err
	}
	return nil
}

func validateSuspension(issue *Issue) error {
	if issue.Suspension == nil {
		return nil
	}
	suspension := issue.Suspension
	if issue.Status == issuedomain.StatusCompleted {
		pendingAdoptionSync := suspension.Status == issuedomain.SuspensionResolved && suspension.Resolution == issuedomain.ResolutionAdoptPR &&
			issue.Continuation != nil && suspension.CheckpointID == issue.Continuation.ID
		if !pendingAdoptionSync {
			return fmt.Errorf("completed lifecycle retains a suspension outside pending adoption synchronization")
		}
	}
	if !issue.Status.Terminal() && suspension.Status != issuedomain.SuspensionResolved {
		return fmt.Errorf("active suspension is attached to executing lifecycle %q", issue.Status)
	}
	if !ValidID(suspension.ID, "suspension_") || strings.TrimSpace(suspension.ReasonCode) == "" ||
		strings.TrimSpace(suspension.Reason) == "" || suspension.SuspendedAt.IsZero() || len(suspension.AllowedActions) == 0 {
		return fmt.Errorf("suspension identity, reason, time, and actions must be complete")
	}
	switch suspension.Status {
	case issuedomain.SuspensionActive, issuedomain.SuspensionQuarantined:
		if !suspension.ResolvedAt.IsZero() || suspension.Resolution != issuedomain.ResolutionNone {
			return fmt.Errorf("active suspension contains a resolution")
		}
	case issuedomain.SuspensionResolved:
		if suspension.ResolvedAt.IsZero() || suspension.Resolution.Validate() != nil || (suspension.Resolution == issuedomain.ResolutionAdoptWorktree || suspension.Resolution == issuedomain.ResolutionApproveConflictPaths) {
			return fmt.Errorf("resolved suspension has no valid resolution")
		}
	default:
		return fmt.Errorf("unknown suspension status %q", suspension.Status)
	}
	switch suspension.Recoverability {
	case issuedomain.RecoverabilityOperator, issuedomain.RecoverabilityAutomatic,
		issuedomain.RecoverabilityNone, issuedomain.RecoverabilityAmbiguous:
	default:
		return fmt.Errorf("unknown suspension recoverability %q", suspension.Recoverability)
	}
	seen := map[issuedomain.ResolutionAction]bool{}
	for _, action := range suspension.AllowedActions {
		if err := action.Validate(); err != nil || action == issuedomain.ResolutionAdoptWorktree || action == issuedomain.ResolutionApproveConflictPaths || seen[action] {
			return fmt.Errorf("suspension contains an invalid or duplicate action %q", action)
		}
		seen[action] = true
	}
	if suspension.CheckpointID != "" {
		if issue.Continuation == nil || issue.Continuation.ID != suspension.CheckpointID {
			return fmt.Errorf("suspension checkpoint identity is inconsistent")
		}
	}
	return nil
}

func validateEffectIntent(snapshot Snapshot, key string, effect *EffectIntent) error {
	if effect == nil || key != strconv.Itoa(effect.IssueNumber) || !ValidID(effect.ID, "effect_") || effect.RunID == "" || effect.CreatedAt.IsZero() {
		return fmt.Errorf("pending effect entry %q has incomplete identity", key)
	}
	if err := effect.Kind.Validate(); err != nil {
		return err
	}
	issue := snapshot.Issues[key]
	if issue == nil || issue.RunID != effect.RunID {
		return fmt.Errorf("pending effect %s has no matching Issue run", effect.ID)
	}
	wantStatus := issuedomain.StatusUnset
	switch effect.Kind {
	case issuedomain.EffectMarkDone:
		wantStatus = issuedomain.StatusCompleted
	case issuedomain.EffectMarkNeedsInput:
		wantStatus = issuedomain.StatusNeedsInput
	case issuedomain.EffectMarkFailed:
		wantStatus = issuedomain.StatusFailed
	case issuedomain.EffectMarkBlocked:
		wantStatus = issuedomain.StatusBlocked
	case issuedomain.EffectRetryConflict:
		wantStatus = issuedomain.StatusResolvingConflict
	case issuedomain.EffectApplyResolution:
		if issue.Status.Terminal() || issue.Suspension == nil || issue.Suspension.Status != issuedomain.SuspensionResolved {
			return fmt.Errorf("Issue resolution effect has no resolved executable suspension")
		}
	}
	if wantStatus != issuedomain.StatusUnset && issue.Status != wantStatus {
		return fmt.Errorf("effect %q is incompatible with status %q", effect.Kind, issue.Status)
	}
	return nil
}

func ValidateRequestAggregate(snapshot Snapshot, id string, request *Request) error {
	if strings.TrimSpace(id) == "" || request.ID != id || !ValidID(id, "req_") {
		return fmt.Errorf("request entry %q has invalid identity", id)
	}
	issue := snapshot.Issues[strconv.Itoa(request.IssueNumber)]
	if issue == nil {
		return fmt.Errorf("request %s refers to missing Issue #%d", id, request.IssueNumber)
	}
	if request.RunID != "" && request.RunID != issue.RunID {
		historicalAnsweredRun := request.Status == issuedomain.RequestStatusAnswered && request.CheckpointID != "" &&
			request.ReleasedExecution != nil && request.ReleasedExecution.RunID == request.RunID
		if !historicalAnsweredRun {
			return fmt.Errorf("request %s run does not match Issue #%d", id, issue.Number)
		}
	}
	if request.ResumeStatus != issuedomain.StatusUnset {
		if err := request.ResumeStatus.Validate(); err != nil {
			return fmt.Errorf("request %s: %w", id, err)
		}
	}
	switch request.Status {
	case issuedomain.RequestStatusPending:
		if request.Answer != "" || request.AnsweredAt != nil {
			return fmt.Errorf("pending request %s already contains an answer", id)
		}
	case issuedomain.RequestStatusAnswered:
		if strings.TrimSpace(request.Answer) == "" {
			return fmt.Errorf("answered request %s has no answer", id)
		}
	case issuedomain.RequestStatusCanceled:
		if request.Answer != "" || request.AnsweredAt != nil {
			return fmt.Errorf("canceled request %s contains an answer", id)
		}
	}
	if provenance := request.AnswerProvenance; provenance != nil {
		if request.Status != issuedomain.RequestStatusAnswered || !ValidSHA256(provenance.BodySHA256) {
			return fmt.Errorf("request %s has invalid answer provenance", id)
		}
		if err := ValidateAnswerObservation(snapshot, request, provenance); err != nil {
			return err
		}
	}
	if request.Status == issuedomain.RequestStatusAnswered {
		for _, answer := range issue.Answers {
			if answer.RequestID == request.ID && answer.Question == request.Question && answer.Answer == request.Answer {
				return nil
			}
		}
	}
	if request.CheckpointID != "" {
		historicalCompletedRequest := issue.Status == issuedomain.StatusCompleted && issue.Continuation == nil &&
			request.Status == issuedomain.RequestStatusAnswered && request.ReleasedExecution != nil &&
			request.RunID != "" && request.ReleasedExecution.RunID == request.RunID
		if historicalCompletedRequest {
			return nil
		}
		if issue.Continuation == nil || issue.Continuation.ID != request.CheckpointID || issue.Continuation.RequestID != request.ID {
			issueCheckpointID, checkpointRequestID := "", ""
			if issue.Continuation != nil {
				issueCheckpointID, checkpointRequestID = issue.Continuation.ID, issue.Continuation.RequestID
			}
			return fmt.Errorf("request %s continuation identity is inconsistent: issue checkpoint=%q request checkpoint=%q checkpoint request=%q", id, issueCheckpointID, request.CheckpointID, checkpointRequestID)
		}
	}
	return nil
}

const (
	SemanticCodeCompatible                 = "SEMANTIC_COMPATIBLE"
	SemanticCodeContractVersionMismatch    = "SEMANTIC_CONTRACT_VERSION_MISMATCH"
	SemanticCodeWorkspaceProvenanceMissing = "EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_MISSING"
	SemanticCodeWorkspaceProvenanceInvalid = "EXECUTION_REQUIRED_WORKSPACE_PROVENANCE_INVALID"
	SemanticCodeExecutionAuthorityMissing  = "EXECUTION_REQUIRED_ACTIVE_EXECUTION_MISSING"
	SemanticCodePreparedTransactionPresent = "PREPARED_TRANSACTION_REQUIRES_OLD_RUNTIME_RECOVERY"
)

type SemanticViolation struct {
	IssueNumber   int    `json:"issue_number"`
	Status        string `json:"status"`
	Field         string `json:"field"`
	Code          string `json:"code"`
	Migratable    bool   `json:"migratable"`
	Reason        string `json:"reason"`
	MigrationRule string `json:"migration_rule"`
	OperatorGuide string `json:"operator_guide,omitempty"`
}

type SemanticCompatibilityError struct {
	Violations []SemanticViolation
}

func (e SemanticCompatibilityError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, violation := range e.Violations {
		parts = append(parts, fmt.Sprintf("Issue #%d %s (%s)", violation.IssueNumber, violation.Reason, violation.Code))
	}
	return "durable state does not satisfy semantic contract: " + strings.Join(parts, "; ")
}

// ValidateSemanticContract never repairs or normalizes provenance.
func ValidateSemanticContract(snapshot Snapshot) error {
	if snapshot.Version != 0 {
		if err := snapshot.ValidateVersion(); err != nil {
			return err
		}
	}

	violations := SemanticViolations(snapshot)
	if len(violations) == 0 {
		return nil
	}
	return SemanticCompatibilityError{Violations: violations}
}

func SemanticViolations(snapshot Snapshot) []SemanticViolation {
	return semanticViolations(snapshot)
}

func semanticViolations(snapshot Snapshot) []SemanticViolation {
	violations := []SemanticViolation{}
	for _, field := range statecontract.Current().Fields {
		if field.Class != statecontract.ExecutionRequiredProvenance {
			continue
		}
		if !SupportsExecutionRequiredField(field.Path) {
			violations = append(violations, SemanticViolation{Field: field.Path, Code: "EXECUTION_REQUIRED_VALIDATOR_MISSING", Migratable: false,
				Reason: "execution-required contract field has no runtime validator", MigrationRule: field.Migration.Code})
		}
	}
	field, ok := statecontract.FieldByPath("issues[].workspace")
	if !ok {
		return append(violations, SemanticViolation{Field: "issues[].workspace", Code: SemanticCodeWorkspaceProvenanceInvalid, Reason: "workspace requirement is missing from the current contract"})
	}
	activeField, ok := statecontract.FieldByPath("active_execution")
	if !ok {
		return append(violations, SemanticViolation{Field: "active_execution", Code: SemanticCodeExecutionAuthorityMissing, Reason: "active execution requirement is missing from the current contract"})
	}
	keys := make([]string, 0, len(snapshot.Issues))
	for key := range snapshot.Issues {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left, _ := strconv.Atoi(keys[i])
		right, _ := strconv.Atoi(keys[j])
		return left < right
	})
	for _, key := range keys {
		issue := snapshot.Issues[key]
		if issue == nil {
			continue
		}
		activeMatches := snapshot.ActiveExecution != nil && snapshot.ActiveExecution.IssueNumber == issue.Number && snapshot.ActiveExecution.RunID == issue.RunID && snapshot.ActiveExecution.Generation == issue.Generation
		if statecontract.RequiredForStatus(activeField, issue.Status) && !activeMatches {
			violations = append(violations, SemanticViolation{
				IssueNumber: issue.Number, Status: string(issue.Status), Field: activeField.Path,
				Code: SemanticCodeExecutionAuthorityMissing, Migratable: false,
				Reason:        "executing lifecycle has no matching repository active execution",
				MigrationRule: activeField.Migration.Code, OperatorGuide: activeField.Migration.OperatorGuide,
			})
		}
		if !statecontract.RequiredForStatus(field, issue.Status) || !CrossedWorkerExecutionBoundary(issue) {
			continue
		}
		base := SemanticViolation{
			IssueNumber: issue.Number, Status: issue.Status.String(), Field: field.Path, Migratable: false,
			MigrationRule: field.Migration.Code, OperatorGuide: field.Migration.OperatorGuide,
		}
		if issue.Workspace == nil {
			base.Code = SemanticCodeWorkspaceProvenanceMissing
			base.Reason = "saved workspace provenance is required before this recovery state can execute"
			violations = append(violations, base)
			continue
		}
		workspace := issue.Workspace
		if workspace.Path == "" || workspace.Path != issue.Worktree || workspace.Branch == "" || workspace.Branch != issue.Branch ||
			workspace.RepoID == "" || workspace.RepoID != snapshot.RepoID || workspace.Repository == "" ||
			workspace.GitCommonDir == "" || workspace.MainCheckout == "" || workspace.CapturedAt.IsZero() {
			base.Code = SemanticCodeWorkspaceProvenanceInvalid
			base.Reason = "saved workspace provenance is incomplete or inconsistent with durable issue identity"
			violations = append(violations, base)
		}
	}
	return violations
}

func SupportsExecutionRequiredField(path string) bool {
	switch path {
	case "issues[].workspace", "issues[].generation", "active_execution":
		return true
	default:
		return false
	}
}

func CrossedWorkerExecutionBoundary(issue *Issue) bool {
	if issue.Workspace != nil || issue.Worktree != "" || issue.Branch != "" || issue.SessionID != "" || issue.Session != nil || issue.Attempts > 0 || issue.Continuations > 0 {
		return true
	}
	return issue.PublicationAudit != nil || issue.ConflictRecovery != nil || issue.Continuation != nil || issue.Suspension != nil
}

type SupervisorState string

const (
	SupervisorStateStarting    SupervisorState = "starting"
	SupervisorStateRunning     SupervisorState = "running"
	SupervisorStatePolling     SupervisorState = "polling"
	SupervisorStateRetryWait   SupervisorState = "retry_wait"
	SupervisorStateBlocked     SupervisorState = "blocked"
	SupervisorStateStopped     SupervisorState = "stopped"
	SupervisorStateMaintenance SupervisorState = "maintenance"
	SupervisorStateDraining    SupervisorState = "draining"
)

type RecoveryState string

const RecoveryStateBlocked RecoveryState = "blocked"

func (s SupervisorState) Validate() error {
	if s == "" {
		return nil
	}
	switch s {
	case SupervisorStateStarting, SupervisorStateRunning, SupervisorStatePolling, SupervisorStateRetryWait,
		SupervisorStateBlocked, SupervisorStateStopped, SupervisorStateMaintenance, SupervisorStateDraining:
		return nil
	default:
		return fmt.Errorf("unknown supervisor state %q", s)
	}
}

func (s RecoveryState) Validate() error {
	if s == RecoveryStateBlocked {
		return nil
	}
	return fmt.Errorf("unknown recovery state %q", s)
}

type HumanNeed struct {
	IssueNumber int    `json:"issue_number"`
	Reason      string `json:"reason"`
}

func (s Snapshot) HumanNeeds(autoMerge bool) []HumanNeed {
	unanswered := map[int]bool{}
	for _, r := range s.Requests() {
		if r.Status == issuedomain.RequestStatusPending {
			unanswered[r.IssueNumber] = true
		}
	}
	needs := []HumanNeed{}
	for _, item := range s.Issues {
		if item == nil {
			continue
		}
		wait := issuedomain.HumanWait{Status: item.Status, Unanswered: unanswered[item.Number], ReviewDecision: item.ReviewDecision, AutoMerge: autoMerge}
		if item.Suspension != nil {
			wait.Recoverability, wait.SuspensionStatus = item.Suspension.Recoverability, item.Suspension.Status
		}
		if reason := wait.Reason(); reason != "" {
			needs = append(needs, HumanNeed{IssueNumber: item.Number, Reason: reason})
		}
	}
	for key, q := range s.QuarantinedIssues {
		if q == nil {
			continue
		}
		number, _ := strconv.Atoi(key)
		wait := issuedomain.HumanWait{Quarantined: true, Unanswered: unanswered[number]}
		needs = append(needs, HumanNeed{IssueNumber: number, Reason: wait.Reason()})
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].IssueNumber < needs[j].IssueNumber })
	return needs
}

func (s Snapshot) NeedsHuman(number int, autoMerge bool) bool {
	for _, need := range s.HumanNeeds(autoMerge) {
		if need.IssueNumber == number {
			return true
		}
	}
	return false
}

type SchemaVersionError struct {
	Kind    string
	Version int
}

func (e SchemaVersionError) Error() string {
	if e.Version > 0 && e.Version < CurrentVersion {
		return fmt.Sprintf("%s schema migration required from version %d to %d", e.Kind, e.Version, CurrentVersion)
	}
	return fmt.Sprintf("unsupported %s version %d; this binary supports version %d; migration required before execution", e.Kind, e.Version, CurrentVersion)
}

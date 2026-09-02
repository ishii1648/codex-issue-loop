package incidentloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
)

const SchemaVersion = 1

var (
	identifierPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	repositoryPattern  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9_.-]{0,99})/[A-Za-z0-9_.-]{1,100}$`)
	allowedSignalNames = stringSet(
		"scheduler_cycle", "external_attempt_completed", "progress", "retry_episode",
		"failure_classified", "runtime_identity", "operation_duration", "host_diagnostic",
		"evidence_coverage", "lifecycle_outcome",
	)
	allowedKinds           = stringSet("event", "status", "log")
	allowedComponents      = stringSet("scheduler", "github", "worker", "supervisor", "review", "ci", "publication", "recovery", "host", "retention", "analysis")
	allowedPhases          = stringSet("poll", "dispatch", "execute", "analyze", "issue", "checks", "review", "merge", "recovery", "startup", "shutdown", "retention")
	allowedOutcomes        = stringSet("started", "succeeded", "failed", "retrying", "blocked", "pending", "observed", "resolved", "reopened", "noop", "rejected", "rate_limited", "canceled")
	allowedFailureKinds    = stringSet("none", "transient", "issue", "supervisor", "operator", "product")
	allowedTriggers        = stringSet("startup", "poll_timer", "retry_timer", "worker_finished", "webhook", "fsnotify", "manual", "schedule")
	allowedLifecycleStages = stringSet("worker_started", "worker_failed", "pull_request_created", "checks_passed", "checks_failed", "review_passed", "review_failed", "merged", "issue_closed", "reopened", "recovery_completed")
	allowedOperationCodes  = stringSet("scheduler_cycle", "list_ready_issues", "incident_analysis", "issue_search", "issue_create", "issue_readback")
	allowedClassifications = stringSet("expected_transient", "degraded", "operator_attention", "suspected_bug", "confirmed_bug", "unknown")
	allowedConfidence      = stringSet("low", "medium", "high")
	allowedEpisodeStates   = stringSet("open", "resolved", "reopened")
)

type EvidenceRef struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

// Signal is the bounded, sanitized contract crossing the supervisor-to-analysis
// boundary. High-cardinality correlation values are retained in events only;
// Metrics never use them as labels.
type Signal struct {
	Version             int       `json:"version"`
	ID                  string    `json:"id"`
	Timestamp           time.Time `json:"timestamp"`
	Repository          string    `json:"repository"`
	CorrelationID       string    `json:"correlation_id"`
	IssueNumber         int       `json:"issue_number,omitempty"`
	RunID               string    `json:"run_id,omitempty"`
	CycleID             string    `json:"cycle_id,omitempty"`
	EpisodeID           string    `json:"episode_id,omitempty"`
	IncidentFingerprint string    `json:"incident_fingerprint,omitempty"`
	Kind                string    `json:"kind"`
	Name                string    `json:"name"`
	Component           string    `json:"component"`
	Phase               string    `json:"phase"`
	OutcomeCode         string    `json:"outcome_code"`
	ReasonCode          string    `json:"reason_code"`

	Trigger              string        `json:"trigger,omitempty"`
	ScheduledDeadline    *time.Time    `json:"scheduled_deadline,omitempty"`
	AttemptAllowed       bool          `json:"attempt_allowed"`
	CoalescedWakeCount   int           `json:"coalesced_wake_count"`
	Provider             string        `json:"provider,omitempty"`
	OperationCode        string        `json:"operation_code,omitempty"`
	RateLimitResource    string        `json:"rate_limit_resource,omitempty"`
	ResetAt              *time.Time    `json:"reset_at,omitempty"`
	ScopeKind            string        `json:"scope_kind,omitempty"`
	ScopeHash            string        `json:"scope_hash,omitempty"`
	Attempt              int           `json:"attempt,omitempty"`
	RetryAt              *time.Time    `json:"retry_at,omitempty"`
	TerminalOutcome      string        `json:"terminal_outcome,omitempty"`
	ProgressKind         string        `json:"progress_kind,omitempty"`
	NextExpectedAt       *time.Time    `json:"next_expected_at,omitempty"`
	ActiveScopeCount     int           `json:"active_scope_count,omitempty"`
	FailureKind          string        `json:"failure_kind,omitempty"`
	FailureCode          string        `json:"failure_code,omitempty"`
	Resumable            bool          `json:"resumable,omitempty"`
	ReleaseVersion       string        `json:"release_version,omitempty"`
	CommitSHA            string        `json:"commit_sha,omitempty"`
	StateSchema          int           `json:"state_schema,omitempty"`
	SemanticContract     int           `json:"semantic_contract_version,omitempty"`
	ElapsedMS            int64         `json:"elapsed_ms,omitempty"`
	DeadlineRemainingMS  int64         `json:"deadline_remaining_ms,omitempty"`
	EventSequence        uint64        `json:"event_sequence,omitempty"`
	SourceKind           string        `json:"source_kind,omitempty"`
	OldestAt             *time.Time    `json:"oldest_at,omitempty"`
	NewestAt             *time.Time    `json:"newest_at,omitempty"`
	RecordCount          int64         `json:"record_count,omitempty"`
	RetentionPolicy      string        `json:"retention_policy,omitempty"`
	LastCompactionAt     *time.Time    `json:"last_compaction_at,omitempty"`
	LifecycleStage       string        `json:"lifecycle_stage,omitempty"`
	InvariantViolation   bool          `json:"invariant_violation,omitempty"`
	IndependentRun       bool          `json:"independent_run,omitempty"`
	ProductFixMerged     bool          `json:"product_fix_merged,omitempty"`
	RequestAmplification bool          `json:"request_amplification,omitempty"`
	ProgressStalled      bool          `json:"progress_stalled,omitempty"`
	HumanActionRequired  bool          `json:"human_action_required,omitempty"`
	ThresholdExceeded    bool          `json:"threshold_exceeded,omitempty"`
	Evidence             []EvidenceRef `json:"evidence,omitempty"`
}

func (s Signal) Validate() error {
	if s.Version != SchemaVersion || !identifierPattern.MatchString(s.ID) || s.Timestamp.IsZero() || !repositoryPattern.MatchString(s.Repository) || !identifierPattern.MatchString(s.CorrelationID) {
		return errors.New("signal requires version 1, bounded IDs, timestamp, and owner/name repository")
	}
	if !allowedKinds[s.Kind] || !allowedSignalNames[s.Name] || !allowedComponents[s.Component] || !allowedPhases[s.Phase] || !allowedOutcomes[s.OutcomeCode] || !identifierPattern.MatchString(s.ReasonCode) {
		return errors.New("signal contains an unsupported bounded vocabulary value")
	}
	if s.IssueNumber < 0 || s.Attempt < 0 || s.CoalescedWakeCount < 0 || s.ActiveScopeCount < 0 || s.ElapsedMS < 0 || s.RecordCount < 0 {
		return errors.New("signal numeric values must not be negative")
	}
	if s.FailureKind != "" && !allowedFailureKinds[s.FailureKind] {
		return fmt.Errorf("unsupported failure_kind %q", s.FailureKind)
	}
	if s.Trigger != "" && !allowedTriggers[s.Trigger] {
		return fmt.Errorf("unsupported trigger %q", s.Trigger)
	}
	if s.OperationCode != "" && !allowedOperationCodes[s.OperationCode] {
		return fmt.Errorf("unsupported operation_code %q", s.OperationCode)
	}
	if s.LifecycleStage != "" && !allowedLifecycleStages[s.LifecycleStage] {
		return fmt.Errorf("unsupported lifecycle_stage %q", s.LifecycleStage)
	}
	if s.IncidentFingerprint != "" && (len(s.IncidentFingerprint) != 64 || strings.Trim(s.IncidentFingerprint, "0123456789abcdef") != "") {
		return errors.New("incident_fingerprint must be lowercase SHA-256 hex")
	}
	switch s.Name {
	case "scheduler_cycle":
		if s.CycleID == "" || s.Trigger == "" {
			return errors.New("scheduler_cycle requires cycle_id and trigger")
		}
	case "external_attempt_completed":
		if s.Provider == "" || s.OperationCode == "" {
			return errors.New("external_attempt_completed requires provider and operation_code")
		}
	case "progress":
		if s.ProgressKind == "" {
			return errors.New("progress requires progress_kind")
		}
	case "retry_episode":
		if s.EpisodeID == "" || s.ScopeKind == "" || s.Attempt < 1 {
			return errors.New("retry_episode requires episode_id, scope_kind, and positive attempt")
		}
	case "failure_classified", "host_diagnostic":
		if s.FailureKind == "" || s.FailureCode == "" {
			return fmt.Errorf("%s requires failure_kind and failure_code", s.Name)
		}
	case "runtime_identity":
		if s.ReleaseVersion == "" || s.StateSchema < 1 || s.SemanticContract < 1 {
			return errors.New("runtime_identity requires release and schema identities")
		}
	case "operation_duration":
		if s.OperationCode == "" {
			return errors.New("operation_duration requires operation_code")
		}
	case "evidence_coverage":
		if s.SourceKind == "" || s.OldestAt == nil || s.NewestAt == nil || s.RetentionPolicy == "" {
			return errors.New("evidence_coverage requires source coverage and retention policy")
		}
	case "lifecycle_outcome":
		if s.LifecycleStage == "" {
			return errors.New("lifecycle_outcome requires lifecycle_stage")
		}
	}
	if len(s.Evidence) > 128 {
		return errors.New("signal evidence exceeds 128 items")
	}
	if err := validateEvidence(s.Evidence); err != nil {
		return err
	}
	return rejectSensitive(s)
}

type Episode struct {
	Version                int                       `json:"version"`
	ID                     string                    `json:"id"`
	Fingerprint            string                    `json:"fingerprint"`
	Repository             string                    `json:"repository"`
	CorrelationID          string                    `json:"correlation_id"`
	IssueNumber            int                       `json:"issue_number,omitempty"`
	RunIDs                 []string                  `json:"run_ids,omitempty"`
	State                  string                    `json:"state"`
	StartedAt              time.Time                 `json:"started_at"`
	UpdatedAt              time.Time                 `json:"updated_at"`
	OccurrenceCount        int                       `json:"occurrence_count"`
	SignalIDs              []string                  `json:"signal_ids"`
	LastSignalID           string                    `json:"last_signal_id"`
	Evidence               []EvidenceRef             `json:"evidence"`
	Features               incidentanalysis.Features `json:"features"`
	PrimaryClassification  string                    `json:"primary_classification"`
	MatchedRules           []string                  `json:"matched_rules"`
	Confidence             string                    `json:"confidence"`
	MissingEvidence        []string                  `json:"missing_evidence,omitempty"`
	AI                     *AIAnalysis               `json:"ai_analysis,omitempty"`
	Issue                  *IssueRef                 `json:"issue,omitempty"`
	Attempts               int                       `json:"analysis_attempts,omitempty"`
	NextAttemptAt          *time.Time                `json:"next_attempt_at,omitempty"`
	CircuitOpen            bool                      `json:"circuit_open,omitempty"`
	CircuitGeneration      int                       `json:"analysis_circuit_generation"`
	IssueAttempts          int                       `json:"issue_attempts,omitempty"`
	IssueNextAttemptAt     *time.Time                `json:"issue_next_attempt_at,omitempty"`
	IssueCircuitOpen       bool                      `json:"issue_circuit_open,omitempty"`
	IssueCircuitGeneration int                       `json:"issue_circuit_generation"`
	Lifecycle              []LifecycleResult         `json:"lifecycle,omitempty"`
}

type EvidenceBundle struct {
	Version               int           `json:"version"`
	EpisodeID             string        `json:"episode_id"`
	Fingerprint           string        `json:"fingerprint"`
	Repository            string        `json:"repository"`
	PrimaryClassification string        `json:"primary_classification"`
	Confidence            string        `json:"confidence"`
	SignalIDs             []string      `json:"signal_ids"`
	Evidence              []EvidenceRef `json:"evidence"`
	MissingEvidence       []string      `json:"missing_evidence,omitempty"`
}

type AIAnalysis struct {
	Version                 int           `json:"version"`
	EpisodeID               string        `json:"episode_id"`
	Classification          string        `json:"classification"`
	Confidence              string        `json:"confidence"`
	Summary                 string        `json:"summary"`
	CauseHypothesis         string        `json:"cause_hypothesis"`
	CounterEvidence         []string      `json:"counter_evidence"`
	AdditionalInvestigation []string      `json:"additional_investigation"`
	RecommendIssue          bool          `json:"recommend_issue"`
	Evidence                []EvidenceRef `json:"evidence"`
}

func (a AIAnalysis) Validate(episode Episode) error {
	if a.Version != SchemaVersion || a.EpisodeID != episode.ID || a.Classification != episode.PrimaryClassification {
		return errors.New("AI analysis must use version 1 and preserve deterministic episode classification")
	}
	if !allowedConfidence[a.Confidence] {
		return errors.New("AI analysis confidence must be low, medium, or high")
	}
	if strings.TrimSpace(a.Summary) == "" || strings.TrimSpace(a.CauseHypothesis) == "" || a.CounterEvidence == nil || a.AdditionalInvestigation == nil || a.Evidence == nil {
		return errors.New("AI analysis requires summary, hypothesis, counter-evidence, investigation, and evidence")
	}
	if len(a.Summary) > 4096 || len(a.CauseHypothesis) > 4096 || len(a.CounterEvidence) > 32 || len(a.AdditionalInvestigation) > 32 || len(a.Evidence) > 128 {
		return errors.New("AI analysis exceeds schema bounds")
	}
	for _, values := range [][]string{a.CounterEvidence, a.AdditionalInvestigation} {
		for _, value := range values {
			if len(value) > 1024 {
				return errors.New("AI analysis item exceeds schema bounds")
			}
		}
	}
	if err := validateEvidence(a.Evidence); err != nil {
		return err
	}
	return rejectSensitive(a)
}

type IssueDraft struct {
	Version     int      `json:"version"`
	EpisodeID   string   `json:"episode_id"`
	Fingerprint string   `json:"fingerprint"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels"`
}

func (d IssueDraft) Validate() error {
	if d.Version != SchemaVersion || !strings.HasPrefix(d.EpisodeID, "inc-") || len(d.EpisodeID) != 24 || !identifierPattern.MatchString(d.EpisodeID) || !validFingerprint(d.Fingerprint) {
		return errors.New("Issue draft requires version 1 and valid episode/fingerprint identifiers")
	}
	if strings.TrimSpace(d.Title) == "" || len(d.Title) > 512 || strings.TrimSpace(d.Body) == "" || len(d.Body) > 65536 || len(d.Labels) == 0 {
		return errors.New("Issue draft title, body, or labels violate schema bounds")
	}
	seen := map[string]bool{}
	for _, label := range d.Labels {
		if strings.TrimSpace(label) != label || label == "" || len(label) > 128 || seen[label] || strings.ContainsAny(label, "\r\n") {
			return errors.New("Issue draft labels must be unique bounded single-line values")
		}
		seen[label] = true
	}
	return rejectSensitive(d)
}

type IssueRef struct {
	Number      int       `json:"number,omitempty"`
	URL         string    `json:"url,omitempty"`
	Labels      []string  `json:"labels"`
	Fingerprint string    `json:"fingerprint"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

type LifecycleResult struct {
	Stage     string    `json:"stage"`
	Outcome   string    `json:"outcome"`
	Timestamp time.Time `json:"timestamp"`
	Ref       string    `json:"ref,omitempty"`
}

type Metrics struct {
	Version          int                        `json:"version"`
	UpdatedAt        time.Time                  `json:"updated_at"`
	SignalsByName    map[string]uint64          `json:"signals_by_name"`
	Outcomes         map[string]uint64          `json:"outcomes"`
	Classifications  map[string]uint64          `json:"classifications"`
	Issues           map[string]uint64          `json:"issues"`
	AnalysisAttempts map[string]uint64          `json:"analysis_attempts"`
	DurationsMS      map[string]DurationSummary `json:"durations_ms"`
	OpenEpisodes     int                        `json:"open_episodes"`
	CircuitOpen      int                        `json:"circuit_open"`
}

func (m Metrics) Validate() error {
	if m.Version != SchemaVersion || m.SignalsByName == nil || m.Outcomes == nil || m.Classifications == nil || m.Issues == nil || m.AnalysisAttempts == nil || m.DurationsMS == nil || m.OpenEpisodes < 0 || m.CircuitOpen < 0 {
		return errors.New("unsupported or invalid incident metrics")
	}
	if err := validateCounterKeys(m.SignalsByName, allowedSignalNames, "signal"); err != nil {
		return err
	}
	if err := validateCounterKeys(m.Outcomes, allowedOutcomes, "outcome"); err != nil {
		return err
	}
	if err := validateCounterKeys(m.Classifications, allowedClassifications, "classification"); err != nil {
		return err
	}
	if err := validateCounterKeys(m.Issues, stringSet("created", "reused", "dry_run", "failed"), "Issue"); err != nil {
		return err
	}
	if err := validateCounterKeys(m.AnalysisAttempts, stringSet("succeeded", "failed"), "analysis attempt"); err != nil {
		return err
	}
	for operation, summary := range m.DurationsMS {
		if !allowedOperationCodes[operation] || summary.Sum < 0 || summary.Max < 0 || summary.Max > summary.Sum || (summary.Count == 0 && (summary.Sum != 0 || summary.Max != 0)) {
			return errors.New("invalid duration metric")
		}
	}
	return rejectSensitive(m)
}

func validateCounterKeys(values map[string]uint64, allowed map[string]bool, kind string) error {
	for key := range values {
		if !allowed[key] {
			return fmt.Errorf("unsupported %s metric key %q", kind, key)
		}
	}
	return nil
}

type DurationSummary struct {
	Count uint64 `json:"count"`
	Sum   int64  `json:"sum"`
	Max   int64  `json:"max"`
}

type DurableState struct {
	Version    int                `json:"version"`
	Revision   uint64             `json:"revision"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Episodes   map[string]Episode `json:"episodes"`
	LastSignal string             `json:"last_signal,omitempty"`
}

func (s DurableState) Validate() error {
	if s.Version != SchemaVersion || s.Episodes == nil {
		return errors.New("unsupported or invalid incident state")
	}
	if s.LastSignal != "" && !identifierPattern.MatchString(s.LastSignal) {
		return errors.New("incident state last_signal is invalid")
	}
	for fingerprint, episode := range s.Episodes {
		if fingerprint != episode.Fingerprint || !validFingerprint(fingerprint) {
			return errors.New("incident state episode fingerprint mismatch")
		}
		if err := episode.Validate(); err != nil {
			return fmt.Errorf("episode %s: %w", episode.ID, err)
		}
	}
	return rejectSensitive(s)
}

func (e Episode) Validate() error {
	if e.Version != SchemaVersion || len(e.ID) != 24 || !strings.HasPrefix(e.ID, "inc-") || !identifierPattern.MatchString(e.ID) || !validFingerprint(e.Fingerprint) || !repositoryPattern.MatchString(e.Repository) || !identifierPattern.MatchString(e.CorrelationID) {
		return errors.New("invalid episode identity")
	}
	if !allowedEpisodeStates[e.State] || e.StartedAt.IsZero() || e.UpdatedAt.Before(e.StartedAt) || e.OccurrenceCount < 1 || len(e.SignalIDs) > 128 || !identifierPattern.MatchString(e.LastSignalID) || e.CircuitGeneration < 0 || e.IssueCircuitGeneration < 0 {
		return errors.New("invalid episode state, time range, count, or signal bounds")
	}
	if e.Attempts < 0 || e.IssueAttempts < 0 {
		return errors.New("incident retry attempts must not be negative")
	}
	for _, id := range e.SignalIDs {
		if !identifierPattern.MatchString(id) {
			return errors.New("invalid episode signal ID")
		}
	}
	if len(e.Evidence) > 128 {
		return errors.New("episode evidence exceeds 128 items")
	}
	if err := validateEvidence(e.Evidence); err != nil {
		return err
	}
	if !allowedClassifications[e.PrimaryClassification] || !allowedConfidence[e.Confidence] {
		return errors.New("invalid episode classification or confidence")
	}
	for _, rule := range e.MatchedRules {
		if !identifierPattern.MatchString(rule) {
			return errors.New("invalid matched rule")
		}
	}
	if len(e.Lifecycle) > 128 {
		return errors.New("episode lifecycle exceeds 128 items")
	}
	for _, result := range e.Lifecycle {
		if !allowedLifecycleStages[result.Stage] || !allowedOutcomes[result.Outcome] || result.Timestamp.IsZero() || len(result.Ref) > 1024 {
			return errors.New("invalid lifecycle result")
		}
	}
	if e.AI != nil {
		if err := e.AI.Validate(e); err != nil {
			return err
		}
	}
	if e.Issue != nil {
		if e.Issue.Fingerprint != e.Fingerprint || len(e.Issue.Labels) == 0 {
			return errors.New("incident Issue fingerprint or labels are invalid")
		}
		if e.Issue.Status != "dry_run" && (e.Issue.Number < 1 || strings.TrimSpace(e.Issue.URL) == "") {
			return errors.New("live incident Issue requires number and URL")
		}
	}
	return nil
}

func validateEvidence(values []EvidenceRef) error {
	for _, evidence := range values {
		if !identifierPattern.MatchString(evidence.Source) || strings.TrimSpace(evidence.Ref) == "" || len(evidence.Ref) > 1024 {
			return errors.New("evidence requires bounded source and ref")
		}
	}
	return nil
}

func validFingerprint(value string) bool {
	return len(value) == 64 && strings.Trim(value, "0123456789abcdef") == ""
}

func stringSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func rejectSensitive(value any) error {
	raw, _ := json.Marshal(value)
	for _, marker := range []string{"/Users/", "ghp_", "github_pat_", "Bearer ", "\"token\":", "-----BEGIN"} {
		if bytes.Contains(raw, []byte(marker)) {
			return fmt.Errorf("prohibited sensitive marker %q", marker)
		}
	}
	return nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	for _, value := range values {
		if value != "" {
			seen[value] = true
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

package state

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

const RecoveryPredicateReportSchemaVersion = 1

type RecoveryPredicateCode string

const (
	RecoveryCodeStatus                  RecoveryPredicateCode = "RECOVERY_STATUS"
	RecoveryCodeChecksFailureProvenance RecoveryPredicateCode = "RECOVERY_CHECKS_FAILURE_PROVENANCE"
	RecoveryCodePRFailureIdentity       RecoveryPredicateCode = "RECOVERY_PR_FAILURE_IDENTITY"
	RecoveryCodeRunWorkspace            RecoveryPredicateCode = "RECOVERY_RUN_WORKSPACE"
	RecoveryCodeIncompatibleState       RecoveryPredicateCode = "RECOVERY_INCOMPATIBLE_STATE"
	RecoveryCodeWorkerProcess           RecoveryPredicateCode = "RECOVERY_WORKER_PROCESS"
	RecoveryCodePendingRequest          RecoveryPredicateCode = "RECOVERY_PENDING_REQUEST"
	RecoveryCodeWorktreeRemote          RecoveryPredicateCode = "RECOVERY_WORKTREE_REMOTE"
	RecoveryCodeGitHubIdentity          RecoveryPredicateCode = "RECOVERY_GITHUB_IDENTITY"
	RecoveryCodeReplacementChecks       RecoveryPredicateCode = "RECOVERY_REPLACEMENT_CHECKS"
	RecoveryCodeReadOnlyInvariant       RecoveryPredicateCode = "RECOVERY_READ_ONLY_INVARIANT"
	RecoveryCodeWorkspace               RecoveryPredicateCode = "RECOVERY_WORKSPACE"
	RecoveryCodeWorkspaceProvenance     RecoveryPredicateCode = "RECOVERY_WORKSPACE_PROVENANCE"
	RecoveryCodeWorktreeHeadRemote      RecoveryPredicateCode = "RECOVERY_WORKTREE_HEAD_REMOTE"
	RecoveryCodeBaseSHAIdentity         RecoveryPredicateCode = "RECOVERY_BASE_SHA_IDENTITY"
	RecoveryCodeGitHubCommentMarkers    RecoveryPredicateCode = "RECOVERY_GITHUB_COMMENT_MARKERS"
	RecoveryCodeGitHubLabels            RecoveryPredicateCode = "RECOVERY_GITHUB_LABELS"
	RecoveryCodeEventCount              RecoveryPredicateCode = "RECOVERY_EVENT_COUNT"
	RecoveryCodeEventOrder              RecoveryPredicateCode = "RECOVERY_EVENT_ORDER"
	RecoveryCodeGitHubMarkers           RecoveryPredicateCode = "RECOVERY_GITHUB_MARKERS"
	RecoveryCodePayloadShape            RecoveryPredicateCode = "RECOVERY_PAYLOAD_SHAPE"
	RecoveryCodeRemoteIdentity          RecoveryPredicateCode = "RECOVERY_REMOTE_IDENTITY"
	RecoveryCodeSessionIdentity         RecoveryPredicateCode = "RECOVERY_SESSION_IDENTITY"
	RecoveryCodeTimestamps              RecoveryPredicateCode = "RECOVERY_TIMESTAMPS"
	RecoveryCodeStartupReconciliation   RecoveryPredicateCode = "RECOVERY_STARTUP_RECONCILIATION"
)

// RecoveryPredicateReport is the stable, automation-facing result of a
// read-only recovery eligibility evaluation. The report intentionally carries
// summaries rather than raw values so paths, tokens, and comment bodies cannot
// cross the diagnostic boundary.
type RecoveryPredicateReport struct {
	SchemaVersion int                 `json:"schema_version"`
	Operation     string              `json:"operation"`
	IssueNumber   int                 `json:"issue_number"`
	Eligible      bool                `json:"eligible"`
	Predicates    []RecoveryPredicate `json:"predicates"`
}

type RecoveryPredicate struct {
	Code        RecoveryPredicateCode `json:"code"`
	Status      string                `json:"status"`
	Evidence    PredicateEvidence     `json:"evidence"`
	Fixability  string                `json:"fixability"`
	Remediation string                `json:"remediation"`
	detail      string
}

type PredicateEvidence struct {
	Source   string `json:"source"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

// RecoveryPredicateError is also returned by mutating commands. Code binds a
// refusal to the exact predicate emitted by the immediately preceding
// read-only report.
type RecoveryPredicateError struct {
	Code RecoveryPredicateCode
	Err  error
}

func (e RecoveryPredicateError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("recovery predicate %s failed", e.Code)
	}
	return fmt.Sprintf("[%s] %v", e.Code, e.Err)
}

func (e RecoveryPredicateError) Unwrap() error { return e.Err }

func newRecoveryPredicateReport(operation string, issueNumber int) RecoveryPredicateReport {
	return RecoveryPredicateReport{
		SchemaVersion: RecoveryPredicateReportSchemaVersion,
		Operation:     operation,
		IssueNumber:   issueNumber,
		Eligible:      true,
		Predicates:    []RecoveryPredicate{},
	}
}

func (r *RecoveryPredicateReport) add(code RecoveryPredicateCode, status, source, expected, actual, fixability, remediation, detail string) {
	if status != "pass" {
		r.Eligible = false
	}
	r.Predicates = append(r.Predicates, RecoveryPredicate{
		Code: code, Status: status,
		Evidence:   PredicateEvidence{Source: source, Expected: expected, Actual: actual},
		Fixability: fixability, Remediation: remediation, detail: detail,
	})
}

// AddPredicate appends an externally evaluated predicate while preserving the
// report's eligibility invariant. Callers must pass redacted summaries only.
func (r *RecoveryPredicateReport) AddPredicate(code RecoveryPredicateCode, status, source, expected, actual, fixability, remediation string) {
	r.add(code, status, source, expected, actual, fixability, remediation, actual)
}

func (r RecoveryPredicateReport) FirstFailure() error {
	for _, predicate := range r.Predicates {
		if predicate.Status == "pass" {
			continue
		}
		detail := predicate.detail
		if strings.TrimSpace(detail) == "" {
			detail = fmt.Sprintf("predicate status is %s", predicate.Status)
		}
		return RecoveryPredicateError{Code: predicate.Code, Err: fmt.Errorf("%s", detail)}
	}
	return nil
}

// ReadRecoveryInputs reads a coherent snapshot/event pair without acquiring a
// lock, creating directories, completing transactions, truncating tails, or
// quarantining files. A concurrent writer causes an explicit retryable error.
func (s Store) ReadRecoveryInputs() (Snapshot, []Event, error) {
	for attempt := 0; attempt < 3; attempt++ {
		stateBefore, err := os.ReadFile(s.StatePath())
		if err != nil {
			return Snapshot{}, nil, fmt.Errorf("read durable state without recovery: %w", err)
		}
		eventsBefore, err := os.ReadFile(s.EventsPath())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, nil, fmt.Errorf("read durable events without recovery: %w", err)
		}
		stateAfter, stateErr := os.ReadFile(s.StatePath())
		eventsAfter, eventsErr := os.ReadFile(s.EventsPath())
		if errors.Is(eventsErr, os.ErrNotExist) {
			eventsAfter = nil
			eventsErr = nil
		}
		if stateErr != nil || eventsErr != nil {
			continue
		}
		if !bytes.Equal(stateBefore, stateAfter) || !bytes.Equal(eventsBefore, eventsAfter) {
			continue
		}
		var snapshot Snapshot
		if err := json.Unmarshal(stateBefore, &snapshot); err != nil {
			return Snapshot{}, nil, fmt.Errorf("decode durable state without recovery: %w", err)
		}
		if !supportedRecoverySchema(snapshot.Version) || snapshot.RepoID != s.RepoID {
			return Snapshot{}, nil, fmt.Errorf("durable state schema or repository identity differs")
		}
		normalizeSnapshot(&snapshot)
		events, err := decodeRecoveryEvents(eventsBefore, s.RepoID)
		if err != nil {
			return Snapshot{}, nil, err
		}
		return snapshot, events, nil
	}
	return Snapshot{}, nil, errors.New("durable state or events changed during read-only recovery diagnosis")
}

// ReadCanonicalSnapshot is the read-only authority for generic Issue planning.
// It deliberately does not inspect event history; events are audit evidence,
// not continuation eligibility.
func (s Store) ReadCanonicalSnapshot() (Snapshot, error) {
	for attempt := 0; attempt < 3; attempt++ {
		before, err := os.ReadFile(s.StatePath())
		if err != nil {
			return Snapshot{}, fmt.Errorf("read canonical state: %w", err)
		}
		after, err := os.ReadFile(s.StatePath())
		if err != nil || !bytes.Equal(before, after) {
			continue
		}
		var snapshot Snapshot
		if err := json.Unmarshal(before, &snapshot); err != nil {
			return Snapshot{}, fmt.Errorf("decode canonical state: %w", err)
		}
		normalizeSnapshot(&snapshot)
		if snapshot.Version != CurrentVersion || snapshot.RepoID != s.RepoID {
			return Snapshot{}, fmt.Errorf("canonical state schema or repository identity differs")
		}
		if err := snapshot.Validate(); err != nil {
			return Snapshot{}, err
		}
		return snapshot, nil
	}
	return Snapshot{}, errors.New("canonical state changed during read-only planning")
}

func decodeRecoveryEvents(data []byte, repoID string) ([]Event, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	events := make([]Event, 0)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, fmt.Errorf("decode durable recovery event: %w", err)
		}
		if !supportedRecoverySchema(event.Version) || event.RepoID != repoID {
			return nil, errors.New("durable recovery event schema or repository identity differs")
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan durable recovery events: %w", err)
	}
	return events, nil
}

func supportedRecoverySchema(version int) bool {
	return version == CurrentVersion || version == CurrentVersion-1
}

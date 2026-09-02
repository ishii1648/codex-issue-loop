package incidentloop

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

type StateEventCollector struct {
	Repository string
	CloseIssue bool
	Source     state.Store
	Target     Store
}

func (c StateEventCollector) Collect() (int, error) {
	events, err := readStateEvents(c.Source.EventsPath())
	if err != nil {
		return 0, err
	}
	incidentState, err := c.Target.LoadState()
	if err != nil {
		return 0, err
	}
	issueFingerprints := map[int]string{}
	for fingerprint, episode := range incidentState.Episodes {
		if episode.Issue != nil && episode.Issue.Number > 0 {
			issueFingerprints[episode.Issue.Number] = fingerprint
		}
	}
	batch := []Signal{}
	for _, event := range events {
		batch = append(batch, signalsFromStateEvent(c.Repository, event, issueFingerprints[event.IssueNumber], c.CloseIssue)...)
	}
	if len(events) > 0 {
		first, last := events[0], events[len(events)-1]
		coverage := Signal{
			Version: SchemaVersion, ID: "coverage-" + OpaqueScopeID(fmt.Sprintf("%s:%s:%d", first.EventID, last.EventID, len(events))), Timestamp: last.Timestamp,
			Repository: c.Repository, CorrelationID: "repo-" + c.Source.RepoID,
			Kind: "status", Name: "evidence_coverage", Component: "retention", Phase: "retention",
			OutcomeCode: "observed", ReasonCode: "state_event_coverage", SourceKind: "state_events",
			OldestAt: &first.Timestamp, NewestAt: &last.Timestamp, RecordCount: int64(len(events)), RetentionPolicy: "configured_event_rotation",
		}
		batch = append(batch, coverage)
	}
	return c.Target.RecordBatch(batch)
}

func signalsFromStateEvent(repository string, event state.Event, incidentFingerprint string, closeIssue bool) []Signal {
	base := Signal{
		Version: SchemaVersion, ID: "state-" + event.EventID, Timestamp: event.Timestamp,
		Repository: repository, CorrelationID: event.RunID, IssueNumber: event.IssueNumber, RunID: event.RunID,
		Kind: "event", Component: "supervisor", Phase: "execute", OutcomeCode: "observed", ReasonCode: event.Type,
		EventSequence: event.Sequence, IncidentFingerprint: incidentFingerprint,
		Evidence: []EvidenceRef{{Source: "state-event", Ref: fmt.Sprintf("seq:%d", event.Sequence)}},
	}
	if base.CorrelationID == "" {
		if event.IssueNumber > 0 {
			base.CorrelationID = "issue-" + strconv.Itoa(event.IssueNumber)
		} else {
			base.CorrelationID = "repo-" + event.RepoID
		}
	}
	payload := map[string]json.RawMessage{}
	_ = json.Unmarshal(event.Payload, &payload)
	stringField := func(name string) string {
		var value string
		_ = json.Unmarshal(payload[name], &value)
		return value
	}
	intField := func(name string) int {
		var value int
		_ = json.Unmarshal(payload[name], &value)
		return value
	}
	timeField := func(name string) *time.Time {
		var value time.Time
		if json.Unmarshal(payload[name], &value) != nil || value.IsZero() {
			return nil
		}
		return &value
	}
	episodeID := ""
	if event.IssueNumber > 0 {
		episodeID = "issue-" + strconv.Itoa(event.IssueNumber)
	} else {
		episodeID = "supervisor-retry-" + OpaqueScopeID(event.RepoID)
	}
	switch event.Type {
	case "supervisor_retry_scheduled", "retry_scheduled", "publication_retry_scheduled", "conflict_recovery_retry_scheduled":
		base.Name, base.OutcomeCode, base.EpisodeID = "retry_episode", "retrying", episodeID
		base.ScopeKind = "repository"
		if event.IssueNumber > 0 {
			base.ScopeKind = "issue"
		}
		base.FailureKind = stringField("failure_kind")
		base.FailureKind = normalizeFailureKind(base.FailureKind, "transient")
		base.FailureCode, base.Attempt, base.RetryAt = event.Type, intField("attempt"), timeField("retry_at")
		if base.Attempt == 0 {
			base.Attempt = intField("attempts")
		}
		if base.Attempt == 0 {
			base.Attempt = 1
		}
		return []Signal{base}
	case "supervisor_blocked", "issue_blocked", "worker_workspace_rejected", "pull_request_checks_retry_exhausted", "publication_failed", "pull_request_lifecycle_blocked":
		base.Name, base.OutcomeCode, base.EpisodeID = "failure_classified", "blocked", episodeID
		base.FailureKind = stringField("failure_kind")
		fallback := "supervisor"
		if event.IssueNumber > 0 {
			fallback = "issue"
		}
		base.FailureKind = normalizeFailureKind(base.FailureKind, fallback)
		base.FailureCode, base.HumanActionRequired = event.Type, true
		base.Component, base.Phase = stateEventBoundary(event.Type)
		result := []Signal{base}
		if event.IssueNumber > 0 {
			lifecycle := base
			lifecycle.ID += "-lifecycle"
			lifecycle.Name, lifecycle.OutcomeCode = "lifecycle_outcome", "failed"
			lifecycle.FailureKind, lifecycle.FailureCode = "", ""
			lifecycle.HumanActionRequired = false
			if event.Type == "pull_request_checks_retry_exhausted" || event.Type == "pull_request_lifecycle_blocked" {
				lifecycle.LifecycleStage, lifecycle.Component, lifecycle.Phase = "checks_failed", "ci", "checks"
			} else {
				lifecycle.LifecycleStage, lifecycle.Component, lifecycle.Phase = "worker_failed", "worker", "execute"
			}
			result = append(result, lifecycle)
		}
		return result
	case "worker_started":
		base.Name, base.OutcomeCode, base.LifecycleStage = "lifecycle_outcome", "started", "worker_started"
	case "pull_request_ready":
		base.Name, base.OutcomeCode, base.LifecycleStage, base.Component, base.Phase = "lifecycle_outcome", "pending", "pull_request_created", "publication", "issue"
		checks := base
		checks.ID += "-checks"
		checks.OutcomeCode, checks.LifecycleStage, checks.Component, checks.Phase = "succeeded", "checks_passed", "ci", "checks"
		return []Signal{base, checks}
	case "awaiting_checks", "awaiting_merge":
		base.Name, base.OutcomeCode, base.LifecycleStage, base.Component, base.Phase = "lifecycle_outcome", "pending", "pull_request_created", "publication", "issue"
	case "pull_request_checks_pending":
		base.Name, base.OutcomeCode, base.LifecycleStage, base.Component, base.Phase = "lifecycle_outcome", "pending", "pull_request_created", "ci", "checks"
	case "issue_completed", "merged_pull_request_adopted":
		base.Name, base.OutcomeCode, base.LifecycleStage, base.Component, base.Phase = "lifecycle_outcome", "succeeded", "merged", "publication", "merge"
		base.ProductFixMerged = incidentFingerprint != ""
	case "github_state_synced":
		if stringField("state") != "done" || !closeIssue {
			return nil
		}
		base.Name, base.OutcomeCode, base.LifecycleStage, base.Component, base.Phase = "lifecycle_outcome", "resolved", "issue_closed", "github", "issue"
	case "pull_request_checks_recovery_requested":
		base.Name, base.OutcomeCode, base.ProgressKind, base.Component, base.Phase = "progress", "observed", "checks_recovery", "ci", "recovery"
	case "pull_request_review_observed":
		base.Name, base.Component, base.Phase = "lifecycle_outcome", "review", "review"
		switch stringField("review_decision") {
		case "APPROVED":
			base.OutcomeCode, base.LifecycleStage = "succeeded", "review_passed"
		case "CHANGES_REQUESTED":
			base.OutcomeCode, base.LifecycleStage, base.HumanActionRequired = "failed", "review_failed", true
		default:
			return nil
		}
	default:
		return nil
	}
	return []Signal{base}
}

func normalizeFailureKind(value, fallback string) string {
	if allowedFailureKinds[value] {
		return value
	}
	return fallback
}

func stateEventBoundary(eventType string) (string, string) {
	switch eventType {
	case "worker_workspace_rejected":
		return "worker", "execute"
	case "pull_request_checks_retry_exhausted", "pull_request_lifecycle_blocked":
		return "ci", "checks"
	case "publication_failed":
		return "publication", "issue"
	default:
		return "supervisor", "execute"
	}
}

func readStateEvents(path string) ([]state.Event, error) {
	var data bytes.Buffer
	if err := retention.WriteHistory(&data, path); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data.Bytes()))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var events []state.Event
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event state.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("decode state event: %w", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

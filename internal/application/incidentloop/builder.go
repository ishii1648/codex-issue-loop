package incidentloop

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
)

func BuildEpisodes(signals []Signal, previous DurableState, rules incidentanalysis.Rules) (DurableState, error) {
	return BuildEpisodesWithLimit(signals, previous, rules, 128)
}

func BuildEpisodesWithLimit(signals []Signal, previous DurableState, rules incidentanalysis.Rules, maxItems int) (DurableState, error) {
	if maxItems < 1 || maxItems > 128 {
		return DurableState{}, errors.New("episode item limit must be between 1 and 128")
	}
	byID := make(map[string]Signal, len(signals))
	for _, signal := range signals {
		if err := signal.Validate(); err != nil {
			return DurableState{}, fmt.Errorf("signal %s: %w", signal.ID, err)
		}
		if prior, exists := byID[signal.ID]; exists && !signalsEqual(prior, signal) {
			return DurableState{}, fmt.Errorf("signal %s was replayed with different content", signal.ID)
		}
		byID[signal.ID] = signal
	}
	ordered := make([]Signal, 0, len(byID))
	for _, signal := range byID {
		ordered = append(ordered, signal)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Timestamp.Equal(ordered[j].Timestamp) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})
	for _, signal := range deriveInvariantSignals(ordered) {
		if err := signal.Validate(); err != nil {
			return DurableState{}, fmt.Errorf("derived signal %s: %w", signal.ID, err)
		}
		if prior, exists := byID[signal.ID]; exists && !signalsEqual(prior, signal) {
			return DurableState{}, fmt.Errorf("derived signal %s conflicts with recorded content", signal.ID)
		}
		byID[signal.ID] = signal
	}
	ordered = ordered[:0]
	for _, signal := range byID {
		ordered = append(ordered, signal)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Timestamp.Equal(ordered[j].Timestamp) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})
	groups := map[string][]Signal{}
	for _, signal := range ordered {
		fingerprint := signalFingerprint(signal)
		groups[fingerprint] = append(groups[fingerprint], signal)
	}
	state := DurableState{Version: SchemaVersion, Revision: previous.Revision, UpdatedAt: previous.UpdatedAt, Episodes: map[string]Episode{}, LastSignal: previous.LastSignal}
	for fingerprint, episode := range previous.Episodes {
		state.Episodes[fingerprint] = episode
	}
	for fingerprint, group := range groups {
		old, exists := previous.Episodes[fingerprint]
		if !containsIncidentSignal(group) && !exists {
			continue
		}
		var episode Episode
		if exists {
			fresh := unseenSignals(group, old)
			if len(fresh) == 0 {
				episode = old
			} else {
				delta, err := buildEpisode(fingerprint, fresh, rules, maxItems)
				if err != nil {
					return DurableState{}, err
				}
				episode, err = mergeEpisode(old, delta, len(fresh), rules, maxItems)
				if err != nil {
					return DurableState{}, err
				}
			}
		} else {
			var err error
			episode, err = buildEpisode(fingerprint, group, rules, maxItems)
			if err != nil {
				return DurableState{}, err
			}
		}
		state.Episodes[fingerprint] = episode
		if state.UpdatedAt.Before(episode.UpdatedAt) {
			state.UpdatedAt = episode.UpdatedAt
		}
	}
	if len(ordered) > 0 {
		state.LastSignal = ordered[len(ordered)-1].ID
	}
	return state, nil
}

func deriveInvariantSignals(signals []Signal) []Signal {
	type cycleKey struct {
		repository string
		cycleID    string
	}
	candidates := map[cycleKey]Signal{}
	for _, signal := range signals {
		if signal.Name != "scheduler_cycle" || signal.CycleID == "" || signal.AttemptAllowed || signal.ScheduledDeadline == nil ||
			!signal.ScheduledDeadline.After(signal.Timestamp) {
			continue
		}
		candidates[cycleKey{repository: signal.Repository, cycleID: signal.CycleID}] = signal
	}
	derived := []Signal{}
	for _, attempt := range signals {
		if attempt.Name != "external_attempt_completed" || attempt.CycleID == "" || attempt.OperationCode != "list_ready_issues" {
			continue
		}
		cycle, ok := candidates[cycleKey{repository: attempt.Repository, cycleID: attempt.CycleID}]
		if !ok || attempt.Timestamp.Before(cycle.Timestamp) || !attempt.Timestamp.Before(*cycle.ScheduledDeadline) {
			continue
		}
		if cycle.RunID != "" && attempt.RunID != "" && cycle.RunID != attempt.RunID {
			continue
		}
		runID := cycle.RunID
		if runID == "" {
			runID = attempt.RunID
		}
		correlationID := cycle.CorrelationID
		if runID != "" {
			correlationID = runID
		}
		scopeHash := cycle.ScopeHash
		if scopeHash == "" {
			scopeHash = OpaqueScopeID(cycle.Repository + ":scheduler:retry-deadline")
		}
		derived = append(derived, Signal{
			Version: SchemaVersion, ID: "inv-" + OpaqueScopeID(cycle.ID+":"+attempt.ID), Timestamp: attempt.Timestamp,
			Repository: cycle.Repository, CorrelationID: correlationID, RunID: runID, CycleID: cycle.CycleID,
			EpisodeID: "retry-deadline-bypass-" + OpaqueScopeID(cycle.Repository),
			Kind:      "event", Name: "failure_classified", Component: "scheduler", Phase: "poll",
			OutcomeCode: "failed", ReasonCode: "retry_deadline_bypass", FailureKind: "product", FailureCode: "retry_deadline_bypass",
			ScheduledDeadline: cycle.ScheduledDeadline, AttemptAllowed: false, OperationCode: "list_ready_issues",
			ScopeKind: "repository", ScopeHash: scopeHash, InvariantViolation: true,
			Evidence: []EvidenceRef{{Source: "incident-signal", Ref: cycle.ID}, {Source: "incident-signal", Ref: attempt.ID}},
		})
	}
	return derived
}

func unseenSignals(signals []Signal, old Episode) []Signal {
	seen := make(map[string]bool, len(old.SignalIDs))
	for _, id := range old.SignalIDs {
		seen[id] = true
	}
	completeHistory := old.OccurrenceCount <= len(old.SignalIDs)
	result := make([]Signal, 0, len(signals))
	for _, signal := range signals {
		if seen[signal.ID] {
			continue
		}
		if !completeHistory && (signal.Timestamp.Before(old.UpdatedAt) || (signal.Timestamp.Equal(old.UpdatedAt) && signal.ID <= old.LastSignalID)) {
			continue
		}
		result = append(result, signal)
	}
	return result
}

func mergeEpisode(old, delta Episode, added int, rules incidentanalysis.Rules, maxItems int) (Episode, error) {
	merged := old
	if delta.StartedAt.Before(merged.StartedAt) {
		merged.StartedAt = delta.StartedAt
	}
	if episodePositionAfter(delta.UpdatedAt, delta.LastSignalID, merged.UpdatedAt, merged.LastSignalID) {
		merged.UpdatedAt = delta.UpdatedAt
		merged.LastSignalID = delta.LastSignalID
	}
	merged.OccurrenceCount += added
	merged.SignalIDs = boundedStrings(uniqueSorted(append(append([]string(nil), old.SignalIDs...), delta.SignalIDs...)), maxItems)
	merged.Evidence = boundedEvidence(uniqueEvidence(append(append([]EvidenceRef(nil), old.Evidence...), delta.Evidence...)), maxItems)
	merged.RunIDs = uniqueSorted(append(append([]string(nil), old.RunIDs...), delta.RunIDs...))
	merged.InvariantCodes = boundedStrings(uniqueSorted(append(append([]string(nil), old.InvariantCodes...), delta.InvariantCodes...)), maxItems)
	merged.Features = mergeFeatures(old.Features, delta.Features)
	if len(merged.RunIDs) >= 2 {
		merged.Features.RepeatedIndependentRuns = true
	}
	merged.Lifecycle = boundedLifecycle(append(append([]LifecycleResult(nil), old.Lifecycle...), delta.Lifecycle...), maxItems)
	merged.State = lifecycleState(merged.Lifecycle, old.State)
	if old.State == "resolved" && len(delta.Lifecycle) == 0 && added > 0 {
		merged.State = "reopened"
	}
	if delta.IssueNumber > 0 && merged.IssueNumber == 0 {
		merged.IssueNumber = delta.IssueNumber
	}
	classification, err := incidentanalysis.Classify(merged.Features, rules)
	if err != nil {
		return Episode{}, fmt.Errorf("classify %s: %w", merged.ID, err)
	}
	merged.PrimaryClassification = classification.Primary
	merged.MatchedRules = classification.MatchedRules
	merged.Confidence = classificationConfidence(merged)
	merged.MissingEvidence = missingEvidence(merged)
	if (merged.PrimaryClassification == "expected_transient" || merged.PrimaryClassification == "degraded") && delta.Features.SuccessfulDomainEvent && !delta.UpdatedAt.Before(old.UpdatedAt) {
		merged.State = "resolved"
	}
	classificationChanged := old.PrimaryClassification != merged.PrimaryClassification
	if classificationChanged || (added > 0 && old.Issue == nil) {
		merged.AI = nil
		merged.Attempts = 0
		merged.NextAttemptAt = nil
		merged.CircuitOpen = false
	}
	if classificationChanged {
		merged.IssueAttempts = 0
		merged.IssueNextAttemptAt = nil
		merged.IssueCircuitOpen = false
	}
	merged.Issue = old.Issue
	return merged, nil
}

func mergeFeatures(left, right incidentanalysis.Features) incidentanalysis.Features {
	retrySeenLeft := left.ConsecutiveFailures > 0
	retrySeenRight := right.ConsecutiveFailures > 0
	retryDeadlineRespected := (retrySeenLeft || retrySeenRight) && (!retrySeenLeft || left.RetryDeadlineRespected) && (!retrySeenRight || right.RetryDeadlineRespected)
	return incidentanalysis.Features{
		CorroboratedProductFix:       left.CorroboratedProductFix || right.CorroboratedProductFix,
		DocumentedInvariantViolation: left.DocumentedInvariantViolation || right.DocumentedInvariantViolation,
		RepeatedIndependentRuns:      left.RepeatedIndependentRuns || right.RepeatedIndependentRuns,
		TypedTransient:               left.TypedTransient || right.TypedTransient,
		RetryDeadlineRespected:       retryDeadlineRespected,
		ConsecutiveFailures:          max(left.ConsecutiveFailures, right.ConsecutiveFailures),
		SuccessfulDomainEvent:        left.SuccessfulDomainEvent || right.SuccessfulDomainEvent,
		RequestAmplification:         left.RequestAmplification || right.RequestAmplification,
		ProgressStalled:              left.ProgressStalled || right.ProgressStalled,
		HumanActionRequired:          left.HumanActionRequired || right.HumanActionRequired,
		PersistenceThresholdExceeded: left.PersistenceThresholdExceeded || right.PersistenceThresholdExceeded,
	}
}

func boundedLifecycle(values []LifecycleResult, limit int) []LifecycleResult {
	seen := map[string]LifecycleResult{}
	for _, value := range values {
		key := value.Timestamp.UTC().Format(time.RFC3339Nano) + "\x00" + value.Stage + "\x00" + value.Outcome + "\x00" + value.Ref
		seen[key] = value
	}
	result := make([]LifecycleResult, 0, len(seen))
	for _, value := range seen {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Timestamp.Equal(result[j].Timestamp) {
			if result[i].Stage == result[j].Stage {
				return result[i].Outcome < result[j].Outcome
			}
			return result[i].Stage < result[j].Stage
		}
		return result[i].Timestamp.Before(result[j].Timestamp)
	})
	if len(result) <= limit {
		return result
	}
	half := limit / 2
	return append(append([]LifecycleResult(nil), result[:half]...), result[len(result)-(limit-half):]...)
}

func lifecycleState(lifecycle []LifecycleResult, fallback string) string {
	state := fallback
	for _, result := range lifecycle {
		switch result.Stage {
		case "merged", "issue_closed", "recovery_completed":
			if result.Outcome == "succeeded" || result.Outcome == "resolved" {
				state = "resolved"
			}
		case "reopened":
			state = "reopened"
		}
	}
	return state
}

func episodePositionAfter(at time.Time, id string, priorAt time.Time, priorID string) bool {
	return at.After(priorAt) || (at.Equal(priorAt) && id > priorID)
}

func containsIncidentSignal(signals []Signal) bool {
	for _, signal := range signals {
		if signal.Name == "retry_episode" || signal.Name == "failure_classified" || signal.InvariantViolation || signal.RequestAmplification || signal.ProgressStalled || signal.HumanActionRequired || signal.ThresholdExceeded {
			return true
		}
		switch signal.OutcomeCode {
		case "failed", "retrying", "blocked", "rejected", "rate_limited":
			return true
		}
		switch signal.LifecycleStage {
		case "worker_failed", "checks_failed", "review_failed", "reopened":
			return true
		}
	}
	return false
}

func buildEpisode(fingerprint string, signals []Signal, rules incidentanalysis.Rules, maxItems int) (Episode, error) {
	first, last := signals[0], signals[len(signals)-1]
	episode := Episode{
		Version: SchemaVersion, ID: "inc-" + fingerprint[:20], Fingerprint: fingerprint,
		Repository: first.Repository, CorrelationID: first.CorrelationID, IssueNumber: first.IssueNumber,
		State: "open", StartedAt: first.Timestamp, UpdatedAt: last.Timestamp,
		OccurrenceCount: len(signals), SignalIDs: make([]string, 0, len(signals)), LastSignalID: last.ID,
		Evidence: []EvidenceRef{}, Lifecycle: []LifecycleResult{},
	}
	runSet := map[string]bool{}
	retryDeadlineRespected := true
	hasRetry := false
	var priorRetryAt *time.Time
	for _, signal := range signals {
		episode.SignalIDs = append(episode.SignalIDs, signal.ID)
		if signal.RunID != "" {
			runSet[signal.RunID] = true
		}
		if signal.IssueNumber > 0 && episode.IssueNumber == 0 {
			episode.IssueNumber = signal.IssueNumber
		}
		episode.Evidence = append(episode.Evidence, signal.Evidence...)
		episode.Evidence = append(episode.Evidence, EvidenceRef{Source: "incident-signal", Ref: signal.ID})
		if signal.FailureKind == "transient" {
			episode.Features.TypedTransient = true
		}
		if signal.InvariantViolation && signal.FailureCode != "" {
			episode.InvariantCodes = append(episode.InvariantCodes, signal.FailureCode)
		}
		if signal.Name == "retry_episode" {
			hasRetry = true
			if priorRetryAt != nil && signal.Timestamp.Before(*priorRetryAt) {
				retryDeadlineRespected = false
				episode.Features.DocumentedInvariantViolation = true
			}
			if signal.RetryAt != nil {
				copy := *signal.RetryAt
				priorRetryAt = &copy
			}
			if signal.Attempt > episode.Features.ConsecutiveFailures {
				episode.Features.ConsecutiveFailures = signal.Attempt
			}
		}
		if signal.OutcomeCode == "succeeded" && signal.Name != "operation_duration" {
			episode.Features.SuccessfulDomainEvent = true
		}
		episode.Features.DocumentedInvariantViolation = episode.Features.DocumentedInvariantViolation || signal.InvariantViolation
		episode.Features.RequestAmplification = episode.Features.RequestAmplification || signal.RequestAmplification
		episode.Features.ProgressStalled = episode.Features.ProgressStalled || signal.ProgressStalled
		episode.Features.HumanActionRequired = episode.Features.HumanActionRequired || signal.HumanActionRequired || signal.OutcomeCode == "blocked"
		episode.Features.PersistenceThresholdExceeded = episode.Features.PersistenceThresholdExceeded || signal.ThresholdExceeded
		episode.Features.CorroboratedProductFix = episode.Features.CorroboratedProductFix || signal.ProductFixMerged
		if signal.IndependentRun {
			episode.Features.RepeatedIndependentRuns = true
		}
		if signal.Name == "lifecycle_outcome" {
			episode.Lifecycle = append(episode.Lifecycle, LifecycleResult{Stage: signal.LifecycleStage, Outcome: signal.OutcomeCode, Timestamp: signal.Timestamp, Ref: firstEvidenceRef(signal.Evidence)})
			switch signal.LifecycleStage {
			case "merged", "issue_closed", "recovery_completed":
				if signal.OutcomeCode == "succeeded" || signal.OutcomeCode == "resolved" {
					episode.State = "resolved"
				}
			case "reopened":
				episode.State = "reopened"
			}
		}
	}
	episode.SignalIDs = boundedStrings(uniqueSorted(episode.SignalIDs), maxItems)
	episode.Evidence = boundedEvidence(uniqueEvidence(episode.Evidence), maxItems)
	for runID := range runSet {
		episode.RunIDs = append(episode.RunIDs, runID)
	}
	sort.Strings(episode.RunIDs)
	episode.InvariantCodes = boundedStrings(uniqueSorted(episode.InvariantCodes), maxItems)
	if len(episode.RunIDs) >= 2 {
		episode.Features.RepeatedIndependentRuns = true
	}
	episode.Features.RetryDeadlineRespected = hasRetry && retryDeadlineRespected
	classification, err := incidentanalysis.Classify(episode.Features, rules)
	if err != nil {
		return Episode{}, fmt.Errorf("classify %s: %w", episode.ID, err)
	}
	episode.PrimaryClassification = classification.Primary
	episode.MatchedRules = classification.MatchedRules
	episode.Confidence = classificationConfidence(episode)
	episode.MissingEvidence = missingEvidence(episode)
	if (episode.PrimaryClassification == "expected_transient" || episode.PrimaryClassification == "degraded") && episode.Features.SuccessfulDomainEvent {
		episode.State = "resolved"
	}
	episode.Lifecycle = boundedLifecycle(episode.Lifecycle, maxItems)
	return episode, nil
}

func signalFingerprint(signal Signal) string {
	if signal.IncidentFingerprint != "" {
		return signal.IncidentFingerprint
	}
	if signal.EpisodeID != "" {
		return digest("episode", signal.Repository, signal.EpisodeID)
	}
	scope := signal.ScopeHash
	if scope == "" {
		scope = signal.CorrelationID
	}
	cause := signal.FailureCode
	if cause == "" {
		cause = signal.OperationCode
	}
	if cause == "" {
		cause = signal.ReasonCode
	}
	return digest(signal.Repository, scope, signal.Component, signal.Phase, cause)
}

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// OpaqueScopeID returns a stable bounded identifier without persisting the
// repository-local scope value in signals or metrics.
func OpaqueScopeID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func classificationConfidence(episode Episode) string {
	switch episode.PrimaryClassification {
	case "confirmed_bug":
		return "high"
	case "suspected_bug", "operator_attention", "degraded":
		return "medium"
	case "expected_transient":
		if episode.Features.SuccessfulDomainEvent {
			return "high"
		}
	}
	return "low"
}

func missingEvidence(episode Episode) []string {
	if episode.PrimaryClassification != "unknown" {
		return nil
	}
	missing := []string{}
	if !episode.Features.TypedTransient {
		missing = append(missing, "typed failure kind")
	}
	if !episode.Features.SuccessfulDomainEvent {
		missing = append(missing, "successful domain outcome")
	}
	if !episode.Features.DocumentedInvariantViolation {
		missing = append(missing, "documented invariant result")
	}
	if !episode.Features.RepeatedIndependentRuns {
		missing = append(missing, "independent reproduction")
	}
	return missing
}

func uniqueEvidence(values []EvidenceRef) []EvidenceRef {
	seen := map[string]EvidenceRef{}
	for _, value := range values {
		if value.Source != "" && value.Ref != "" {
			seen[value.Source+"\x00"+value.Ref] = value
		}
	}
	result := make([]EvidenceRef, 0, len(seen))
	for _, value := range seen {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Source == result[j].Source {
			return result[i].Ref < result[j].Ref
		}
		return result[i].Source < result[j].Source
	})
	return result
}

func firstEvidenceRef(evidence []EvidenceRef) string {
	if len(evidence) == 0 {
		return ""
	}
	return evidence[0].Ref
}

func boundedStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	half := limit / 2
	return append(append([]string(nil), values[:half]...), values[len(values)-(limit-half):]...)
}

func boundedEvidence(values []EvidenceRef, limit int) []EvidenceRef {
	if len(values) <= limit {
		return values
	}
	half := limit / 2
	return append(append([]EvidenceRef(nil), values[:half]...), values[len(values)-(limit-half):]...)
}

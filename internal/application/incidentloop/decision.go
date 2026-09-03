package incidentloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

const DecisionRetention = 7 * 24 * time.Hour

var (
	allowedDecisionOutcomes = stringSet("skipped", "dry_run", "created", "reused", "failed")
	allowedDecisionReasons  = stringSet(
		"episode_resolved",
		"expected_transient",
		"analysis_retry_scheduled",
		"analysis_circuit_open",
		"analysis_backoff",
		"analysis_missing",
		"ai_did_not_recommend",
		"deterministic_confidence_low",
		"ai_confidence_low",
		"classification_not_issue_eligible",
		"documented_invariant_missing",
		"corroborated_product_fix_missing",
		"independent_reproduction_missing",
		"eligible_dry_run",
		"issue_created",
		"issue_reused",
		"issue_already_linked",
		"issue_retry_scheduled",
		"issue_circuit_open",
		"issue_backoff",
	)
)

// IssueDecision is the durable audit record for one episode's Issue decision in
// one pipeline cycle. Existing records are immutable; retention may only remove
// complete records whose decided_at is strictly older than DecisionRetention.
type IssueDecision struct {
	Version                  int       `json:"version"`
	ID                       string    `json:"id"`
	DecidedAt                time.Time `json:"decided_at"`
	Repository               string    `json:"repository"`
	EpisodeID                string    `json:"episode_id"`
	Fingerprint              string    `json:"fingerprint"`
	Classification           string    `json:"classification"`
	ClassificationConfidence string    `json:"classification_confidence"`
	MatchedRules             []string  `json:"matched_rules"`
	InvariantCodes           []string  `json:"invariant_codes"`
	AIRecommendation         *bool     `json:"ai_recommendation,omitempty"`
	AIConfidence             string    `json:"ai_confidence,omitempty"`
	Eligible                 bool      `json:"eligible"`
	Outcome                  string    `json:"outcome"`
	ReasonCode               string    `json:"reason_code"`
	Issue                    *IssueRef `json:"issue,omitempty"`
}

func newIssueDecision(at time.Time, episode Episode, eligible bool, outcome, reason string, issue *IssueRef) IssueDecision {
	decision := IssueDecision{
		Version:                  SchemaVersion,
		ID:                       "dec-" + digest("issue-decision", episode.Repository, episode.Fingerprint, at.UTC().Format(time.RFC3339Nano), outcome, reason),
		DecidedAt:                at.UTC(),
		Repository:               episode.Repository,
		EpisodeID:                episode.ID,
		Fingerprint:              episode.Fingerprint,
		Classification:           episode.PrimaryClassification,
		ClassificationConfidence: episode.Confidence,
		MatchedRules:             append([]string{}, episode.MatchedRules...),
		InvariantCodes:           append([]string{}, episode.InvariantCodes...),
		Eligible:                 eligible,
		Outcome:                  outcome,
		ReasonCode:               reason,
	}
	if episode.AI != nil {
		recommendation := episode.AI.RecommendIssue
		decision.AIRecommendation = &recommendation
		decision.AIConfidence = episode.AI.Confidence
	}
	if issue != nil {
		copy := *issue
		copy.Labels = append([]string{}, issue.Labels...)
		decision.Issue = &copy
	}
	return decision
}

func (d IssueDecision) Validate() error {
	if d.Version != SchemaVersion || !identifierPattern.MatchString(d.ID) || d.DecidedAt.IsZero() || !repositoryPattern.MatchString(d.Repository) {
		return errors.New("Issue decision requires version 1, bounded ID, timestamp, and owner/name repository")
	}
	expectedID := "dec-" + digest("issue-decision", d.Repository, d.Fingerprint, d.DecidedAt.UTC().Format(time.RFC3339Nano), d.Outcome, d.ReasonCode)
	if d.ID != expectedID {
		return errors.New("Issue decision ID does not match its immutable identity fields")
	}
	if !strings.HasPrefix(d.EpisodeID, "inc-") || len(d.EpisodeID) != 24 || !identifierPattern.MatchString(d.EpisodeID) || !validFingerprint(d.Fingerprint) {
		return errors.New("Issue decision has invalid episode or fingerprint identity")
	}
	if !allowedClassifications[d.Classification] || !allowedConfidence[d.ClassificationConfidence] {
		return errors.New("Issue decision has invalid classification or confidence")
	}
	if d.MatchedRules == nil || d.InvariantCodes == nil || len(d.MatchedRules) > 128 || len(d.InvariantCodes) > 128 {
		return errors.New("Issue decision requires bounded rule and invariant arrays")
	}
	for _, values := range [][]string{d.MatchedRules, d.InvariantCodes} {
		seen := map[string]bool{}
		for _, value := range values {
			if !identifierPattern.MatchString(value) || seen[value] {
				return errors.New("Issue decision has invalid rule or invariant code")
			}
			seen[value] = true
		}
	}
	if (d.AIRecommendation == nil) != (d.AIConfidence == "") {
		return errors.New("Issue decision AI recommendation and confidence must be present together")
	}
	if d.AIConfidence != "" && !allowedConfidence[d.AIConfidence] {
		return errors.New("Issue decision has invalid AI confidence")
	}
	if !allowedDecisionOutcomes[d.Outcome] || !allowedDecisionReasons[d.ReasonCode] {
		return errors.New("Issue decision has invalid outcome or reason code")
	}
	if d.Eligible != (d.Outcome == "dry_run" || d.Outcome == "created" || d.Outcome == "reused" || strings.HasPrefix(d.ReasonCode, "issue_")) {
		return errors.New("Issue decision eligibility is inconsistent with outcome")
	}
	if d.Outcome == "dry_run" && d.ReasonCode != "eligible_dry_run" || d.Outcome == "created" && d.ReasonCode != "issue_created" || d.Outcome == "reused" && d.ReasonCode != "issue_reused" {
		return errors.New("Issue decision outcome and reason code are inconsistent")
	}
	if d.Issue != nil {
		if d.Issue.Fingerprint != d.Fingerprint || len(d.Issue.Labels) == 0 {
			return errors.New("Issue decision Issue fingerprint or labels are invalid")
		}
		if !stringSet("dry_run", "created", "existing", "read_back")[d.Issue.Status] {
			return errors.New("Issue decision has invalid Issue status")
		}
		if d.Issue.Status != "dry_run" && (d.Issue.Number < 1 || strings.TrimSpace(d.Issue.URL) == "") {
			return errors.New("Issue decision live Issue requires number and URL")
		}
	}
	if (d.Outcome == "created" || d.Outcome == "reused" || d.ReasonCode == "issue_already_linked") && d.Issue == nil {
		return errors.New("Issue decision outcome requires an Issue reference")
	}
	return rejectSensitive(d)
}

func sanitizeDecision(decision IssueDecision, secrets []string) (IssueDecision, error) {
	raw, err := redact.Marshal(decision, secrets)
	if err != nil {
		return IssueDecision{}, err
	}
	var safe IssueDecision
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&safe); err != nil {
		return IssueDecision{}, err
	}
	if err := safe.Validate(); err != nil {
		return IssueDecision{}, err
	}
	return safe, nil
}

func decisionsEqual(left, right IssueDecision) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}

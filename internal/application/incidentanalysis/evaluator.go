package incidentanalysis

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var primaryClassifications = []string{
	"expected_transient",
	"degraded",
	"operator_attention",
	"suspected_bug",
	"confirmed_bug",
	"unknown",
}

type Corpus struct {
	Version         int        `json:"version"`
	GeneratedAt     string     `json:"generated_at"`
	SelectionPolicy string     `json:"selection_policy"`
	Incidents       []Incident `json:"incidents"`
}

type Incident struct {
	ID                  string      `json:"id"`
	Window              TimeWindow  `json:"window"`
	FirstSignal         Evidence    `json:"first_signal"`
	EventTypes          []string    `json:"event_types"`
	Statuses            []string    `json:"statuses"`
	Links               Links       `json:"links"`
	Symptom             string      `json:"symptom"`
	RootCause           string      `json:"root_cause"`
	Recovery            string      `json:"recovery"`
	ProductCodeChange   string      `json:"product_code_change"`
	Confidence          string      `json:"confidence"`
	Fingerprint         string      `json:"fingerprint"`
	Evidence            []Evidence  `json:"evidence"`
	Features            Features    `json:"features"`
	GroundTruth         GroundTruth `json:"ground_truth"`
	SecondaryAttributes []string    `json:"secondary_attributes"`
	MissingEvidence     []string    `json:"missing_evidence"`
	WouldResolveWith    []string    `json:"would_resolve_with"`
}

type TimeWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

type Evidence struct {
	Source string `json:"source"`
	Ref    string `json:"ref"`
}

type Links struct {
	Issues  []string `json:"issues"`
	PRs     []string `json:"pull_requests"`
	Commits []string `json:"commits"`
}

type Features struct {
	CorroboratedProductFix       bool `json:"corroborated_product_fix"`
	DocumentedInvariantViolation bool `json:"documented_invariant_violation"`
	RepeatedIndependentRuns      bool `json:"repeated_independent_runs"`
	TypedTransient               bool `json:"typed_transient"`
	RetryDeadlineRespected       bool `json:"retry_deadline_respected"`
	ConsecutiveFailures          int  `json:"consecutive_failures"`
	SuccessfulDomainEvent        bool `json:"successful_domain_event"`
	RequestAmplification         bool `json:"request_amplification"`
	ProgressStalled              bool `json:"progress_stalled"`
	HumanActionRequired          bool `json:"human_action_required"`
	PersistenceThresholdExceeded bool `json:"persistence_threshold_exceeded"`
}

type GroundTruth struct {
	PrimaryClassification string `json:"primary_classification"`
	Basis                 string `json:"basis"`
}

type Rules struct {
	Version                int                        `json:"version"`
	PrimaryClassifications []string                   `json:"primary_classifications"`
	Classifications        []ClassificationDefinition `json:"classifications"`
	ErrorStates            []ErrorStateDefinition     `json:"error_states"`
	Rules                  []Rule                     `json:"rules"`
}

type ClassificationDefinition struct {
	Name                 string   `json:"name"`
	Definition           string   `json:"definition"`
	EntryCondition       string   `json:"entry_condition"`
	ExitCondition        string   `json:"exit_condition"`
	PersistenceThreshold string   `json:"persistence_threshold"`
	Severity             string   `json:"severity"`
	RequiredEvidence     []string `json:"required_evidence"`
	Exclusions           []string `json:"exclusions"`
	RetryRelation        string   `json:"retry_relation"`
	OperatorAction       string   `json:"operator_action"`
	AIHandoff            string   `json:"ai_handoff"`
	IssueCondition       string   `json:"issue_condition"`
	FingerprintFields    []string `json:"fingerprint_fields"`
	Representative       []string `json:"representative_incidents"`
	DeterministicNow     bool     `json:"deterministic_with_current_signals"`
}

type ErrorStateDefinition struct {
	Name                 string   `json:"name"`
	Definition           string   `json:"definition"`
	EntryCondition       string   `json:"entry_condition"`
	ExitCondition        string   `json:"exit_condition"`
	PersistenceThreshold string   `json:"persistence_threshold"`
	Severity             string   `json:"severity"`
	RequiredEvidence     []string `json:"required_evidence"`
	Exclusions           []string `json:"exclusions"`
	RetryRelation        string   `json:"retry_relation"`
	OperatorAction       string   `json:"operator_action"`
	AIHandoff            string   `json:"ai_handoff"`
	IssueCondition       string   `json:"issue_condition"`
	FingerprintFields    []string `json:"fingerprint_fields"`
	Representative       []string `json:"representative_incidents"`
	DeterministicNow     bool     `json:"deterministic_with_current_signals"`
}

type Rule struct {
	ID             string      `json:"id"`
	Priority       int         `json:"priority"`
	Classification string      `json:"classification"`
	Description    string      `json:"description"`
	Always         bool        `json:"always,omitempty"`
	All            []Predicate `json:"all,omitempty"`
}

type Predicate struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
}

type Evaluation struct {
	Version                     int            `json:"version"`
	CorpusVersion               int            `json:"corpus_version"`
	RulesVersion                int            `json:"rules_version"`
	Total                       int            `json:"incident_total"`
	Classified                  int            `json:"classified"`
	Unknown                     int            `json:"unknown"`
	ByClassification            map[string]int `json:"by_classification"`
	ConfirmedBug                Metrics        `json:"confirmed_bug"`
	ExpectedTransient           Metrics        `json:"expected_transient"`
	RuleConflicts               []string       `json:"rule_conflicts"`
	Mismatches                  []Mismatch     `json:"mismatches"`
	InsufficientEvidence        []string       `json:"insufficient_evidence"`
	ResolvableWithObservability []string       `json:"resolvable_with_observability"`
	Results                     []Result       `json:"results"`
}

type Metrics struct {
	GroundTruth   int `json:"ground_truth"`
	Predicted     int `json:"predicted"`
	TruePositive  int `json:"true_positive"`
	FalsePositive int `json:"false_positive"`
	FalseNegative int `json:"false_negative"`
	Unassessable  int `json:"unassessable"`
}

type Result struct {
	IncidentID     string   `json:"incident_id"`
	GroundTruth    string   `json:"ground_truth"`
	Classification string   `json:"classification"`
	MatchedRules   []string `json:"matched_rules"`
}

type Mismatch struct {
	IncidentID  string `json:"incident_id"`
	GroundTruth string `json:"ground_truth"`
	Predicted   string `json:"predicted"`
}

type Classification struct {
	Primary      string   `json:"primary_classification"`
	MatchedRules []string `json:"matched_rules"`
}

type SourceInventory struct {
	Version     int             `json:"version"`
	RetrievedAt string          `json:"retrieved_at"`
	Sources     []SourceSummary `json:"sources"`
}

type SourceSummary struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	PeriodStart *string  `json:"period_start"`
	PeriodEnd   *string  `json:"period_end"`
	Count       int      `json:"count"`
	Unit        string   `json:"unit"`
	Query       string   `json:"query"`
	Gaps        []string `json:"gaps"`
	Constraints []string `json:"constraints"`
}

type Coverage struct {
	Version                       int              `json:"version"`
	Population                    string           `json:"population"`
	Total                         int              `json:"total"`
	CorroboratedWithMergedFix     int              `json:"corroborated_with_merged_fix"`
	Unknown                       int              `json:"unknown"`
	Records                       []CoverageRecord `json:"records"`
	AdditionalNonBugLabelEvidence []CoverageRecord `json:"additional_non_bug_label_ground_truth"`
}

type CoverageRecord struct {
	Issue       int     `json:"issue"`
	PullRequest *int    `json:"pull_request"`
	MergeCommit *string `json:"merge_commit"`
	Disposition string  `json:"disposition,omitempty"`
	IncidentID  *string `json:"incident_id"`
	Reason      string  `json:"reason,omitempty"`
}

type ObservabilityPlan struct {
	Version int                `json:"version"`
	Gaps    []ObservabilityGap `json:"gaps"`
}

type ObservabilityGap struct {
	ID                     string   `json:"id"`
	IncidentRefs           []string `json:"incident_refs"`
	InvariantRefs          []string `json:"invariant_refs"`
	CurrentMissingEvidence []string `json:"current_missing_evidence"`
	Signal                 Signal   `json:"signal"`
	Enables                []string `json:"enables"`
	Security               string   `json:"security"`
	Cardinality            string   `json:"cardinality"`
	Retention              string   `json:"retention"`
}

type Signal struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	Fields []string `json:"fields"`
}

func Load(corpusPath, rulesPath string) (Corpus, Rules, error) {
	var corpus Corpus
	if err := decodeStrict(corpusPath, &corpus); err != nil {
		return Corpus{}, Rules{}, err
	}
	var rules Rules
	if err := decodeStrict(rulesPath, &rules); err != nil {
		return Corpus{}, Rules{}, err
	}
	if err := Validate(corpus, rules); err != nil {
		return Corpus{}, Rules{}, err
	}
	return corpus, rules, nil
}

func LoadRules(path string) (Rules, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Rules{}, err
	}
	return ParseRules(raw)
}

func ParseRules(raw []byte) (Rules, error) {
	var rules Rules
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return Rules{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Rules{}, errors.New("incident rules contain trailing JSON")
	}
	if rules.Version != 1 || !sameStrings(rules.PrimaryClassifications, primaryClassifications) {
		return Rules{}, errors.New("only the complete incident rules version 1 contract is supported")
	}
	if err := validateDefinitions(rules); err != nil {
		return Rules{}, err
	}
	if err := validateRules(rules.Rules); err != nil {
		return Rules{}, err
	}
	return rules, nil
}

func ValidateBundle(dataDir string, corpus Corpus) error {
	var inventory SourceInventory
	if err := decodeStrict(filepath.Join(dataDir, "source-inventory.json"), &inventory); err != nil {
		return err
	}
	if err := validateInventory(inventory); err != nil {
		return err
	}
	var coverage Coverage
	if err := decodeStrict(filepath.Join(dataDir, "coverage.json"), &coverage); err != nil {
		return err
	}
	if err := validateCoverage(coverage, corpus); err != nil {
		return err
	}
	var observability ObservabilityPlan
	if err := decodeStrict(filepath.Join(dataDir, "observability-gaps.json"), &observability); err != nil {
		return err
	}
	if err := validateObservability(observability, corpus); err != nil {
		return err
	}
	schemas := []string{
		"incident-corpus.schema.json",
		"incident-rules.schema.json",
		"incident-source-inventory.schema.json",
		"incident-coverage.schema.json",
		"incident-observability.schema.json",
		"incident-evaluation.schema.json",
		"incident-signal.schema.json",
		"incident-state.schema.json",
		"incident-metrics.schema.json",
		"incident-ai-analysis.schema.json",
		"incident-issue-payload.schema.json",
		"incident-decision.schema.json",
	}
	for _, name := range schemas {
		path := filepath.Join("schemas", name)
		var schema map[string]any
		if err := decodeStrict(path, &schema); err != nil {
			return err
		}
		if schema["$schema"] != "https://json-schema.org/draft/2020-12/schema" || strings.TrimSpace(fmt.Sprint(schema["title"])) == "" {
			return fmt.Errorf("schema %s has invalid dialect or title", path)
		}
	}
	return nil
}

func validateObservability(plan ObservabilityPlan, corpus Corpus) error {
	if plan.Version != 1 || len(plan.Gaps) == 0 {
		return errors.New("observability plan version 1 and gaps are required")
	}
	incidentIDs := map[string]bool{}
	for _, incident := range corpus.Incidents {
		incidentIDs[incident.ID] = true
	}
	seen := map[string]bool{}
	for _, gap := range plan.Gaps {
		if strings.TrimSpace(gap.ID) == "" || seen[gap.ID] || len(gap.IncidentRefs)+len(gap.InvariantRefs) == 0 || len(gap.CurrentMissingEvidence) == 0 || strings.TrimSpace(gap.Signal.Kind) == "" || strings.TrimSpace(gap.Signal.Name) == "" || len(gap.Signal.Fields) == 0 || len(gap.Enables) == 0 || strings.TrimSpace(gap.Security) == "" || strings.TrimSpace(gap.Cardinality) == "" || strings.TrimSpace(gap.Retention) == "" {
			return fmt.Errorf("invalid observability gap %q", gap.ID)
		}
		seen[gap.ID] = true
		for _, incidentID := range gap.IncidentRefs {
			if !incidentIDs[incidentID] {
				return fmt.Errorf("observability gap %s references unknown incident %s", gap.ID, incidentID)
			}
		}
	}
	return rejectSensitive(plan)
}

func decodeStrict(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", path)
		}
		return fmt.Errorf("decode %s trailing content: %w", path, err)
	}
	return nil
}

func validateInventory(inventory SourceInventory) error {
	if inventory.Version != 1 {
		return errors.New("source inventory version must be 1")
	}
	if _, err := time.Parse(time.RFC3339, inventory.RetrievedAt); err != nil {
		return fmt.Errorf("source inventory retrieved_at: %w", err)
	}
	if len(inventory.Sources) == 0 {
		return errors.New("source inventory is empty")
	}
	seen := map[string]bool{}
	for _, source := range inventory.Sources {
		if strings.TrimSpace(source.ID) == "" || seen[source.ID] || strings.TrimSpace(source.Kind) == "" || strings.TrimSpace(source.Unit) == "" || strings.TrimSpace(source.Query) == "" || source.Count < 0 || len(source.Gaps) == 0 || len(source.Constraints) == 0 {
			return fmt.Errorf("invalid source inventory record %q", source.ID)
		}
		seen[source.ID] = true
		for _, value := range []*string{source.PeriodStart, source.PeriodEnd} {
			if value != nil {
				if _, err := time.Parse(time.RFC3339Nano, *value); err != nil {
					return fmt.Errorf("source %s period: %w", source.ID, err)
				}
			}
		}
	}
	return rejectSensitive(inventory)
}

func validateCoverage(coverage Coverage, corpus Corpus) error {
	if coverage.Version != 1 || coverage.Total != 34 || len(coverage.Records) != coverage.Total || coverage.CorroboratedWithMergedFix+coverage.Unknown != coverage.Total {
		return errors.New("coverage totals must describe all 34 bug-labelled Issues")
	}
	incidentIDs := map[string]bool{}
	for _, incident := range corpus.Incidents {
		incidentIDs[incident.ID] = true
	}
	issues := map[int]bool{}
	corroborated, unknown := 0, 0
	for _, record := range coverage.Records {
		if record.Issue <= 0 || issues[record.Issue] {
			return fmt.Errorf("invalid or duplicate coverage issue %d", record.Issue)
		}
		issues[record.Issue] = true
		switch record.Disposition {
		case "included", "corroborated_not_sampled":
			corroborated++
			if record.PullRequest == nil || record.MergeCommit == nil || len(*record.MergeCommit) != 40 {
				return fmt.Errorf("coverage issue %d lacks corroborating fix", record.Issue)
			}
		case "unknown_included":
			unknown++
			if record.PullRequest != nil || record.MergeCommit != nil {
				return fmt.Errorf("unknown coverage issue %d unexpectedly has fix evidence", record.Issue)
			}
		default:
			return fmt.Errorf("coverage issue %d has invalid disposition %q", record.Issue, record.Disposition)
		}
		if strings.Contains(record.Disposition, "included") {
			if record.IncidentID == nil || !incidentIDs[*record.IncidentID] {
				return fmt.Errorf("coverage issue %d has no corpus record", record.Issue)
			}
		}
	}
	if corroborated != coverage.CorroboratedWithMergedFix || unknown != coverage.Unknown {
		return errors.New("coverage disposition counts do not match totals")
	}
	if len(coverage.AdditionalNonBugLabelEvidence) != 1 || coverage.AdditionalNonBugLabelEvidence[0].Issue != 102 || coverage.AdditionalNonBugLabelEvidence[0].IncidentID == nil || !incidentIDs[*coverage.AdditionalNonBugLabelEvidence[0].IncidentID] {
		return errors.New("coverage must include non-bug-labelled Issue 102 ground truth")
	}
	return rejectSensitive(coverage)
}

func rejectSensitive(value any) error {
	raw, _ := json.Marshal(value)
	for _, marker := range []string{"/Users/", "ghp_", "github_pat_", "Bearer ", "\"token\":"} {
		if bytes.Contains(raw, []byte(marker)) {
			return fmt.Errorf("prohibited raw or secret marker %q", marker)
		}
	}
	return nil
}

func Validate(corpus Corpus, rules Rules) error {
	if corpus.Version != 1 || rules.Version != 1 {
		return errors.New("only corpus/rules version 1 is supported")
	}
	if _, err := time.Parse(time.RFC3339, corpus.GeneratedAt); err != nil {
		return fmt.Errorf("generated_at: %w", err)
	}
	if strings.TrimSpace(corpus.SelectionPolicy) == "" || len(corpus.Incidents) == 0 {
		return errors.New("selection_policy and incidents are required")
	}
	if !sameStrings(rules.PrimaryClassifications, primaryClassifications) {
		return fmt.Errorf("primary classifications must be exactly %v", primaryClassifications)
	}
	if err := validateDefinitions(rules); err != nil {
		return err
	}
	if err := validateRules(rules.Rules); err != nil {
		return err
	}
	ids := map[string]struct{}{}
	for i, incident := range corpus.Incidents {
		if err := validateIncident(incident); err != nil {
			return fmt.Errorf("incidents[%d]: %w", i, err)
		}
		if _, exists := ids[incident.ID]; exists {
			return fmt.Errorf("duplicate incident id %q", incident.ID)
		}
		ids[incident.ID] = struct{}{}
	}
	return nil
}

func validateDefinitions(rules Rules) error {
	seen := map[string]bool{}
	for _, definition := range rules.Classifications {
		if !isClassification(definition.Name) || seen[definition.Name] {
			return fmt.Errorf("invalid or duplicate classification definition %q", definition.Name)
		}
		seen[definition.Name] = true
		if err := validateDefinitionFields(definition.Definition, definition.EntryCondition, definition.ExitCondition, definition.PersistenceThreshold, definition.Severity, definition.RetryRelation, definition.OperatorAction, definition.AIHandoff, definition.IssueCondition, definition.RequiredEvidence, definition.Exclusions, definition.FingerprintFields, definition.Representative); err != nil {
			return fmt.Errorf("classification %s: %w", definition.Name, err)
		}
	}
	for _, name := range primaryClassifications {
		if !seen[name] {
			return fmt.Errorf("missing classification definition %q", name)
		}
	}
	stateNames := map[string]bool{}
	for _, state := range rules.ErrorStates {
		if strings.TrimSpace(state.Name) == "" || stateNames[state.Name] {
			return fmt.Errorf("invalid or duplicate error state %q", state.Name)
		}
		stateNames[state.Name] = true
		if err := validateDefinitionFields(state.Definition, state.EntryCondition, state.ExitCondition, state.PersistenceThreshold, state.Severity, state.RetryRelation, state.OperatorAction, state.AIHandoff, state.IssueCondition, state.RequiredEvidence, state.Exclusions, state.FingerprintFields, state.Representative); err != nil {
			return fmt.Errorf("error state %s: %w", state.Name, err)
		}
	}
	if len(stateNames) == 0 {
		return errors.New("at least one error state is required")
	}
	return nil
}

func validateDefinitionFields(values ...any) error {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if strings.TrimSpace(typed) == "" {
				return errors.New("definition field is empty")
			}
		case []string:
			if len(typed) == 0 {
				return errors.New("definition list is empty")
			}
		}
	}
	return nil
}

func validateRules(rules []Rule) error {
	if len(rules) == 0 {
		return errors.New("rules are required")
	}
	ids := map[string]bool{}
	priorities := map[int]string{}
	fallbacks := 0
	for _, rule := range rules {
		if strings.TrimSpace(rule.ID) == "" || ids[rule.ID] {
			return fmt.Errorf("invalid or duplicate rule id %q", rule.ID)
		}
		ids[rule.ID] = true
		if other, exists := priorities[rule.Priority]; exists {
			return fmt.Errorf("rule priority conflict %d: %s and %s", rule.Priority, other, rule.ID)
		}
		priorities[rule.Priority] = rule.ID
		if !isClassification(rule.Classification) {
			return fmt.Errorf("rule %s has invalid classification %q", rule.ID, rule.Classification)
		}
		if rule.Always {
			fallbacks++
			if rule.Classification != "unknown" || len(rule.All) != 0 {
				return fmt.Errorf("rule %s: only an unconditional unknown fallback is allowed", rule.ID)
			}
		} else if len(rule.All) == 0 {
			return fmt.Errorf("rule %s has no predicates", rule.ID)
		}
		for _, predicate := range rule.All {
			if err := validatePredicate(predicate); err != nil {
				return fmt.Errorf("rule %s: %w", rule.ID, err)
			}
		}
	}
	if fallbacks != 1 {
		return fmt.Errorf("exactly one unknown fallback is required, got %d", fallbacks)
	}
	return nil
}

func validatePredicate(predicate Predicate) error {
	known := map[string]string{
		"corroborated_product_fix": "bool", "documented_invariant_violation": "bool",
		"repeated_independent_runs": "bool", "typed_transient": "bool",
		"retry_deadline_respected": "bool", "consecutive_failures": "number",
		"successful_domain_event": "bool", "request_amplification": "bool",
		"progress_stalled": "bool", "human_action_required": "bool",
		"persistence_threshold_exceeded": "bool",
	}
	kind, ok := known[predicate.Field]
	if !ok {
		return fmt.Errorf("unknown feature %q", predicate.Field)
	}
	if predicate.Op != "eq" && predicate.Op != "lte" && predicate.Op != "gte" {
		return fmt.Errorf("unsupported operator %q", predicate.Op)
	}
	if kind == "bool" && predicate.Op != "eq" {
		return fmt.Errorf("boolean feature %q only supports eq", predicate.Field)
	}
	var value any
	if err := json.Unmarshal(predicate.Value, &value); err != nil {
		return fmt.Errorf("invalid predicate value: %w", err)
	}
	if kind == "bool" {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("feature %q requires boolean value", predicate.Field)
		}
	} else if _, ok := value.(float64); !ok {
		return fmt.Errorf("feature %q requires numeric value", predicate.Field)
	}
	return nil
}

func validateIncident(incident Incident) error {
	if strings.TrimSpace(incident.ID) == "" || strings.TrimSpace(incident.Fingerprint) == "" {
		return errors.New("id and fingerprint are required")
	}
	start, err := time.Parse(time.RFC3339, incident.Window.Start)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	end, err := time.Parse(time.RFC3339, incident.Window.End)
	if err != nil || end.Before(start) {
		return errors.New("end must be RFC3339 and not before start")
	}
	if !isClassification(incident.GroundTruth.PrimaryClassification) {
		return fmt.Errorf("invalid ground truth %q", incident.GroundTruth.PrimaryClassification)
	}
	if strings.TrimSpace(incident.GroundTruth.Basis) == "" || strings.TrimSpace(incident.Symptom) == "" || strings.TrimSpace(incident.RootCause) == "" || strings.TrimSpace(incident.Recovery) == "" {
		return errors.New("symptom, root cause, recovery, and ground-truth basis are required")
	}
	if incident.ProductCodeChange != "merged" && incident.ProductCodeChange != "none" && incident.ProductCodeChange != "unknown" {
		return fmt.Errorf("invalid product_code_change %q", incident.ProductCodeChange)
	}
	if incident.Confidence != "high" && incident.Confidence != "medium" && incident.Confidence != "low" {
		return fmt.Errorf("invalid confidence %q", incident.Confidence)
	}
	if len(incident.EventTypes) == 0 || len(incident.Statuses) == 0 || len(incident.Evidence) == 0 || strings.TrimSpace(incident.FirstSignal.Source) == "" || strings.TrimSpace(incident.FirstSignal.Ref) == "" {
		return errors.New("event types, statuses, first signal, and evidence are required")
	}
	if incident.GroundTruth.PrimaryClassification == "unknown" && len(incident.MissingEvidence) == 0 {
		return errors.New("unknown incident requires missing_evidence")
	}
	if incident.GroundTruth.PrimaryClassification == "confirmed_bug" && (!incident.Features.CorroboratedProductFix || !incident.Features.DocumentedInvariantViolation || incident.ProductCodeChange != "merged") {
		return errors.New("confirmed_bug requires an invariant violation and corroborated merged product fix")
	}
	if incident.GroundTruth.PrimaryClassification == "expected_transient" && (!incident.Features.TypedTransient || !incident.Features.RetryDeadlineRespected || incident.Features.ConsecutiveFailures > 4 || !incident.Features.SuccessfulDomainEvent || incident.Features.RequestAmplification || incident.Features.ProgressStalled) {
		return errors.New("expected_transient does not satisfy the transient invariant")
	}
	return rejectSensitive(incident)
}

func Evaluate(corpus Corpus, rules Rules) (Evaluation, error) {
	if err := Validate(corpus, rules); err != nil {
		return Evaluation{}, err
	}
	evaluation := Evaluation{
		Version: 1, CorpusVersion: corpus.Version, RulesVersion: rules.Version,
		ByClassification: map[string]int{}, RuleConflicts: []string{}, Mismatches: []Mismatch{},
		InsufficientEvidence: []string{}, ResolvableWithObservability: []string{}, Results: []Result{},
	}
	for _, name := range primaryClassifications {
		evaluation.ByClassification[name] = 0
	}
	for _, incident := range corpus.Incidents {
		classification, err := Classify(incident.Features, rules)
		if err != nil {
			return Evaluation{}, fmt.Errorf("incident %s: %w", incident.ID, err)
		}
		winner := classification.Primary
		truth := incident.GroundTruth.PrimaryClassification
		evaluation.Total++
		evaluation.ByClassification[winner]++
		if winner == "unknown" {
			evaluation.Unknown++
		} else {
			evaluation.Classified++
		}
		if truth != winner {
			evaluation.Mismatches = append(evaluation.Mismatches, Mismatch{incident.ID, truth, winner})
		}
		if truth == "unknown" {
			evaluation.InsufficientEvidence = append(evaluation.InsufficientEvidence, incident.ID)
			if len(incident.WouldResolveWith) > 0 {
				evaluation.ResolvableWithObservability = append(evaluation.ResolvableWithObservability, incident.ID)
			}
		}
		evaluation.Results = append(evaluation.Results, Result{incident.ID, truth, winner, classification.MatchedRules})
		updateMetrics(&evaluation.ConfirmedBug, truth, winner, "confirmed_bug")
		updateMetrics(&evaluation.ExpectedTransient, truth, winner, "expected_transient")
	}
	sort.Strings(evaluation.InsufficientEvidence)
	sort.Strings(evaluation.ResolvableWithObservability)
	return evaluation, nil
}

// Classify applies the validated historical rule contract to one runtime
// feature vector. Callers can depend on priority order and exactly one primary
// result without constructing synthetic corpus records.
func Classify(features Features, rules Rules) (Classification, error) {
	if err := validateRules(rules.Rules); err != nil {
		return Classification{}, err
	}
	ordered := append([]Rule(nil), rules.Rules...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Priority < ordered[j].Priority })
	matched := make([]Rule, 0)
	for _, rule := range ordered {
		if rule.Always && len(matched) > 0 {
			continue
		}
		ok, err := matches(features, rule)
		if err != nil {
			return Classification{}, fmt.Errorf("rule %s: %w", rule.ID, err)
		}
		if ok {
			matched = append(matched, rule)
		}
	}
	if len(matched) == 0 {
		return Classification{}, errors.New("feature vector matched no rule")
	}
	result := Classification{Primary: matched[0].Classification, MatchedRules: make([]string, len(matched))}
	for index, rule := range matched {
		result.MatchedRules[index] = rule.ID
		if rule.Priority == matched[0].Priority && rule.Classification != matched[0].Classification {
			return Classification{}, fmt.Errorf("rule priority conflict %d", rule.Priority)
		}
	}
	return result, nil
}

func updateMetrics(metrics *Metrics, truth, predicted, target string) {
	if truth == "unknown" {
		metrics.Unassessable++
	}
	if truth == target {
		metrics.GroundTruth++
	}
	if predicted == target {
		metrics.Predicted++
	}
	if truth == target && predicted == target {
		metrics.TruePositive++
	} else if truth != target && truth != "unknown" && predicted == target {
		metrics.FalsePositive++
	} else if truth == target && predicted != target {
		metrics.FalseNegative++
	}
}

func matches(features Features, rule Rule) (bool, error) {
	if rule.Always {
		return true, nil
	}
	raw, _ := json.Marshal(features)
	values := map[string]any{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return false, err
	}
	for _, predicate := range rule.All {
		actual := values[predicate.Field]
		var expected any
		if err := json.Unmarshal(predicate.Value, &expected); err != nil {
			return false, err
		}
		switch predicate.Op {
		case "eq":
			if fmt.Sprint(actual) != fmt.Sprint(expected) {
				return false, nil
			}
		case "lte":
			if actual.(float64) > expected.(float64) {
				return false, nil
			}
		case "gte":
			if actual.(float64) < expected.(float64) {
				return false, nil
			}
		}
	}
	return true, nil
}

func MarshalCanonical(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func isClassification(value string) bool {
	for _, classification := range primaryClassifications {
		if value == classification {
			return true
		}
	}
	return false
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

package incidentloop

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
	"github.com/ishii1648/codex-issue-loop/internal/platform/redact"
)

type Analyzer interface {
	Analyze(context.Context, EvidenceBundle) (AIAnalysis, error)
}

type IssueRepository interface {
	FindByFingerprint(context.Context, string) (*IssueRef, error)
	Create(context.Context, IssueDraft) (IssueRef, error)
	ReadBack(context.Context, int) (IssueRef, error)
}

type PipelineConfig struct {
	Repository      string
	RulesPath       string
	RulesJSON       []byte
	ReadyLabels     []string
	DryRun          bool
	MaxAttempts     int
	BaseBackoff     time.Duration
	MaxEpisodeItems int
	Now             func() time.Time
}

type Pipeline struct {
	Config   PipelineConfig
	Store    Store
	Analyzer Analyzer
	Issues   IssueRepository
}

type RunReport struct {
	Version       int          `json:"version"`
	ProcessedAt   time.Time    `json:"processed_at"`
	SignalCount   int          `json:"signal_count"`
	EpisodeCount  int          `json:"episode_count"`
	Analyzed      int          `json:"analyzed"`
	AnalysisRetry int          `json:"analysis_retry"`
	CircuitOpened int          `json:"circuit_opened"`
	IssueRetry    int          `json:"issue_retry"`
	IssueDrafts   []IssueDraft `json:"issue_drafts"`
	IssuesCreated []IssueRef   `json:"issues_created"`
	IssuesReused  []IssueRef   `json:"issues_reused"`
}

func (p Pipeline) RunOnce(ctx context.Context) (RunReport, error) {
	if err := p.validate(); err != nil {
		return RunReport{}, err
	}
	release, err := p.Store.TryProcessLock()
	if err != nil {
		return RunReport{}, err
	}
	defer release()
	now := p.now()
	report := RunReport{Version: SchemaVersion, ProcessedAt: now, IssueDrafts: []IssueDraft{}, IssuesCreated: []IssueRef{}, IssuesReused: []IssueRef{}}
	signals, err := p.Store.ReadSignals()
	if err != nil {
		return report, err
	}
	report.SignalCount = len(signals)
	previous, err := p.Store.LoadState()
	if err != nil {
		return report, err
	}
	var rules incidentanalysis.Rules
	if len(p.Config.RulesJSON) > 0 {
		rules, err = incidentanalysis.ParseRules(p.Config.RulesJSON)
	} else {
		rules, err = incidentanalysis.LoadRules(p.Config.RulesPath)
	}
	if err != nil {
		return report, err
	}
	maxEpisodeItems := p.Config.MaxEpisodeItems
	if maxEpisodeItems == 0 {
		maxEpisodeItems = 128
	}
	state, err := BuildEpisodesWithLimit(signals, previous, rules, maxEpisodeItems)
	if err != nil {
		return report, err
	}
	metrics, err := p.Store.LoadMetrics()
	if err != nil {
		return report, err
	}
	fingerprints := make([]string, 0, len(state.Episodes))
	for fingerprint := range state.Episodes {
		fingerprints = append(fingerprints, fingerprint)
	}
	sort.Strings(fingerprints)
	var circuitSignals []Signal
	for _, fingerprint := range fingerprints {
		episode := state.Episodes[fingerprint]
		if episode.State == "resolved" || episode.PrimaryClassification == "expected_transient" {
			state.Episodes[fingerprint] = episode
			continue
		}
		if episode.AI == nil && !episode.CircuitOpen && (episode.NextAttemptAt == nil || !episode.NextAttemptAt.After(now)) {
			analysis, analyzeErr := p.analyze(ctx, episode)
			metrics.AnalysisAttempts[outcomeKey(analyzeErr)]++
			if analyzeErr != nil {
				episode.Attempts++
				if episode.Attempts >= p.Config.MaxAttempts {
					episode.CircuitOpen = true
					episode.CircuitGeneration++
					episode.NextAttemptAt = nil
					report.CircuitOpened++
					circuitSignals = append(circuitSignals, p.circuitSignal(episode, now, "ai_analysis_retry_exhausted"))
				} else {
					next := now.Add(backoff(p.Config.BaseBackoff, episode.Attempts))
					episode.NextAttemptAt = &next
					report.AnalysisRetry++
				}
				state.Episodes[fingerprint] = episode
				continue
			}
			episode.AI = &analysis
			episode.Attempts++
			episode.NextAttemptAt = nil
			report.Analyzed++
		}
		if episode.AI != nil && episode.Issue == nil && !episode.IssueCircuitOpen && (episode.IssueNextAttemptAt == nil || !episode.IssueNextAttemptAt.After(now)) && issueEligible(episode) {
			draft, draftErr := p.issueDraft(episode)
			if draftErr != nil {
				return report, draftErr
			}
			report.IssueDrafts = append(report.IssueDrafts, draft)
			if p.Config.DryRun {
				metrics.Issues["dry_run"] = uint64(len(report.IssueDrafts))
			} else {
				ref, reused, createErr := p.ensureIssue(ctx, draft)
				if createErr != nil {
					episode.IssueAttempts++
					metrics.Issues["failed"]++
					if episode.IssueAttempts >= p.Config.MaxAttempts {
						episode.IssueCircuitOpen = true
						episode.IssueCircuitGeneration++
						episode.IssueNextAttemptAt = nil
						report.CircuitOpened++
						circuitSignals = append(circuitSignals, p.circuitSignal(episode, now, "github_issue_retry_exhausted"))
					} else {
						next := now.Add(backoff(p.Config.BaseBackoff, episode.IssueAttempts))
						episode.IssueNextAttemptAt = &next
						report.IssueRetry++
					}
					state.Episodes[fingerprint] = episode
					continue
				}
				episode.Issue = &ref
				episode.IssueAttempts++
				episode.IssueNextAttemptAt = nil
				if reused {
					report.IssuesReused = append(report.IssuesReused, ref)
					metrics.Issues["reused"]++
				} else {
					report.IssuesCreated = append(report.IssuesCreated, ref)
					metrics.Issues["created"]++
				}
			}
		}
		state.Episodes[fingerprint] = episode
	}
	if len(circuitSignals) > 0 {
		withCircuitSignals := append(append([]Signal(nil), signals...), circuitSignals...)
		state, err = BuildEpisodesWithLimit(withCircuitSignals, state, rules, maxEpisodeItems)
		if err != nil {
			return report, err
		}
	}
	report.EpisodeCount = len(state.Episodes)
	recomputeEpisodeMetrics(&metrics, state)
	metrics.UpdatedAt = now
	state.UpdatedAt = now
	if err := p.Store.SaveState(state, metrics); err != nil {
		return report, err
	}
	if p.Config.DryRun {
		if err := p.Store.WriteDryRun(report.IssueDrafts); err != nil {
			return report, err
		}
	}
	if _, err := p.Store.RecordBatch(circuitSignals); err != nil {
		return report, err
	}
	return report, nil
}

func (p Pipeline) circuitSignal(episode Episode, at time.Time, code string) Signal {
	generation := episode.CircuitGeneration
	if code == "github_issue_retry_exhausted" {
		generation = episode.IssueCircuitGeneration
	}
	return Signal{
		Version: SchemaVersion, ID: fmt.Sprintf("circuit-%s-%s-%d", code, episode.ID, generation), Timestamp: at,
		Repository: p.Config.Repository, CorrelationID: episode.ID, EpisodeID: "automation-circuit-" + episode.ID,
		Kind: "event", Name: "failure_classified", Component: "analysis", Phase: "analyze",
		OutcomeCode: "blocked", ReasonCode: code, FailureKind: "operator", FailureCode: code,
		HumanActionRequired: true, Evidence: []EvidenceRef{{Source: "incident-state", Ref: episode.ID}},
	}
}

func (p Pipeline) validate() error {
	if !repositoryPattern.MatchString(p.Config.Repository) || (strings.TrimSpace(p.Config.RulesPath) == "" && len(p.Config.RulesJSON) == 0) {
		return errors.New("pipeline requires owner/name repository and rules path")
	}
	if len(p.Config.ReadyLabels) == 0 {
		return errors.New("at least one ready label is required")
	}
	for _, label := range p.Config.ReadyLabels {
		if strings.TrimSpace(label) == "" || strings.TrimSpace(label) != label || strings.ContainsAny(label, "\r\n") {
			return errors.New("ready labels must be non-empty single-line values")
		}
	}
	if p.Config.MaxAttempts < 1 || p.Config.BaseBackoff <= 0 || p.Analyzer == nil {
		return errors.New("pipeline requires analyzer, positive retry count, and backoff")
	}
	if p.Config.MaxEpisodeItems != 0 && (p.Config.MaxEpisodeItems < 16 || p.Config.MaxEpisodeItems > 128) {
		return errors.New("pipeline max episode items must be between 16 and 128")
	}
	if !p.Config.DryRun && p.Issues == nil {
		return errors.New("live pipeline requires an Issue repository")
	}
	return nil
}

func (p Pipeline) analyze(ctx context.Context, episode Episode) (AIAnalysis, error) {
	bundle := EvidenceBundle{
		Version: SchemaVersion, EpisodeID: episode.ID, Fingerprint: episode.Fingerprint,
		Repository: episode.Repository, PrimaryClassification: episode.PrimaryClassification,
		Confidence: episode.Confidence, SignalIDs: append([]string(nil), episode.SignalIDs...),
		Evidence: append([]EvidenceRef(nil), episode.Evidence...), MissingEvidence: append([]string(nil), episode.MissingEvidence...),
	}
	analysis, err := p.Analyzer.Analyze(ctx, bundle)
	if err != nil {
		return AIAnalysis{}, err
	}
	raw, err := redact.Marshal(analysis, p.Store.Secrets)
	if err != nil {
		return AIAnalysis{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&analysis); err != nil {
		return AIAnalysis{}, err
	}
	if err := analysis.Validate(episode); err != nil {
		return AIAnalysis{}, err
	}
	return analysis, nil
}

func issueEligible(episode Episode) bool {
	if episode.AI == nil || !episode.AI.RecommendIssue || episode.Confidence == "low" || episode.AI.Confidence == "low" {
		return false
	}
	switch episode.PrimaryClassification {
	case "confirmed_bug":
		return episode.Features.DocumentedInvariantViolation && episode.Features.CorroboratedProductFix
	case "suspected_bug":
		return episode.Features.DocumentedInvariantViolation && episode.Features.RepeatedIndependentRuns
	default:
		return false
	}
}

func (p Pipeline) issueDraft(episode Episode) (IssueDraft, error) {
	title := fmt.Sprintf("[auto-detected] %s incident %s", episode.PrimaryClassification, episode.Fingerprint[:12])
	body := fmt.Sprintf("## 自動検出したincident\n\n- Classification: `%s`\n- Confidence: `%s`\n- First observed: `%s`\n- Last observed: `%s`\n- Occurrences: %d\n- Matched rules: `%s`\n- Signal IDs: `%s`\n\n## 期待値\n\n監視対象の不変条件を維持し、同一scopeの処理が保存済みdeadlineとlifecycle契約に従うこと。\n\n## 実測値\n\n決定論的分類器が `%s` を検出しました。再現証拠は上記signal IDと保存済みincident stateで参照できます。\n\n## 分析\n\n%s\n\n## 原因仮説\n\n%s\n\n## 反証・追加調査\n\n- Counter evidence: %s\n- Additional investigation: %s\n\n## Acceptance criteria\n\n- 同じfingerprintを再現する回帰testが追加される\n- CIとreviewが成功する\n- merge後にincidentがresolvedとなり、再発時はreopenedとなる\n\n<!-- incident-fingerprint:%s -->\n",
		episode.PrimaryClassification, episode.Confidence, episode.StartedAt.UTC().Format(time.RFC3339), episode.UpdatedAt.UTC().Format(time.RFC3339), episode.OccurrenceCount,
		strings.Join(episode.MatchedRules, "`, `"), strings.Join(episode.SignalIDs, "`, `"), episode.PrimaryClassification, episode.AI.Summary, episode.AI.CauseHypothesis,
		boundedIssueList(episode.AI.CounterEvidence), boundedIssueList(episode.AI.AdditionalInvestigation), episode.Fingerprint)
	draft := IssueDraft{Version: SchemaVersion, EpisodeID: episode.ID, Fingerprint: episode.Fingerprint, Title: title, Body: body, Labels: append([]string(nil), p.Config.ReadyLabels...)}
	raw, err := redact.Marshal(draft, p.Store.Secrets)
	if err != nil {
		return IssueDraft{}, err
	}
	if err := json.Unmarshal(raw, &draft); err != nil {
		return IssueDraft{}, err
	}
	if err := rejectSensitive(draft); err != nil {
		return IssueDraft{}, err
	}
	if err := draft.Validate(); err != nil {
		return IssueDraft{}, err
	}
	return draft, nil
}

func boundedIssueList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	const maxItems = 8
	if len(values) > maxItems {
		values = values[:maxItems]
	}
	return strings.Join(values, "; ")
}

func (p Pipeline) ensureIssue(ctx context.Context, draft IssueDraft) (IssueRef, bool, error) {
	if existing, err := p.Issues.FindByFingerprint(ctx, draft.Fingerprint); err != nil {
		return IssueRef{}, false, err
	} else if existing != nil {
		if existing.Fingerprint != draft.Fingerprint {
			return IssueRef{}, false, errors.New("Issue fingerprint readback mismatch")
		}
		return *existing, true, nil
	}
	created, err := p.Issues.Create(ctx, draft)
	if err != nil {
		return IssueRef{}, false, err
	}
	readback, err := p.Issues.ReadBack(ctx, created.Number)
	if err != nil {
		return IssueRef{}, false, err
	}
	if readback.Fingerprint != draft.Fingerprint || !sameLabelSet(readback.Labels, draft.Labels) {
		return IssueRef{}, false, errors.New("created Issue failed fingerprint or ready-label readback")
	}
	readback.Status = "created"
	return readback, false, nil
}

func (p Pipeline) now() time.Time {
	if p.Config.Now != nil {
		return p.Config.Now().UTC()
	}
	return time.Now().UTC()
}

func backoff(base time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	return base * time.Duration(1<<(attempt-1))
}

func outcomeKey(err error) string {
	if err == nil {
		return "succeeded"
	}
	return "failed"
}

func recomputeEpisodeMetrics(metrics *Metrics, state DurableState) {
	metrics.Classifications = map[string]uint64{}
	metrics.OpenEpisodes = 0
	metrics.CircuitOpen = 0
	for _, episode := range state.Episodes {
		metrics.Classifications[episode.PrimaryClassification]++
		if episode.State != "resolved" {
			metrics.OpenEpisodes++
		}
		if episode.CircuitOpen || episode.IssueCircuitOpen {
			metrics.CircuitOpen++
		}
	}
}

func sameLabelSet(left, right []string) bool {
	a := uniqueSorted(left)
	b := uniqueSorted(right)
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

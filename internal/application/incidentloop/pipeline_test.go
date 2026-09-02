package incidentloop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
	"github.com/ishii1648/codex-issue-loop/internal/platform/retention"
)

type fakeAnalyzer struct {
	calls      int
	bundles    []EvidenceBundle
	err        error
	alter      string
	confidence string
	issue      bool
}

func (a *fakeAnalyzer) Analyze(_ context.Context, bundle EvidenceBundle) (AIAnalysis, error) {
	a.calls++
	a.bundles = append(a.bundles, bundle)
	if a.err != nil {
		return AIAnalysis{}, a.err
	}
	classification := bundle.PrimaryClassification
	if a.alter != "" {
		classification = a.alter
	}
	confidence := bundle.Confidence
	if a.confidence != "" {
		confidence = a.confidence
	}
	return AIAnalysis{
		Version: SchemaVersion, EpisodeID: bundle.EpisodeID, Classification: classification, Confidence: confidence,
		Summary: "deterministic summary", CauseHypothesis: "bounded cause", CounterEvidence: []string{},
		AdditionalInvestigation: []string{}, RecommendIssue: a.issue, Evidence: append([]EvidenceRef(nil), bundle.Evidence...),
	}, nil
}

func TestAIErrorAndLowConfidenceNeverCreateIssue(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 15, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		analyzer *fakeAnalyzer
	}{
		{name: "timeout", analyzer: &fakeAnalyzer{err: context.DeadlineExceeded, issue: true}},
		{name: "low-confidence", analyzer: &fakeAnalyzer{confidence: "low", issue: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			recordSignals(t, store,
				signalAt(now, test.name+"-1", test.name, "failure_classified", "failed", func(s *Signal) {
					s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-"+test.name, "run-1", "product", "invariant", true
				}),
				signalAt(now.Add(time.Second), test.name+"-2", test.name, "failure_classified", "failed", func(s *Signal) {
					s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-"+test.name, "run-2", "product", "invariant", true
				}),
			)
			issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
			report, err := testPipeline(store, test.analyzer, issues, &now, false).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(issues.drafts) != 0 {
				t.Fatal("AI failure or low confidence created an Issue")
			}
			if test.name == "timeout" {
				if len(report.AnalysisFailures) != 1 || report.AnalysisFailures[0].Code != "timeout" {
					t.Fatalf("analysis failures=%+v", report.AnalysisFailures)
				}
				metrics, metricsErr := store.LoadMetrics()
				if metricsErr != nil || metrics.AnalysisFailures["timeout"] != 1 {
					t.Fatalf("analysis failure metrics=%+v err=%v", metrics.AnalysisFailures, metricsErr)
				}
			}
			if test.name == "low-confidence" {
				test.analyzer.confidence = "high"
				now = now.Add(time.Minute)
				recordSignals(t, store, signalAt(now, "low-confidence-3", "low-confidence", "failure_classified", "failed", func(s *Signal) {
					s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-low-confidence", "run-3", "product", "invariant", true
				}))
				if _, err := testPipeline(store, test.analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(issues.drafts) != 1 {
					t.Fatal("new evidence did not trigger a fresh AI analysis")
				}
			}
		})
	}
}

func TestDryRunPersistsExactIssueDraftWithoutGitHub(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 20, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store,
		signalAt(now, "dry-run-1", "dry-run", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-dry-run", "run-1", "product", "invariant", true
		}),
		signalAt(now.Add(time.Second), "dry-run-2", "dry-run", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-dry-run", "run-2", "product", "invariant", true
		}),
	)
	report, err := testPipeline(store, &fakeAnalyzer{issue: true}, nil, &now, true).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.IssueDrafts) != 1 || len(report.IssuesCreated) != 0 || report.IssueDrafts[0].Labels[0] != "codex-loop:ready" {
		t.Fatalf("dry-run report=%+v", report)
	}
	data, err := os.ReadFile(store.DryRunPath())
	if err != nil {
		t.Fatal(err)
	}
	var drafts []IssueDraft
	if err := json.Unmarshal(data, &drafts); err != nil || len(drafts) != 1 || drafts[0].Fingerprint != report.IssueDrafts[0].Fingerprint || !strings.Contains(drafts[0].Body, "## 期待値") {
		t.Fatalf("persisted dry-run=%s err=%v", data, err)
	}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	liveReport, err := testPipeline(store, &fakeAnalyzer{issue: true}, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(liveReport.IssuesCreated) != 1 || len(issues.drafts) != 1 {
		t.Fatalf("dry-run prevented later live creation: report=%+v drafts=%d", liveReport, len(issues.drafts))
	}
}

type fakeIssues struct {
	byFingerprint map[string]IssueRef
	drafts        []IssueDraft
	createErr     error
}

func (f *fakeIssues) FindByFingerprint(_ context.Context, fingerprint string) (*IssueRef, error) {
	if issue, ok := f.byFingerprint[fingerprint]; ok {
		copy := issue
		return &copy, nil
	}
	return nil, nil
}

func (f *fakeIssues) Create(_ context.Context, draft IssueDraft) (IssueRef, error) {
	if f.createErr != nil {
		return IssueRef{}, f.createErr
	}
	f.drafts = append(f.drafts, draft)
	ref := IssueRef{Number: len(f.drafts), URL: "https://github.com/owner/repo/issues/1", Labels: append([]string(nil), draft.Labels...), Fingerprint: draft.Fingerprint, Status: "created", CreatedAt: time.Unix(10, 0).UTC()}
	f.byFingerprint[draft.Fingerprint] = ref
	return ref, nil
}

func (f *fakeIssues) ReadBack(_ context.Context, number int) (IssueRef, error) {
	for _, issue := range f.byFingerprint {
		if issue.Number == number {
			return issue, nil
		}
	}
	return IssueRef{}, errors.New("not found")
}

func TestExpectedTransientNeverReachesAIOrIssue(t *testing.T) {
	now := time.Date(2026, 9, 2, 1, 0, 0, 0, time.UTC)
	store := testStore(t)
	retryAt := now.Add(time.Minute)
	recordSignals(t, store,
		signalAt(now, "retry-1", "transient-1", "retry_episode", "retrying", func(s *Signal) {
			s.FailureKind, s.EpisodeID, s.ScopeKind, s.Attempt, s.RetryAt = "transient", "episode-transient", "supervisor", 1, &retryAt
		}),
		signalAt(retryAt, "progress-1", "transient-1", "progress", "succeeded", func(s *Signal) {
			s.EpisodeID, s.ProgressKind = "episode-transient", "github_poll"
		}),
	)
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	report, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if analyzer.calls != 0 || len(issues.drafts) != 0 || len(report.IssuesCreated) != 0 {
		t.Fatalf("expected transient crossed an automation boundary: report=%+v calls=%d", report, analyzer.calls)
	}
	state, _ := store.LoadState()
	for _, episode := range state.Episodes {
		if episode.PrimaryClassification != "expected_transient" {
			t.Fatalf("classification=%s", episode.PrimaryClassification)
		}
	}
}

func TestSuspectedBugCreatesExactlyOneIssueAcrossRestart(t *testing.T) {
	now := time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC)
	store := testStore(t)
	firstDeadline := now.Add(time.Minute)
	recordSignals(t, store,
		signalAt(now, "cycle-1", "run-1", "scheduler_cycle", "started", func(s *Signal) {
			s.Component, s.Phase, s.ReasonCode = "scheduler", "poll", "scheduler_wake"
			s.CycleID, s.RunID, s.Trigger, s.ScheduledDeadline = "cycle-1", "run-1", "fsnotify", &firstDeadline
		}),
		signalAt(now.Add(time.Second), "attempt-1", "run-1", "external_attempt_completed", "succeeded", func(s *Signal) {
			s.Component, s.Phase, s.ReasonCode = "github", "poll", "github_queue_polled"
			s.CycleID, s.RunID, s.Provider, s.OperationCode = "cycle-1", "run-1", "github", "list_ready_issues"
		}),
	)
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	pipeline := testPipeline(store, analyzer, issues, &now, false)
	first, err := pipeline.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.IssuesCreated) != 0 || len(issues.drafts) != 0 {
		t.Fatalf("one runtime reproduction created an Issue: report=%+v drafts=%d", first, len(issues.drafts))
	}

	secondStarted := now.Add(2 * time.Minute)
	secondDeadline := secondStarted.Add(time.Minute)
	recordSignals(t, store,
		signalAt(secondStarted, "cycle-2", "run-2", "scheduler_cycle", "started", func(s *Signal) {
			s.Component, s.Phase, s.ReasonCode = "scheduler", "poll", "scheduler_wake"
			s.CycleID, s.RunID, s.Trigger, s.ScheduledDeadline = "cycle-2", "run-2", "fsnotify", &secondDeadline
		}),
		signalAt(secondStarted.Add(time.Second), "attempt-2", "run-2", "external_attempt_completed", "succeeded", func(s *Signal) {
			s.Component, s.Phase, s.ReasonCode = "github", "poll", "github_queue_polled"
			s.CycleID, s.RunID, s.Provider, s.OperationCode = "cycle-2", "run-2", "github", "list_ready_issues"
		}),
	)
	now = secondStarted.Add(2 * time.Second)
	second, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.IssuesCreated) != 1 || len(issues.drafts) != 1 {
		t.Fatalf("two runtime reproductions did not create one Issue: report=%+v drafts=%d", second, len(issues.drafts))
	}
	latestBundle := analyzer.bundles[len(analyzer.bundles)-1]
	if len(latestBundle.Signals) != 2 || latestBundle.Signals[0].FailureCode != "retry_deadline_bypass" || latestBundle.Signals[0].ScheduledDeadline == nil ||
		!strings.Contains(issues.drafts[0].Body, "Invariant codes: `retry_deadline_bypass`") {
		t.Fatalf("AI or Issue did not receive invariant evidence: bundle=%+v body=%s", latestBundle, issues.drafts[0].Body)
	}
	third, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(third.IssuesCreated) != 0 || len(issues.drafts) != 1 || analyzer.calls != 2 {
		t.Fatalf("restart duplicated work: third=%+v drafts=%d calls=%d", third, len(issues.drafts), analyzer.calls)
	}
}

func TestNonBugClassificationsCannotCreateIssue(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		set  func(*Signal)
		want string
	}{
		{"degraded", func(s *Signal) { s.ProgressKind, s.ProgressStalled, s.ThresholdExceeded = "queue", true, true }, "degraded"},
		{"operator", func(s *Signal) { s.ProgressKind, s.HumanActionRequired = "auth", true }, "operator_attention"},
		{"unknown", func(s *Signal) { s.ProgressKind = "ambiguous" }, "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			recordSignals(t, store, signalAt(now, "signal-"+test.name, "scope-"+test.name, "progress", "observed", test.set))
			analyzer := &fakeAnalyzer{issue: true}
			issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
			if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			state, _ := store.LoadState()
			for _, episode := range state.Episodes {
				if episode.PrimaryClassification != test.want {
					t.Fatalf("classification=%s want=%s", episode.PrimaryClassification, test.want)
				}
			}
			if len(issues.drafts) != 0 {
				t.Fatal("non-bug classification created an Issue")
			}
		})
	}
}

func TestDegradationThresholdGateResolveAndReopen(t *testing.T) {
	now := time.Date(2026, 9, 2, 3, 30, 0, 0, time.UTC)
	store := testStore(t)
	below := signalAt(now, "duration-below", "performance", "operation_duration", "observed", func(s *Signal) {
		s.EpisodeID, s.OperationCode, s.ElapsedMS = "performance-window", "scheduler_cycle", 119_999
	})
	recordSignals(t, store, below)
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	report, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.EpisodeCount != 0 || analyzer.calls != 0 || len(issues.drafts) != 0 {
		t.Fatalf("below threshold report=%+v calls=%d", report, analyzer.calls)
	}

	now = now.Add(time.Minute)
	above := signalAt(now, "duration-above", "performance", "operation_duration", "observed", func(s *Signal) {
		s.EpisodeID, s.OperationCode, s.ElapsedMS, s.ProgressStalled, s.ThresholdExceeded = "performance-window", "scheduler_cycle", 120_000, true, true
	})
	recordSignals(t, store, above)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateValue, _ := store.LoadState()
	var fingerprint string
	for key, episode := range stateValue.Episodes {
		fingerprint = key
		if episode.PrimaryClassification != "degraded" || episode.State != "open" {
			t.Fatalf("degraded episode=%+v", episode)
		}
	}
	if analyzer.calls != 1 || len(issues.drafts) != 0 {
		t.Fatalf("threshold handling calls=%d drafts=%d", analyzer.calls, len(issues.drafts))
	}

	now = now.Add(time.Minute)
	recovered := signalAt(now, "duration-recovered", "performance", "progress", "succeeded", func(s *Signal) {
		s.EpisodeID, s.ProgressKind = "performance-window", "scheduler_cycle"
	})
	recordSignals(t, store, recovered)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateValue, _ = store.LoadState()
	if stateValue.Episodes[fingerprint].State != "resolved" {
		t.Fatalf("recovered episode=%+v", stateValue.Episodes[fingerprint])
	}

	now = now.Add(time.Minute)
	recurrence := signalAt(now, "duration-reopened", "performance", "operation_duration", "observed", func(s *Signal) {
		s.EpisodeID, s.OperationCode, s.ElapsedMS, s.ProgressStalled, s.ThresholdExceeded = "performance-window", "scheduler_cycle", 150_000, true, true
	})
	recordSignals(t, store, recurrence)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stateValue, _ = store.LoadState()
	if stateValue.Episodes[fingerprint].State != "reopened" {
		t.Fatalf("reopened episode=%+v", stateValue.Episodes[fingerprint])
	}
}

func TestInvalidAIOutputRetriesThenOpensCircuitWithoutIssue(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 0, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store,
		signalAt(now, "bad-ai-1", "bad-ai", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-ai", "run-1", "product", "invariant", true
		}),
		signalAt(now.Add(time.Second), "bad-ai-2", "bad-ai", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-ai", "run-2", "product", "invariant", true
		}),
	)
	analyzer := &fakeAnalyzer{alter: "confirmed_bug", issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.LoadState()
	metrics, metricsErr := store.LoadMetrics()
	if metricsErr != nil || metrics.AnalysisFailures["invalid_output"] != 2 {
		t.Fatalf("analysis failure metrics=%+v err=%v", metrics.AnalysisFailures, metricsErr)
	}
	foundCircuit := false
	foundAttention := false
	for _, episode := range state.Episodes {
		if episode.PrimaryClassification == "suspected_bug" {
			foundCircuit = episode.CircuitOpen && episode.Attempts == 2 && episode.AI == nil
		}
		foundAttention = foundAttention || episode.PrimaryClassification == "operator_attention"
	}
	if !foundCircuit || !foundAttention {
		t.Fatalf("retry circuit state=%+v", state.Episodes)
	}
	if len(issues.drafts) != 0 {
		t.Fatal("invalid AI output created an Issue")
	}
}

func TestIssueFailureUsesBoundedRetryAndBecomesOperatorAttention(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 30, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store,
		signalAt(now, "issue-fail-1", "issue-fail", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-issue-fail", "run-1", "product", "invariant", true
		}),
		signalAt(now.Add(time.Second), "issue-fail-2", "issue-fail", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-issue-fail", "run-2", "product", "invariant", true
		}),
	)
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}, createErr: errors.New("GitHub unavailable")}
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	report, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.CircuitOpened != 1 {
		t.Fatalf("report=%+v", report)
	}
	now = now.Add(time.Minute)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	foundAttention := false
	circuitFingerprint := ""
	for fingerprint, episode := range state.Episodes {
		foundAttention = foundAttention || episode.PrimaryClassification == "operator_attention"
		if episode.IssueCircuitOpen {
			circuitFingerprint = fingerprint
		}
	}
	if !foundAttention || circuitFingerprint == "" {
		t.Fatal("retry exhaustion was not exposed as operator_attention")
	}
	now = now.Add(time.Minute)
	if _, err := store.ResetCircuit(circuitFingerprint, now); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Episodes[circuitFingerprint].IssueCircuitOpen || recovered.Episodes[circuitFingerprint].IssueAttempts != 0 {
		t.Fatalf("circuit retry state=%+v", recovered.Episodes[circuitFingerprint])
	}
	attentionResolved := false
	for _, episode := range recovered.Episodes {
		attentionResolved = attentionResolved || episode.PrimaryClassification == "operator_attention" && episode.State == "resolved"
	}
	if !attentionResolved {
		t.Fatal("operator attention episode was not resolved by explicit recovery")
	}
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatalf("second circuit generation collided with prior signal: %v", err)
	}
}

func TestLifecycleMergeResolvesEpisodeAndRecordsFixEvidence(t *testing.T) {
	now := time.Date(2026, 9, 2, 5, 0, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store,
		signalAt(now, "life-1", "life", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-life", "run-1", "product", "invariant", true
		}),
		signalAt(now.Add(time.Second), "life-2", "life", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-life", "run-2", "product", "invariant", true
		}),
	)
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	merged := signalAt(now.Add(time.Hour), "life-3", "life", "lifecycle_outcome", "succeeded", func(s *Signal) {
		s.EpisodeID, s.RunID, s.LifecycleStage, s.ProductFixMerged, s.InvariantViolation = "episode-life", "run-2", "merged", true, true
	})
	recordSignals(t, store, merged)
	now = now.Add(time.Hour)
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.LoadState()
	for _, episode := range state.Episodes {
		if episode.State != "resolved" || episode.PrimaryClassification != "confirmed_bug" || episode.Issue == nil || len(episode.Lifecycle) != 1 {
			t.Fatalf("resolved episode=%+v", episode)
		}
	}
}

func TestSignalPersistenceRedactsSecretsAndBuildIsOrderIndependent(t *testing.T) {
	now := time.Date(2026, 9, 2, 6, 0, 0, 0, time.UTC)
	secret := "sk-abcdefghijklmnopqrstuvwxyz123456"
	store := testStore(t)
	store.Secrets = []string{secret}
	first := signalAt(now, "order-a", "order", "failure_classified", "failed", func(s *Signal) {
		s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation, s.Evidence = "episode-order", "run-1", "product", "invariant", true, []EvidenceRef{{Source: "state-event", Ref: secret}}
	})
	second := signalAt(now, "order-b", "order", "failure_classified", "failed", func(s *Signal) {
		s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-order", "run-2", "product", "invariant", true
	})
	recordSignals(t, store, first, second)
	data, err := os.ReadFile(store.SignalsPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "[REDACTED]") {
		t.Fatal("signal store did not redact configured secret")
	}
	rules, err := incidentanalysis.LoadRules(rulesPath())
	if err != nil {
		t.Fatal(err)
	}
	a, err := BuildEpisodes([]Signal{first, second}, DurableState{}, rules)
	if err != nil {
		t.Fatal(err)
	}
	b, err := BuildEpisodes([]Signal{second, first}, DurableState{}, rules)
	if err != nil {
		t.Fatal(err)
	}
	aJSON, _ := json.Marshal(a)
	bJSON, _ := json.Marshal(b)
	if string(aJSON) != string(bJSON) {
		t.Fatal("episode build depends on input order")
	}
}

func testStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: t.TempDir(), Retention: retention.Policy{MaxBytes: 1024 * 1024, MaxAge: time.Hour, Keep: 2}}
}

func testPipeline(store Store, analyzer Analyzer, issues IssueRepository, now *time.Time, dryRun bool) Pipeline {
	return Pipeline{Store: store, Analyzer: analyzer, Issues: issues, Config: PipelineConfig{
		Repository: "owner/repo", RulesPath: rulesPath(), ReadyLabels: []string{"codex-loop:ready"}, DryRun: dryRun,
		MaxAttempts: 2, BaseBackoff: time.Minute, Now: func() time.Time { return *now },
	}}
}

func rulesPath() string {
	return filepath.Join("..", "..", "..", "analysis", "incident-taxonomy", "rules.json")
}

func recordSignals(t *testing.T, store Store, signals ...Signal) {
	t.Helper()
	for _, signal := range signals {
		if err := store.Record(signal); err != nil {
			t.Fatal(err)
		}
	}
}

func signalAt(at time.Time, id, correlation, name, outcome string, configure func(*Signal)) Signal {
	signal := Signal{
		Version: SchemaVersion, ID: id, Timestamp: at, Repository: "owner/repo", CorrelationID: correlation,
		Kind: "event", Name: name, Component: "analysis", Phase: "analyze", OutcomeCode: outcome, ReasonCode: "test_fixture",
	}
	configure(&signal)
	return signal
}

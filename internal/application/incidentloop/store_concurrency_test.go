package incidentloop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type interleavedAnalyzer struct {
	fakeAnalyzer
	duringAnalysis func()
}

func (a *interleavedAnalyzer) Analyze(ctx context.Context, bundle EvidenceBundle) (AIAnalysis, error) {
	a.duringAnalysis()
	return a.fakeAnalyzer.Analyze(ctx, bundle)
}

func TestRunOncePreservesRecordedMetricsDuringAnalysis(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			now := time.Date(2026, 9, 2, 4, 30, 0, 0, time.UTC)
			store := testStore(t)
			for i, id := range []string{"first", "second"} {
				recordSignals(t, store, signalAt(now, id, "bug", "failure_classified", "failed", func(s *Signal) {
					s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "bug", id, "product", "invariant", true
					s.Timestamp = now.Add(time.Duration(i) * time.Second)
				}))
			}
			initial, err := store.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			delta := emptyMetrics()
			delta.AnalysisAttempts["succeeded"] = 3
			delta.AnalysisFailures["timeout"] = 2
			delta.Issues["dry_run"] = 4
			if err := store.SaveState(initial, initial.Revision, delta); err != nil {
				t.Fatal(err)
			}
			analyzer := &interleavedAnalyzer{fakeAnalyzer: fakeAnalyzer{issue: true}, duringAnalysis: func() {
				recordSignals(t, Store{Dir: store.Dir}, signalAt(now, "duration", "scheduler", "operation_duration", "observed", func(s *Signal) {
					s.OperationCode, s.ElapsedMS = "scheduler_cycle", 123
				}))
			}}
			if fail {
				analyzer.err = context.DeadlineExceeded
			}
			report, err := testPipeline(store, analyzer, nil, &now, true).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			metrics, err := store.LoadMetrics()
			if err != nil {
				t.Fatal(err)
			}
			if metrics.SignalsByName["failure_classified"] != 2 || metrics.SignalsByName["operation_duration"] != 1 || metrics.Outcomes["failed"] != 2 || metrics.Outcomes["observed"] != 1 || metrics.DurationsMS["scheduler_cycle"] != (DurationSummary{Count: 1, Sum: 123, Max: 123}) {
				t.Fatalf("recorded metrics lost: %+v", metrics)
			}
			if fail {
				if metrics.AnalysisAttempts["succeeded"] != 3 || metrics.AnalysisAttempts["failed"] != 1 || metrics.AnalysisFailures["timeout"] != 3 || metrics.Issues["dry_run"] != 4 {
					t.Fatalf("analysis delta lost: %+v", metrics)
				}
			} else if metrics.AnalysisAttempts["succeeded"] != 4 || metrics.AnalysisFailures["timeout"] != 2 || metrics.Issues["dry_run"] != 5 || len(report.IssueDrafts) != 1 {
				t.Fatalf("analysis/Issue delta lost: %+v", metrics)
			}
			state, err := store.LoadState()
			if err != nil {
				t.Fatal(err)
			}
			expected := emptyMetrics()
			recomputeEpisodeMetrics(&expected, state)
			if !reflect.DeepEqual(metrics.Classifications, expected.Classifications) || metrics.OpenEpisodes != expected.OpenEpisodes || metrics.CircuitOpen != expected.CircuitOpen {
				t.Fatalf("episode gauges stale: %+v", metrics)
			}
		})
	}
}

func TestRunOnceRejectsConcurrentResetCircuit(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 30, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store, signalAt(now, "failure", "bug", "failure_classified", "failed", func(s *Signal) {
		s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "bug", "run", "product", "invariant", true
	}))
	pipeline := testPipeline(store, &fakeAnalyzer{err: context.DeadlineExceeded}, nil, &now, true)
	pipeline.Config.MaxAttempts = 1
	if _, err := pipeline.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ""
	for key, episode := range before.Episodes {
		if episode.CircuitOpen {
			fingerprint = key
		}
	}
	if fingerprint == "" {
		t.Fatal("missing open circuit")
	}
	calls := 0
	pipeline.Analyzer = &interleavedAnalyzer{duringAnalysis: func() {
		calls++
		if _, err := (Store{Dir: store.Dir}).ResetCircuit(fingerprint, now); !errors.Is(err, ErrAlreadyRunning) {
			t.Fatalf("reset error = %v", err)
		}
	}}
	if _, err := pipeline.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || after.Revision != before.Revision+1 || !after.Episodes[fingerprint].CircuitOpen {
		t.Fatalf("pipeline changes not preserved: %+v", after)
	}
	if _, err := store.ResetCircuit(fingerprint, now); err != nil {
		t.Fatal(err)
	}
	stateBytes, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	metricsBytes, err := os.ReadFile(store.MetricsPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveState(after, after.Revision, emptyMetrics()); err == nil || !strings.Contains(err.Error(), "revision mismatch") {
		t.Fatalf("stale save error = %v", err)
	}
	for path, expected := range map[string][]byte{store.StatePath(): stateBytes, store.MetricsPath(): metricsBytes} {
		actual, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual, expected) {
			t.Fatalf("stale save modified %s", path)
		}
	}
	recovered, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Episodes[fingerprint].CircuitOpen {
		t.Fatal("reset was lost")
	}
	for _, episode := range recovered.Episodes {
		if episode.PrimaryClassification == "operator_attention" && episode.State != "resolved" {
			t.Fatal("attention resolution was lost")
		}
	}
}

func TestRunOnceRejectsStaleRevisionAndCanRerun(t *testing.T) {
	now := time.Date(2026, 9, 2, 4, 30, 0, 0, time.UTC)
	store := testStore(t)
	recordSignals(t, store, signalAt(now, "failure", "bug", "failure_classified", "failed", func(s *Signal) {
		s.EpisodeID, s.FailureKind, s.FailureCode, s.InvariantViolation = "bug", "product", "invariant", true
	}))
	var committed []byte
	analyzer := &interleavedAnalyzer{duringAnalysis: func() {
		current, err := store.LoadState()
		if err != nil {
			t.Fatal(err)
		}
		current.UpdatedAt = now.Add(time.Minute)
		if err := store.SaveState(current, current.Revision, emptyMetrics()); err != nil {
			t.Fatal(err)
		}
		committed, err = os.ReadFile(store.StatePath())
		if err != nil {
			t.Fatal(err)
		}
	}}
	pipeline := testPipeline(store, analyzer, nil, &now, true)
	if _, err := pipeline.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "revision mismatch") {
		t.Fatalf("run error = %v", err)
	}
	actual, err := os.ReadFile(store.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, committed) {
		t.Fatal("pipeline overwrote concurrent state")
	}
	metrics, err := store.LoadMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if len(metrics.AnalysisAttempts) != 0 {
		t.Fatalf("rejected run applied metrics: %+v", metrics)
	}
	pipeline.Analyzer = &fakeAnalyzer{}
	if _, err := pipeline.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 2 || len(state.Episodes) != 1 {
		t.Fatalf("rerun state = %+v", state)
	}
}

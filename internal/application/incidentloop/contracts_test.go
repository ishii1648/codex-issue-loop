package incidentloop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/application/incidentanalysis"
)

func TestPublishedIncidentSchemasAreVersionedDraft202012Contracts(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, name := range []string{
		"incident-signal.schema.json", "incident-state.schema.json", "incident-metrics.schema.json",
		"incident-ai-analysis.schema.json", "incident-issue-payload.schema.json",
	} {
		data, err := os.ReadFile(filepath.Join(root, "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if document["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Fatalf("%s has wrong schema dialect", name)
		}
		if document["title"] == nil && document["$ref"] == nil {
			t.Fatalf("%s has neither title nor runtime reference", name)
		}
	}
	var published struct {
		Reference string `json:"$ref"`
	}
	data, _ := os.ReadFile(filepath.Join(root, "schemas", "incident-ai-analysis.schema.json"))
	if err := json.Unmarshal(data, &published); err != nil {
		t.Fatal(err)
	}
	referenced, err := os.ReadFile(filepath.Clean(filepath.Join(root, "schemas", published.Reference)))
	if err != nil {
		t.Fatal(err)
	}
	if string(referenced) != string(incidentAnalysisSchema) {
		t.Fatal("published AI schema and embedded runtime schema differ")
	}
}

func TestStateEventCollectorIsIdempotentAndLinksFixLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)
	sourceDir := t.TempDir()
	target := testStore(t)
	fingerprint := digest("linked-incident")
	prior := DurableState{Version: SchemaVersion, UpdatedAt: now, Episodes: map[string]Episode{
		fingerprint: {Version: SchemaVersion, ID: "inc-" + fingerprint[:20], Fingerprint: fingerprint, Repository: "owner/repo", CorrelationID: "original", State: "open", StartedAt: now, UpdatedAt: now, OccurrenceCount: 1, SignalIDs: []string{"original"}, LastSignalID: "original", Evidence: []EvidenceRef{}, PrimaryClassification: "suspected_bug", MatchedRules: []string{"repeated-invariant-violation"}, Confidence: "medium", Issue: &IssueRef{Number: 42, URL: "https://github.com/owner/repo/issues/42", Fingerprint: fingerprint, Labels: []string{"codex-loop:ready"}, Status: "created"}, Lifecycle: []LifecycleResult{}},
	}}
	if err := target.SaveState(prior, emptyMetrics()); err != nil {
		t.Fatal(err)
	}
	events := []state.Event{
		{Version: 4, EventID: "evt_merge_42", Sequence: 1, Timestamp: now.Add(time.Minute), RepoID: "repoid", IssueNumber: 42, RunID: "run_42", Type: "issue_completed", Payload: json.RawMessage(`{}`)},
		{Version: 4, EventID: "evt_close_42", Sequence: 2, Timestamp: now.Add(2 * time.Minute), RepoID: "repoid", IssueNumber: 42, RunID: "run_42", Type: "github_state_synced", Payload: json.RawMessage(`{"state":"done"}`)},
	}
	lines := make([]byte, 0)
	for _, event := range events {
		line, _ := json.Marshal(event)
		lines = append(lines, append(line, '\n')...)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "events.jsonl"), lines, 0o600); err != nil {
		t.Fatal(err)
	}
	collector := StateEventCollector{Repository: "owner/repo", CloseIssue: true, Source: state.Store{Dir: sourceDir, RepoID: "repoid"}, Target: target}
	first, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	second, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if first != 3 || second != 0 {
		t.Fatalf("collector writes first=%d second=%d", first, second)
	}
	signals, err := target.ReadSignals()
	if err != nil {
		t.Fatal(err)
	}
	var lifecycle Signal
	closed := false
	for _, signal := range signals {
		if signal.LifecycleStage == "merged" {
			lifecycle = signal
		}
		closed = closed || signal.LifecycleStage == "issue_closed"
	}
	if len(signals) != 3 || lifecycle.IncidentFingerprint != fingerprint || !lifecycle.ProductFixMerged || lifecycle.LifecycleStage != "merged" || !closed {
		t.Fatalf("linked lifecycle signal=%+v", signals)
	}
}

func TestProcessLockRejectsConcurrentScheduler(t *testing.T) {
	store := testStore(t)
	release, err := store.TryProcessLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := store.TryProcessLock(); err != ErrAlreadyRunning {
		t.Fatalf("second lock error=%v", err)
	}
}

func TestEpisodeSurvivesRotationAndMergesNewLifecycleExactlyOnce(t *testing.T) {
	now := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	rules, err := incidentanalysis.LoadRules(rulesPath())
	if err != nil {
		t.Fatal(err)
	}
	initialSignals := []Signal{
		signalAt(now, "rotation-1", "rotation", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-rotation", "run-1", "product", "invariant", true
		}),
		signalAt(now.Add(time.Second), "rotation-2", "rotation", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-rotation", "run-2", "product", "invariant", true
		}),
	}
	prior, err := BuildEpisodes(initialSignals, DurableState{}, rules)
	if err != nil {
		t.Fatal(err)
	}
	var fingerprint string
	for fingerprint = range prior.Episodes {
	}
	merged := signalAt(now.Add(time.Hour), "rotation-3", "rotation", "lifecycle_outcome", "succeeded", func(s *Signal) {
		s.IncidentFingerprint, s.RunID, s.LifecycleStage, s.ProductFixMerged = fingerprint, "run-2", "merged", true
	})
	first, err := BuildEpisodes([]Signal{merged}, prior, rules)
	if err != nil {
		t.Fatal(err)
	}
	episode := first.Episodes[fingerprint]
	if episode.OccurrenceCount != 3 || episode.State != "resolved" || episode.PrimaryClassification != "confirmed_bug" || len(episode.Lifecycle) != 1 {
		t.Fatalf("merged episode=%+v", episode)
	}
	second, err := BuildEpisodes([]Signal{merged}, first, rules)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("replayed retained signal changed state\nfirst=%s\nsecond=%s", firstJSON, secondJSON)
	}
	withoutSignals, err := BuildEpisodes(nil, second, rules)
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutSignals.Episodes) != 1 || withoutSignals.Episodes[fingerprint].OccurrenceCount != 3 {
		t.Fatalf("rotation dropped prior episode: %+v", withoutSignals)
	}
}

func TestStateEventsCoverFixChecksReviewMergeAndCloseLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	fingerprint := digest("lifecycle-contract")
	events := []state.Event{
		{Version: 4, EventID: "life-worker", Sequence: 1, Timestamp: now, RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "worker_started", Payload: json.RawMessage(`{}`)},
		{Version: 4, EventID: "life-ready", Sequence: 2, Timestamp: now.Add(time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "pull_request_ready", Payload: json.RawMessage(`{}`)},
		{Version: 4, EventID: "life-review", Sequence: 3, Timestamp: now.Add(2 * time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "pull_request_review_observed", Payload: json.RawMessage(`{"review_decision":"APPROVED"}`)},
		{Version: 4, EventID: "life-merged", Sequence: 4, Timestamp: now.Add(3 * time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "issue_completed", Payload: json.RawMessage(`{}`)},
		{Version: 4, EventID: "life-closed", Sequence: 5, Timestamp: now.Add(4 * time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "github_state_synced", Payload: json.RawMessage(`{"state":"done"}`)},
	}
	want := map[string]string{
		"worker_started":       "started",
		"pull_request_created": "pending",
		"checks_passed":        "succeeded",
		"review_passed":        "succeeded",
		"merged":               "succeeded",
		"issue_closed":         "resolved",
	}
	got := map[string]string{}
	for _, event := range events {
		for _, signal := range signalsFromStateEvent("owner/repo", event, fingerprint, true) {
			if signal.LifecycleStage != "" {
				got[signal.LifecycleStage] = signal.OutcomeCode
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("lifecycle stages=%v want=%v", got, want)
	}
	for stage, outcome := range want {
		if got[stage] != outcome {
			t.Fatalf("stage %s=%q want=%q", stage, got[stage], outcome)
		}
	}

	failedChecks := state.Event{Version: 4, EventID: "life-checks-failed", Sequence: 6, Timestamp: now.Add(5 * time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "pull_request_checks_retry_exhausted", Payload: json.RawMessage(`{}`)}
	failedReview := state.Event{Version: 4, EventID: "life-review-failed", Sequence: 7, Timestamp: now.Add(6 * time.Second), RepoID: "repoid", IssueNumber: 9, RunID: "run-9", Type: "pull_request_review_observed", Payload: json.RawMessage(`{"review_decision":"CHANGES_REQUESTED"}`)}
	failures := append(signalsFromStateEvent("owner/repo", failedChecks, fingerprint, true), signalsFromStateEvent("owner/repo", failedReview, fingerprint, true)...)
	failureStages := map[string]bool{}
	for _, signal := range failures {
		if signal.Name == "lifecycle_outcome" && signal.OutcomeCode == "failed" {
			failureStages[signal.LifecycleStage] = true
		}
	}
	if !failureStages["checks_failed"] || !failureStages["review_failed"] {
		t.Fatalf("failure stages=%v", failureStages)
	}
}

func TestEpisodeCardinalityLimitIsConfigurableAndPreservesOccurrenceCount(t *testing.T) {
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	rules, err := incidentanalysis.LoadRules(rulesPath())
	if err != nil {
		t.Fatal(err)
	}
	signals := make([]Signal, 20)
	for index := range signals {
		signals[index] = signalAt(now.Add(time.Duration(index)*time.Second), fmt.Sprintf("bounded-%02d", index), "bounded", "failure_classified", "failed", func(s *Signal) {
			s.EpisodeID, s.RunID, s.FailureKind, s.FailureCode, s.InvariantViolation = "episode-bounded", fmt.Sprintf("run-%02d", index), "product", "invariant", true
		})
	}
	stateValue, err := BuildEpisodesWithLimit(signals, DurableState{}, rules, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, episode := range stateValue.Episodes {
		if episode.OccurrenceCount != 20 || len(episode.SignalIDs) != 16 || len(episode.Evidence) != 16 {
			t.Fatalf("bounded episode=%+v", episode)
		}
	}
}

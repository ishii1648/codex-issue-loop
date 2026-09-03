package incidentloop

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestDecisionLogAppendsWithoutRewritingExistingBytes(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	first := decisionFixture(now, 1)
	if err := store.AppendDecisions([]IssueDecision{first}, now); err != nil {
		t.Fatal(err)
	}
	prefix, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	second := decisionFixture(now.Add(time.Second), 2)
	if err := store.AppendDecisions([]IssueDecision{second}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, prefix) || bytes.Count(data, []byte{'\n'}) != 2 {
		t.Fatalf("normal write was not append-only: prefix=%q data=%q", prefix, data)
	}
	info, err := os.Stat(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("decision log permissions=%o", info.Mode().Perm())
	}
}

func TestDecisionLogRetentionKeepsExactSevenDayBoundary(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	older := decisionFixture(now.Add(-DecisionRetention-time.Nanosecond), 1)
	boundary := decisionFixture(now.Add(-DecisionRetention), 2)
	recent := decisionFixture(now.Add(-time.Hour), 3)
	for _, decision := range []IssueDecision{older, boundary, recent} {
		if err := store.AppendDecisions([]IssueDecision{decision}, decision.DecidedAt); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	firstLineEnd := bytes.IndexByte(before, '\n')
	if firstLineEnd < 0 {
		t.Fatal("fixture has no complete first record")
	}
	custom := append([]byte(nil), before[:firstLineEnd+1]...)
	custom = append(custom, ' ', ' ')
	custom = append(custom, before[firstLineEnd+1:]...)
	if err := os.WriteFile(store.DecisionsPath(), custom, 0o600); err != nil {
		t.Fatal(err)
	}
	wantBytes := append([]byte(nil), custom[firstLineEnd+1:]...)
	if err := store.AppendDecisions(nil, now); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, wantBytes) {
		t.Fatal("retention modified a retained record")
	}
	records, err := store.ReadDecisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != boundary.ID || records[1].ID != recent.ID {
		t.Fatalf("retained records=%+v", records)
	}
}

func TestDecisionLogSurvivesRestartAndConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	firstStore := Store{Dir: dir}
	if err := firstStore.AppendDecisions([]IssueDecision{decisionFixture(now, 1)}, now); err != nil {
		t.Fatal(err)
	}
	restarted := Store{Dir: dir}
	const writers = 24
	var wait sync.WaitGroup
	errorsByWriter := make(chan error, writers)
	for index := 2; index < writers+2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			decision := decisionFixture(now.Add(time.Duration(index)*time.Second), index)
			errorsByWriter <- restarted.AppendDecisions([]IssueDecision{decision}, decision.DecidedAt)
		}(index)
	}
	wait.Wait()
	close(errorsByWriter)
	for err := range errorsByWriter {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := restarted.ReadDecisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != writers+1 {
		t.Fatalf("record count=%d want=%d", len(records), writers+1)
	}
}

func TestDecisionLogCorruptionFailsClosedWithoutMutation(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := store.AppendDecisions([]IssueDecision{decisionFixture(now, 1)}, now); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append(append([]byte{}, valid...), []byte(`{"version":1`)...)
	if err := os.WriteFile(store.DecisionsPath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	err = store.AppendDecisions([]IssueDecision{decisionFixture(now.Add(time.Second), 2)}, now.Add(time.Second))
	if err == nil {
		t.Fatal("partial final record was accepted")
	}
	after, readErr := os.ReadFile(store.DecisionsPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("corrupt decision log was modified")
	}
}

func TestPipelineRejectsCorruptDecisionLogBeforeAnalysis(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"version":1`)
	if err := os.WriteFile(store.DecisionsPath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	analyzer := &fakeAnalyzer{issue: true}
	issues := &fakeIssues{byFingerprint: map[string]IssueRef{}}
	if _, err := testPipeline(store, analyzer, issues, &now, false).RunOnce(context.Background()); err == nil {
		t.Fatal("pipeline accepted a corrupt decision log")
	}
	if analyzer.calls != 0 || len(issues.drafts) != 0 {
		t.Fatalf("corrupt log crossed side-effect boundary: analyzer=%d drafts=%d", analyzer.calls, len(issues.drafts))
	}
	after, err := os.ReadFile(store.DecisionsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, corrupt) {
		t.Fatal("pipeline modified corrupt decision log")
	}
}

func decisionFixture(at time.Time, sequence int) IssueDecision {
	fingerprint := digest("decision-fixture", fmt.Sprint(sequence))
	episode := Episode{
		Version: SchemaVersion, ID: "inc-" + fingerprint[:20], Fingerprint: fingerprint,
		Repository: "owner/repo", PrimaryClassification: "unknown", Confidence: "medium",
		MatchedRules: []string{}, InvariantCodes: []string{},
	}
	return newIssueDecision(at, episode, false, "skipped", "classification_not_issue_eligible", nil)
}

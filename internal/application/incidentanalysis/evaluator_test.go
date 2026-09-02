package incidentanalysis

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCommittedCorpusEvaluatesDeterministically(t *testing.T) {
	root := repositoryRoot(t)
	withWorkingDirectory(t, root)
	dataDir := filepath.Join(root, "analysis", "incident-taxonomy")
	corpus, rules, err := Load(filepath.Join(dataDir, "corpus.json"), filepath.Join(dataDir, "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateBundle(dataDir, corpus); err != nil {
		t.Fatal(err)
	}
	first, err := Evaluate(corpus, rules)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(corpus, rules)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := MarshalCanonical(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := MarshalCanonical(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatal("same corpus and rules produced different output")
	}
	golden, err := os.ReadFile(filepath.Join(dataDir, "evaluation.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, golden) {
		t.Fatal("evaluation does not match committed golden")
	}
	if first.Total != 16 || first.Classified != 13 || first.Unknown != 3 || len(first.Mismatches) != 0 || len(first.RuleConflicts) != 0 {
		t.Fatalf("unexpected evaluation summary: %+v", first)
	}
	if first.ConfirmedBug.TruePositive != 9 || first.ConfirmedBug.FalsePositive != 0 || first.ConfirmedBug.FalseNegative != 0 {
		t.Fatalf("unexpected confirmed bug metrics: %+v", first.ConfirmedBug)
	}
	if first.ExpectedTransient.TruePositive != 1 || first.ExpectedTransient.FalsePositive != 0 || first.ExpectedTransient.FalseNegative != 0 {
		t.Fatalf("unexpected expected transient metrics: %+v", first.ExpectedTransient)
	}
}

func TestRulePriorityConflictIsRejected(t *testing.T) {
	root := repositoryRoot(t)
	corpus, rules, err := Load(filepath.Join(root, "analysis", "incident-taxonomy", "corpus.json"), filepath.Join(root, "analysis", "incident-taxonomy", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	rules.Rules = append(rules.Rules, Rule{
		ID: "conflict", Priority: rules.Rules[0].Priority, Classification: "unknown",
		Description: "test conflict", Always: true,
	})
	if err := Validate(corpus, rules); err == nil || !strings.Contains(err.Error(), "priority conflict") {
		t.Fatalf("expected priority conflict, got %v", err)
	}
}

func TestEveryPrimaryClassificationHasADeterministicRule(t *testing.T) {
	root := repositoryRoot(t)
	corpus, rules, err := Load(filepath.Join(root, "analysis", "incident-taxonomy", "corpus.json"), filepath.Join(root, "analysis", "incident-taxonomy", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name          string
		features      Features
		productChange string
		missing       []string
	}{
		{name: "confirmed_bug", features: Features{DocumentedInvariantViolation: true, CorroboratedProductFix: true}, productChange: "merged"},
		{name: "suspected_bug", features: Features{DocumentedInvariantViolation: true, RepeatedIndependentRuns: true}, productChange: "unknown"},
		{name: "operator_attention", features: Features{HumanActionRequired: true}, productChange: "unknown"},
		{name: "degraded", features: Features{ProgressStalled: true}, productChange: "unknown"},
		{name: "expected_transient", features: Features{TypedTransient: true, RetryDeadlineRespected: true, ConsecutiveFailures: 1, SuccessfulDomainEvent: true}, productChange: "none"},
		{name: "unknown", productChange: "unknown", missing: []string{"ground truth"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			incident := corpus.Incidents[0]
			incident.ID = "cil-synthetic-" + test.name
			incident.Fingerprint = "synthetic:" + test.name
			incident.Features = test.features
			incident.ProductCodeChange = test.productChange
			incident.GroundTruth = GroundTruth{PrimaryClassification: test.name, Basis: "synthetic rule coverage"}
			incident.MissingEvidence = test.missing
			result, err := Evaluate(Corpus{Version: 1, GeneratedAt: corpus.GeneratedAt, SelectionPolicy: "synthetic rule coverage", Incidents: []Incident{incident}}, rules)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Results[0].Classification; got != test.name {
				t.Fatalf("classification=%s want=%s", got, test.name)
			}
		})
	}
}

func TestUnknownAndSensitiveDataAreRejected(t *testing.T) {
	root := repositoryRoot(t)
	corpus, rules, err := Load(filepath.Join(root, "analysis", "incident-taxonomy", "corpus.json"), filepath.Join(root, "analysis", "incident-taxonomy", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	unknownIndex := -1
	for i := range corpus.Incidents {
		if corpus.Incidents[i].GroundTruth.PrimaryClassification == "unknown" {
			unknownIndex = i
			break
		}
	}
	if unknownIndex < 0 {
		t.Fatal("fixture has no unknown incident")
	}
	copyCorpus := cloneCorpus(t, corpus)
	copyCorpus.Incidents[unknownIndex].MissingEvidence = nil
	if err := Validate(copyCorpus, rules); err == nil || !strings.Contains(err.Error(), "missing_evidence") {
		t.Fatalf("expected missing evidence rejection, got %v", err)
	}
	copyCorpus = cloneCorpus(t, corpus)
	copyCorpus.Incidents[0].Symptom = "raw path /Users/example/private"
	if err := Validate(copyCorpus, rules); err == nil || !strings.Contains(err.Error(), "prohibited") {
		t.Fatalf("expected sensitive marker rejection, got %v", err)
	}
}

func TestAnalysisArtifactsContainNoRawOrSecretMarkers(t *testing.T) {
	root := repositoryRoot(t)
	paths := []string{filepath.Join(root, "analysis", "incident-taxonomy"), filepath.Join(root, "schemas")}
	markers := [][]byte{[]byte("/Users/"), []byte("ghp_"), []byte("github_pat_"), []byte("Bearer "), []byte("\"token\":")}
	for _, path := range paths {
		err := filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (!strings.HasSuffix(name, ".json") && !strings.HasSuffix(name, ".md")) {
				return nil
			}
			raw, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			for _, marker := range markers {
				if bytes.Contains(raw, marker) {
					t.Fatalf("%s contains prohibited marker %q", name, marker)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func cloneCorpus(t *testing.T, corpus Corpus) Corpus {
	t.Helper()
	raw, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	var cloned Corpus
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", ".."))
}

func withWorkingDirectory(t *testing.T, directory string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

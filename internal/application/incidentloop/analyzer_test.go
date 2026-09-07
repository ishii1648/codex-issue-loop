package incidentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexAnalysisSchemaDeclaresTypeForConstProperties(t *testing.T) {
	var schema struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(incidentAnalysisSchema, &schema); err != nil {
		t.Fatal(err)
	}
	for name, property := range schema.Properties {
		if _, hasConst := property["const"]; hasConst && property["type"] == nil {
			t.Errorf("property %q uses const without type", name)
		}
	}
}

func TestCodexAnalyzerClassifiesInvalidOutputSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '{\"error\":{\"code\":\"invalid_json_schema\"}}' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	analyzer := CodexAnalyzer{Path: path, RepoPath: dir, StateDir: dir, Timeout: 10 * time.Second}
	_, err := analyzer.Analyze(context.Background(), EvidenceBundle{Version: SchemaVersion})
	if err == nil || analysisFailureCode(err) != "invalid_output_schema" {
		t.Fatalf("err=%v code=%s", err, analysisFailureCode(err))
	}
}

func TestCommandAnalyzerRejectsMalformedJSONAndBoundsTimeout(t *testing.T) {
	bundle := EvidenceBundle{Version: SchemaVersion, EpisodeID: "inc-test", Fingerprint: strings.Repeat("a", 64), Repository: "owner/repo", PrimaryClassification: "unknown", Confidence: "low", SignalIDs: []string{}, Evidence: []EvidenceRef{}}
	for _, test := range []struct {
		name    string
		mode    string
		timeout time.Duration
		match   string
	}{
		{name: "malformed", mode: "malformed", timeout: 10 * time.Second, match: "decode AI analyzer output"},
		{name: "timeout", mode: "timeout", timeout: 20 * time.Millisecond, match: "AI analyzer timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			analyzer := CommandAnalyzer{Path: os.Args[0], Args: []string{"-test.run=TestAnalyzerHelperProcess", "--", test.mode}, Timeout: test.timeout}
			_, err := analyzer.Analyze(context.Background(), bundle)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestAnalyzerHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "malformed":
		fmt.Print(`{"version":`)
	case "timeout":
		time.Sleep(time.Minute)
	default:
		t.Fatal(errors.New("unknown helper mode"))
	}
}

func TestAnalyzersDistinguishCancellationFromTimeout(t *testing.T) {
	dir := t.TempDir()
	analyzers := map[string]Analyzer{
		"command": CommandAnalyzer{Path: os.Args[0], Timeout: time.Minute},
		"codex":   CodexAnalyzer{Path: os.Args[0], RepoPath: dir, StateDir: dir, Timeout: time.Minute},
	}
	for name, analyzer := range analyzers {
		t.Run(name, func(t *testing.T) {
			for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
				ctx, cancel := context.WithCancel(context.Background())
				if cause == context.DeadlineExceeded {
					cancel()
					ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				} else {
					cancel()
				}
				_, err := analyzer.Analyze(ctx, EvidenceBundle{Version: SchemaVersion})
				cancel()
				wantCode := "canceled"
				if cause == context.DeadlineExceeded {
					wantCode = "timeout"
				}
				if !errors.Is(err, cause) || analysisFailureCode(err) != wantCode {
					t.Fatalf("err=%v code=%s want=%s", err, analysisFailureCode(err), wantCode)
				}
			}
		})
	}
}

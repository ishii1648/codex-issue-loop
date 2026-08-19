package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishii1648/codex-issue-loop/internal/config"
)

func writeLabelFake(t *testing.T, existing []LabelSpec, failName string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	response := filepath.Join(dir, "labels.json")
	logPath := filepath.Join(dir, "calls.log")
	encoded, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(response, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "fake-gh")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "label list") printf 'harmless warning\n' >&2; cat %q ;;
  "label create")
    printf '%%s\n' "$*" >> %q
    if [ "$3" = %q ]; then printf 'permission denied\n' >&2; exit 1; fi
    ;;
  *) exit 2 ;;
esac
`, response, logPath, failName)
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fake, logPath
}

func TestBootstrapLabelsPreviewsCreatesAndPreservesExistingMetadata(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	existing := []LabelSpec{{Name: cfg.GitHub.RunningLabel, Color: "FFFFFF", Description: "repository owner metadata"}}
	fake, logPath := writeLabelFake(t, existing, "")
	client := CLI{Path: fake}
	preview, err := client.BootstrapLabels(context.Background(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Applied || preview.DeletesLabels || preview.ExistingMetadataPolicy != "preserve" {
		t.Fatalf("preview=%+v", preview)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("preview changed labels: %v", err)
	}
	result, err := client.BootstrapLabels(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 5 {
		t.Fatalf("created=%v", result.Created)
	}
	for _, action := range result.Actions {
		if action.Desired.Name == cfg.GitHub.RunningLabel {
			if action.Action != "preserve" || !action.MetadataDiffers || action.ExistingDescription != "repository owner metadata" {
				t.Fatalf("existing metadata was not preserved: %+v", action)
			}
		}
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "label create "+cfg.GitHub.RunningLabel+" ") || strings.Contains(string(calls), "--force") || strings.Contains(string(calls), "label delete") {
		t.Fatalf("unsafe label mutation: %s", calls)
	}
}

func TestBootstrapLabelsIsIdempotentWhenEveryLabelExists(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	fake, logPath := writeLabelFake(t, RequiredLabelSpecs(cfg), "")
	result, err := (CLI{Path: fake}).BootstrapLabels(context.Background(), cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Created) != 0 {
		t.Fatalf("created=%v", result.Created)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("idempotent run issued create calls: %v", err)
	}
	for _, action := range result.Actions {
		if action.Action != "preserve" || action.MetadataDiffers {
			t.Fatalf("action=%+v", action)
		}
	}
}

func TestRequiredLabelSpecsIncludesPriorityLabelsWithoutCaseDuplicate(t *testing.T) {
	cfg := config.Defaults()
	cfg.Queue.PriorityLabels = []string{"priority:high", "PRIORITY:LOW", strings.ToUpper(cfg.GitHub.ReadyLabels[0])}
	cfg.Resources.Definitions = []config.ResourceDefinition{{Name: "Git", Paths: []string{"internal/git/**"}}}
	specs := RequiredLabelSpecs(cfg)
	counts := map[string]int{}
	for _, spec := range specs {
		counts[strings.ToLower(spec.Name)]++
	}
	if counts["priority:high"] != 1 || counts["priority:low"] != 1 || counts[strings.ToLower(cfg.GitHub.ReadyLabels[0])] != 1 {
		t.Fatalf("priority label specs are missing or duplicated: %+v", specs)
	}
	if counts["area:git"] != 1 {
		t.Fatalf("resource label spec is missing: %+v", specs)
	}
}

func TestFaultBootstrapLabelsReportsPartialSuccessAndCanBeRerun(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitHub.Repo = "owner/repo"
	failName := cfg.GitHub.NeedsInputLabel
	fake, _ := writeLabelFake(t, nil, failName)
	result, err := (CLI{Path: fake}).BootstrapLabels(context.Background(), cfg, true)
	var bootstrapErr *LabelBootstrapError
	if !errors.As(err, &bootstrapErr) || !strings.Contains(err.Error(), "rerunning is safe") || len(result.Failures) != 1 || result.Failures[0].Name != failName || len(result.Created) != 5 {
		t.Fatalf("result=%+v error=%T %v", result, bootstrapErr, err)
	}
	if strings.Contains(result.Failures[0].Reason, "ghp_") {
		t.Fatalf("unsafe failure reason: %s", result.Failures[0].Reason)
	}
}

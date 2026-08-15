package compat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionComparison(t *testing.T) {
	for _, test := range []struct {
		current, minimum string
		ok               bool
	}{{"0.136.0", "0.136.0", true}, {"0.137.0", "0.136.0", true}, {"1.0.0", "0.136.0", true}, {"0.135.9", "0.136.0", false}, {"bad", "0.136.0", false}} {
		if got := AtLeast(test.current, test.minimum); got != test.ok {
			t.Fatalf("AtLeast(%q, %q)=%v", test.current, test.minimum, got)
		}
	}
}

func TestCapabilityProbes(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	gh := filepath.Join(dir, "gh")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.136.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
if [ "$1 $2 $3" = "exec resume --help" ]; then echo '--json --output-schema --output-last-message'; exit 0; fi
exit 2
`)
	writeExecutable(t, gh, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'gh version 2.69.0'; exit 0; fi
case "$1 $2 $3" in
  'issue list --help') echo '--json --limit --label --assignee --milestone' ;;
  'issue edit --help') echo '--add-label --remove-label' ;;
  'issue comment --help') echo '--body' ;;
  *) exit 2 ;;
esac
`)
	if report := ProbeCodex(context.Background(), codex); !report.OK() || !report.Has("session_resume") {
		t.Fatalf("codex report=%+v", report)
	}
	if report := ProbeGH(context.Background(), gh); !report.OK() {
		t.Fatalf("gh report=%+v", report)
	}
}

func TestCodexProbeAllowsMissingResumeAsFallbackCapability(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	writeExecutable(t, codex, `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'codex-cli 0.136.0'; exit 0; fi
if [ "$1 $2" = "exec --help" ]; then echo '--json --output-schema --output-last-message --sandbox --cd'; exit 0; fi
exit 2
`)
	report := ProbeCodex(context.Background(), codex)
	if !report.VersionOK || !report.Has("exec_structured") || report.Has("session_resume") || len(report.Missing) != 0 {
		t.Fatalf("report=%+v", report)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(fmt.Errorf("write fake executable: %w", err))
	}
}

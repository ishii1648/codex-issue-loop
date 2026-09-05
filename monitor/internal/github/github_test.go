package github

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
)

func TestCLIObservesOnlyRelevantLabelProgress(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "gh")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
  *issues\?*) printf '%s\n' '[[{"number":1,"created_at":"2026-09-05T10:00:00Z","labels":[{"name":"codex-loop:running"}]},{"number":2,"created_at":"2026-09-05T10:00:00Z","labels":[{"name":"codex-loop:done"}]}]]' ;;
  *events\?*) printf '%s\n' '[{"id":10,"event":"labeled","created_at":"2026-09-05T10:01:00Z","label":{"name":"codex-loop:ready"},"issue":{"number":1}},{"id":11,"event":"renamed","created_at":"2026-09-05T10:02:00Z","label":{"name":""},"issue":{"number":1}},{"id":12,"event":"labeled","created_at":"2026-09-05T10:03:00Z","label":{"name":"codex-loop:running"},"issue":{"number":1}}]' ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"codex-loop:ready"}, RunningLabel: "codex-loop:running", TerminalLabels: []string{"codex-loop:done"}, AcceptanceTimeout: config.Duration{Duration: time.Minute}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
	observation, err := (CLI{Path: script}).Observe(context.Background(), repo, 10, time.Date(2026, 9, 5, 10, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Items) != 1 || observation.Items[0].Phase != model.Running || !observation.Items[0].PhaseSince.Equal(time.Date(2026, 9, 5, 10, 3, 0, 0, time.UTC)) {
		t.Fatalf("observation = %+v", observation)
	}
	if len(observation.EventIDs) != 1 || observation.EventIDs[0] != 12 || observation.Cursor != 12 {
		t.Fatalf("event replay metadata = %+v", observation)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		if !strings.HasPrefix(line, "api --method GET ") {
			t.Fatalf("non-read-only gh invocation: %q", line)
		}
	}
}

func TestCursorSearchStopsAtPageLimit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "args")
	script := filepath.Join(dir, "gh")
	body := `#!/bin/sh
printf '%s\n' "$*" >> "` + logPath + `"
case "$*" in
 *events\?*)
 printf '['
 i=0
 while [ "$i" -lt 100 ]; do
  [ "$i" -eq 0 ] || printf ','
  printf '{"id":1000,"event":"renamed"}'
  i=$((i+1))
 done
 printf ']'
 ;;
 *) printf '[[]]' ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	_, err := (CLI{Path: script}).Observe(context.Background(), config.Repository{Name: "owner/repo"}, 1, time.Now())
	if err == nil || !strings.Contains(err.Error(), "cursor not found") {
		t.Fatalf("error = %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "/issues/events?"); got != eventPageLimit {
		t.Fatalf("event requests = %d", got)
	}
	if strings.Contains(string(data), "page=11") || strings.Contains(string(data), "--paginate --slurp repos/owner/repo/issues/events") {
		t.Fatalf("unbounded requests: %s", data)
	}
}

func TestQueueExitAndReentryEventContract(t *testing.T) {
	for _, test := range []struct{ event, label, kind string }{
		{"unlabeled", "ready", "remove"},
		{"unlabeled", "running", "remove"},
		{"unlabeled", "excluded", "unproven"},
		{"unlabeled", "done", "unproven"},
		{"reopened", "", "unproven"},
		{"closed", "", "exit"},
		{"labeled", "done", "exit"},
		{"labeled", "excluded", "exit"},
	} {
		t.Run(test.event+"/"+test.label, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			body := `#!/bin/sh
case "$*" in
 *events\?*) printf '%s' '[{"id":2,"event":"` + test.event + `","created_at":"2026-09-05T10:01:00Z","label":{"name":"` + test.label + `"},"issue":{"number":1}},{"id":1,"event":"renamed"}]' ;;
 *) printf '[[]]' ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done"}, ExcludeLabels: []string{"excluded"}}
			observation, err := (CLI{Path: script}).Observe(context.Background(), repo, 1, time.Date(2026, 9, 5, 10, 2, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			if len(observation.Events) != 1 || observation.Events[0].Kind != test.kind || observation.Cursor != 2 {
				t.Fatalf("observation = %+v", observation)
			}
		})
	}
}

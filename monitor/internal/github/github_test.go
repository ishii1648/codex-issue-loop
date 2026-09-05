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
  *issues/events*) printf '%s\n' '[{"id":12,"event":"labeled","created_at":"2026-09-05T10:03:00Z","label":{"name":"codex-loop:running"},"issue":{"number":1}},{"id":11,"event":"renamed","created_at":"2026-09-05T10:02:00Z","label":{"name":""},"issue":{"number":1}},{"id":10,"event":"labeled","created_at":"2026-09-05T10:01:00Z","label":{"name":"codex-loop:ready"},"issue":{"number":1}}]' ;;
  *issues\?*) printf '%s\n' '[[{"number":1,"created_at":"2026-09-05T10:00:00Z","labels":[{"name":"codex-loop:running"}]},{"number":2,"created_at":"2026-09-05T10:00:00Z","labels":[{"name":"codex-loop:done"}]}]]' ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"codex-loop:ready"}, RunningLabel: "codex-loop:running", TerminalLabels: []string{"codex-loop:done"}, AcceptanceTimeout: config.Duration{Duration: time.Minute}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
	observation, err := (CLI{Path: script}).Observe(context.Background(), repo, 10, true, time.Date(2026, 9, 5, 10, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(observation.Items) != 1 || observation.Items[0].Phase != model.Running {
		t.Fatalf("observation = %+v", observation)
	}
	if len(observation.Events) != 1 || observation.Events[0].ID != 12 || observation.Events[0].Kind != model.RunningLabeled || observation.Cursor != 12 {
		t.Fatalf("event replay metadata = %+v", observation)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(args)), "\n") {
		if !strings.HasPrefix(line, "api --method GET") {
			t.Fatalf("non-read-only gh invocation: %q", line)
		}
		if strings.Contains(line, "issues/events") && strings.Contains(line, "--paginate") {
			t.Fatalf("repository events were fetched without a cursor bound: %q", line)
		}
	}
}

func TestCLITerminalEventsEndQueueAtGitHubTime(t *testing.T) {
	for _, event := range []string{
		`"event":"closed"`,
		`"event":"labeled","label":{"name":"done"}`,
	} {
		t.Run(event, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			body := `#!/bin/sh
case "$*" in
  *issues/events*) printf '%s\n' '[{"id":11,` + event + `,"created_at":"2026-09-05T10:10:00Z","issue":{"number":1}},{"id":10}]' ;;
  *issues\?*) printf '%s\n' '[[]]' ;;
  *) exit 9 ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
			repo := config.Repository{Name: "owner/repo", RunningLabel: "running", TerminalLabels: []string{"done"}}
			initial, _, err := model.Apply(nil, model.Observation{
				Repository: repo.Name, ObservedAt: base, Cursor: 10, CursorInitialized: true,
				Items: []model.QueueItem{{Number: 1, Phase: model.Running, PhaseSince: base, Deadline: base.Add(time.Hour)}},
			})
			if err != nil {
				t.Fatal(err)
			}
			observation, err := (CLI{Path: script}).Observe(context.Background(), repo, 10, true, base.Add(20*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			next, closed, err := model.Apply(&initial, observation)
			if err != nil {
				t.Fatal(err)
			}
			terminalAt := base.Add(10 * time.Minute)
			if next.Current.Status != model.Idle || !next.Current.StartedAt.Equal(terminalAt) || len(closed) != 1 || !closed[0].EndedAt.Equal(terminalAt) {
				t.Fatalf("terminal replay: next=%+v closed=%+v", next, closed)
			}
		})
	}
}

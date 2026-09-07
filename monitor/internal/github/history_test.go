package github

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

func TestIssueHistoryIgnoresUnmonitoredLabelDisagreement(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	repo := config.Repository{ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done"}, ExcludeLabels: []string{"blocked"}}
	for _, tc := range []struct {
		name, event, labels string
	}{
		{"deleted label", "labeled", `[{"name":"ready"}]`},
		{"renamed label", "labeled", `[{"name":"ready"},{"name":"defect"}]`},
		{"unlabeled but present", "unlabeled", `[{"name":"ready"},{"name":"bug"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := rawIssue{Number: 1, State: "open"}
			if err := json.Unmarshal([]byte(tc.labels), &issue.Labels); err != nil {
				t.Fatal(err)
			}
			history := []rawEvent{
				{ID: 1, Event: tc.event, CreatedAt: base},
				{ID: 2, Event: "labeled", CreatedAt: base.Add(time.Minute)},
				{ID: 3, Event: tc.event, CreatedAt: base.Add(2 * time.Minute)},
			}
			history[0].Label.Name = "bug"
			history[1].Label.Name = "ready"
			history[2].Label.Name = "bug"
			events, since, err := issueHistory(repo, issue, history, base.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			want := []model.QueueEvent{{ID: 2, IssueNumber: 1, Kind: model.ReadyLabeled, At: base.Add(time.Minute)}}
			if !reflect.DeepEqual(events, want) || !since.Equal(want[0].At) {
				t.Fatalf("events=%+v, since=%s", events, since)
			}
		})
	}
}

func TestIssueHistoryRejectsMonitoredLabelDisagreement(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	repo := config.Repository{ReadyLabels: []string{"Ready", "Ready-Other"}, RunningLabel: "Running", TerminalLabels: []string{"Done"}, ExcludeLabels: []string{"Blocked"}}
	for _, label := range []string{"ready", "ready-other", "running", "done", "blocked"} {
		for _, kind := range []string{"labeled", "unlabeled"} {
			t.Run(label+"/"+kind, func(t *testing.T) {
				issue := rawIssue{Number: 1, State: "open"}
				if kind == "unlabeled" {
					issue.Labels = append(issue.Labels, struct {
						Name string `json:"name"`
					}{strings.ToUpper(label)})
				}
				event := rawEvent{ID: 1, Event: kind, CreatedAt: base}
				event.Label.Name = label
				_, _, err := issueHistory(repo, issue, []rawEvent{event}, base.Add(time.Hour))
				if !errors.Is(err, errHistoryIncomplete) {
					t.Fatalf("error=%v, want errHistoryIncomplete", err)
				}
			})
		}
	}
}

func TestReentryHistories(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", ExcludeLabels: []string{"blocked", "excluded"}, TerminalLabels: []string{"done"}, AcceptanceTimeout: config.Duration{Duration: time.Minute}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
	for _, tc := range []struct {
		name, labels, state string
		history             [][2]string
		want                []model.EventKind
		since               int
		pr                  bool
	}{
		{name: "280 reopened with ready", labels: `["ready"]`, history: [][2]string{{"labeled", "ready"}, {"closed", ""}, {"reopened", ""}}, want: []model.EventKind{model.ReadyLabeled, model.QueueExited, model.ReadyLabeled}, since: 3},
		{name: "reopened with running", labels: `["running"]`, history: [][2]string{{"labeled", "running"}, {"closed", ""}, {"reopened", ""}}, want: []model.EventKind{model.RunningLabeled, model.QueueExited, model.RunningLabeled}, since: 3},
		{name: "459 blocked removed", labels: `["ready"]`, history: [][2]string{{"labeled", "ready"}, {"labeled", "blocked"}, {"unlabeled", "blocked"}}, want: []model.EventKind{model.ReadyLabeled, model.QueueExited, model.ReadyLabeled}, since: 3},
		{name: "multiple exclusions", labels: `["ready","excluded"]`, history: [][2]string{{"labeled", "ready"}, {"labeled", "blocked"}, {"labeled", "excluded"}, {"unlabeled", "blocked"}}, want: []model.EventKind{model.QueueExited, model.ReadyLabeled}},
		{name: "last exclusion removed", labels: `["ready"]`, history: [][2]string{{"labeled", "ready"}, {"labeled", "blocked"}, {"labeled", "done"}, {"unlabeled", "blocked"}, {"unlabeled", "done"}}, want: []model.EventKind{model.ReadyLabeled, model.QueueExited, model.ReadyLabeled}, since: 5},
		{name: "labels change while excluded", labels: `["running"]`, history: [][2]string{{"labeled", "ready"}, {"labeled", "blocked"}, {"unlabeled", "ready"}, {"labeled", "running"}, {"unlabeled", "blocked"}}, want: []model.EventKind{model.RunningLabeled, model.QueueExited, model.ReadyLabeled}, since: 5},
		{name: "unrelated reopen", labels: `[]`, history: [][2]string{{"closed", ""}, {"reopened", ""}}},
		{name: "PR", labels: `["ready"]`, history: [][2]string{{"labeled", "ready"}, {"closed", ""}, {"reopened", ""}}, pr: true},
		{name: "reentry then exit", labels: `["ready"]`, state: "closed", history: [][2]string{{"labeled", "ready"}, {"closed", ""}, {"reopened", ""}, {"closed", ""}}, want: []model.EventKind{model.QueueExited, model.ReadyLabeled, model.QueueExited, model.ReadyLabeled}},
		{name: "unknown demand", labels: `["ready"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := rawIssue{Number: 280, State: tc.state}
			if issue.State == "" {
				issue.State = "open"
			}
			if tc.pr {
				issue.PullRequest = &struct{}{}
			}
			var labels []string
			if err := json.Unmarshal([]byte(tc.labels), &labels); err != nil {
				t.Fatal(err)
			}
			for _, label := range labels {
				issue.Labels = append(issue.Labels, struct {
					Name string `json:"name"`
				}{label})
			}
			var history []rawEvent
			for i, entry := range tc.history {
				e := rawEvent{ID: int64(i + 1), Event: entry[0], CreatedAt: base.Add(time.Duration(i+1) * time.Minute)}
				e.Label.Name = entry[1]
				e.Issue.Number = 280
				history = append(history, e)
			}
			events, since, err := issueHistory(repo, issue, history, base.Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != len(tc.want) {
				t.Fatalf("events=%+v", events)
			}
			for i, kind := range tc.want {
				if events[i].Kind != kind {
					t.Fatalf("events=%+v", events)
				}
			}
			if tc.since > 0 && !since.Equal(base.Add(time.Duration(tc.since)*time.Minute)) {
				t.Fatalf("since=%s", since)
			}
			if tc.name == "unknown demand" && !since.IsZero() {
				t.Fatalf("unproven since=%s", since)
			}
		})
	}
}

func TestVerifiedCurrentResynchronization(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", ExcludeLabels: []string{"blocked"}, AcceptanceTimeout: config.Duration{Duration: time.Minute}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
	for _, tc := range []struct {
		name, issues, history string
		want                  model.Status
	}{
		{"empty", `[]`, `[]`, model.Idle},
		{"expired", `[{"number":459,"state":"open","labels":[{"name":"ready"}]}]`, `[{"id":2,"event":"labeled","label":{"name":"ready"},"created_at":"2026-09-06T10:00:00Z"}]`, model.Down},
		{"unproven", `[{"number":459,"state":"open","labels":[{"name":"ready"}]}]`, `[]`, model.Unknown},
		{"reopened", `[{"number":459,"state":"open","labels":[{"name":"running"}]}]`, `[{"id":2,"event":"labeled","label":{"name":"running"},"created_at":"2026-09-06T10:00:00Z"},{"id":3,"event":"closed","created_at":"2026-09-06T10:01:00Z"},{"id":4,"event":"reopened","created_at":"2026-09-06T10:02:00Z"}]`, model.Healthy},
		{"blocked", `[{"number":459,"state":"open","labels":[{"name":"ready"}]}]`, `[{"id":2,"event":"labeled","label":{"name":"ready"},"created_at":"2026-09-06T10:00:00Z"},{"id":3,"event":"labeled","label":{"name":"blocked"},"created_at":"2026-09-06T10:01:00Z"},{"id":4,"event":"unlabeled","label":{"name":"blocked"},"created_at":"2026-09-06T10:02:00Z"}]`, model.Down},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			body := `#!/bin/sh
case "$*" in
 *issues/events*) printf '%s' '[{"id":4,"event":"renamed"}]' ;;
 *issues/459/events*) printf '%s' '[` + tc.history + `]' ;;
 *issues\?*) printf '%s' '[` + tc.issues + `]' ;;
 *) exit 9 ;;
esac
`
			if tc.name == "reopened" || tc.name == "blocked" {
				var events []rawEvent
				if err := json.Unmarshal([]byte(tc.history), &events); err != nil {
					t.Fatal(err)
				}
				var repoEvents []rawEvent
				for i := len(events) - 1; i >= 0; i-- {
					event := events[i]
					event.Issue.Number = 459
					repoEvents = append(repoEvents, event)
				}
				repoEvents = append(repoEvents, rawEvent{ID: 1})
				data, err := json.Marshal(repoEvents)
				if err != nil {
					t.Fatal(err)
				}
				body = strings.Replace(body, `[{"id":4,"event":"renamed"}]`, string(data), 1)
			}
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			previous, _, err := model.Apply(nil, model.Observation{Repository: repo.Name, ObservedAt: base, Cursor: 1, CursorInitialized: true, Error: "previous observation failed"})
			if err != nil {
				t.Fatal(err)
			}
			disk := store.Store{Root: filepath.Join(dir, "state")}
			if err := disk.Commit(previous, nil); err != nil {
				t.Fatal(err)
			}
			for poll := 0; poll < 3; poll++ {
				previous, err := disk.Load(repo.Name)
				if err != nil {
					t.Fatal(err)
				}
				at := base.Add(time.Duration(10+poll) * time.Minute)
				obs, err := (CLI{Path: script}).Observe(context.Background(), repo, previous.EventCursor, true, at)
				if err != nil {
					t.Fatal(err)
				}
				next, closed, err := model.Apply(previous, obs)
				if err != nil {
					t.Fatal(err)
				}
				if next.Current.Status != tc.want || next.EventCursor != 4 {
					t.Fatalf("next=%+v", next)
				}
				if poll == 0 && tc.want != model.Unknown && (len(closed) != 1 || closed[0].Status != model.Unknown || !closed[0].EndedAt.Equal(at)) {
					t.Fatalf("closed=%+v", closed)
				}
				if tc.name == "expired" && !next.QueueDeadline.Equal(base.Add(time.Minute)) {
					t.Fatalf("deadline=%s", next.QueueDeadline)
				}
				if err := disk.Commit(next, closed); err != nil {
					t.Fatal(err)
				}
				disk = store.Store{Root: filepath.Join(dir, "state")}
			}
			intervals, err := disk.AllIntervals(repo.Name)
			if err != nil {
				t.Fatal(err)
			}
			if intervals[0].Status != model.Unknown || !intervals[0].StartedAt.Equal(base) {
				t.Fatalf("intervals=%+v", intervals)
			}
			report := model.BuildReport(repo.Name, intervals, base, base.Add(12*time.Minute))
			unknown := float64(600)
			if tc.want == model.Unknown {
				unknown = 720
			}
			if report.DurationsSeconds[model.Unknown] != unknown {
				t.Fatalf("report=%+v", report)
			}

		})
	}
}

func TestObservationRejectsChangingOrFailedSnapshot(t *testing.T) {
	for _, mode := range []string{"head", "snapshot", "http", "conflict", "history", "null"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			body := `#!/bin/sh
case "$*" in
 *issues/events*)
 if [ '` + mode + `' = head ]; then n=2; [ ! -f '` + dir + `/head' ] || n=$(cat '` + dir + `/head'); n=$((n+1)); printf '%s' "$n" > '` + dir + `/head'; printf '[{"id":%s}]' "$n"; else printf '%s' '[{"id":2}]'; fi ;;
 *issues/1/events*) if [ '` + mode + `' = history ]; then exit 8; fi; printf '%s' '[[]]' ;;
 *issues\?*)
 if [ '` + mode + `' = http ]; then exit 7; fi
 if [ '` + mode + `' = null ]; then printf 'null'; exit 0; fi
 if [ '` + mode + `' = conflict ]; then printf '%s' '[[{"number":1,"labels":[{"name":"ready"},{"name":"running"}]}]]'
 elif [ -f '` + dir + `/snapshot' ] && [ '` + mode + `' = snapshot ]; then printf '%s' '[[]]'
 else touch '` + dir + `/snapshot'; printf '%s' '[[{"number":1,"labels":[{"name":"ready"}]}]]'; fi ;;
 *) exit 9 ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := (CLI{Path: script}).Observe(context.Background(), config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running"}, 1, true, time.Now())
			if err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}

func TestIncompletePastHistoryResynchronization(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	for _, mode := range []string{"closed twice", "missing label", "unproven current", "expired current", "current history incomplete", "current conflict", "http", "head", "snapshot", "other history http"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			history := `[{"id":2,"event":"closed","created_at":"2026-08-19T17:10:38Z"},{"id":3,"event":"closed","created_at":"2026-09-06T09:00:00Z"}]`
			if mode == "missing label" {
				history = `[{"id":2,"event":"unlabeled","label":{"name":"ready"},"created_at":"2026-08-19T17:10:38Z"},{"id":3,"event":"unlabeled","label":{"name":"ready"},"created_at":"2026-09-06T09:00:00Z"}]`
			}
			body := `#!/bin/sh
case "$*" in
 *issues/events*)
 if [ -f '` + dir + `/head' ] && [ '` + mode + `' = head ]; then printf '%s' '[{"id":5}]'
 else touch '` + dir + `/head'; printf '%s' '[{"id":4,"event":"closed","issue":{"number":456},"created_at":"2026-09-06T09:01:00Z"},{"id":3,"event":"closed","issue":{"number":455},"created_at":"2026-09-06T09:00:00Z"},{"id":1}]'; fi ;;
 *issues/455/events*) if [ '` + mode + `' = http ]; then exit 7; fi; printf '%s' '[` + history + `]' ;;
 *issues/456/events*) if [ '` + mode + `' = 'other history http' ]; then exit 8; fi; printf '%s' '[[]]' ;;
 *issues/459/events*)
 if [ '` + mode + `' = 'current history incomplete' ]; then printf '%s' '[` + history + `]'
 elif [ '` + mode + `' = 'expired current' ]; then printf '%s' '[[{"id":2,"event":"labeled","label":{"name":"ready"},"created_at":"2026-09-06T09:00:00Z"}]]'
 else printf '%s' '[[]]'; fi ;;
 *issues/455*) printf '%s' '{"number":455,"state":"closed","labels":[]}' ;;
 *issues/456*) printf '%s' '{"number":456,"state":"closed","labels":[]}' ;;
 *issues\?*)
 if [ '` + mode + `' = 'current conflict' ]; then printf '%s' '[[{"number":459,"state":"open","labels":[{"name":"ready"},{"name":"running"}]}]]'
 elif [ '` + mode + `' = 'unproven current' ] || [ '` + mode + `' = 'expired current' ] || [ '` + mode + `' = 'current history incomplete' ] || { [ '` + mode + `' = snapshot ] && [ -f '` + dir + `/snapshot' ]; }; then printf '%s' '[[{"number":459,"state":"open","labels":[{"name":"ready"}]}]]'
 else touch '` + dir + `/snapshot'; printf '%s' '[[]]'; fi ;;
 *) exit 9 ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", AcceptanceTimeout: config.Duration{Duration: 10 * time.Minute}}
			disk := store.Store{Root: filepath.Join(dir, "state")}
			previous, _, err := model.Apply(nil, model.Observation{Repository: repo.Name, ObservedAt: base, Cursor: 1, CursorInitialized: true, Error: "unavailable"})
			if err != nil {
				t.Fatal(err)
			}
			if err := disk.Commit(previous, nil); err != nil {
				t.Fatal(err)
			}
			for poll := 0; poll < 4; poll++ {
				saved, err := disk.Load(repo.Name)
				if err != nil {
					t.Fatal(err)
				}
				at := base.Add(time.Duration(poll+1) * time.Minute)
				obs, err := (CLI{Path: script}).Observe(context.Background(), repo, saved.EventCursor, true, at)
				switch mode {
				case "http", "head", "snapshot", "other history http", "current conflict", "current history incomplete":
					if err == nil {
						t.Fatal("invalid observation accepted")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !obs.CurrentVerified || (poll == 0 && !obs.Resynchronized) {
					t.Fatalf("observation=%+v", obs)
				}
				next, closed, err := model.Apply(saved, obs)
				if err != nil {
					t.Fatal(err)
				}
				want := model.Idle
				if mode == "unproven current" {
					want = model.Unknown
				}
				if mode == "expired current" {
					want = model.Down
					if !next.QueueDeadline.Equal(base.Add(-50 * time.Minute)) {
						t.Fatalf("deadline=%s", next.QueueDeadline)
					}
				}
				if next.Current.Status != want || next.EventCursor != 4 {
					t.Fatalf("next=%+v", next)
				}
				if poll == 0 && want != model.Unknown && (len(closed) != 1 || closed[0].Status != model.Unknown || !closed[0].StartedAt.Equal(base) || !closed[0].EndedAt.Equal(at)) {
					t.Fatalf("closed=%+v", closed)
				}
				if err := disk.Commit(next, closed); err != nil {
					t.Fatal(err)
				}
				disk = store.Store{Root: disk.Root}
			}
		})
	}
}

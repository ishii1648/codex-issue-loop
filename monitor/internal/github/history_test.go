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
		{name: "ready to running replacement", labels: `["running"]`, history: [][2]string{{"labeled", "ready"}, {"unlabeled", "ready"}, {"labeled", "running"}}, want: []model.EventKind{model.RunningLabeled, model.ReadyLabeled}, since: 3},
		{name: "running to ready replacement", labels: `["ready"]`, history: [][2]string{{"labeled", "running"}, {"unlabeled", "running"}, {"labeled", "ready"}}, want: []model.EventKind{model.ReadyLabeled, model.RunningLabeled}, since: 3},
		{name: "280 reopened with ready", labels: `["ready"]`, history: [][2]string{{"labeled", "ready"}, {"closed", ""}, {"reopened", ""}}, want: []model.EventKind{model.ReadyLabeled, model.QueueExited, model.ReadyLabeled}, since: 3},
		{name: "reopened with running", labels: `["running"]`, history: [][2]string{{"labeled", "running"}, {"closed", ""}, {"reopened", ""}}, want: []model.EventKind{model.RunningLabeled, model.ProcessingClosed, model.RunningLabeled}, since: 3},
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
		{"expired", `[{"number":459,"state":"open","labels":[{"name":"ready"}]}]`, `[{"id":2,"event":"labeled","label":{"name":"ready"},"created_at":"2026-09-06T10:00:00Z"}]`, model.Healthy},
		{"unproven", `[{"number":459,"state":"open","labels":[{"name":"ready"}]}]`, `[]`, model.Unknown},
		{"reopened", `[{"number":459,"state":"open","labels":[{"name":"running"}]}]`, `[{"id":2,"event":"labeled","label":{"name":"running"},"created_at":"2026-09-06T10:00:00Z"},{"id":3,"event":"closed","created_at":"2026-09-06T10:01:00Z"},{"id":4,"event":"reopened","created_at":"2026-09-06T10:02:00Z"}]`, model.Unknown},
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
				want := tc.want
				if tc.name == "expired" && poll > 0 {
					want = model.Down
				}
				if next.Current.Status != want || next.EventCursor != 4 {
					t.Fatalf("next=%+v", next)
				}
				if poll == 0 && tc.want != model.Unknown && (len(closed) != 1 || closed[0].Status != model.Unknown || !closed[0].EndedAt.Equal(at)) {
					t.Fatalf("closed=%+v", closed)
				}
				if tc.name == "expired" && !next.QueueDeadline.Equal(base.Add(11*time.Minute)) {
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
					want = model.Healthy
					if !next.QueueDeadline.Equal(base.Add(11 * time.Minute)) {
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

func TestSamePhaseRelabelPreservesWindow(t *testing.T) {
	base := time.Date(2026, 9, 6, 10, 0, 0, 0, time.UTC)
	for _, phase := range []model.Phase{model.Ready, model.Running} {
		t.Run(string(phase), func(t *testing.T) {
			label, kind := "ready", model.ReadyLabeled
			if phase == model.Running {
				label, kind = "running", model.RunningLabeled
			}
			repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running"}
			var issue rawIssue
			if err := json.Unmarshal([]byte(`{"number":1,"state":"open","labels":[{"name":"`+label+`"}]}`), &issue); err != nil {
				t.Fatal(err)
			}
			times := []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Hour)}
			history := make([]rawEvent, 3)
			for i, name := range []string{"labeled", "unlabeled", "labeled"} {
				history[i] = rawEvent{ID: int64(i + 1), Event: name, CreatedAt: times[i]}
				history[i].Label.Name = label
			}
			events, since, err := issueHistory(repo, issue, history, times[2])
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 1 || events[0].Kind != kind || !events[0].At.Equal(base) || !since.Equal(base) {
				t.Fatalf("events=%+v since=%s", events, since)
			}
			previous, _, err := model.Apply(nil, model.Observation{Repository: repo.Name, ObservedAt: base, Cursor: 1, CursorInitialized: true,
				Items: []model.QueueItem{{Number: 1, Phase: phase, PhaseSince: base, Deadline: base.Add(10 * time.Minute)}}})
			if err != nil {
				t.Fatal(err)
			}
			next, closed, err := model.Apply(&previous, model.Observation{Repository: repo.Name, ObservedAt: times[2], Cursor: 3, CursorInitialized: true,
				Events: events, Items: []model.QueueItem{{Number: 1, Phase: phase, PhaseSince: since}}, AcceptanceTimeout: 10 * time.Minute, ProcessingTimeout: 10 * time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if phase == model.Ready {
				if next.Current.Status != model.Down || !next.QueueDeadline.Equal(base.Add(10*time.Minute)) || len(closed) != 1 || !closed[0].EndedAt.Equal(base.Add(10*time.Minute)) {
					t.Fatalf("next=%+v closed=%+v", next, closed)
				}
			} else if next.Current.Status != model.Unknown || !next.QueueDeadline.IsZero() || len(closed) != 0 {
				t.Fatalf("next=%+v closed=%+v", next, closed)
			}
		})
	}
}

func TestClosedProgressAcrossPollsAndRestart(t *testing.T) {
	for _, mode := range []string{"split", "split labels", "failure", "close first", "same timestamp"} {
		t.Run(mode, func(t *testing.T) {
			base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
			repo := config.Repository{Name: "owner/repo", ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done"}, AcceptanceTimeout: config.Duration{Duration: 10 * time.Minute}, ProcessingTimeout: config.Duration{Duration: time.Hour}}
			dir := t.TempDir()
			script := filepath.Join(dir, "gh")
			if err := os.WriteFile(script, []byte(`#!/bin/sh
cd "$(dirname "$0")" || exit 1
case "$*" in
 *issues/events*) cat feed ;;
 *issues/1/events*) cat history1 ;;
 *issues/2/events*) cat history2 ;;
 *issues/1) cat issue1 ;;
 *issues\?*) cat issues ;;
 *) exit 9 ;;
esac
`), 0700); err != nil {
				t.Fatal(err)
			}
			write := func(name string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			var issues []rawIssue
			if err := json.Unmarshal([]byte(`[{"number":1,"state":"open","labels":[{"name":"running"}]},{"number":2,"state":"open","labels":[{"name":"ready"}]}]`), &issues); err != nil {
				t.Fatal(err)
			}
			histories := map[int][]rawEvent{}
			var feed []rawEvent
			add := func(number int, kind, label string, minute int) {
				e := rawEvent{ID: int64(len(feed) + 1), Event: kind, CreatedAt: base.Add(time.Duration(minute) * time.Minute)}
				e.Label.Name, e.Issue.Number = label, number
				feed = append([]rawEvent{e}, feed...)
				histories[number] = append(histories[number], e)
			}
			publish := func() {
				write("feed", feed)
				write("history1", [][]rawEvent{histories[1]})
				write("history2", [][]rawEvent{histories[2]})
				write("issue1", issues[0])
				open := issues
				if issues[0].State == "closed" {
					open = issues[1:]
				}
				write("issues", [][]rawIssue{open})
			}
			add(1, "labeled", "running", 0)
			add(2, "labeled", "ready", 0)
			publish()
			disk := store.Store{Root: filepath.Join(dir, "state")}
			var previous *model.Snapshot
			poll := func(minute int) model.Snapshot {
				t.Helper()
				cursor := int64(0)
				if previous != nil {
					cursor = previous.EventCursor
				}
				obs, err := (CLI{Path: script}).Observe(context.Background(), repo, cursor, previous != nil, base.Add(time.Duration(minute)*time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if obs.Resynchronized {
					t.Fatal("unexpected resynchronization")
				}
				next, closed, err := model.Apply(previous, obs)
				if err != nil {
					t.Fatal(err)
				}
				if err := disk.Commit(next, closed); err != nil {
					t.Fatal(err)
				}
				previous, err = (store.Store{Root: disk.Root}).Load(repo.Name)
				if err != nil {
					t.Fatal(err)
				}
				return next
			}
			if next := poll(1); next.Current.Status != model.Unknown {
				t.Fatalf("bootstrap=%+v", next)
			}
			add(2, "unlabeled", "ready", 2)
			add(2, "labeled", "running", 2)
			issues[1].Labels[0].Name = "running"
			if mode == "failure" {
				failed, closed, err := model.Apply(previous, model.Observation{Repository: repo.Name, ObservedAt: base.Add(3 * time.Minute), Error: "unavailable"})
				if err != nil {
					t.Fatal(err)
				}
				if err := disk.Commit(failed, closed); err != nil {
					t.Fatal(err)
				}
				previous, err = disk.Load(repo.Name)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				publish()
				if next := poll(3); !next.QueuePhaseSince.Equal(base.Add(2 * time.Minute)) {
					t.Fatalf("admission=%+v", next)
				}
			}
			closeMinute := 6
			if mode == "close first" {
				add(1, "closed", "", 4)
				closeMinute = 4
			}
			add(1, "unlabeled", "running", 4)
			if mode == "split labels" {
				issues[0].Labels = nil
				publish()
				if next := poll(4); !next.QueuePhaseSince.Equal(base.Add(2 * time.Minute)) {
					t.Fatalf("running removal extended progress=%+v", next)
				}
				issues[0].Labels = append(issues[0].Labels, issues[1].Labels[0])
			}
			add(1, "labeled", "done", 4)
			issues[0].Labels[0].Name = "done"
			if mode == "split" || mode == "split labels" {
				publish()
				if next := poll(5); !next.QueuePhaseSince.Equal(base.Add(2 * time.Minute)) {
					t.Fatalf("labels extended progress=%+v", next)
				}
			}
			if mode == "same timestamp" {
				closeMinute = 4
			}
			if mode != "close first" {
				add(1, "closed", "", closeMinute)
			}
			issues[0].State = "closed"
			publish()
			next := poll(7)
			if next.Current.Status != model.Healthy || !next.QueuePhaseSince.Equal(base.Add(time.Duration(closeMinute)*time.Minute)) || !next.QueueDeadline.Equal(next.QueuePhaseSince.Add(time.Hour)) {
				t.Fatalf("completion=%+v", next)
			}
			again := poll(8)
			if !again.QueueDeadline.Equal(next.QueueDeadline) {
				t.Fatalf("duplicate extended progress=%+v", again)
			}
		})
	}
}

func TestCloseRequiresRunningHistoryInSameExecution(t *testing.T) {
	base := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	repo := config.Repository{ReadyLabels: []string{"ready"}, RunningLabel: "running", TerminalLabels: []string{"done", "failed"}, ExcludeLabels: []string{"blocked"}}
	for _, tc := range []struct {
		name    string
		history [][2]string
		want    []int64
		pr      bool
	}{
		{name: "removed running", history: [][2]string{{"labeled", "running"}, {"unlabeled", "running"}, {"labeled", "done"}, {"closed", ""}}, want: []int64{4}},
		{name: "failed only", history: [][2]string{{"labeled", "running"}, {"labeled", "failed"}}},
		{name: "excluded only", history: [][2]string{{"labeled", "running"}, {"labeled", "blocked"}}},
		{name: "ready close", history: [][2]string{{"labeled", "ready"}, {"closed", ""}}},
		{name: "returned to ready", history: [][2]string{{"labeled", "running"}, {"unlabeled", "running"}, {"labeled", "ready"}, {"unlabeled", "ready"}, {"closed", ""}}},
		{name: "ready while excluded", history: [][2]string{{"labeled", "running"}, {"labeled", "blocked"}, {"unlabeled", "running"}, {"labeled", "ready"}, {"unlabeled", "ready"}, {"closed", ""}}},
		{name: "reopened without running", history: [][2]string{{"labeled", "running"}, {"unlabeled", "running"}, {"closed", ""}, {"reopened", ""}, {"closed", ""}}, want: []int64{3}},
		{name: "rerun", history: [][2]string{{"labeled", "running"}, {"unlabeled", "running"}, {"closed", ""}, {"reopened", ""}, {"labeled", "ready"}, {"unlabeled", "ready"}, {"labeled", "running"}, {"closed", ""}}, want: []int64{8, 3}},
		{name: "no running evidence", history: [][2]string{{"labeled", "done"}, {"closed", ""}}},
		{name: "PR", history: [][2]string{{"labeled", "running"}, {"closed", ""}}, pr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := rawIssue{Number: 1, State: "open"}
			if tc.pr {
				issue.PullRequest = &struct{}{}
			}
			labels := map[string]bool{}
			var history []rawEvent
			for i, entry := range tc.history {
				e := rawEvent{ID: int64(i + 1), Event: entry[0], CreatedAt: base}
				e.Label.Name = entry[1]
				switch entry[0] {
				case "labeled":
					labels[entry[1]] = true
				case "unlabeled":
					delete(labels, entry[1])
				case "closed":
					issue.State = "closed"
				case "reopened":
					issue.State = "open"
				}
				history = append(history, e, e)
			}
			for label := range labels {
				issue.Labels = append(issue.Labels, struct {
					Name string `json:"name"`
				}{label})
			}
			events, _, err := issueHistory(repo, issue, history, base)
			if err != nil {
				t.Fatal(err)
			}
			var completed []int64
			for _, e := range events {
				if e.Kind == model.ProcessingClosed {
					completed = append(completed, e.ID)
				}
			}
			if !reflect.DeepEqual(completed, tc.want) {
				t.Fatalf("completed=%v want=%v events=%+v", completed, tc.want, events)
			}
		})
	}
}

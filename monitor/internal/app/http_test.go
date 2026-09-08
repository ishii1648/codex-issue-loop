package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/monitor/internal/config"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/model"
	"github.com/ishii1648/codex-issue-loop/monitor/internal/store"
)

func dashboardFixture(t *testing.T) (config.Config, store.Store, time.Time) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("version: 1\nstate_dir: %q\nrepositories:\n  - name: owner/repo\n", root)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, store.Store{Root: root}, time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
}

func request(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", "http://127.0.0.1:19110"+path, nil))
	return recorder
}

func TestDashboardCLIParityAndReplay(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Down, StartedAt: at.Add(-time.Hour), Reason: "issue 42"}}
	closed := []model.Interval{{DecisionVersion: model.DecisionVersion, ID: "healthy", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-3 * time.Hour), EndedAt: at.Add(-time.Hour)}}
	if err := storage.Commit(snapshot, closed); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	for _, command := range []string{"status", "history", "report"} {
		args := []string{command, "--config", cfg.Path, "--json"}
		query := ""
		if command == "status" {
			args = append(args, "--at", at.Format(time.RFC3339))
			query = "?at=" + at.Format(time.RFC3339)
		} else {
			from := at.Add(-24 * time.Hour).Format(time.RFC3339)
			args = append(args, "--from", from, "--to", at.Format(time.RFC3339))
			query = "?from=" + from + "&to=" + at.Format(time.RFC3339)
		}
		var out, stderr bytes.Buffer
		cli := a
		cli.Out = &out
		cli.Err = &stderr
		if code := cli.Run(context.Background(), args); code != 0 {
			t.Fatal(stderr.String())
		}
		response := request(t, a.monitorHandler(cfg), "/api/"+command+query)
		if response.Code != 200 || response.Body.String() != out.String() {
			t.Fatalf("%s differs: %d %s versus %s", command, response.Code, response.Body.String(), out.String())
		}
	}
	metrics := request(t, a.monitorHandler(cfg), "/metrics")
	if metrics.Code != 200 || !strings.Contains(metrics.Body.String(), `agent_loop_monitor_demand_availability{repository="owner/repo",window="1d"} 0.6666666666666666`) || strings.Contains(metrics.Body.String(), "issue 42") || strings.Contains(metrics.Body.String(), " counter") {
		t.Fatal(metrics.Body.String())
	}
	for _, days := range []int{1, 7, 30} {
		expected := model.BuildReport(snapshot.Repository, append(closed, snapshot.Current), at.Add(-time.Duration(days)*24*time.Hour), at)
		if !strings.Contains(metrics.Body.String(), fmt.Sprintf("agent_loop_monitor_observation_coverage{repository=\"owner/repo\",window=\"%dd\"} %g", days, expected.ObservationCoverage)) {
			t.Fatal(metrics.Body.String())
		}
	}
	snapshot.Current.Status = model.Healthy
	snapshot.Current.Reason = "replayed"
	if err := storage.Commit(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	response := request(t, a.monitorHandler(cfg), "/api/history")
	if !strings.Contains(response.Body.String(), "replayed") || strings.Contains(response.Body.String(), "issue 42") {
		t.Fatal(response.Body.String())
	}
	metrics = request(t, a.monitorHandler(cfg), "/metrics")
	if !strings.Contains(metrics.Body.String(), `agent_loop_monitor_demand_availability{repository="owner/repo",window="1d"} 1`) {
		t.Fatal(metrics.Body.String())
	}
}

func TestDashboardStatesAndMissingData(t *testing.T) {
	for _, state := range []model.Status{model.Healthy, model.Down, model.Idle, model.Unknown, "missing", "stale", "corrupt"} {
		t.Run(string(state), func(t *testing.T) {
			cfg, storage, at := dashboardFixture(t)
			current := state
			if state == "stale" {
				current = model.Healthy
			}
			observed := at
			if state == "stale" {
				observed = at.Add(-10 * time.Minute)
			}
			snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: observed, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: current, StartedAt: observed.Add(-time.Hour)}}
			if state != "missing" {
				if err := storage.Commit(snapshot, nil); err != nil {
					t.Fatal(err)
				}
			}
			if state == "corrupt" {
				if err := os.WriteFile(filepath.Join(cfg.StateDir, "repositories/owner--repo/current.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			a := App{Now: func() time.Time { return at }}
			response := request(t, a.monitorHandler(cfg), "/metrics")
			if state == "corrupt" {
				if response.Code != 503 || strings.Contains(response.Body.String(), "agent_loop_monitor_state{") {
					t.Fatal(response)
				}
				return
			}
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			code := map[model.Status]int{model.Healthy: 1, model.Down: 2, model.Idle: 3}[state]
			if !strings.Contains(response.Body.String(), fmt.Sprintf("agent_loop_monitor_state{repository=\"owner/repo\"} %d\n", code)) {
				t.Fatal(response.Body.String())
			}
			if state == model.Idle || state == model.Unknown || state == "missing" {
				if !strings.Contains(response.Body.String(), `agent_loop_monitor_demand_availability{repository="owner/repo",window="1d"} -1`) {
					t.Fatal(response.Body.String())
				}
			}
			before, _ := storage.Load("owner/repo")
			timeline := request(t, a.monitorHandler(cfg), "/api/timeline?from="+at.Add(-24*time.Hour).Format(time.RFC3339)+"&to="+at.Format(time.RFC3339))
			var history struct {
				Repositories map[string][]model.Interval `json:"repositories"`
			}
			if err := json.Unmarshal(timeline.Body.Bytes(), &history); err != nil {
				t.Fatal(err)
			}
			cursor := at.Add(-24 * time.Hour)
			for _, interval := range history.Repositories["owner/repo"] {
				if !interval.StartedAt.Equal(cursor) || !interval.EndedAt.After(cursor) {
					t.Fatal(history)
				}
				cursor = interval.EndedAt
			}
			if !cursor.Equal(at) {
				t.Fatal(history)
			}
			after, _ := storage.Load("owner/repo")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("read mutated store")
			}
		})
	}
}

func TestDashboardReadOnlyBoundary(t *testing.T) {
	cfg, _, at := dashboardFixture(t)
	a := App{Now: func() time.Time { return at }}
	handler := a.monitorHandler(cfg)
	for _, tc := range []struct {
		method, url string
		code        int
	}{
		{"POST", "http://127.0.0.1:19110/api/status", 405},
		{"POST", "http://127.0.0.1:19110/api/details", 405},
		{"GET", "http://attacker.example/api/details", 403},
		{"GET", "http://127.0.0.1:19110/api/details?config=/etc/passwd", 400},
		{"GET", "http://127.0.0.1:19110/api/details?repo=unknown/repo", 503},
		{"GET", "http://attacker.example/api/status", 403},
		{"GET", "http://127.0.0.1:19110/api/status?config=/etc/passwd", 400},
		{"GET", "http://127.0.0.1:19110/api/status?at=invalid", 503},
		{"GET", "http://127.0.0.1:19110/api/report?from=2026-09-07T00:00:00Z&to=2026-09-06T00:00:00Z", 503},
		{"GET", "http://127.0.0.1:19110/unknown", 404},
	} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest(tc.method, tc.url, nil))
		if r.Code != tc.code {
			t.Fatalf("%s: %d", tc.url, r.Code)
		}
	}
	for _, addr := range []string{"0.0.0.0:19110", "localhost:19110", "192.168.1.2:19110", "[::]:19110"} {
		if err := a.serve(context.Background(), []string{"--listen", addr, "--config", cfg.Path}); err == nil {
			t.Fatalf("allowed %s", addr)
		}
	}
}

func TestStatusReferenceMustNotPrecedeObservation(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Healthy, StartedAt: at}}
	if err := storage.Commit(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	response := request(t, a.monitorHandler(cfg), "/api/status?at="+at.Add(-time.Second).Format(time.RFC3339))
	if response.Code != 503 {
		t.Fatal(response.Body.String())
	}
}

func TestDashboardRejectsPartiallyCommittedIntervals(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-time.Hour)}}
	closed := snapshot.Current
	closed.EndedAt = at
	if err := storage.Commit(snapshot, []model.Interval{closed}); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	for _, path := range []string{"/metrics", "/api/history", "/api/report?from=" + at.Add(-24*time.Hour).Format(time.RFC3339)} {
		response := request(t, a.monitorHandler(cfg), path)
		if response.Code != 503 {
			t.Fatalf("%s = %d %s", path, response.Code, response.Body.String())
		}
	}
}

func TestDashboardRejectsGapDuringReplayCommit(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Unknown, StartedAt: at.Add(-time.Hour + 3*time.Minute)}}
	closed := model.Interval{DecisionVersion: model.DecisionVersion, ID: "healthy", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-2 * time.Hour), EndedAt: at.Add(-time.Hour)}
	if err := storage.Commit(snapshot, []model.Interval{closed}); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	for _, path := range []string{"/metrics", "/api/history", "/api/report?from=" + at.Add(-24*time.Hour).Format(time.RFC3339)} {
		t.Run(path, func(t *testing.T) {
			response := request(t, a.monitorHandler(cfg), path)
			if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "monitor interval commit is incomplete") {
				t.Fatalf("%s = %d %s", path, response.Code, response.Body.String())
			}
		})
	}
}

func TestMetricsUsesExactWholeSecondReference(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-time.Hour)}}
	if err := storage.Commit(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at.Add(987654321 * time.Nanosecond) }}
	response := request(t, a.monitorHandler(cfg), "/metrics")
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	expected := model.BuildReport(snapshot.Repository, []model.Interval{snapshot.Current}, at.Add(-24*time.Hour), at)
	for _, value := range []string{
		fmt.Sprintf("agent_loop_monitor_reference_time_seconds %d\n", at.Unix()),
		`agent_loop_monitor_state_duration_seconds{repository="owner/repo"} 3600` + "\n",
		fmt.Sprintf("agent_loop_monitor_observation_coverage{repository=\"owner/repo\",window=\"1d\"} %g\n", expected.ObservationCoverage),
	} {
		if !strings.Contains(response.Body.String(), value) {
			t.Fatalf("missing %s in %s", value, response.Body.String())
		}
	}
}

func TestHistoryRejectsMissingCurrentDuringCommit(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: model.Down, StartedAt: at}}
	if err := storage.Commit(snapshot, []model.Interval{{DecisionVersion: model.DecisionVersion, ID: "closed", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-time.Hour), EndedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(storage.Root, "repositories/owner--repo/current.json")); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	for _, endpoint := range []string{"/api/history", "/api/report", "/api/timeline", "/metrics"} {
		response := request(t, a.monitorHandler(cfg), endpoint)
		if response.Code != 503 {
			t.Fatalf("%s: %d %s", endpoint, response.Code, response.Body.String())
		}
	}
}

type dashboardTransport func(*http.Request) (*http.Response, error)

func (f dashboardTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFreshnessQueriesGrafanaDatasourceAndFailsClosed(t *testing.T) {
	cfg, _, at := dashboardFixture(t)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	for _, status := range []int{200, 502} {
		http.DefaultTransport = dashboardTransport(func(r *http.Request) (*http.Response, error) {
			if r.Method != "GET" || r.URL.Host != "127.0.0.1:13000" || r.URL.Path != "/api/datasources/proxy/uid/monitor-prometheus/api/v1/query" {
				t.Fatalf("unexpected upstream: %s", r.URL)
			}
			if r.URL.Query().Get("query") != `agent_loop_monitor_reference_time_seconds and on() (up{job="agent-loop-monitor"} == 1)` {
				t.Fatal(r.URL.RawQuery)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"result":[]}}`))}, nil
		})
		response := request(t, (App{Now: func() time.Time { return at }}).monitorHandler(cfg), "/api/freshness?query=ignored")
		want := 200
		if status != 200 {
			want = 503
		}
		if response.Code != want {
			t.Fatal(response)
		}
	}
	http.DefaultTransport = dashboardTransport(func(r *http.Request) (*http.Response, error) { return nil, fmt.Errorf("offline") })
	response := request(t, (App{}).monitorHandler(cfg), "/api/freshness")
	if response.Code != 503 {
		t.Fatal(response)
	}
	page := request(t, (App{}).monitorHandler(cfg), "/")
	if page.Code != 200 || !strings.Contains(page.Body.String(), `body class="expired"`) {
		t.Fatal(page)
	}
}

func TestDashboardDetailTargets(t *testing.T) {
	data, err := os.ReadFile("../../dashboard/dashboard.json")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard struct {
		Panels []struct {
			Type    string
			Targets []struct {
				URL          string
				Parser       string
				RootSelector string `json:"root_selector"`
				Columns      []struct{ Selector, Text, Type string }
			}
			FieldConfig struct {
				Overrides []struct {
					Matcher    struct{ Options string }
					Properties []struct {
						ID    string
						Value json.RawMessage
					}
				}
			}
		}
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	tables := 0
	for _, panel := range dashboard.Panels {
		if panel.Type != "table" {
			continue
		}
		tables++
		target := panel.Targets[0]
		if target.Parser != "backend" || target.RootSelector != "rows" {
			t.Fatalf("expected simple backend rows selector: %+v", target)
		}
		u, err := url.Parse(target.URL)
		if err != nil {
			t.Fatal(err)
		}
		repo := u.Query().Get("repo")
		link := ""
		for _, override := range panel.FieldConfig.Overrides {
			if override.Matcher.Options != "Issue" {
				continue
			}
			for _, property := range override.Properties {
				if property.ID != "links" {
					continue
				}
				var links []struct{ URL string }
				if err := json.Unmarshal(property.Value, &links); err != nil {
					t.Fatal(err)
				}
				link = links[0].URL
			}
		}
		for _, scenario := range []string{"multiple", "down-empty", "idle", "unknown", "missing", "stale"} {
			t.Run(repo+"/"+scenario, func(t *testing.T) {
				cfg, storage, at := dashboardFixture(t)
				configData, err := os.ReadFile(cfg.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.Path, bytes.ReplaceAll(configData, []byte("owner/repo"), []byte(repo)), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err = config.Load(cfg.Path)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: repo, LastObservationAt: at, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: repo, Status: model.Down, StartedAt: at.Add(-time.Hour), Reason: "queue progress deadline exceeded"}}
				wantStatus, wantDetail := "DOWN", snapshot.Current.Reason
				numbers := []int{}
				switch scenario {
				case "multiple":
					numbers = []int{239, 271, 297}
					snapshot.QueueDeadline = at.Add(-time.Minute)
					for i, number := range numbers {
						snapshot.Queue = append(snapshot.Queue, model.QueueItem{Number: number, Phase: model.Ready, PhaseSince: at.Add(-time.Hour), Deadline: at.Add(time.Duration(i) * time.Minute)})
					}
				case "idle":
					snapshot.Current.Status, snapshot.Current.Reason = model.Idle, "queue empty"
					wantStatus, wantDetail = "IDLE", "queue empty"
				case "unknown":
					snapshot.Current.Status, snapshot.Current.Reason = model.Unknown, "observation failed"
					snapshot.LastError = "GitHub unavailable"
					wantStatus, wantDetail = "UNKNOWN", "observation failed\n観測エラー: GitHub unavailable"
				case "missing":
					wantStatus, wantDetail = "UNKNOWN", "no observations recorded"
				case "stale":
					snapshot.LastObservationAt = at.Add(-time.Hour)
					wantStatus, wantDetail = "UNKNOWN", "monitor observation history has a gap\n観測エラー: monitor observation history has a gap"
				}
				if scenario != "missing" {
					if err := storage.Commit(snapshot, nil); err != nil {
						t.Fatal(err)
					}
				}
				before, err := storage.Load(repo)
				if err != nil {
					t.Fatal(err)
				}
				response := request(t, (App{Now: func() time.Time { return at }}).monitorHandler(cfg), u.RequestURI())
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				var payload map[string][]map[string]any
				if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				rows := payload[target.RootSelector]
				wantCount := len(numbers)
				if wantCount == 0 {
					wantCount = 1
				}
				if len(rows) != wantCount {
					t.Fatalf("rows = %+v", rows)
				}
				for i, row := range rows {
					cells := map[string]any{}
					for _, column := range target.Columns {
						value, exists := row[column.Selector]
						if !exists {
							t.Fatalf("missing column %s", column.Selector)
						}
						cells[column.Text] = value
						if value != nil && column.Type == "timestamp" {
							if _, err := time.Parse(time.RFC3339, value.(string)); err != nil {
								t.Fatal(err)
							}
						}
					}
					want := map[string]any{"状態": wantStatus, "理由 / 観測エラー": wantDetail, "Issue": nil, "queue期限": nil, "Issue期限": nil}
					if len(numbers) > 0 {
						want["Issue"] = float64(numbers[i])
						want["queue期限"] = snapshot.QueueDeadline.Format(time.RFC3339)
						want["Issue期限"] = snapshot.Queue[i].Deadline.Format(time.RFC3339)
						actualLink := strings.ReplaceAll(link, "${__value.raw}", strconv.Itoa(int(cells["Issue"].(float64))))
						if actualLink != fmt.Sprintf("https://github.com/%s/issues/%d", repo, numbers[i]) {
							t.Fatal(actualLink)
						}
					}
					if !reflect.DeepEqual(cells, want) {
						t.Fatalf("cells = %+v, want %+v", cells, want)
					}
				}
				after, err := storage.Load(repo)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatal("details read mutated store", err)
				}
			})
		}
	}
	if tables != 2 {
		t.Fatalf("table count = %d", tables)
	}
}

func TestSelectedPeriodTimelineMatchesCLIReport(t *testing.T) {
	for _, scenario := range []string{"mixed", "HEALTHY", "DOWN", "IDLE", "UNKNOWN", "missing", "stale"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, storage, at := dashboardFixture(t)
			observed := at
			if scenario == "stale" {
				observed = at.Add(-10 * time.Minute)
			}
			states := []model.Status{model.Idle, model.Healthy, model.Down, model.Unknown}
			if scenario != "mixed" {
				state := model.Status(scenario)
				if scenario == "stale" || scenario == "missing" {
					state = model.Healthy
				}
				states = []model.Status{state, state, state, state}
			}
			var intervals []model.Interval
			for i, state := range states {
				start := observed.Add(time.Duration(i-4) * time.Hour)
				intervals = append(intervals, model.Interval{DecisionVersion: model.DecisionVersion, ID: fmt.Sprint(i), Repository: "owner/repo", Status: state, StartedAt: start, EndedAt: start.Add(time.Hour)})
			}
			snapshot := model.Snapshot{DecisionVersion: model.DecisionVersion, SchemaVersion: model.SchemaVersion, Repository: "owner/repo", LastObservationAt: observed, Current: model.Interval{DecisionVersion: model.DecisionVersion, ID: "current", Repository: "owner/repo", Status: states[3], StartedAt: observed}}
			if scenario != "missing" {
				if err := storage.Commit(snapshot, intervals); err != nil {
					t.Fatal(err)
				}
			}
			for _, bounds := range [][2]time.Time{{at.Add(-24 * time.Hour), at}, {at.Add(-7 * 24 * time.Hour), at}, {at.Add(-30 * 24 * time.Hour), at}, {at.Add(-3 * time.Hour), at.Add(-2 * time.Hour)}} {
				from, to := bounds[0], bounds[1]
				var stdout, stderr bytes.Buffer
				a := App{Out: &stdout, Err: &stderr, Now: func() time.Time { return at }}
				if code := a.Run(context.Background(), []string{"report", "--config", cfg.Path, "--json", "--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339)}); code != 0 {
					t.Fatal(stderr.String())
				}
				var report struct{ Reports []model.Report }
				if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				query := "?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
				response := request(t, a.monitorHandler(cfg), "/api/timeline"+query)
				var timeline struct{ Repositories map[string][]model.Interval }
				if response.Code != 200 {
					t.Fatal(response.Body.String())
				}
				if err := json.Unmarshal(response.Body.Bytes(), &timeline); err != nil {
					t.Fatal(err)
				}
				actual := model.BuildReport("owner/repo", timeline.Repositories["owner/repo"], from, to)
				expected := report.Reports[0]
				known := expected.DurationsSeconds[model.Healthy] + expected.DurationsSeconds[model.Down] + expected.DurationsSeconds[model.Idle]
				expected.DurationsSeconds[model.Unknown] = to.Sub(from).Seconds() - known
				if !reflect.DeepEqual(actual, expected) {
					t.Fatalf("timeline %+v differs from CLI report %+v", actual, expected)
				}
			}
		})
	}
}

func TestLegacyContractIsSeparatedAcrossDashboardAndCLI(t *testing.T) {
	cfg, storage, at := dashboardFixture(t)
	legacy := model.Snapshot{SchemaVersion: 1, Repository: "owner/repo", LastObservationAt: at.Add(-time.Hour),
		Current: model.Interval{ID: "legacy", Repository: "owner/repo", Status: model.Healthy, StartedAt: at.Add(-3 * time.Hour)}}
	if err := storage.Commit(legacy, nil); err != nil {
		t.Fatal(err)
	}
	a := App{Now: func() time.Time { return at }}
	response := request(t, a.monitorHandler(cfg), "/api/status")
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"status":"UNKNOWN"`) || !strings.Contains(response.Body.String(), "legacy decision contract") {
		t.Fatal(response.Body.String())
	}
	obs := model.Observation{Repository: legacy.Repository, ObservedAt: at.Add(-30 * time.Minute), Cursor: 1, CursorInitialized: true, Items: []model.QueueItem{{Number: 1, Phase: model.Ready, PhaseSince: at.Add(-time.Hour), Deadline: at.Add(-40 * time.Minute)}}}
	next, closed, err := model.Apply(&legacy, obs)
	if err != nil {
		t.Fatal(err)
	}
	obs.ObservedAt = at
	var subsequent []model.Interval
	next, subsequent, err = model.Apply(&next, obs)
	if err != nil {
		t.Fatal(err)
	}
	closed = append(closed, subsequent...)
	if err := storage.Commit(next, closed); err != nil {
		t.Fatal(err)
	}
	from := at.Add(-3 * time.Hour)
	query := "?from=" + from.Format(time.RFC3339) + "&to=" + at.Format(time.RFC3339)
	var output, stderr bytes.Buffer
	cli := a
	cli.Out, cli.Err = &output, &stderr
	if code := cli.Run(context.Background(), []string{"report", "--config", cfg.Path, "--json", "--from", from.Format(time.RFC3339), "--to", at.Format(time.RFC3339)}); code != 0 {
		t.Fatal(stderr.String())
	}
	response = request(t, a.monitorHandler(cfg), "/api/report"+query)
	if response.Code != 200 || response.Body.String() != output.String() {
		t.Fatalf("API=%s CLI=%s", response.Body.String(), output.String())
	}
	var reports struct {
		Reports []model.Report `json:"reports"`
	}
	if err := json.Unmarshal(output.Bytes(), &reports); err != nil {
		t.Fatal(err)
	}
	report := reports.Reports[0]
	if report.LegacySeconds != 9000 || report.DurationsSeconds[model.Unknown] != 9000 || report.DurationsSeconds[model.Down] != 600 || report.DurationsSeconds[model.Healthy] != 1200 || report.DemandAvailability == nil || *report.DemandAvailability != float64(2)/3 {
		t.Fatalf("report=%+v", report)
	}
	response = request(t, a.monitorHandler(cfg), "/api/timeline"+query)
	var timeline struct {
		Repositories map[string][]model.Interval `json:"repositories"`
	}
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &timeline); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(model.BuildReport(legacy.Repository, timeline.Repositories[legacy.Repository], from, at), report) {
		t.Fatalf("timeline=%s report=%+v", response.Body.String(), report)
	}
	if !strings.Contains(response.Body.String(), "status=HEALTHY") {
		t.Fatal(response.Body.String())
	}
	history := request(t, a.monitorHandler(cfg), "/api/history"+query)
	if !strings.Contains(history.Body.String(), `"status":"HEALTHY"`) {
		t.Fatal(history.Body.String())
	}
	metrics := request(t, a.monitorHandler(cfg), "/metrics")
	if metrics.Code != 200 || !strings.Contains(metrics.Body.String(), `agent_loop_monitor_state{repository="owner/repo"} 2`) || !strings.Contains(metrics.Body.String(), `agent_loop_monitor_demand_availability{repository="owner/repo",window="1d"} 0.6666666666666666`) {
		t.Fatal(metrics.Body.String())
	}
}
